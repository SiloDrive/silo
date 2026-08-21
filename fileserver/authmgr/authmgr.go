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
	"time"

	"golang.org/x/crypto/pbkdf2"

	"github.com/dkam/silo/fileserver/dbutil"
	"github.com/dkam/silo/fileserver/option"
	"github.com/dkam/silo/fileserver/utils"
	jwt "github.com/golang-jwt/jwt/v5"
	log "github.com/sirupsen/logrus"
)

var readDB *sql.DB
var writeDB *sql.DB

func Init(siloReadDB, siloWriteDB *sql.DB) {
	readDB = siloReadDB
	writeDB = siloWriteDB
}

// Legacy fixed salt used by old Seafile SHA256 password hashing.
var legacySalt = []byte{0xdb, 0x91, 0x45, 0xc3, 0x06, 0xc7, 0xcc, 0x26}

// ValidatePassword checks email/password against the EmailUser table.
// Returns the user email (possibly lowercased) on success.
func ValidatePassword(email, password string) (string, error) {
	if password == "!" {
		return "", fmt.Errorf("invalid password")
	}

	ctx, cancel := context.WithTimeout(context.Background(), option.DBOpTimeout)
	defer cancel()

	var storedPasswd string
	row := readDB.QueryRowContext(ctx, "SELECT passwd FROM EmailUser WHERE email=?", email)
	err := row.Scan(&storedPasswd)
	if err == sql.ErrNoRows {
		emailDown := strings.ToLower(email)
		row = readDB.QueryRowContext(ctx, "SELECT passwd FROM EmailUser WHERE email=?", emailDown)
		err = row.Scan(&storedPasswd)
		if err != nil {
			return "", fmt.Errorf("user not found")
		}
		email = emailDown
	} else if err != nil {
		return "", fmt.Errorf("database error: %v", err)
	}

	if !validatePasswd(password, storedPasswd) {
		return "", fmt.Errorf("incorrect password")
	}

	if needsRehash(storedPasswd) {
		upgradeHash(ctx, email, password)
	}

	return email, nil
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

type SessionClaims struct {
	Email string `json:"email"`
	jwt.RegisteredClaims
}

func GenerateSessionToken(email string) (string, error) {
	if email == "" {
		return "", fmt.Errorf("refusing to issue a session token with no email")
	}

	now := time.Now()
	claims := SessionClaims{
		Email: email,
		RegisteredClaims: jwt.RegisteredClaims{
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(24 * time.Hour)),
			Audience:  jwt.ClaimStrings{utils.AudSession},
		},
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	tokenString, err := token.SignedString([]byte(option.JWTPrivateKey))
	if err != nil {
		return "", fmt.Errorf("failed to sign session token: %v", err)
	}

	return tokenString, nil
}

func ValidateSessionToken(tokenString string) (string, error) {
	token, err := jwt.ParseWithClaims(tokenString, &SessionClaims{},
		func(token *jwt.Token) (interface{}, error) {
			return []byte(option.JWTPrivateKey), nil
		},
		// The notification tokens are signed with this same key, so the
		// signature alone proves nothing about which validator a token was
		// meant for. WithAudience makes that explicit and rejects a token
		// carrying no audience at all.
		jwt.WithValidMethods([]string{utils.SigningAlg}),
		jwt.WithAudience(utils.AudSession),
	)
	if err != nil {
		return "", fmt.Errorf("invalid token: %v", err)
	}

	claims, ok := token.Claims.(*SessionClaims)
	if !ok || !token.Valid {
		return "", fmt.Errorf("invalid token claims")
	}

	// A token of another kind that somehow satisfied the checks above would
	// carry no email, and an empty identity must never reach a handler:
	// share.CheckPerm("") denies, but repo creation would happily accept it.
	if claims.Email == "" {
		return "", fmt.Errorf("token has no email claim")
	}

	return claims.Email, nil
}

// PBKDF2Iterations is the work factor for new password hashes, at OWASP's
// current recommendation for PBKDF2-HMAC-SHA256. The previous value, 10,000,
// dates from a Seafile of some years ago and is now sixty times too cheap:
// it puts a stolen EmailUser table within reach of ordinary offline cracking.
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

func hashPassword(password string) (string, error) {
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
// Seafile kept its original one indefinitely — unsalted SHA1, or SHA256 with
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
func upgradeHash(ctx context.Context, email, password string) {
	if writeDB == nil {
		return
	}
	hash, err := hashPassword(password)
	if err != nil {
		log.Warnf("Failed to rehash password for %s: %v", email, err)
		return
	}
	if _, err := writeDB.ExecContext(ctx,
		"UPDATE EmailUser SET passwd = ? WHERE email = ?", hash, email); err != nil {
		log.Warnf("Failed to store upgraded password hash for %s: %v", email, err)
		return
	}
	log.Infof("Upgraded stored password hash for %s", email)
}

// EnsureAdmin creates an admin user if it doesn't already exist.
// Uses INSERT OR IGNORE to avoid TOCTOU races.
func EnsureAdmin(email, password string) error {
	_, err := ensureAdmin(email, password)
	return err
}

// ensureAdmin is EnsureAdmin plus the one fact the bootstrap path needs: did
// this call actually write the row? A generated password is only worth
// printing if the account it belongs to is the account that was created.
func ensureAdmin(email, password string) (created bool, err error) {
	if email == "" || password == "" {
		return false, nil
	}

	hash, err := hashPassword(password)
	if err != nil {
		return false, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), option.DBOpTimeout)
	defer cancel()

	sqlStr := dbutil.InsertOrIgnore("EmailUser", "email, passwd, is_staff, is_active, ctime")
	result, err := writeDB.ExecContext(ctx, sqlStr, email, hash, 1, 1, time.Now().Unix())
	if err != nil {
		return false, fmt.Errorf("failed to create admin user: %v", err)
	}

	rows, _ := result.RowsAffected()
	if rows > 0 {
		log.Infof("Created admin user: %s", email)
	} else {
		log.Infof("Admin user %s already exists", email)
	}
	return rows > 0, nil
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

	password, err = generatePassword()
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
	ctx, cancel := context.WithTimeout(context.Background(), option.DBOpTimeout)
	defer cancel()

	var n int
	if err := readDB.QueryRowContext(ctx, "SELECT COUNT(*) FROM EmailUser").Scan(&n); err != nil {
		return 0, fmt.Errorf("failed to count users: %v", err)
	}
	return n, nil
}

// passwordAlphabet excludes the characters that get lost between a terminal
// and a keyboard: 0/O, 1/l/I. A generated password is read off a log line and
// typed by hand at least once, so ambiguity costs more than the two bits.
const passwordAlphabet = "abcdefghijkmnopqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"

// generatedPasswordLen gives ~110 bits over the alphabet above, which is well
// past anything an online attacker gets through the login rate limiter and
// still short enough to retype.
const generatedPasswordLen = 20

// generatePassword returns a random password drawn from passwordAlphabet.
func generatePassword() (string, error) {
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
