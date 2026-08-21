package authmgr

import (
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/pbkdf2"

	"github.com/dkam/silo/fileserver/dbutil"
	"github.com/dkam/silo/fileserver/option"
	"github.com/dkam/silo/fileserver/utils"
	jwt "github.com/golang-jwt/jwt/v5"
)

func init() {
	option.JWTPrivateKey = "test-secret-key-for-unit-tests"
}

// Helper: generate a PBKDF2SHA256 stored hash for testing.
func makePBKDF2Hash(password string, iter int, salt []byte) string {
	derived := pbkdf2.Key([]byte(password), salt, iter, sha256.Size, sha256.New)
	return "PBKDF2SHA256$" +
		strconv.Itoa(iter) + "$" +
		hex.EncodeToString(salt) + "$" +
		hex.EncodeToString(derived)
}

func TestValidatePasswdPBKDF2(t *testing.T) {
	salt := []byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08}
	stored := makePBKDF2Hash("correct-password", 10000, salt)

	if !validatePasswd("correct-password", stored) {
		t.Error("expected correct password to validate")
	}
	if validatePasswd("wrong-password", stored) {
		t.Error("expected wrong password to fail")
	}
}

func TestValidatePasswdSHA256Salted(t *testing.T) {
	h := sha256.New()
	h.Write([]byte("mypassword"))
	h.Write(legacySalt)
	stored := hex.EncodeToString(h.Sum(nil))

	if len(stored) != sha256.Size*2 {
		t.Fatalf("unexpected hash length: %d", len(stored))
	}

	if !validatePasswd("mypassword", stored) {
		t.Error("expected correct password to validate")
	}
	if validatePasswd("wrong", stored) {
		t.Error("expected wrong password to fail")
	}
}

func TestValidatePasswdSHA1(t *testing.T) {
	h := sha1.New()
	h.Write([]byte("mypassword"))
	stored := hex.EncodeToString(h.Sum(nil))

	if len(stored) != sha1.Size*2 {
		t.Fatalf("unexpected hash length: %d", len(stored))
	}

	if !validatePasswd("mypassword", stored) {
		t.Error("expected correct password to validate")
	}
	if validatePasswd("wrong", stored) {
		t.Error("expected wrong password to fail")
	}
}

func TestValidatePasswdDisabledAccount(t *testing.T) {
	if validatePasswd("anything", "!") {
		t.Error("disabled account (!) should never validate")
	}
}

func TestValidatePasswdMalformedPBKDF2(t *testing.T) {
	// Wrong number of parts
	if validatePasswd("test", "PBKDF2SHA256$only$two") {
		t.Error("malformed PBKDF2 should fail")
	}
	// Non-numeric iterations
	if validatePasswd("test", "PBKDF2SHA256$notanumber$0102$abcd") {
		t.Error("non-numeric iterations should fail")
	}
	// Invalid hex salt
	if validatePasswd("test", "PBKDF2SHA256$10000$zzzz$abcd") {
		t.Error("invalid hex salt should fail")
	}
}

func TestGenerateAndValidateSessionToken(t *testing.T) {
	token, err := GenerateSessionToken("user@example.com")
	if err != nil {
		t.Fatalf("failed to generate token: %v", err)
	}
	if token == "" {
		t.Fatal("expected non-empty token")
	}

	email, err := ValidateSessionToken(token)
	if err != nil {
		t.Fatalf("failed to validate token: %v", err)
	}
	if email != "user@example.com" {
		t.Errorf("expected user@example.com, got %s", email)
	}
}

func TestValidateSessionTokenInvalid(t *testing.T) {
	_, err := ValidateSessionToken("not-a-valid-jwt")
	if err == nil {
		t.Error("expected error for invalid token")
	}
}

func TestValidateSessionTokenWrongKey(t *testing.T) {
	token, err := GenerateSessionToken("user@example.com")
	if err != nil {
		t.Fatalf("failed to generate token: %v", err)
	}

	// Change the signing key
	original := option.JWTPrivateKey
	option.JWTPrivateKey = "different-secret-key"
	defer func() { option.JWTPrivateKey = original }()

	_, err = ValidateSessionToken(token)
	if err == nil {
		t.Error("expected error when validating with wrong key")
	}
}

func TestSessionTokenDifferentUsers(t *testing.T) {
	t1, _ := GenerateSessionToken("alice@example.com")
	t2, _ := GenerateSessionToken("bob@example.com")
	if t1 == t2 {
		t.Error("expected different tokens for different users")
	}
}

