package dbutil

// Schema migrations: how a database written by one build becomes the shape
// the next build expects.
//
// Two paths, and which one a database takes is decided by what is already in
// it. A fresh database loads siloSchema in one transaction and records every
// migration as applied, because the schema constant already describes the
// shape those migrations produce. An existing database runs only the
// migrations it has not recorded, in the order they are listed here, each in
// its own transaction with its row written as that transaction's last
// statement -- so a migration either happened and says so, or did not happen
// at all.
//
// The record is a set of names rather than a version number. A number has to
// be bumped, and two branches that each bump it collide on the value while
// agreeing on nothing else; a name is a fact about this database, and the
// list below is the only thing that orders it. Order is the slice, not the
// name: merging a branch means placing its migration in the list, which is a
// decision made in a diff rather than by whichever timestamp sorts first.
//
// What that means for a change to siloSchema: edit the constant, and add the
// migration that takes an existing database to the same place. Nothing here
// re-applies the schema to a database that already has one, so a change made
// in one and not the other is a database that drifts, and the baseline test in
// migrate_test.go is what notices.

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
)

// Migration is one change to the shape of silo.db, named so that a database
// can record that it has happened.
type Migration struct {
	// Name identifies the migration for the rest of the database's life. It
	// never changes once a build carrying it has run against a database
	// anyone keeps.
	Name string
	// Apply makes the change. It runs inside the transaction that records the
	// migration, so an error here means nothing was recorded either.
	Apply func(tx *sql.Tx) error
}

// sqlMigration is a Migration whose whole change is a list of statements.
// Comments and the split on ";" follow the same rules as siloSchema.
func sqlMigration(name, statements string) Migration {
	return Migration{Name: name, Apply: func(tx *sql.Tx) error {
		return execStatements(tx, statements)
	}}
}

// migrations is every migration, in the order it applies. Append only. An
// entry that has shipped is never reordered, renamed or removed: a database
// that recorded it would then hold a name this build does not know, which
// reads as a newer build's work and refuses to start.
var migrations = []Migration{
	// Seven tables inherited from upstream that nothing wrote and, by the end,
	// nothing read: three for groups, three for the share model LibraryGrant
	// replaced, and one for per-token peer records on a lane that was
	// deleted. See docs/plans/sharing.md § The grant model.
	sqlMigration("drop-inherited-group-and-share-tables", `
		DROP TABLE IF EXISTS GroupStructure;
		DROP TABLE IF EXISTS GroupUser;
		DROP TABLE IF EXISTS "Group";
		DROP TABLE IF EXISTS LibraryGroup;
		DROP TABLE IF EXISTS InnerPubLibrary;
		DROP TABLE IF EXISTS SharedLibrary;
		DROP TABLE IF EXISTS LibraryTokenPeerInfo;
	`),
}

// migrationTable is the record. It is created by this package rather than by
// siloSchema because it has to exist before the question "what has been
// applied here" can be asked of the schema at all.
const migrationTable = `CREATE TABLE IF NOT EXISTS SchemaMigration (
  name       TEXT    PRIMARY KEY,
  applied_at INTEGER NOT NULL
)`

// Prepare makes a database ready for a process that does not hold the data
// directory lock: a fresh one is created at the current shape, and an
// existing one is checked to be there already. It never migrates, because a
// migration under a running server is a change to the shape of tables that
// server is mid-statement on, and only the process holding the lock may make
// one. See Migrate.
func Prepare(db *sql.DB) error {
	return prepare(db, migrations)
}

// Migrate brings a database to the current shape and reports what it did: a
// fresh database is created, and an existing one has every migration it has
// not recorded applied in order. The caller holds the data directory lock.
func Migrate(db *sql.DB) ([]string, error) {
	return migrate(db, migrations)
}

// Pending lists the migrations an existing database has not applied, in the
// order they would run. A fresh database has none: it is created at the
// current shape rather than migrated to it.
func Pending(db *sql.DB) ([]string, error) {
	st, err := inspect(db, migrations)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, m := range st.pending {
		names = append(names, m.Name)
	}
	return names, nil
}

func prepare(db *sql.DB, migs []Migration) error {
	st, err := inspect(db, migs)
	if err != nil {
		return err
	}
	if st.fresh {
		return loadSchema(db, siloSchema, migs)
	}
	if len(st.pending) > 0 {
		names := make([]string, 0, len(st.pending))
		for _, m := range st.pending {
			names = append(names, m.Name)
		}
		return fmt.Errorf("this database is %d migration(s) behind this build (%s); "+
			"run \"silo migrate\", or start the server, which migrates on start",
			len(names), strings.Join(names, ", "))
	}
	return nil
}

func migrate(db *sql.DB, migs []Migration) ([]string, error) {
	st, err := inspect(db, migs)
	if err != nil {
		return nil, err
	}
	if st.fresh {
		return nil, loadSchema(db, siloSchema, migs)
	}
	var applied []string
	for _, m := range st.pending {
		if err := applyOne(db, m); err != nil {
			return applied, err
		}
		log.Infof("Applied migration %s", m.Name)
		applied = append(applied, m.Name)
	}
	return applied, nil
}

// state is what inspect learns about a database before anything touches it.
type state struct {
	// fresh means no tables at all: a database this process is about to
	// create.
	fresh bool
	// pending is every listed migration the database has not recorded, in
	// list order.
	pending []Migration
}

