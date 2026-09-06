package credential

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"slices"
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

	// acct comes from the join in load, not from the Credential row. It is
	// unexported, and read through Account, because it is a snapshot of the
	// account at the moment of the read rather than a property of the
	// credential.
	acct *account.Account
}

// Account is the account the credential belongs to, as it stood when the
// credential resolved.
//
// It comes out of the same query the credential does, which is the whole
// reason load joins rather than looking the account up afterwards: every
// authenticated request needs both, and doing it in two would make a second
// round trip per request structural rather than incidental.
func (c *Credential) Account() *account.Account { return c.acct }

// Bearer reports whether the credential is proven by presenting a secret
// rather than by signing the request.
func (c *Credential) Bearer() bool { return len(c.secretHash) > 0 }

// Resolve is the single verification path: every lane goes through it. See
// accountIsActive for what that buys.
//
// It parses the presented credential, verifies its checksum, looks the row up
// by id, joins the account and checks is_active, checks expiry, proves the
// secret, and stamps last_used.
//
// kinds is the set of lanes the caller accepts. The API routes take a session
// credential from the CLI and a device credential from silo-drive, so the question
// is which of several rather than which one -- but it is still a question the
// caller has to answer, and an empty set matches nothing: a caller that named
// no kind forgot to say which lane it was, and reading that as "any lane will
// do" would accept a credential from the wrong one.
// ResolveInvite verifies an invite token presented as a string.
//
// It exists because Resolve cannot serve this lane, and the reason is not an
// oversight in either: Resolve refuses a credential whose account is inactive,
// and an invite's account is inactive by definition -- being inactive is the
// state redeeming changes. Loosening that check inside Resolve would loosen it
// for every lane, to buy one.
//
// Everything else Resolve checks still applies here: the token's checksum, the
// proof of the secret, the stored kind having the last word, and the expiry.
// What is deliberately absent is the is_active test, and nothing else.
//
// It takes a token rather than a request, so it cannot be reached by the header
// path at all. That, and the API's kind list excluding invite, are the two
// things that keep a leaked invite from being a session token for the address
// it names: one route consumes it, and it is not this package's job to find it.
func ResolveInvite(ctx context.Context, token string) (*Credential, error) {
	tok, err := ParseToken(token)
	if err != nil {
		return nil, err
	}
	if tok.Kind != KindInvite {
		return nil, ErrWrongKind
	}
	cred, err := load(ctx, tok.ID)
	if err != nil {
		return nil, err
	}
	if err := prove(cred, tok); err != nil {
		return nil, err
	}
	if cred.Kind != tok.Kind {
		return nil, ErrWrongKind
	}
	if cred.ExpiresAt != 0 && cred.ExpiresAt <= time.Now().Unix() {
		return nil, ErrExpired
	}
	return cred, nil
}

func Resolve(r *http.Request, kinds ...Kind) (*Credential, error) {
	tok, err := tokenFromRequest(r)
	if err != nil {
		return nil, err
	}
	if !slices.Contains(kinds, tok.Kind) {
		return nil, ErrWrongKind
	}

	ctx, cancel := option.WithDBTimeout(r.Context())
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
	if err := stillGood(cred); err != nil {
		return nil, err
	}

	// Detached, not merely given a detached context. The write goes to a pool
	// of one connection (dbutil sets SetMaxOpenConns(1)), so calling it inline
	// queued this bookkeeping UPDATE behind every commit, upload and GC write
	// on the server while the request waited -- with a 60s timeout in front of
	// it. The comment below always claimed it was out of the request's way;
	// now it is.
	stampLastUsed(cred)
	return cred, nil
}

// stillGood is the part of resolving that is about the row rather than the
// proof: not expired, and the account behind it active.
//
// The account check is the one that could not be retrofitted. Disabling an
// account has to kill every lane at once, and it only does if every lane asks
// -- which is what having one Resolve buys, and what three separate token
// stores made impossible. load reads it alongside the credential row.
func stillGood(cred *Credential) error {
	if cred.ExpiresAt != 0 && cred.ExpiresAt <= time.Now().Unix() {
		return ErrExpired
	}
	if !cred.acct.IsActive {
		return ErrInactive
	}
	return nil
}

