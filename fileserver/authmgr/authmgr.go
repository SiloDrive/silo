package authmgr

import (
	"context"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/crypto/pbkdf2"

	"github.com/dkam/silo/fileserver/account"
	"github.com/dkam/silo/fileserver/option"
	"github.com/dkam/silo/fileserver/setup"
	log "github.com/sirupsen/logrus"
)

var readDB *sql.DB
var writeDB *sql.DB

func Init(siloReadDB, siloWriteDB *sql.DB) {
	readDB = siloReadDB
	writeDB = siloWriteDB
}

// Legacy fixed salt, from the SHA256 password hashing this inherited.
var legacySalt = []byte{0xdb, 0x91, 0x45, 0xc3, 0x06, 0xc7, 0xcc, 0x26}

// dummyHash is what a login for an address with no account is verified
// against, so that a miss costs what a hit costs. See ValidatePassword.
//
// It is derived once, lazily, at whatever work factor HashPassword currently
// uses -- so raising the iteration count raises this too, and the gap this
// closes cannot quietly reopen. Lazily because the CLI paths never reach it
// and should not pay 80ms at startup to prepare for a request they will not
// serve.
//
// The password is a constant and is not a secret: what it guards is the
// timing, and knowing the input tells an attacker nothing about which
// addresses exist. If deriving it ever fails, the fallback is a well-formed
// hash of the wrong shape -- validatePasswd will still walk it and refuse,
// which is slower than returning early and is the property that matters.
var dummyHash = sync.OnceValue(func() string {
	h, err := HashPassword("silo/dummy-password/6f3a1c/not-a-secret")
	if err != nil {
		log.Errorf("Could not derive the dummy password hash: %v", err)
		return "PBKDF2SHA256$" + strconv.Itoa(PBKDF2Iterations) + "$00$00"
	}
	return h
})

// ValidatePassword checks an address and password against the account behind
// them, and returns the account on success.
//
// It returns the account rather than an address because the address is no
// longer the thing anything is looked up by. Callers that need one -- a
// response body, a commit author -- read it off the account.
//
// There is one failure message for "no such account", "no password on this
// account" and "wrong password". Distinguishing them would turn the login form
// into a way to ask which addresses exist here.
func ValidatePassword(email, password string) (*account.Account, error) {
	if password == "!" {
		return nil, fmt.Errorf("invalid password")
	}

	ctx, cancel := option.WithDBTimeout(context.Background())
	defer cancel()

	id, storedPasswd, err := account.PasswordHash(ctx, email)
	if err != nil {
		// docs/auth.md finding 7. Returning here costs a database round trip;
		// the path below costs 600,000 PBKDF2 rounds, and the difference is a
		// directory listing for anyone willing to time this endpoint. The
		// login limiter does not help, because enumeration needs one attempt
		// per address rather than ten.
		//
		// So a miss does the work a hit does, against a hash of a password
		// nobody has. This covers an account with no AccountPassword row as
		// well, which is the same question asked a different way.
		if validatePasswd(password, dummyHash()) {
			// Unreachable: the dummy password is not a caller's. The branch is
			// here so the call cannot be read as dead by a compiler or by the
			// next person, and so that being wrong about that is loud.
			log.Errorf("A login matched the dummy password hash; refusing it")
		}
		return nil, fmt.Errorf("user not found")
	}

	if !validatePasswd(password, storedPasswd) {
		return nil, fmt.Errorf("incorrect password")
	}

	acct, err := account.ByID(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("user not found")
	}
	// An account nobody may use any more must not be able to log in. Every
	// other lane asks this through credential.Resolve; this is the lane that
	// mints the credentials, so it has to ask for itself.
	if !acct.IsActive {
		return nil, fmt.Errorf("account is not active")
	}

	if needsRehash(storedPasswd) {
		upgradeHash(ctx, acct, password)
	}

	return acct, nil
}

func validatePasswd(password, storedPasswd string) bool {
	if storedPasswd == "!" {
		return false
	}

	// Check for known prefix before falling back to length-based dispatch
	if strings.HasPrefix(storedPasswd, "PBKDF2SHA256$") {
		return validatePBKDF2SHA256(password, storedPasswd)
	}

	hashLen := len(storedPasswd)
	switch hashLen {
	case sha256.Size * 2:
		return validateSHA256Salted(password, storedPasswd)
	case sha1.Size * 2:
		return validateSHA1(password, storedPasswd)
	default:
		return false
	}
}

