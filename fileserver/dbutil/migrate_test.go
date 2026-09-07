package dbutil

import (
	"database/sql"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// baselinePath is the oldest database this package promises to migrate: the
// shape a fresh database had when the migration record landed, written by
// that build. Every migration since is exercised on the way up from it, which
// is why there is one fixture rather than one per change.
//
// Regenerate it only when the promise changes -- when an install this old is
// no longer worth migrating -- with SILO_WRITE_BASELINE=1 go test ./fileserver/dbutil
// -run TestWriteBaseline. Regenerating it to make a failing test pass defeats
// the test.
const baselinePath = "testdata/baseline.db"

func openTemp(t *testing.T) *DBPair {
	t.Helper()
	pair, err := OpenSQLite(filepath.Join(t.TempDir(), "silo.db"))
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	t.Cleanup(func() { _ = pair.Close() })
	return pair
}

// openBaseline copies the fixture into a temp dir before opening it, because
// opening it in place would write the WAL and the record beside the committed
// file.
func openBaseline(t *testing.T) *DBPair {
	t.Helper()
	src, err := os.Open(baselinePath)
	if err != nil {
		t.Fatalf("the baseline fixture is missing: %v", err)
	}
	defer func() { _ = src.Close() }()
	path := filepath.Join(t.TempDir(), "silo.db")
	dst, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(dst, src); err != nil {
		t.Fatal(err)
	}
	if err := dst.Close(); err != nil {
		t.Fatal(err)
	}
	pair, err := OpenSQLite(path)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	t.Cleanup(func() { _ = pair.Close() })
	return pair
}

// shape is everything about a database's tables and indexes that a statement
// can observe: column names, types, constraints and defaults, and which
// columns each index covers. It deliberately ignores the CREATE text in
// sqlite_master, which differs in whitespace between a table the schema
// created and one a migration rebuilt.
type tableShape struct {
	Columns []columnShape
	Indexes []indexShape
}

type columnShape struct {
	Name, Type string
	NotNull    bool
	Default    sql.NullString
	PK         int
}

type indexShape struct {
	Name    string
	Unique  bool
	Partial bool
	Columns []string
}

func shapeOf(t *testing.T, db *sql.DB) map[string]tableShape {
	t.Helper()
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, name)
	}
	_ = rows.Close()

	out := map[string]tableShape{}
	for _, table := range tables {
		var ts tableShape
		cols, err := db.Query(`SELECT name, type, "notnull", dflt_value, pk FROM pragma_table_info(?) ORDER BY cid`, table)
		if err != nil {
			t.Fatal(err)
		}
		for cols.Next() {
			var c columnShape
			if err := cols.Scan(&c.Name, &c.Type, &c.NotNull, &c.Default, &c.PK); err != nil {
				t.Fatal(err)
			}
			ts.Columns = append(ts.Columns, c)
		}
		_ = cols.Close()

		idx, err := db.Query(`SELECT name, "unique", partial FROM pragma_index_list(?) WHERE origin = 'c' ORDER BY name`, table)
		if err != nil {
			t.Fatal(err)
		}
		var indexes []indexShape
		for idx.Next() {
			var i indexShape
			if err := idx.Scan(&i.Name, &i.Unique, &i.Partial); err != nil {
				t.Fatal(err)
			}
			indexes = append(indexes, i)
		}
		_ = idx.Close()
		for k := range indexes {
			cols, err := db.Query(`SELECT name FROM pragma_index_info(?) ORDER BY seqno`, indexes[k].Name)
			if err != nil {
				t.Fatal(err)
			}
			for cols.Next() {
				var name sql.NullString
				if err := cols.Scan(&name); err != nil {
					t.Fatal(err)
				}
				indexes[k].Columns = append(indexes[k].Columns, name.String)
			}
			_ = cols.Close()
		}
		ts.Indexes = indexes
		out[table] = ts
	}
	return out
}

// The test that keeps the two halves of a schema change honest: a database
// created at the baseline and migrated to head has exactly the shape a
// database created at head has. A column added to siloSchema with no
// migration, or a migration that produces a different type than the schema
// declares, fails here and nowhere else.
func TestTheBaselineMigratesToTheFreshShape(t *testing.T) {
	migrated := openBaseline(t)
	applied, err := Migrate(migrated.Write)
	if err != nil {
		t.Fatalf("migrating the baseline: %v", err)
	}
	if len(applied) != len(migrations) {
		t.Errorf("migrating the baseline applied %v, want every one of the %d listed", applied, len(migrations))
	}

	fresh := openTemp(t)
	if err := Prepare(fresh.Write); err != nil {
		t.Fatal(err)
	}

	got, want := shapeOf(t, migrated.Write), shapeOf(t, fresh.Write)
	for table, w := range want {
		g, ok := got[table]
		if !ok {
			t.Errorf("migrated database lacks table %s", table)
			continue
		}
		if !reflect.DeepEqual(g, w) {
			t.Errorf("table %s differs after migration:\n migrated: %+v\n fresh:    %+v", table, g, w)
		}
	}
	for table := range got {
		if _, ok := want[table]; !ok {
			t.Errorf("migrated database still has table %s, which the schema no longer declares", table)
		}
	}

	// And a migrated database is a current one: nothing pending, and Prepare
	// lets the next process in.
	if pending, err := Pending(migrated.Write); err != nil || len(pending) != 0 {
		t.Errorf("Pending after migrating = %v, %v; want none", pending, err)
	}
	if err := Prepare(migrated.Write); err != nil {
		t.Errorf("Prepare refuses a database Migrate just brought to head: %v", err)
	}
}

