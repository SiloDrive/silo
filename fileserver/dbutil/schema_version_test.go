package dbutil

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/SiloDrive/silo/internal/lexicon"
)

// The migration record has to cover the databases written before it existed.
//
// That is not a hypothetical population: it is every database in the field on
// the day the record landed, and the rename that first made a guard necessary
// had already gone out. A guard that only protects recorded databases protects
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
// migration record, and LastGCID carrying the column the rename replaced.
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

func TestADatabaseFromBeforeMigrationsIsRefusedClearly(t *testing.T) {
	pair := oldShapeDB(t)

	err := Prepare(pair.Write)
	if err == nil {
		t.Fatal("started against a database in the old shape")
	}

	// The whole point is which error. "no such column: library_id" is true and
	// useless: it names the statement that happened to be first, not the
	// reason, and it reads as a bug in Silo rather than as a database this
	// build cannot use.
	if strings.Contains(err.Error(), "no such column") {
		t.Errorf("got the raw SQLite failure, which is what the record exists to replace: %v", err)
	}
	for _, want := range []string{"migrations", "delete it"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q, so it does not say what to do: %v", want, err)
		}
	}

	// Migrate has no more idea what shape this is than Prepare does.
	if _, err := Migrate(pair.Write); err == nil {
		t.Error("Migrate ran against a database with no record of its shape")
	}
}

// A genuinely new database is the case that must keep working, and it is the
// only thing separating it from the one above: both have no record, and the
// difference is whether anything is in them.
func TestAFreshDatabaseIsCreatedAtTheCurrentShape(t *testing.T) {
	pair, err := OpenSQLite(filepath.Join(t.TempDir(), "silo.db"))
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	t.Cleanup(func() {
		_ = pair.Read.Close()
		_ = pair.Write.Close()
	})

	if err := Prepare(pair.Write); err != nil {
		t.Fatalf("a fresh database was refused: %v", err)
	}

	applied, err := appliedMigrations(pair.Write)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range migrations {
		if !applied[m.Name] {
			t.Errorf("fresh database does not record %s, so a later start would run it against a shape that already has it", m.Name)
		}
	}
	if len(applied) != len(migrations) {
		t.Errorf("fresh database records %d migrations, want %d", len(applied), len(migrations))
	}
}

// Running twice must stay a no-op. The refusal keys on "no record and not
// empty", and a database this build just created is not empty — so a guard
// that read only that would refuse the server its own second start.
func TestARecordedDatabaseStartsAgain(t *testing.T) {
	pair, err := OpenSQLite(filepath.Join(t.TempDir(), "silo.db"))
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	t.Cleanup(func() {
		_ = pair.Read.Close()
		_ = pair.Write.Close()
	})

	if err := Prepare(pair.Write); err != nil {
		t.Fatalf("first start: %v", err)
	}
	if err := Prepare(pair.Write); err != nil {
		t.Fatalf("second start against a database this build wrote: %v", err)
	}
	if applied, err := Migrate(pair.Write); err != nil || len(applied) != 0 {
		t.Fatalf("Migrate on a current database = %v, %v; want nothing applied", applied, err)
	}
}