// inspect reads the record and decides which path the database takes.
//
// Three populations, told apart by two queries. No tables at all is a fresh
// database. Tables but no record is a database written before this package
// tracked migrations, refused: nothing can say what shape it is in. A record
// naming a migration this build does not list is a database a newer build
// has migrated, also refused: the shape has moved past what this build's
// statements assume, and the failure that would produce is a raw SQLite
// error deep in whichever statement first meets the difference.
func inspect(db *sql.DB, migs []Migration) (state, error) {
	var st state
	hasRecord, err := hasTable(db, "SchemaMigration")
	if err != nil {
		return st, fmt.Errorf("failed to inspect the database: %w", err)
	}
	if !hasRecord {
		populated, err := hasTables(db)
		if err != nil {
			return st, fmt.Errorf("failed to inspect the database: %w", err)
		}
		if populated {
			return st, errors.New("this database has tables but no record of which migrations produced them, " +
				"so it was written before this build tracked migrations; " +
				"refusing to start rather than run a mismatched schema against it. " +
				"If this database is disposable, delete it and let Silo recreate it; " +
				"otherwise migrate it by hand")
		}
		st.fresh = true
		return st, nil
	}

	applied, err := appliedMigrations(db)
	if err != nil {
		return st, err
	}
	known := make(map[string]bool, len(migs))
	for _, m := range migs {
		known[m.Name] = true
	}
	for name := range applied {
		if !known[name] {
			return st, fmt.Errorf("this database records a migration this build does not know (%q), "+
				"so a newer build has already migrated it; "+
				"refusing to start rather than run an older schema against it. "+
				"Run the binary that wrote it", name)
		}
	}
	for _, m := range migs {
		if !applied[m.Name] {
			st.pending = append(st.pending, m)
		}
	}
	return st, nil
}

// loadSchema creates a fresh database: the record table, the schema, and a
// row for every migration, in one transaction. A crash partway leaves no
// tables, so the next start takes the fresh path again rather than finding
// tables with no record and refusing.
func loadSchema(db *sql.DB, schema string, migs []Migration) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("failed to begin the schema load: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec(migrationTable); err != nil {
		return fmt.Errorf("failed to create the migration record: %w", err)
	}
	if err := execStatements(tx, schema); err != nil {
		return err
	}
	now := time.Now().Unix()
	for _, m := range migs {
		if err := record(tx, m.Name, now); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("failed to commit the schema load: %w", err)
	}
	log.Info("Database tables created successfully")
	return nil
}

// applyOne runs a migration and records it in the same transaction.
func applyOne(db *sql.DB, m Migration) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("migration %s: failed to begin: %w", m.Name, err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := m.Apply(tx); err != nil {
		return fmt.Errorf("migration %s: %w", m.Name, err)
	}
	if err := record(tx, m.Name, time.Now().Unix()); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migration %s: failed to commit: %w", m.Name, err)
	}
	return nil
}

func record(tx *sql.Tx, name string, at int64) error {
	if _, err := tx.Exec("INSERT INTO SchemaMigration (name, applied_at) VALUES (?, ?)", name, at); err != nil {
		return fmt.Errorf("failed to record migration %s: %w", name, err)
	}
	return nil
}

func appliedMigrations(db *sql.DB) (map[string]bool, error) {
	rows, err := db.Query("SELECT name FROM SchemaMigration")
	if err != nil {
		return nil, fmt.Errorf("failed to read the migration record: %w", err)
	}
	defer func() { _ = rows.Close() }()
	applied := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		applied[name] = true
	}
	return applied, rows.Err()
}

func hasTable(db *sql.DB, name string) (bool, error) {
	var n int
	err := db.QueryRow(
		`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, name).Scan(&n)
	return n > 0, err
}

// hasTables reports whether the database holds any table of its own.
//
// sqlite_% is excluded because those are SQLite's: sqlite_sequence appears on
// its own the first time an AUTOINCREMENT column is written, and counting it
// would make a database SQLite populated look like one Silo populated.
func hasTables(db *sql.DB) (bool, error) {
	var n int
	err := db.QueryRow(
		`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%'`).Scan(&n)
	return n > 0, err
}

// execer is what execStatements needs: a transaction, or the database when
// no transaction is wanted.
type execer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

// stripComments removes -- comments before the schema is split on semicolons.
//
// Splitting on ";" is how the statements are separated, and a comment
// containing one used to be cut in half and executed as SQL — a startup
// failure caused by a sentence. Every string that passes through here is this
// package's own constant, so there are no string literals to worry about
// escaping around: every -- in it starts a comment.
func stripComments(schema string) string {
	var b strings.Builder
	for _, line := range strings.Split(schema, "\n") {
		if i := strings.Index(line, "--"); i >= 0 {
			line = line[:i]
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}

func execStatements(e execer, statements string) error {
	for _, stmt := range strings.Split(stripComments(statements), ";") {
		stmt = strings.TrimSpace(stmt)
		if stmt == "" {
			continue
		}
		if _, err := e.Exec(stmt); err != nil {
			return fmt.Errorf("failed to execute schema statement: %v\nSQL: %s", err, stmt)
		}
	}
	return nil
}