func validatePBKDF2SHA256(password, storedPasswd string) bool {
	parts := strings.Split(storedPasswd, "$")
	if len(parts) != 4 {
		return false
	}

	iter, err := strconv.Atoi(parts[1])
	if err != nil {
		return false
	}

	salt, err := hex.DecodeString(parts[2])
	if err != nil {
		return false
	}

	expectedHash := parts[3]

	derived := pbkdf2.Key([]byte(password), salt, iter, sha256.Size, sha256.New)
	computedHash := hex.EncodeToString(derived)

	return subtle.ConstantTimeCompare([]byte(computedHash), []byte(expectedHash)) == 1
}

func validateSHA256Salted(password, storedPasswd string) bool {
	h := sha256.New()
	h.Write([]byte(password))
	h.Write(legacySalt)
	computed := hex.EncodeToString(h.Sum(nil))
	return subtle.ConstantTimeCompare([]byte(computed), []byte(storedPasswd)) == 1
}

func validateSHA1(password, storedPasswd string) bool {
	h := sha1.New()
	h.Write([]byte(password))
	computed := hex.EncodeToString(h.Sum(nil))
	return subtle.ConstantTimeCompare([]byte(computed), []byte(storedPasswd)) == 1
}

// PBKDF2Iterations is the work factor for new password hashes, at OWASP's
// current recommendation for PBKDF2-HMAC-SHA256. The previous value, 10,000,
// dates from some years ago and is now sixty times too cheap:
// it puts a stolen AccountPassword table within reach of ordinary offline
// cracking.
//
// Measured at roughly 80ms per verification on a 2020s x86 core, against
// 1.4ms before. That is a cost worth paying at login — logins are rare here,
// since both kinds of token are durable — and one an attacker pays on every
// single guess. Online guessing is separately bounded by the login rate
// limit, so this cost is not a lever an attacker can pull.
//
// Raising it again later needs no migration: validatePBKDF2SHA256 reads the
// count out of each stored hash, and a successful login rewrites any hash
// weaker than this one.
const PBKDF2Iterations = 600000

// HashPassword derives a storable hash from a plaintext password, in the
// self-describing format validatePasswd dispatches on. It is exported for
// "silo user add" and "silo user passwd", which mint accounts and passwords
// outside any request.
func HashPassword(password string) (string, error) {
	salt := make([]byte, 32)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("failed to generate salt: %v", err)
	}
	derived := pbkdf2.Key([]byte(password), salt, PBKDF2Iterations, sha256.Size, sha256.New)
	return fmt.Sprintf("PBKDF2SHA256$%d$%s$%s",
		PBKDF2Iterations, hex.EncodeToString(salt), hex.EncodeToString(derived)), nil
}

// needsRehash reports whether a stored hash should be replaced now that the
// password behind it is known to be correct.
//
// Nothing ever upgraded a hash before, so an account created against an old
// The scheme this inherited kept its original one indefinitely — unsalted SHA1, or SHA256 with
// a salt that is a public constant a few lines up in this file. Neither
// survives contact with a stolen database. A successful login is the only
// moment the plaintext is in hand and known good, so it is the only chance to
// fix that without asking the user to do anything.
func needsRehash(storedPasswd string) bool {
	if !strings.HasPrefix(storedPasswd, "PBKDF2SHA256$") {
		return true
	}
	parts := strings.Split(storedPasswd, "$")
	if len(parts) != 4 {
		return true
	}
	iter, err := strconv.Atoi(parts[1])
	return err != nil || iter < PBKDF2Iterations
}

// upgradeHash rewrites a user's password hash to the current format and work
// factor.
//
// Errors are logged and swallowed. The caller has already authenticated
// successfully; refusing the login because an upgrade could not be written
// would turn a transient database problem into a lockout, and the old hash
// still works.
func upgradeHash(ctx context.Context, acct *account.Account, password string) {
	if writeDB == nil {
		return
	}
	hash, err := HashPassword(password)
	if err != nil {
		log.Warnf("Failed to rehash password for %s: %v", acct.Email, err)
		return
	}
	if err := account.SetPassword(ctx, acct.ID, hash); err != nil {
		log.Warnf("Failed to store upgraded password hash for %s: %v", acct.Email, err)
		return
	}
	log.Infof("Upgraded stored password hash for %s", acct.Email)
}

