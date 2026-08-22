package credential

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/dkam/silo/fileserver/account"
	"github.com/dkam/silo/fileserver/option"
)

var (
	readDB  *sql.DB
	writeDB *sql.DB
)

// Init wires the package to the database, following the same shape as
// authmgr.Init.
func Init(siloReadDB, siloWriteDB *sql.DB) {
	readDB = siloReadDB
	writeDB = siloWriteDB
}

// Resolve's errors. Callers log these to tell one failure from another and
// must not report them to a client: see ErrInvalid.
var (
	// ErrMissing means no credential was presented at all. It is separate from
	// ErrInvalid because it is the honest answer to an anonymous request, and a
	// handler may want to fall through to a public lane rather than refuse.
	ErrMissing = errors.New("no credential presented")

	// ErrMalformed means the string could not be a credential — bad prefix,
	// wrong length, failed checksum. Nothing was looked up, so nothing about
	// any credential's existence has been learned.
	ErrMalformed = errors.New("malformed credential")

	// ErrInvalid covers both "no such credential" and "wrong secret", and it
	// covers them deliberately. Separating them would answer the question "is
	// this id real?" for anyone who cares to ask, which is the enumeration
	// oracle docs/auth.md warns about. The lookup path is written to take the
	// same work either way.
	ErrInvalid = errors.New("invalid credential")

	// ErrWrongKind means a credential for another lane was presented. The
	// kind is public and travels in the token, so saying so leaks nothing that
	// the client did not already send.
	ErrWrongKind = errors.New("credential is for another lane")

	// ErrExpired and ErrInactive are the two ways a real, correctly proven
	// credential still fails.
	ErrExpired  = errors.New("credential has expired")
	ErrInactive = errors.New("account is not active")

	// ErrProofUnsupported means the row carries no proof material at all —
	// neither a secret hash nor a public key. An s3 row is the intended case:
	// its secret is derived from the master key and proven by SigV4
	// elsewhere, so reaching prove() with one is a row in the wrong lane.
	ErrProofUnsupported = errors.New("credential cannot be proven that way")

	// ErrSignatureNotImplemented marks the proof-of-possession branch, which
	// needs an RFC 9421 verifier that does not exist yet. It is a distinct
	// error so that a row with a public key fails loudly during development
	// rather than quietly falling through to some weaker check.
	ErrSignatureNotImplemented = errors.New("signature verification is not implemented")
)

// Credential is one row of the Credential table.
//
// The proof material is unexported. Nothing outside this package needs the
// stored hash or the public key, and keeping them off the struct's surface
// means they cannot reach a log line through a %+v.
type Credential struct {
	ID        string
	Kind      Kind
	AccountID account.ID
	Label     string
	Scope     Scope
	Perm      string
	ClientID  string

	Ctime     int64
	ExpiresAt int64 // 0 = no expiry
	LastUsed  int64 // 0 = never used

	secretHash []byte
	publicKey  []byte
}

// Bearer reports whether the credential is proven by presenting a secret
// rather than by signing the request.
func (c *Credential) Bearer() bool { return len(c.secretHash) > 0 }

// Resolve is the single verification path: every lane goes through it. See
// accountIsActive for what that buys.
//
// It parses the presented credential, verifies its checksum, looks the row up
// by id, joins the account and checks is_active, checks expiry, proves the
// secret, and stamps last_used.
func Resolve(r *http.Request, kind Kind) (*Credential, error) {
	tok, err := tokenFromRequest(r)
	if err != nil {
		return nil, err
	}
	if tok.Kind != kind {
		return nil, ErrWrongKind
	}

	ctx, cancel := context.WithTimeout(r.Context(), option.DBOpTimeout)
	defer cancel()

	cred, err := load(ctx, tok.ID)
	if err != nil {
		return nil, err
	}

	if err := prove(cred, tok); err != nil {
		return nil, err
	}

	// The stored kind is authoritative. A token whose kind was edited fails
	// its checksum long before here, so this fires only if the row and the
	// token were minted inconsistently — but the row is the thing that says
	// what a credential is allowed to be, so it gets the last word.
	if cred.Kind != tok.Kind {
		return nil, ErrWrongKind
	}

	// Expiry and the account check come after the proof, so a caller who
	// cannot prove the credential does not learn that an id they guessed
	// belongs to a disabled account.
	if cred.ExpiresAt != 0 && cred.ExpiresAt <= time.Now().Unix() {
		return nil, ErrExpired
	}

	// The check that could not be retrofitted. Disabling an account has to
	// kill every lane at once, and it only does if every lane asks — which is
	// what having one Resolve buys, and what three separate token stores made
	// impossible.
	active, err := account.IsActive(ctx, cred.AccountID)
	if err != nil {
		return nil, err
	}
	if !active {
		return nil, ErrInactive
	}

	stampLastUsed(cred)
	return cred, nil
}

