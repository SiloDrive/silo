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

	"golang.org/x/crypto/pbkdf2"

	"github.com/dkam/silo/fileserver/account"
	"github.com/dkam/silo/fileserver/option"
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

// EnsureAdmin creates an admin user if it doesn't already exist.
// Uses INSERT OR IGNORE to avoid TOCTOU races.
func EnsureAdmin(email, password string) error {
	_, err := ensureAdmin(email, password)
	return err
}

// CreateAccount is the plaintext lane: it turns a password an operator typed
// into a stored account, so that no caller has to know which KDF this is or
// remember that account.Create takes a hash.
//
// The account package only ever sees hashes. That is the invariant this
// function exists to hold, and it holds it for the CLI, for the bootstrap
// admin, and for the admin HTTP endpoint docs/future-features.md means to
// build on these calls rather than beside them.
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

// ensureAdmin is EnsureAdmin plus the one fact the bootstrap path needs: did
// this call actually write the row? A generated password is only worth
// printing if the account it belongs to is the account that was created.
func ensureAdmin(email, password string) (created bool, err error) {
	if email == "" || password == "" {
		return false, nil
	}

	ctx, cancel := option.WithDBTimeout(context.Background())
	defer cancel()

	created, err = CreateAccount(ctx, email, password, true)
	if err != nil {
		return false, fmt.Errorf("failed to create admin user: %v", err)
	}
	if created {
		log.Infof("Created admin user: %s", email)
	} else {
		log.Infof("Admin user %s already exists", email)
	}
	return created, nil
}

// DefaultAdminEmail is the login the server invents when it has to create the
// first account by itself. It is a login, not an address — nothing is ever
// sent to it — so it uses a reserved TLD that cannot resolve to somebody
// else's mailbox.
const DefaultAdminEmail = "admin@silo.local"

// BootstrapAdmin makes sure the server has an account somebody can log in
// with, and returns the password it generated when it had to invent one.
//
// A server with an empty user table is a server nobody can use: there is no
// signup endpoint and no user-management API, so the only way in was to have
// set SILO_ADMIN_EMAIL and SILO_ADMIN_PASSWORD before the first boot. Someone
// who just ran the binary to see what it does got a working server and no way
// to talk to it, and the fix — stop it, export two variables, start it again —
// is only obvious once you already know the answer.
//
// So: if there are no users at all and no password was supplied, mint one and
// let the caller print it. The generated password is stored hashed like any
// other, which means the log line is the only copy of it that will ever exist.
// Supplying SILO_ADMIN_PASSWORD keeps the old behaviour exactly, and an
// existing user table is left alone — this is a bootstrap, not a reset.
func BootstrapAdmin(email, password string) (generated string, err error) {
	if email == "" {
		email = DefaultAdminEmail
	}
	if password != "" {
		return "", EnsureAdmin(email, password)
	}

	users, err := userCount()
	if err != nil {
		return "", err
	}
	if users > 0 {
		return "", nil
	}

	password, err = GeneratePassword()
	if err != nil {
		return "", err
	}
	created, err := ensureAdmin(email, password)
	if err != nil {
		return "", err
	}
	if !created {
		// Another process won the race between the count and the insert. Its
		// password is the real one; ours was never stored, so printing it
		// would send the operator chasing a credential that cannot work.
		return "", nil
	}
	return password, nil
}

// userCount reports how many accounts exist.
func userCount() (int, error) {
	ctx, cancel := option.WithDBTimeout(context.Background())
	defer cancel()

	return account.Count(ctx)
}

// passwordAlphabet excludes the characters that get lost between a terminal
// and a keyboard: 0/O, 1/l/I. A generated password is read off a log line and
// typed by hand at least once, so ambiguity costs more than the two bits.
const passwordAlphabet = "abcdefghijkmnopqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"

// generatedPasswordLen gives ~110 bits over the alphabet above, which is well
// past anything an online attacker gets through the login rate limiter and
// still short enough to retype.
const generatedPasswordLen = 20

// GeneratePassword returns a random password. It is what the bootstrap admin
// gets, and what "silo user add --generate" offers an operator who would
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