// The mechanism itself, on a list this test controls rather than the real one:
// a pending migration runs once, is recorded, and does not run again.
func TestAPendingMigrationRunsOnceAndIsRecorded(t *testing.T) {
	pair := openTemp(t)
	if err := Prepare(pair.Write); err != nil {
		t.Fatal(err)
	}

	runs := 0
	extra := append(append([]Migration{}, migrations...), Migration{
		Name: "add-a-table",
		Apply: func(tx *sql.Tx) error {
			runs++
			_, err := tx.Exec("CREATE TABLE Extra (id INTEGER PRIMARY KEY)")
			return err
		},
	})

	if err := prepare(pair.Write, extra); err == nil || !strings.Contains(err.Error(), "add-a-table") {
		t.Errorf("Prepare with a pending migration = %v; want a refusal naming it", err)
	}

	applied, err := migrate(pair.Write, extra)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(applied, []string{"add-a-table"}) || runs != 1 {
		t.Errorf("first migrate applied %v with %d runs; want [add-a-table] once", applied, runs)
	}
	if ok, _ := hasTable(pair.Write, "Extra"); !ok {
		t.Error("the migration's table is not there")
	}

	applied, err = migrate(pair.Write, extra)
	if err != nil || len(applied) != 0 || runs != 1 {
		t.Errorf("second migrate = %v, %v with %d runs; want nothing applied and no second run", applied, err, runs)
	}
	if err := prepare(pair.Write, extra); err != nil {
		t.Errorf("Prepare after the migration ran: %v", err)
	}
}

// A migration that fails leaves no trace: the change is rolled back and the
// record does not say it happened, so the next start tries it again rather
// than skipping a change that was never made.
func TestAFailedMigrationIsNotRecorded(t *testing.T) {
	pair := openTemp(t)
	if err := Prepare(pair.Write); err != nil {
		t.Fatal(err)
	}
	boom := errors.New("boom")
	extra := append(append([]Migration{}, migrations...), Migration{
		Name: "half-done",
		Apply: func(tx *sql.Tx) error {
			if _, err := tx.Exec("CREATE TABLE Half (id INTEGER PRIMARY KEY)"); err != nil {
				return err
			}
			return boom
		},
	})

	_, err := migrate(pair.Write, extra)
	if !errors.Is(err, boom) {
		t.Fatalf("migrate = %v; want the migration's own error", err)
	}
	if ok, _ := hasTable(pair.Write, "Half"); ok {
		t.Error("the failed migration's table survived the rollback")
	}
	applied, err := appliedMigrations(pair.Write)
	if err != nil {
		t.Fatal(err)
	}
	if applied["half-done"] {
		t.Error("a migration that failed is recorded as applied")
	}
}

// A database a newer build has migrated is refused by an older one, before
// any statement runs against a shape it does not know.
func TestADatabaseFromANewerBuildIsRefused(t *testing.T) {
	pair := openTemp(t)
	newer := append(append([]Migration{}, migrations...), sqlMigration("from-the-future", "CREATE TABLE Future (id INTEGER PRIMARY KEY)"))
	if _, err := migrate(pair.Write, newer); err != nil {
		t.Fatal(err)
	}

	for name, f := range map[string]func() error{
		"Prepare": func() error { return prepare(pair.Write, migrations) },
		"Migrate": func() error { _, err := migrate(pair.Write, migrations); return err },
	} {
		err := f()
		if err == nil {
			t.Errorf("%s accepted a database migrated by a newer build", name)
			continue
		}
		if !strings.Contains(err.Error(), "from-the-future") || !strings.Contains(err.Error(), "newer build") {
			t.Errorf("%s: error does not say which migration or what it means: %v", name, err)
		}
	}
}

// A fresh load is one transaction. A crash partway used to leave tables with
// no record, which the next start refused as a database from before the
// record existed -- blaming the operator for a failure that was its own.
func TestAFailedSchemaLoadLeavesNothingBehind(t *testing.T) {
	pair := openTemp(t)
	bad := "CREATE TABLE Half (id INTEGER PRIMARY KEY); CREATE TABLE Half (id INTEGER PRIMARY KEY)"
	if err := loadSchema(pair.Write, bad, migrations); err == nil {
		t.Fatal("a schema that cannot apply was accepted")
	}
	if populated, _ := hasTables(pair.Write); populated {
		t.Fatal("a failed load left tables behind")
	}
	if err := Prepare(pair.Write); err != nil {
		t.Errorf("the next start after a failed load was refused: %v", err)
	}
}

// TestWriteBaseline rewrites the fixture from the current schema. It is a
// tool, not a test, and runs only when asked; see baselinePath for when that
// is appropriate.
func TestWriteBaseline(t *testing.T) {
	if os.Getenv("SILO_WRITE_BASELINE") == "" {
		t.Skip("set SILO_WRITE_BASELINE=1 to rewrite " + baselinePath)
	}
	// A plain connection rather than OpenSQLite, because the page size can
	// only be set before the first table exists and not once in WAL mode, and
	// a schema-only database on 1 KiB pages is a quarter the size of one on
	// the default.
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "silo.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec("PRAGMA page_size = 1024"); err != nil {
		t.Fatal(err)
	}
	if err := Prepare(db); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(baselinePath), 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(baselinePath)
	if _, err := db.Exec("VACUUM INTO ?", baselinePath); err != nil {
		t.Fatal(err)
	}
	t.Logf("wrote %s", baselinePath)
}