// tokenFromRequest pulls the credential out of the request without touching
// the database.
//
//	Authorization: Bearer silo_<kind>_…   every Silo client
//	Authorization: Silo   <credential-id> proof of possession, signature below
//
// Any other scheme is malformed. Nothing is looked up, so presenting a
// credential from some other system says nothing about what this one holds.
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
	// account_id is a declared foreign key with an index, so the whole account
	// comes out of the same row read rather than a second round trip. Resolve
	// is the path every request takes, so the join is the difference between
	// one query per authenticated call and two.
	//
	// AccountEmail is joined for the primary address, which is still what a
	// commit records as its author and what a response body hands back. The
	// join is inner and matches account.ByID exactly, so an account with no
	// primary address is unresolvable here for the same reason it is
	// unreadable there -- one rule, not two.
	const q = `SELECT c.id, c.kind, c.secret_hash, c.public_key, c.account_id, c.label,
	                  c.scope, c.perm, c.client_id, c.ctime, c.expires_at, c.last_used,
	                  a.is_active, a.role, e.email
	           FROM Credential c
	           JOIN Account a ON a.id = c.account_id
	           JOIN AccountEmail e ON e.account_id = a.id AND e.is_primary = 1
	           WHERE c.id = ?`

	var (
		c        Credential
		acct     account.Account
		kind     string
		scope    string
		clientID sql.NullString
		expires  sql.NullInt64
		lastUsed sql.NullInt64
	)
	err := readDB.QueryRowContext(ctx, q, id).Scan(
		&c.ID, &kind, &c.secretHash, &c.publicKey, &c.AccountID, &c.Label, &scope, &c.Perm,
		&clientID, &c.Ctime, &expires, &lastUsed, &acct.IsActive, &acct.Role, &acct.Email)
	if err == sql.ErrNoRows {
		subtle.ConstantTimeCompare(zeroHash, zeroHash)
		return nil, ErrInvalid
	}
	if err != nil {
		return nil, fmt.Errorf("looking up credential: %v", err)
	}

	acct.ID = c.AccountID
	c.acct = &acct
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

	// Read on this goroutine, before the write is detached. option.DBOpTimeout
	// is a package global that tests rewrite in cleanup, so a goroutine that
	// read it later would race them -- and would be reading configuration from
	// a point in time nobody chose.
	timeout := option.DBOpTimeout
	id, db := c.ID, writeDB

	// Detached, not merely given a detached context. The write goes to a pool
	// of one connection (dbutil sets SetMaxOpenConns(1)), so inline it queued
	// this bookkeeping UPDATE behind every commit, upload and GC write on the
	// server while the request waited. Best effort: the client is already
	// authenticated, and delaying their request for a row nothing reads back
	// would be the wrong trade.
	//
	// It captures three values rather than the Credential, so the struct --
	// and the request it belongs to -- can be collected without waiting for
	// this. Nothing is written back to c for the same reason: the request has
	// been answered by the time this runs.
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		_, _ = db.ExecContext(ctx,
			"UPDATE Credential SET last_used = ? WHERE id = ?", now, id)
	}()
}

// EffectivePerm intersects what the account may do with what the credential
// allows, per docs/auth.md's ceiling rule. A credential can only ever narrow:
// it cannot outlive or exceed the account behind it, and if the account's own
// permission is withdrawn the credential follows immediately.
//
// accountPerm is what share.CheckPerm returned for this user and library.
func (c *Credential) EffectivePerm(accountPerm, libraryID, path string) string {
	if !c.Scope.Covers(libraryID, path) {
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

// ByID re-reads a credential the caller has already proven.
//
// Resolve is the answer to "who is this request from", and it needs the
// secret. A holder that resolved once and kept the credential -- the
// notification socket, for the life of its connection -- has a different
// question: is this still good. Revoked, expired, and a disabled account are
// each their Resolve error; the row's current scope comes back for the
// caller to compare against the one it holds. No proof, because the proof
// was given; no last-used stamp, because nothing was used.
func ByID(ctx context.Context, id string) (*Credential, error) {
	cred, err := load(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := stillGood(cred); err != nil {
		return nil, err
	}
	return cred, nil
}
