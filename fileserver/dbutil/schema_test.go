package dbutil

import (
	"path/filepath"
	"strings"
	"testing"
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

// An account with two primary addresses makes "what is this account called"
// depend on row order. The partial unique index is what stops it, and an index
// that silently failed to be created would not be noticed any other way.
func TestAccountEmailAllowsOnePrimary(t *testing.T) {
	pair := schemaTestDB(t)
	db := pair.Write

	id := []byte("0123456789abcdef")
	if _, err := db.Exec(
		"INSERT INTO Account (id, is_active, role, ctime) VALUES (?, 1, 'user', 0)", id); err != nil {
		t.Fatalf("failed to create an account: %v", err)
	}
	if _, err := db.Exec(
		"INSERT INTO AccountEmail (email, account_id, is_primary) VALUES (?, ?, 1)",
		"dan@example.com", id); err != nil {
		t.Fatalf("failed to claim an address: %v", err)
	}

	// A second address is fine — that is what the table is for.
	if _, err := db.Exec(
		"INSERT INTO AccountEmail (email, account_id, is_primary) VALUES (?, ?, 0)",
		"dan@work.example.com", id); err != nil {
		t.Fatalf("an account could not hold a second address: %v", err)
	}

	// A second *primary* is not.
	if _, err := db.Exec(
		"INSERT INTO AccountEmail (email, account_id, is_primary) VALUES (?, ?, 1)",
		"dan@other.example.com", id); err == nil {
		t.Error("an account accepted a second primary address")
	}
}

// A fresh database gets stamped with the schema version it was created at,
// so a later binary can tell what it is looking at without guessing.
func TestCreateSiloTablesStampsSchemaVersion(t *testing.T) {
	pair := schemaTestDB(t)

	var got int
	if err := pair.Write.QueryRow("PRAGMA user_version").Scan(&got); err != nil {
		t.Fatalf("failed to read user_version: %v", err)
	}
	if got != SchemaVersion {
		t.Errorf("fresh database has user_version %d, want %d", got, SchemaVersion)
	}
}

// A database stamped with a schema version other than the one this binary
// expects must be refused outright — not partway through, on whichever
// CREATE statement first mentions a column the old shape lacks, but before
// any statement runs at all. That partway failure is exactly what a stale
// database produces today: a raw SQLite error with no indication that the
// real problem is a schema mismatch, not a corrupt database.
func TestCreateSiloTablesRefusesAMismatchedVersion(t *testing.T) {
	pair, err := OpenSQLite(filepath.Join(t.TempDir(), "silo.db"))
	if err != nil {
		t.Fatalf("failed to open test database: %v", err)
	}
	t.Cleanup(func() {
		_ = pair.Read.Close()
		_ = pair.Write.Close()
	})

	if _, err := pair.Write.Exec("PRAGMA user_version = 999"); err != nil {
		t.Fatalf("failed to stamp a future version: %v", err)
	}

	err = CreateSiloTables(pair.Write)
	if err == nil {
		t.Fatal("CreateSiloTables accepted a database stamped with a different schema version")
	}
	if !strings.Contains(err.Error(), "999") || !strings.Contains(err.Error(), "schema version") {
		t.Errorf("error does not name the mismatch clearly: %v", err)
	}
}

// One address belongs to one account, which is the primary key's whole job.
func TestAccountEmailIsExclusive(t *testing.T) {
	pair := schemaTestDB(t)
	db := pair.Write

	for _, id := range [][]byte{[]byte("0123456789abcdef"), []byte("fedcba9876543210")} {
		if _, err := db.Exec(
			"INSERT INTO Account (id, is_active, role, ctime) VALUES (?, 1, 'user', 0)", id); err != nil {
			t.Fatalf("failed to create an account: %v", err)
		}
	}

	if _, err := db.Exec(
		"INSERT INTO AccountEmail (email, account_id, is_primary) VALUES (?, ?, 1)",
		"dan@example.com", []byte("0123456789abcdef")); err != nil {
		t.Fatalf("failed to claim an address: %v", err)
	}
	if _, err := db.Exec(
		"INSERT INTO AccountEmail (email, account_id, is_primary) VALUES (?, ?, 1)",
		"dan@example.com", []byte("fedcba9876543210")); err == nil {
		t.Error("two accounts claimed the same address")
	}
}