// TestValidateSessionTokenRejectsNotifToken is the regression test for the
// token-confusion bug: notification tokens are signed with the same key, carry
// "username" rather than "email", and so used to parse as session claims with
// an empty email and a nil error — admitting an empty identity.
func TestValidateSessionTokenRejectsNotifToken(t *testing.T) {
	notifTok, err := utils.GenNotifJWTToken(
		"aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", "mallory@example.com",
		time.Now().Add(72*time.Hour).Unix())
	if err != nil {
		t.Fatalf("failed to generate notif token: %v", err)
	}

	email, err := ValidateSessionToken(notifTok)
	if err == nil {
		t.Errorf("notification token was accepted as a session token (email=%q)", email)
	}
	if email != "" {
		t.Errorf("expected no email, got %q", email)
	}
}

// A token with no audience at all — i.e. one issued before audiences existed —
// must not be accepted.
func TestValidateSessionTokenRejectsMissingAudience(t *testing.T) {
	claims := SessionClaims{
		Email: "user@example.com",
		RegisteredClaims: jwt.RegisteredClaims{
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		},
	}
	legacy, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).
		SignedString([]byte(option.JWTPrivateKey))
	if err != nil {
		t.Fatalf("failed to sign legacy token: %v", err)
	}

	if _, err := ValidateSessionToken(legacy); err == nil {
		t.Error("token with no audience was accepted")
	}
}

func TestValidateSessionTokenRejectsWrongAudience(t *testing.T) {
	claims := SessionClaims{
		Email: "user@example.com",
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
			Audience:  jwt.ClaimStrings{utils.AudNotif},
		},
	}
	tok, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).
		SignedString([]byte(option.JWTPrivateKey))
	if err != nil {
		t.Fatalf("failed to sign token: %v", err)
	}

	if _, err := ValidateSessionToken(tok); err == nil {
		t.Error("token with the notification audience was accepted as a session")
	}
}

// The validator pins HS256 rather than accepting whatever the token's header
// selects, so a correctly-keyed token under another HMAC variant is refused.
func TestValidateSessionTokenPinsAlgorithm(t *testing.T) {
	claims := SessionClaims{
		Email: "user@example.com",
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
			Audience:  jwt.ClaimStrings{utils.AudSession},
		},
	}
	hs512, err := jwt.NewWithClaims(jwt.SigningMethodHS512, claims).
		SignedString([]byte(option.JWTPrivateKey))
	if err != nil {
		t.Fatalf("failed to sign HS512 token: %v", err)
	}

	if _, err := ValidateSessionToken(hs512); err == nil {
		t.Error("HS512 token was accepted despite the HS256 pin")
	}
}

func TestValidateSessionTokenRejectsEmptyEmail(t *testing.T) {
	claims := SessionClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
			Audience:  jwt.ClaimStrings{utils.AudSession},
		},
	}
	tok, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).
		SignedString([]byte(option.JWTPrivateKey))
	if err != nil {
		t.Fatalf("failed to sign token: %v", err)
	}

	if _, err := ValidateSessionToken(tok); err == nil {
		t.Error("session token with an empty email was accepted")
	}
}

func TestGenerateSessionTokenRejectsEmptyEmail(t *testing.T) {
	if _, err := GenerateSessionToken(""); err == nil {
		t.Error("expected an error when generating a token with no email")
	}
}

// authTestDB points the package at a throwaway database.
func authTestDB(t *testing.T) {
	t.Helper()

	origTimeout := option.DBOpTimeout
	option.DBOpTimeout = 5 * time.Second

	// The statement builders dispatch on this; unset, they emit MySQL.

	pair, err := dbutil.OpenSQLite(filepath.Join(t.TempDir(), "silo.db"))
	if err != nil {
		t.Fatalf("failed to open test database: %v", err)
	}
	if err := dbutil.CreateSiloTables(pair.Write); err != nil {
		t.Fatalf("failed to create test tables: %v", err)
	}

	origRead, origWrite := readDB, writeDB
	Init(pair.Read, pair.Write)

	t.Cleanup(func() {
		readDB, writeDB = origRead, origWrite
		option.DBOpTimeout = origTimeout
		_ = pair.Close()
	})
}

func seedUser(t *testing.T, email, storedPasswd string) {
	t.Helper()
	if _, err := writeDB.Exec(
		"INSERT INTO EmailUser (email, passwd, is_staff, is_active, ctime) VALUES (?, ?, 0, 1, ?)",
		email, storedPasswd, time.Now().Unix()); err != nil {
		t.Fatalf("failed to seed user: %v", err)
	}
}

func storedHash(t *testing.T, email string) string {
	t.Helper()
	var hash string
	if err := readDB.QueryRow("SELECT passwd FROM EmailUser WHERE email = ?", email).Scan(&hash); err != nil {
		t.Fatalf("failed to read stored hash: %v", err)
	}
	return hash
}