// tokenFromRequest pulls the credential out of the request without touching
// the database.
//
//	Authorization: Bearer silo_<kind>_…   every Silo client
//	Authorization: Token  <40 hex>        SeaDrive and Seafile Desktop on /api2
//	Authorization: Silo   <credential-id> proof of possession, signature below
func tokenFromRequest(r *http.Request) (Token, error) {
	h := r.Header.Get("Authorization")
	if h == "" {
		return Token{}, ErrMissing
	}

	scheme, rest, ok := strings.Cut(h, " ")
	if !ok {
		return Token{}, ErrMalformed
	}
	rest = strings.TrimSpace(rest)

	var parse func(string) (Token, error)
	switch {
	case strings.EqualFold(scheme, "Bearer"):
		parse = ParseToken

	case strings.EqualFold(scheme, "Token"):
		parse = ParseLegacyToken

	case strings.EqualFold(scheme, "Silo"):
		// Proof of possession. The credential id travels in the clear and the
		// proof is in Signature/Signature-Input, so there is no secret to
		// parse — but verifying it needs an RFC 9421 signature base, which is
		// its own piece of work. Failing here is deliberate: a half-verifier
		// that checks the id and not the signature would be a bearer scheme
		// wearing a signature's name.
		return Token{}, ErrSignatureNotImplemented

	default:
		return Token{}, ErrMalformed
	}

	tok, err := parse(rest)
	if err != nil {
		// The parser's message says which part of the string was wrong. It
		// describes the presented text only, so it carries nothing about
		// whether any credential exists.
		return Token{}, fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	return tok, nil
}

// zeroHash gives the not-found path something to compare against, so a lookup
// that misses does the same work as one that hits. Without it, "no such
// credential" returns measurably sooner than "wrong secret" and the pair
// becomes an oracle for which ids exist.
var zeroHash = make([]byte, sha256.Size)

func load(ctx context.Context, id string) (*Credential, error) {
	const q = `SELECT id, kind, secret_hash, public_key, account_id, label, scope, perm,
	                  client_id, ctime, expires_at, last_used
	           FROM Credential WHERE id = ?`

	var (
		c        Credential
		kind     string
		scope    string
		clientID sql.NullString
		expires  sql.NullInt64
		lastUsed sql.NullInt64
	)
	err := readDB.QueryRowContext(ctx, q, id).Scan(
		&c.ID, &kind, &c.secretHash, &c.publicKey, &c.AccountID, &c.Label, &scope, &c.Perm,
		&clientID, &c.Ctime, &expires, &lastUsed)
	if err == sql.ErrNoRows {
		subtle.ConstantTimeCompare(zeroHash, zeroHash)
		return nil, ErrInvalid
	}
	if err != nil {
		return nil, fmt.Errorf("looking up credential: %v", err)
	}

	c.Kind = Kind(kind)
	c.ClientID = clientID.String
	c.ExpiresAt = expires.Int64
	c.LastUsed = lastUsed.Int64

	c.Scope, err = ParseScope(scope)
	if err != nil {
		// A scope that will not parse is a corrupt row, not a bad request.
		// Refusing is the only safe reading: the alternative is treating an
		// unreadable narrowing as no narrowing at all.
		return nil, fmt.Errorf("credential %s has an unparseable scope: %v", id, err)
	}
	return &c, nil
}

func prove(c *Credential, tok Token) error {
	switch {
	case len(c.publicKey) > 0:
		return ErrSignatureNotImplemented
	case len(c.secretHash) == 0:
		// Neither hash nor key. An s3 row derives its secret from the master
		// key and is proven by SigV4 elsewhere; anything else is a row that
		// cannot be proven at all.
		return ErrProofUnsupported
	}

	presented := tok.SecretHash()
	if subtle.ConstantTimeCompare(presented, c.secretHash) != 1 {
		return ErrInvalid
	}
	return nil
}

// lastUsedGranularity is how stale last_used is allowed to get before it is
// rewritten.
//
// Stamping on every request would put a write transaction in front of every
// read, which on SQLite means serialising an otherwise concurrent workload
// behind an always-on mount's polling. auth.md wants last_used so that an
// operator revoking a credential is making a decision rather than a guess, and
// five-minute granularity answers that question exactly as well as
// per-request precision does.
const lastUsedGranularity = 5 * time.Minute

func stampLastUsed(c *Credential) {
	now := time.Now().Unix()
	if now-c.LastUsed < int64(lastUsedGranularity.Seconds()) {
		return
	}

	// Best effort and deliberately not in the request's context: the client
	// has already been authenticated, and failing their request because a
	// bookkeeping write lost a race would be the wrong trade.
	ctx, cancel := context.WithTimeout(context.Background(), option.DBOpTimeout)
	defer cancel()

	if _, err := writeDB.ExecContext(ctx,
		"UPDATE Credential SET last_used = ? WHERE id = ?", now, c.ID); err != nil {
		return
	}
	c.LastUsed = now
}

// EffectivePerm intersects what the account may do with what the credential
// allows, per docs/auth.md's ceiling rule. A credential can only ever narrow:
// it cannot outlive or exceed the account behind it, and if the account's own
// permission is withdrawn the credential follows immediately.
//
// accountPerm is what share.CheckPerm returned for this user and library.
func (c *Credential) EffectivePerm(accountPerm, repoID, path string) string {
	if !c.Scope.Covers(repoID, path) {
		return ""
	}
	return minPerm(accountPerm, c.Perm)
}

// minPerm returns the weaker of two permissions, spelled the way
// share.CheckPerm spells them: "" for none, "r" for read, "rw" for read-write.
//
// An unrecognised permission ranks as no access rather than as read-write. A
// typo in a column should cost a user their access and be reported, not
// silently grant more than either side intended.
func minPerm(a, b string) string {
	if permRank(b) < permRank(a) {
		a = b
	}
	if permRank(a) == 0 {
		// Unrecognised, so not spellable as anything but no access.
		return ""
	}
	return a
}

func permRank(p string) int {
	switch p {
	case "r":
		return 1
	case "rw":
		return 2
	default:
		return 0
	}
}
