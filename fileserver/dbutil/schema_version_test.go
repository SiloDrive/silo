package dbutil

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/dkam/silo/internal/lexicon"
)

// The version stamp has to cover the databases written before it existed.
//
// That is not a hypothetical population: it is every database in the field on
// the day the stamp landed, and the rename that made the stamp necessary had
// already gone out. A guard that only protects stamped databases protects
// against the next rename and does nothing about the one that has happened.
//
// The failure it has to catch is specific. Most renamed tables cause no error
// at all — CREATE TABLE IF NOT EXISTS simply makes a new empty one beside the
// orphaned old one, which is quiet data abandonment rather than a crash.
// LastGCID is where the two shapes collide loudly, because the rename moved
// its column and not its name: the table is skipped as already existing, and
// then the index, whose name did change, names a column the old table has
// never had.

// oldShapeDB is a database from before the library rename: tables present, no
// version stamp, and LastGCID carrying the column the rename replaced.
func oldShapeDB(t *testing.T) *DBPair {
	t.Helper()
	pair, err := OpenSQLite(filepath.Join(t.TempDir(), "silo.db"))
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	t.Cleanup(func() {
		_ = pair.Read.Close()
		_ = pair.Write.Close()
	})
	// The retired column name comes from lexicon rather than being written
	// out, for the same reason the route tests take it from there: this file
	// has to name the old vocabulary to reproduce the bug, and the guard that
	// forbids the old vocabulary would otherwise forbid the reproduction.
	if _, err := pair.Write.Exec(
		`CREATE TABLE LastGCID (id INTEGER PRIMARY KEY AUTOINCREMENT, ` +
			lexicon.RetiredNoun + `_id CHAR(36) NOT NULL, client_id VARCHAR(128) NOT NULL,
		 gc_id VARCHAR(10) NOT NULL)`); err != nil {
		t.Fatalf("creating the old shape: %v", err)
	}
	return pair
}

func TestADatabaseFromBeforeVersioningIsRefusedClearly(t *testing.T) {
	pair := oldShapeDB(t)

	err := CreateSiloTables(pair.Write)
	if err == nil {
		t.Fatal("started against a database in the old shape")
	}

	// The whole point is which error. "no such column: library_id" is true and
	// useless: it names the statement that happened to be first, not the
	// reason, and it reads as a bug in Silo rather than as a database this
	// build cannot use.
	if strings.Contains(err.Error(), "no such column") {
		t.Errorf("got the raw SQLite failure, which is what the stamp exists to replace: %v", err)
	}
	for _, want := range []string{"schema version", "delete it"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q, so it does not say what to do: %v", want, err)
		}
	}
}

// A genuinely new database is the case that must keep working, and it is the
// only thing separating it from the one above: both are unstamped, and the
// difference is whether anything is in them.
func TestAFreshDatabaseIsStillCreated(t *testing.T) {
	pair, err := OpenSQLite(filepath.Join(t.TempDir(), "silo.db"))
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	t.Cleanup(func() {
		_ = pair.Read.Close()
		_ = pair.Write.Close()
	})

	if err := CreateSiloTables(pair.Write); err != nil {
		t.Fatalf("a fresh database was refused: %v", err)
	}

	var v int
	if err := pair.Write.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		t.Fatalf("reading the stamp: %v", err)
	}
	if v != SchemaVersion {
		t.Errorf("user_version = %d, want %d — a database this build created is unstamped", v, SchemaVersion)
	}
}

// Running twice must stay a no-op. The refusal keys on "unstamped and not
// empty", and a database this build just created is not empty — so a guard
// that read only that would refuse the server its own second start.
func TestAStampedDatabaseStartsAgain(t *testing.T) {
	pair, err := OpenSQLite(filepath.Join(t.TempDir(), "silo.db"))
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	t.Cleanup(func() {
		_ = pair.Read.Close()
		_ = pair.Write.Close()
	})

	if err := CreateSiloTables(pair.Write); err != nil {
		t.Fatalf("first start: %v", err)
	}
	if err := CreateSiloTables(pair.Write); err != nil {
		t.Fatalf("second start against a database this build wrote: %v", err)
	}
}
