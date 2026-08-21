package apitokenstore

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/dkam/silo/fileserver/dbutil"
	"github.com/dkam/silo/fileserver/option"
)

const testEmail = "alice@example.com"

// setupStore gives each test its own SQLite pair, migrated to the current
// schema, and returns the write handle for tests that need to age a row.
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
	if err := dbutil.MigrateSiloTables(pair.Write, option.APITokenTTL); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	Init(pair.Read, pair.Write)
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

	token, err := Create(testEmail)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if len(token) != 40 {
		t.Errorf("expected a 40-char token, got %d chars", len(token))
	}

	email, err := Lookup(token)
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if email != testEmail {
		t.Errorf("expected %s, got %s", testEmail, email)
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

	token, err := Create(testEmail)
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

	first, err := Create(testEmail)
	if err != nil {
		t.Fatalf("create first: %v", err)
	}
	second, err := Create(testEmail)
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

	token, err := Create(testEmail)
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

	token, err := Create(testEmail)
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

// A row predating the migration must not become a permanent credential.
func TestLookupGivesNullExpiryATTL(t *testing.T) {
	setupStore(t)

	token, err := Create(testEmail)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := writeDB.Exec(
		"UPDATE ApiToken SET expires_at = NULL WHERE token = ?", token); err != nil {
		t.Fatalf("null out expiry: %v", err)
	}

	if _, err := Lookup(token); err != nil {
		t.Fatalf("legacy token rejected: %v", err)
	}
	if expiryOf(t, token) <= time.Now().Unix() {
		t.Error("legacy token was not given a future expiry")
	}
}

func TestDeleteByEmailRevokesEveryDevice(t *testing.T) {
	setupStore(t)

	first, _ := Create(testEmail)
	second, _ := Create(testEmail)
	other, _ := Create("bob@example.com")

	n, err := DeleteByEmail(testEmail)
	if err != nil {
		t.Fatalf("delete by email: %v", err)
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

	live, _ := Create(testEmail)
	dead, _ := Create(testEmail)
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

// Regression test for a startup crash: a database created by an earlier
// version has an ApiToken table without expires_at, and CREATE TABLE IF NOT
// EXISTS leaves it that way. Declaring the expires_at index alongside the
// table therefore aborted startup on every existing deployment — a fresh
// database never hit it, so only an upgrade did.
func TestMigrationUpgradesPreExistingDatabase(t *testing.T) {
	option.LoadFileServerOptions("")

	pair, err := dbutil.OpenSQLite(filepath.Join(t.TempDir(), "silo.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = pair.Close() })

	// Build the old shapes by hand, then seed a row in each.
	for _, stmt := range []string{
		"CREATE TABLE ApiToken (token CHAR(40) PRIMARY KEY, email VARCHAR(255) NOT NULL, ctime BIGINT)",
		"CREATE TABLE RepoUserToken (repo_id CHAR(37), email VARCHAR(255), token CHAR(41))",
		"INSERT INTO ApiToken VALUES ('legacyapi', 'old@example.com', 1)",
		"INSERT INTO RepoUserToken VALUES ('repo', 'old@example.com', 'legacysync')",
	} {
		if _, err := pair.Write.Exec(stmt); err != nil {
			t.Fatalf("seed %q: %v", stmt, err)
		}
	}

	// This is the ordering the server uses: create, then migrate.
	if err := dbutil.CreateSiloTables(pair.Write); err != nil {
		t.Fatalf("CreateSeafileTables against a pre-existing database: %v", err)
	}
	if err := dbutil.MigrateSiloTables(pair.Write, option.APITokenTTL); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	Init(pair.Read, pair.Write)

	// The legacy API token must have been given a future expiry, not expired
	// on the spot — an upgrade must not sign every client out.
	var exp int64
	if err := pair.Read.QueryRow(
		"SELECT expires_at FROM ApiToken WHERE token = 'legacyapi'").Scan(&exp); err != nil {
		t.Fatalf("legacy token has no expiry after migration: %v", err)
	}
	if exp <= time.Now().Unix() {
		t.Errorf("migration expired a pre-existing token (expires_at=%d)", exp)
	}
	if _, err := Lookup("legacyapi"); err != nil {
		t.Errorf("pre-existing token stopped working after upgrade: %v", err)
	}

	var ctime int64
	if err := pair.Read.QueryRow(
		"SELECT ctime FROM RepoUserToken WHERE token = 'legacysync'").Scan(&ctime); err != nil {
		t.Fatalf("legacy sync token has no ctime after migration: %v", err)
	}
	if ctime == 0 {
		t.Error("migration left RepoUserToken.ctime unset")
	}
}

// The migration must be safe to run against an already-current schema, since
// it runs on every start.
func TestMigrationIsIdempotent(t *testing.T) {
	setupStore(t)

	for i := 0; i < 3; i++ {
		if err := dbutil.MigrateSiloTables(writeDB, option.APITokenTTL); err != nil {
			t.Fatalf("migration run %d failed: %v", i+1, err)
		}
	}

	token, err := Create(testEmail)
	if err != nil {
		t.Fatalf("create after re-migration: %v", err)
	}
	if _, err := Lookup(token); err != nil {
		t.Errorf("lookup after re-migration: %v", err)
	}
}
