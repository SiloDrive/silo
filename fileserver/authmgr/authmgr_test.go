package authmgr

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/pbkdf2"

	"github.com/dkam/silo/fileserver/account"
	"github.com/dkam/silo/fileserver/dbutil"
	"github.com/dkam/silo/fileserver/option"
)

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
	account.Init(pair.Read, pair.Write)

	t.Cleanup(func() {
		readDB, writeDB = origRead, origWrite
		option.DBOpTimeout = origTimeout
		_ = pair.Close()
	})
}

func seedUser(t *testing.T, email, storedPasswd string) account.ID {
	t.Helper()
	ctx, cancel := option.WithDBTimeout(context.Background())
	defer cancel()
	id, _, err := account.Create(ctx, email, storedPasswd, account.RoleUser)
	if err != nil {
		t.Fatalf("failed to seed user: %v", err)
	}
	return id
}

func storedHash(t *testing.T, email string) string {
	t.Helper()
	ctx, cancel := option.WithDBTimeout(context.Background())
	defer cancel()
	_, hash, err := account.PasswordHash(ctx, email)
	if err != nil {
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
// The scheme this inherited kept unsalted SHA1 — or SHA256 with a salt that is a public
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

	before, err := HashPassword("correct-password")
	if err != nil {
		t.Fatalf("HashPassword returned %v", err)
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
	hash, err := HashPassword("pw")
	if err != nil {
		t.Fatalf("HashPassword returned %v", err)
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

func TestGeneratePassword(t *testing.T) {
	seen := make(map[string]bool, 100)
	for i := 0; i < 100; i++ {
		password, err := GeneratePassword()
		if err != nil {
			t.Fatalf("GeneratePassword returned %v", err)
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
			t.Fatalf("GeneratePassword repeated itself: %q", password)
		}
		seen[password] = true
	}
}

// Finding 7: an address that exists costs 600,000 PBKDF2 rounds -- tens of
// milliseconds -- and one that does not costs a database round trip. That gap
// is not noise; it is a directory listing for anyone willing to time the
// endpoint, and the login limiter does not help because enumeration needs one
// attempt per address rather than ten.
//
// The fix is to verify against a fixed dummy hash when there is no account, so
// a miss does the same work as a hit. It is measured by timing because timing
// is the property: a test that only checked the error message would pass on
// the code this replaces.
func TestAMissingAccountCostsWhatAPresentOneDoes(t *testing.T) {
	if testing.Short() {
		t.Skip("times two KDF derivations")
	}
	authTestDB(t)

	const email, password = "present@example.com", "correct horse battery staple"
	if _, err := CreateAccount(context.Background(), email, password, account.RoleUser); err != nil {
		t.Fatalf("creating the account: %v", err)
	}

	// The wrong password against a real account is the expensive path: the
	// hash is found and verified. It is the cost a miss has to match.
	measure := func(addr string) time.Duration {
		start := time.Now()
		for i := 0; i < 3; i++ {
			if _, err := ValidatePassword(addr, "not the password"); err == nil {
				t.Fatalf("%s: expected a refusal", addr)
			}
		}
		return time.Since(start)
	}

	hit := measure(email)
	miss := measure("absent@example.com")

	// Half, not equal. The point is that the two are the same order of
	// magnitude rather than a database round trip against a KDF; asserting
	// equality would be a flaky test measuring scheduler noise.
	if miss < hit/2 {
		t.Errorf("a miss took %s and a hit took %s: the gap answers which addresses exist", miss, hit)
	}
}

// A crossed-over account takes the authKey and not the password. This is the
// plain statement of what split-derivation login buys: the password stops
// being a wire credential, so `curl -u user:password` against such an account
// is refused no matter how right the password is.
func TestACrossedOverAccountTakesTheAuthKeyAndNotThePassword(t *testing.T) {
	authTestDB(t)

	const authKey = "3f7a1c9e5b2d8046a1f3c7e9b5d2048c6a2e0f8d4b7159c3e6a0d2f4b8c15790"
	hash, err := HashAuthKey(authKey)
	if err != nil {
		t.Fatalf("HashAuthKey returned %v", err)
	}
	seedUser(t, "crossed@example.com", hash)

	if _, err := ValidatePassword("crossed@example.com", authKey); err != nil {
		t.Fatalf("the authKey was refused: %v", err)
	}
	if _, err := ValidatePassword("crossed@example.com", "the password behind it"); err == nil {
		t.Error("a raw password opened a crossed-over account")
	}
}

// The rehash path must leave a crossed-over hash alone.
//
// It rewrites anything that is not PBKDF2 at the current work factor, which
// before this included an authKey hash — so the first successful login after a
// crossover would have rewritten it as 600k rounds over the authKey. No
// stronger, since the entropy is already 256 bits, and the next login would
// present the authKey against a hash of an authKey run through PBKDF2, and
// fail. A crossover undone by using it.
func TestALoginDoesNotRehashACrossedOverAccount(t *testing.T) {
	authTestDB(t)

	const authKey = "9d4c2a7e1f8b3506c9a2e7d0b4f81c53a6e9027d4b1f8c35e0a7d2b96f4c8103"
	before, err := HashAuthKey(authKey)
	if err != nil {
		t.Fatalf("HashAuthKey returned %v", err)
	}
	seedUser(t, "stable@example.com", before)

	if _, err := ValidatePassword("stable@example.com", authKey); err != nil {
		t.Fatalf("ValidatePassword returned %v", err)
	}
	if after := storedHash(t, "stable@example.com"); after != before {
		t.Errorf("the login rewrote a crossed-over hash:\n before %s\n after  %s", before, after)
	}
	if _, err := ValidatePassword("stable@example.com", authKey); err != nil {
		t.Fatalf("the authKey stopped working after one login: %v", err)
	}
}

func TestHashAuthKeyIsSaltedAndSelfDescribing(t *testing.T) {
	first, err := HashAuthKey("an authKey")
	if err != nil {
		t.Fatalf("HashAuthKey returned %v", err)
	}
	second, err := HashAuthKey("an authKey")
	if err != nil {
		t.Fatalf("HashAuthKey returned %v", err)
	}
	if first == second {
		t.Error("two hashes of one authKey are identical, so the salt is not random")
	}
	if !IsAuthKeyHash(first) {
		t.Errorf("HashAuthKey wrote %q, which IsAuthKeyHash does not recognise", first)
	}
	if IsAuthKeyHash(mustHashPassword(t, "a password")) {
		t.Error("a password hash was read as a crossed-over one")
	}
	if validatePasswd("a different authKey", first) {
		t.Error("the wrong authKey validated")
	}
}

func mustHashPassword(t *testing.T, pw string) string {
	t.Helper()
	h, err := HashPassword(pw)
	if err != nil {
		t.Fatalf("HashPassword returned %v", err)
	}
	return h
}