func TestNeedsRehash(t *testing.T) {
	salt := []byte("0123456789abcdef0123456789abcdef")

	cases := []struct {
		name   string
		stored string
		want   bool
	}{
		{"unsalted sha1", hex.EncodeToString(sha1Sum("pw")), true},
		{"legacy salted sha256", hex.EncodeToString(sha256SaltedSum("pw")), true},
		{"pbkdf2 below the current work factor", makePBKDF2Hash("pw", 10000, salt), true},
		{"pbkdf2 one iteration short", makePBKDF2Hash("pw", PBKDF2Iterations-1, salt), true},
		{"malformed pbkdf2", "PBKDF2SHA256$notanumber$aa$bb", true},
		{"truncated pbkdf2", "PBKDF2SHA256$10000$aa", true},
		{"current", makePBKDF2Hash("pw", PBKDF2Iterations, salt), false},
		{"stronger than current", makePBKDF2Hash("pw", PBKDF2Iterations*2, salt), false},
	}
	for _, c := range cases {
		if got := needsRehash(c.stored); got != c.want {
			t.Errorf("needsRehash(%s) = %v, want %v", c.name, got, c.want)
		}
	}
}

// Nothing upgraded a stored hash before, so an account created against an old
// Seafile kept unsalted SHA1 — or SHA256 with a salt that is a public
// constant — indefinitely. A successful login is the only moment the
// plaintext is in hand and known good.
func TestValidatePasswordUpgradesLegacyHashes(t *testing.T) {
	authTestDB(t)

	legacy := map[string]string{
		"sha1@example.com":   hex.EncodeToString(sha1Sum("correct-password")),
		"sha256@example.com": hex.EncodeToString(sha256SaltedSum("correct-password")),
		"weak@example.com":   makePBKDF2Hash("correct-password", 10000, []byte("saltsaltsaltsalt")),
	}
	for email, hash := range legacy {
		seedUser(t, email, hash)
	}

	for email, before := range legacy {
		if _, err := ValidatePassword(email, "correct-password"); err != nil {
			t.Fatalf("ValidatePassword(%s) returned %v", email, err)
		}

		after := storedHash(t, email)
		if after == before {
			t.Errorf("%s: the stored hash was not upgraded", email)
			continue
		}
		if needsRehash(after) {
			t.Errorf("%s: the upgraded hash still needs rehashing: %.20s...", email, after)
		}
		// The upgrade must not lock the user out of their own password.
		if _, err := ValidatePassword(email, "correct-password"); err != nil {
			t.Errorf("%s: the password stopped working after the upgrade: %v", email, err)
		}
		if _, err := ValidatePassword(email, "wrong-password"); err == nil {
			t.Errorf("%s: the wrong password was accepted after the upgrade", email)
		}
	}
}

// A wrong password must not cause a rewrite — the plaintext is not known good.
func TestValidatePasswordDoesNotRehashOnFailure(t *testing.T) {
	authTestDB(t)

	before := hex.EncodeToString(sha1Sum("correct-password"))
	seedUser(t, "sha1@example.com", before)

	if _, err := ValidatePassword("sha1@example.com", "wrong-password"); err == nil {
		t.Fatal("the wrong password was accepted")
	}
	if after := storedHash(t, "sha1@example.com"); after != before {
		t.Error("a failed login rewrote the stored hash")
	}
}

func TestValidatePasswordLeavesCurrentHashAlone(t *testing.T) {
	authTestDB(t)

	before, err := hashPassword("correct-password")
	if err != nil {
		t.Fatalf("hashPassword returned %v", err)
	}
	seedUser(t, "current@example.com", before)

	if _, err := ValidatePassword("current@example.com", "correct-password"); err != nil {
		t.Fatalf("ValidatePassword returned %v", err)
	}
	if after := storedHash(t, "current@example.com"); after != before {
		t.Error("a hash already at the current work factor was rewritten")
	}
}

// New hashes have to be written at the current work factor, or the rehash
// path would rewrite every hash on every login.
func TestHashPasswordUsesCurrentWorkFactor(t *testing.T) {
	hash, err := hashPassword("pw")
	if err != nil {
		t.Fatalf("hashPassword returned %v", err)
	}
	if needsRehash(hash) {
		t.Errorf("a freshly generated hash reports as needing a rehash: %.30s...", hash)
	}
	if !validatePasswd("pw", hash) {
		t.Error("a freshly generated hash does not validate its own password")
	}
	if validatePasswd("other", hash) {
		t.Error("a freshly generated hash validates the wrong password")
	}
}

func sha1Sum(s string) []byte {
	h := sha1.New()
	h.Write([]byte(s))
	return h.Sum(nil)
}

func sha256SaltedSum(s string) []byte {
	h := sha256.New()
	h.Write([]byte(s))
	h.Write(legacySalt)
	return h.Sum(nil)
}

