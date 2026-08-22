package apitokenstore

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/dkam/silo/fileserver/account"
	"github.com/dkam/silo/fileserver/dbutil"
	"github.com/dkam/silo/fileserver/option"
)

const testEmail = "alice@example.com"

// testAccount is the account setupStore mints, and the one every test in this
// file issues tokens for.
var testAccount account.ID

// setupStore gives each test its own SQLite pair and one account to hold
// tokens.
func setupStore(t *testing.T) {
	t.Helper()

	option.LoadFileServerOptions("")

	pair, err := dbutil.OpenSQLite(filepath.Join(t.TempDir(), "silo.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = pair.Close() })

	if err := dbutil.CreateSiloTables(pair.Write); err != nil {
		t.Fatalf("create tables: %v", err)
	}

	Init(pair.Read, pair.Write)
	account.Init(pair.Read, pair.Write)

	ctx, cancel := option.WithDBTimeout()
	defer cancel()
	id, _, err := account.Create(ctx, testEmail, "PBKDF2SHA256$1$00$00", false)
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	testAccount = id
}

// setExpiry forces a token's expires_at, standing in for the passage of time.
func setExpiry(t *testing.T, token string, at time.Time) {
	t.Helper()
	if _, err := writeDB.Exec(
		"UPDATE ApiToken SET expires_at = ? WHERE token = ?", at.Unix(), token); err != nil {
		t.Fatalf("set expiry: %v", err)
	}
}

func expiryOf(t *testing.T, token string) int64 {
	t.Helper()
	var exp int64
	if err := readDB.QueryRow(
		"SELECT expires_at FROM ApiToken WHERE token = ?", token).Scan(&exp); err != nil {
		t.Fatalf("read expiry: %v", err)
	}
	return exp
}

func TestCreateAndLookup(t *testing.T) {
	setupStore(t)

	token, err := Create(testAccount)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if len(token) != 40 {
		t.Errorf("expected a 40-char token, got %d chars", len(token))
	}

	id, err := Lookup(token)
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if id != testAccount {
		t.Errorf("expected the account %s holds, got %s", testEmail, id)
	}
}

func TestLookupUnknownToken(t *testing.T) {
	setupStore(t)

	if _, err := Lookup("0000000000000000000000000000000000000000"); !errors.Is(err, ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

// The core of the finding: a token past its expiry must stop working.
func TestLookupRejectsExpiredToken(t *testing.T) {
	setupStore(t)

	token, err := Create(testAccount)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	setExpiry(t, token, time.Now().Add(-time.Minute))

	if _, err := Lookup(token); !errors.Is(err, ErrNotFound) {
		t.Errorf("expired token was accepted (err=%v)", err)
	}
}

// Two devices logging in as one user must get independent tokens, so revoking
// one does not sign the other out.
func TestTokensArePerLoginNotShared(t *testing.T) {
	setupStore(t)

	first, err := Create(testAccount)
	if err != nil {
		t.Fatalf("create first: %v", err)
	}
	second, err := Create(testAccount)
	if err != nil {
		t.Fatalf("create second: %v", err)
	}
	if first == second {
		t.Fatal("two logins produced the same token; per-device revocation impossible")
	}

	if err := Delete(first); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := Lookup(first); !errors.Is(err, ErrNotFound) {
		t.Errorf("revoked token still works (err=%v)", err)
	}
	if _, err := Lookup(second); err != nil {
		t.Errorf("revoking one device signed out the other: %v", err)
	}
}

// Past the halfway mark, using a token slides its expiry forward — so an
// actively used token never expires.
func TestLookupSlidesExpiryPastThreshold(t *testing.T) {
	setupStore(t)

	token, err := Create(testAccount)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Leave a quarter of the TTL remaining: three quarters elapsed, well past
	// the halfway renewal threshold.
	nearExpiry := time.Now().Add(option.APITokenTTL / 4)
	setExpiry(t, token, nearExpiry)

	if _, err := Lookup(token); err != nil {
		t.Fatalf("lookup: %v", err)
	}

	if got := expiryOf(t, token); got <= nearExpiry.Unix() {
		t.Errorf("expiry did not slide: was %d, still %d", nearExpiry.Unix(), got)
	}
}

// A freshly issued token is left alone, keeping a DB write off the hot path.
func TestLookupDoesNotSlideFreshToken(t *testing.T) {
	setupStore(t)

	token, err := Create(testAccount)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	before := expiryOf(t, token)

	if _, err := Lookup(token); err != nil {
		t.Fatalf("lookup: %v", err)
	}

	if got := expiryOf(t, token); got != before {
		t.Errorf("fresh token was rewritten: %d → %d", before, got)
	}
}

func TestDeleteByAccountRevokesEveryDevice(t *testing.T) {
	setupStore(t)

	first, _ := Create(testAccount)
	second, _ := Create(testAccount)

	ctx, cancel := option.WithDBTimeout()
	defer cancel()
	bob, _, err := account.Create(ctx, "bob@example.com", "PBKDF2SHA256$1$00$00", false)
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	other, _ := Create(bob)

	n, err := DeleteByAccount(testAccount)
	if err != nil {
		t.Fatalf("delete by account: %v", err)
	}
	if n != 2 {
		t.Errorf("expected 2 tokens revoked, got %d", n)
	}

	for _, tok := range []string{first, second} {
		if _, err := Lookup(tok); !errors.Is(err, ErrNotFound) {
			t.Errorf("token survived a full revoke (err=%v)", err)
		}
	}
	if _, err := Lookup(other); err != nil {
		t.Errorf("another user's token was revoked: %v", err)
	}
}

func TestDeleteExpiredSweepsOnlyExpired(t *testing.T) {
	setupStore(t)

	live, _ := Create(testAccount)
	dead, _ := Create(testAccount)
	setExpiry(t, dead, time.Now().Add(-time.Hour))

	n, err := DeleteExpired()
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 1 {
		t.Errorf("expected 1 row swept, got %d", n)
	}
	if _, err := Lookup(live); err != nil {
		t.Errorf("sweep removed a live token: %v", err)
	}
}
