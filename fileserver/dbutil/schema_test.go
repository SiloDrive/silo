package dbutil

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

func schemaTestDB(t *testing.T) *DBPair {
	t.Helper()

	pair, err := OpenSQLite(filepath.Join(t.TempDir(), "silo.db"))
	if err != nil {
		t.Fatalf("failed to open test database: %v", err)
	}
	if err := CreateSiloTables(pair.Write); err != nil {
		t.Fatalf("failed to create test tables: %v", err)
	}

	t.Cleanup(func() {
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

	// Stand in for a database from before expires_at existed: the backfill only
	// runs in the migration that adds the column, so a table that already has
	// it is not the case under test.
	const token = "0401fc662e3bc87a41f299a907c056aaf8322a27"
	if _, err := pair.Write.Exec("DROP TABLE ApiToken"); err != nil {
		t.Fatalf("failed to drop ApiToken: %v", err)
	}
	if _, err := pair.Write.Exec(
		"CREATE TABLE ApiToken (token CHAR(40) PRIMARY KEY, email VARCHAR(255) NOT NULL, ctime BIGINT)"); err != nil {
		t.Fatalf("failed to create legacy ApiToken: %v", err)
	}
	if _, err := pair.Write.Exec(
		"INSERT INTO ApiToken (token, email, ctime) VALUES (?, ?, ?)",
		token, "user@example.com", time.Now().Unix()); err != nil {
		t.Fatalf("failed to seed token: %v", err)
	}

	for _, ttl := range []time.Duration{0, -time.Hour} {
		if err := MigrateSiloTables(pair.Write, ttl); err == nil {
			t.Errorf("MigrateSeafileTables accepted a TTL of %v, want refusal", ttl)
		}

		// The refusal has to happen before the migration touches anything, so
		// the column it would have backfilled should not even exist yet.
		if _, err := pair.Read.Exec("SELECT expires_at FROM ApiToken LIMIT 0"); err == nil {
			t.Errorf("TTL %v: migration added expires_at despite the refusal", ttl)
		}
	}

	// A real TTL still migrates, so the guard has not broken the happy path.
	if err := MigrateSiloTables(pair.Write, 30*24*time.Hour); err != nil {
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
