package dbutil

import (
	"path/filepath"
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

// Two spellings of one address reaching two accounts is the failure the
// identity split exists to prevent, so the rule that stops it is worth
// pinning rather than leaving to whoever writes the next call site.
func TestNormalizeEmail(t *testing.T) {
	tests := []struct{ in, want string }{
		{"dan@example.com", "dan@example.com"},
		{"Dan@Example.COM", "dan@example.com"},
		{"  dan@example.com  ", "dan@example.com"},
		{"DAN@EXAMPLE.COM", "dan@example.com"},
		{"", ""},
		{"   ", ""},
	}
	for _, tt := range tests {
		if got := NormalizeEmail(tt.in); got != tt.want {
			t.Errorf("NormalizeEmail(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// An account with two primary addresses makes "what is this account called"
// depend on row order. The partial unique index is what stops it, and an index
// that silently failed to be created would not be noticed any other way.
func TestAccountEmailAllowsOnePrimary(t *testing.T) {
	pair := schemaTestDB(t)
	db := pair.Write

	id := []byte("0123456789abcdef")
	if _, err := db.Exec(
		"INSERT INTO Account (id, is_active, is_staff, ctime) VALUES (?, 1, 0, 0)", id); err != nil {
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

// One address belongs to one account, which is the primary key's whole job.
func TestAccountEmailIsExclusive(t *testing.T) {
	pair := schemaTestDB(t)
	db := pair.Write

	for _, id := range [][]byte{[]byte("0123456789abcdef"), []byte("fedcba9876543210")} {
		if _, err := db.Exec(
			"INSERT INTO Account (id, is_active, is_staff, ctime) VALUES (?, 1, 0, 0)", id); err != nil {
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