// CreateAccount is the plaintext lane: it turns a password an operator typed
// into a stored account, so that no caller has to know which KDF this is or
// remember that account.Create takes a hash.
//
// The account package only ever sees hashes. That is the invariant this
// function exists to hold, and it holds it for the CLI, for the setup token's
// claim (ClaimSetup below), and for the admin HTTP endpoint docs/roadmap.md
// means to build on these calls rather than beside them.
//
// created is false when the address was already claimed, in which case
// nothing was written and the existing password is still the live one.
func CreateAccount(ctx context.Context, email, password string, isStaff bool) (created bool, err error) {
	// Ask before deriving. HashPassword is 600k PBKDF2 iterations by design,
	// and the answer is thrown away whenever the address is taken: account.Create
	// returns the existing row the moment it finds it. Checking first keeps
	// ~80ms of work off the common path — on every server restart, and before
	// an operator has typed a password the CLI would then discard. Create
	// still decides; this is a fast path, not the guard against a race.
	if _, err := account.ByEmail(ctx, email); err == nil {
		return false, nil
	}

	hash, err := HashPassword(password)
	if err != nil {
		return false, err
	}

	_, created, err = account.Create(ctx, email, hash, isStaff)
	return created, err
}

// SetAccountPassword replaces an account's password, hashing it on the way in
// for the same reason CreateAccount does.
func SetAccountPassword(ctx context.Context, id account.ID, password string) error {
	hash, err := HashPassword(password)
	if err != nil {
		return err
	}
	return account.SetPassword(ctx, id, hash)
}

// ClaimSetup is the plaintext lane for the one account that has no operator
// behind a shell to create it: the first one, made by trading the setup token
// over HTTP.
//
// It is here rather than in the handler for the reason CreateAccount is. The
// KDF has one entry point, and a caller outside this package should no more
// have to know that setup.Claim takes a hash than it has to know what the hash
// is made of. Adding a pepper, moving to argon2 or putting the parameters in a
// column is then an edit to this package, not an edit to this package and a
// thing to remember about an HTTP handler.
//
// The order is load-bearing twice over. The token is compared first, so a wrong
// guess costs a single-row read rather than 600k PBKDF2 rounds. The hash is
// derived second, before setup.Claim opens its transaction, because writeDB is
// a pool of one connection and eighty milliseconds of hashing inside that
// transaction would hold it against every other writer. Neither read is the
// guard: setup.Claim re-reads the row and re-compares inside the transaction it
// commits, which is what makes the token single-use.
func ClaimSetup(ctx context.Context, presented setup.Token, email, password string) (account.ID, error) {
	stored, err := setup.Peek(ctx)
	if err != nil {
		return account.Zero, err
	}
	if stored.IsZero() || !stored.Equal(presented) {
		return account.Zero, setup.ErrBadToken
	}

	hash, err := HashPassword(password)
	if err != nil {
		return account.Zero, err
	}
	return setup.Claim(ctx, presented, email, hash)
}

// passwordAlphabet excludes the characters that get lost between a terminal
// and a keyboard: 0/O, 1/l/I. A generated password is read off a log line and
// typed by hand at least once, so ambiguity costs more than the two bits.
const passwordAlphabet = "abcdefghijkmnopqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"

// generatedPasswordLen gives ~110 bits over the alphabet above, which is well
// past anything an online attacker gets through the login rate limiter and
// still short enough to retype.
const generatedPasswordLen = 20

// GeneratePassword returns a random password. It is what "silo user add
// --generate" and "silo user passwd --generate" offer an operator who would
// otherwise invent one by hand.
func GeneratePassword() (string, error) {
	buf := make([]byte, generatedPasswordLen)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("failed to generate password: %v", err)
	}
	// len(passwordAlphabet) is 56, which does not divide 256, so folding a
	// byte with % would favour the first 32 characters. Reject and redraw
	// instead; the loop terminates with probability 1 and in practice on the
	// first or second try.
	const limit = 256 - (256 % len(passwordAlphabet))
	out := make([]byte, 0, generatedPasswordLen)
	for len(out) < generatedPasswordLen {
		for _, b := range buf {
			if int(b) >= limit {
				continue
			}
			out = append(out, passwordAlphabet[int(b)%len(passwordAlphabet)])
			if len(out) == generatedPasswordLen {
				break
			}
		}
		if len(out) < generatedPasswordLen {
			if _, err := rand.Read(buf); err != nil {
				return "", fmt.Errorf("failed to generate password: %v", err)
			}
		}
	}
	return string(out), nil
}