// A fresh server with nothing in the environment has to end up with an
// account somebody can actually log in to, and the password it hands back has
// to be that account's password — not merely a random string.
func TestBootstrapAdminGeneratesUsableCredentials(t *testing.T) {
	authTestDB(t)

	password, err := BootstrapAdmin("", "")
	if err != nil {
		t.Fatalf("BootstrapAdmin returned %v", err)
	}
	if password == "" {
		t.Fatal("no password was generated for an empty user table")
	}
	if _, err := ValidatePassword(DefaultAdminEmail, password); err != nil {
		t.Errorf("the generated password does not log in: %v", err)
	}

	var isStaff int
	if err := readDB.QueryRow("SELECT is_staff FROM EmailUser WHERE email = ?",
		DefaultAdminEmail).Scan(&isStaff); err != nil {
		t.Fatalf("failed to read the created user: %v", err)
	}
	if isStaff != 1 {
		t.Error("the bootstrap account was not created as staff")
	}
}

// SILO_ADMIN_EMAIL on its own is enough to say who the account belongs to.
func TestBootstrapAdminHonoursSuppliedEmail(t *testing.T) {
	authTestDB(t)

	password, err := BootstrapAdmin("someone@example.com", "")
	if err != nil {
		t.Fatalf("BootstrapAdmin returned %v", err)
	}
	if password == "" {
		t.Fatal("no password was generated for an empty user table")
	}
	if _, err := ValidatePassword("someone@example.com", password); err != nil {
		t.Errorf("the generated password does not log in: %v", err)
	}
}

// A supplied password is the old behaviour exactly: the account is created and
// nothing is printed, because the operator already knows the password.
func TestBootstrapAdminWithSuppliedPasswordGeneratesNothing(t *testing.T) {
	authTestDB(t)

	password, err := BootstrapAdmin("admin@example.com", "from-the-environment")
	if err != nil {
		t.Fatalf("BootstrapAdmin returned %v", err)
	}
	if password != "" {
		t.Errorf("a password was generated even though one was supplied: %q", password)
	}
	if _, err := ValidatePassword("admin@example.com", "from-the-environment"); err != nil {
		t.Errorf("the supplied password does not log in: %v", err)
	}
}

// This is a bootstrap, not a reset: a server that already has users must not
// acquire another account on every restart.
func TestBootstrapAdminLeavesAnExistingUserTableAlone(t *testing.T) {
	authTestDB(t)

	hash, err := hashPassword("their-password")
	if err != nil {
		t.Fatalf("hashPassword returned %v", err)
	}
	seedUser(t, "existing@example.com", hash)

	password, err := BootstrapAdmin("", "")
	if err != nil {
		t.Fatalf("BootstrapAdmin returned %v", err)
	}
	if password != "" {
		t.Errorf("a password was generated despite an existing user: %q", password)
	}

	var users int
	if err := readDB.QueryRow("SELECT COUNT(*) FROM EmailUser").Scan(&users); err != nil {
		t.Fatalf("failed to count users: %v", err)
	}
	if users != 1 {
		t.Errorf("user count is %d, want 1", users)
	}
}

// A second boot must not print a password that was never stored: the account
// already exists, so the credential the operator saved on the first boot is
// still the one that works.
func TestBootstrapAdminIsIdempotent(t *testing.T) {
	authTestDB(t)

	first, err := BootstrapAdmin("", "")
	if err != nil {
		t.Fatalf("BootstrapAdmin returned %v", err)
	}
	second, err := BootstrapAdmin("", "")
	if err != nil {
		t.Fatalf("second BootstrapAdmin returned %v", err)
	}
	if second != "" {
		t.Errorf("the second boot generated another password: %q", second)
	}
	if _, err := ValidatePassword(DefaultAdminEmail, first); err != nil {
		t.Errorf("the first boot's password stopped working: %v", err)
	}
}

func TestGeneratePassword(t *testing.T) {
	seen := make(map[string]bool, 100)
	for i := 0; i < 100; i++ {
		password, err := generatePassword()
		if err != nil {
			t.Fatalf("generatePassword returned %v", err)
		}
		if len(password) != generatedPasswordLen {
			t.Fatalf("password length is %d, want %d", len(password), generatedPasswordLen)
		}
		if strings.ContainsAny(password, "0O1lI") {
			t.Errorf("password contains an ambiguous character: %q", password)
		}
		for _, c := range password {
			if !strings.ContainsRune(passwordAlphabet, c) {
				t.Fatalf("password contains %q, which is outside the alphabet: %q", c, password)
			}
		}
		if seen[password] {
			t.Fatalf("generatePassword repeated itself: %q", password)
		}
		seen[password] = true
	}
}
