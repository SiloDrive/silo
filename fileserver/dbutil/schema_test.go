package dbutil

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

func schemaTestDB(t *testing.T) *DBPair {
	t.Helper()

	origEngine := DBEngine
	DBEngine = EngineSQLite

	pair, err := OpenSQLite(filepath.Join(t.TempDir(), "seafile.db"))
	if err != nil {
		t.Fatalf("failed to open test database: %v", err)
	}
	if err := CreateSeafileTables(pair.Write); err != nil {
		t.Fatalf("failed to create test tables: %v", err)
	}

	t.Cleanup(func() {
		DBEngine = origEngine
		_ = pair.Read.Close()
		_ = pair.Write.Close()
	})
	return pair
}

// A caller that reaches the migration without loading options first passes a
// zero TTL. Backfilling with it would stamp every existing token with
// expires_at = now and sign out every client, so the migration must refuse
// rather than trust the caller.
func TestMigrateRejectsNonPositiveTTL(t *testing.T) {
	pair := schemaTestDB(t)

	const token = "0401fc662e3bc87a41f299a907c056aaf8322a27"
	if _, err := pair.Write.Exec(
		"INSERT INTO ApiToken (token, email, ctime, expires_at) VALUES (?, ?, ?, NULL)",
		token, "user@example.com", time.Now().Unix()); err != nil {
		t.Fatalf("failed to seed token: %v", err)
	}

	for _, ttl := range []time.Duration{0, -time.Hour} {
		if err := MigrateSeafileTables(pair.Write, ttl); err == nil {
			t.Errorf("MigrateSeafileTables accepted a TTL of %v, want refusal", ttl)
		}

		// The refusal has to happen before the backfill, not after it.
		var expiresAt sql.NullInt64
		if err := pair.Read.QueryRow(
			"SELECT expires_at FROM ApiToken WHERE token = ?", token).Scan(&expiresAt); err != nil {
			t.Fatalf("failed to read back token: %v", err)
		}
		if expiresAt.Valid {
			t.Errorf("TTL %v: token was stamped with expires_at=%d despite the refusal",
				ttl, expiresAt.Int64)
		}
	}

	// A real TTL still migrates, so the guard has not broken the happy path.
	if err := MigrateSeafileTables(pair.Write, 30*24*time.Hour); err != nil {
		t.Fatalf("MigrateSeafileTables rejected a valid TTL: %v", err)
	}
	var expiresAt sql.NullInt64
	if err := pair.Read.QueryRow(
		"SELECT expires_at FROM ApiToken WHERE token = ?", token).Scan(&expiresAt); err != nil {
		t.Fatalf("failed to read back token: %v", err)
	}
	if !expiresAt.Valid {
		t.Fatal("valid TTL left expires_at NULL")
	}
	if expiresAt.Int64 <= time.Now().Unix() {
		t.Errorf("backfilled expires_at %d is not in the future", expiresAt.Int64)
	}
}
