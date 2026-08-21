package dbutil

import (
	"database/sql"
	"fmt"
	"strings"

	_ "modernc.org/sqlite"
)

// InsertOrReplace returns an upsert statement that inserts a row or
// overwrites it if a conflict on the primary key is found.
//
//	dbutil.InsertOrReplace("RepoHead", "repo_id, branch_name")
//	→ "INSERT OR REPLACE INTO RepoHead (repo_id, branch_name) VALUES (?, ?)"
func InsertOrReplace(table, columns string) string {
	return fmt.Sprintf("INSERT OR REPLACE INTO %s (%s) VALUES (%s)",
		table, columns, makePlaceholders(countColumns(columns)))
}

// InsertOrIgnore returns a statement that inserts a row or silently
// does nothing if a conflict on the primary key is found.
//
//	dbutil.InsertOrIgnore("GarbageRepos", "repo_id")
//	→ "INSERT OR IGNORE INTO GarbageRepos (repo_id) VALUES (?)"
func InsertOrIgnore(table, columns string) string {
	return fmt.Sprintf("INSERT OR IGNORE INTO %s (%s) VALUES (%s)",
		table, columns, makePlaceholders(countColumns(columns)))
}

// RowsAffected reports how many rows a statement touched, or zero when the
// driver cannot say.
//
// The statement has already succeeded by the time this is called — only the
// count is in doubt — so a driver that does not support the count must not
// turn a completed delete into a caller-visible failure. Callers that report
// a count to a user get a truthful "0" instead of a spurious error.
func RowsAffected(res sql.Result) int64 {
	n, err := res.RowsAffected()
	if err != nil {
		return 0
	}
	return n
}

func countColumns(columns string) int {
	return len(strings.Split(columns, ","))
}

func makePlaceholders(n int) string {
	ph := make([]string, n)
	for i := range ph {
		ph[i] = "?"
	}
	return strings.Join(ph, ", ")
}

// DBPair holds separate read and write connections to the one database.
// Write has MaxOpenConns(1) to serialise writes, Read has MaxOpenConns(4)
// for concurrent reads. Both use WAL mode.
type DBPair struct {
	Read  *sql.DB
	Write *sql.DB
}

// Close closes both database connections.
func (p *DBPair) Close() error {
	var err error
	if p.Write != nil {
		err = p.Write.Close()
	}
	if p.Read != nil && p.Read != p.Write {
		if rerr := p.Read.Close(); rerr != nil && err == nil {
			err = rerr
		}
	}
	return err
}

// OpenSQLite opens a SQLite database with WAL mode and read/write connection split.
func OpenSQLite(path string) (*DBPair, error) {
	writeDSN := fmt.Sprintf("file:%s?_pragma=journal_mode%%3DWAL&_pragma=busy_timeout%%3D5000&_pragma=synchronous%%3DNORMAL&_pragma=foreign_keys%%3DON", path)
	writeDB, err := sql.Open("sqlite", writeDSN)
	if err != nil {
		return nil, fmt.Errorf("failed to open sqlite write connection: %v", err)
	}
	writeDB.SetMaxOpenConns(1)
	writeDB.SetMaxIdleConns(1)
	writeDB.SetConnMaxLifetime(0)

	// Verify WAL mode is set on the write connection
	var journalMode string
	if err := writeDB.QueryRow("PRAGMA journal_mode").Scan(&journalMode); err != nil {
		_ = writeDB.Close()
		return nil, fmt.Errorf("failed to check journal mode: %v", err)
	}
	if journalMode != "wal" {
		// Set WAL explicitly if pragma DSN didn't work
		if _, err := writeDB.Exec("PRAGMA journal_mode=WAL"); err != nil {
			_ = writeDB.Close()
			return nil, fmt.Errorf("failed to set WAL mode: %v", err)
		}
	}

	// Read connection uses PRAGMA query_only=ON so any accidental write
	// via pair.Read fails loudly with SQLITE_READONLY. We deliberately do
	// NOT use URI mode=ro here — a previous attempt (commit d6ec679) had
	// to back that out because true read-only opens interact badly with
	// WAL shared-memory setup and prepared statements. query_only is
	// enforced at the engine level after a normal read-write open, so
	// WAL/shm/prepared statements all behave as usual while writes still
	// get rejected. The _pragma= DSN parameter applies on every new pool
	// connection, so it persists across the connection pool.
	readDSN := fmt.Sprintf("file:%s?_pragma=journal_mode%%3DWAL&_pragma=busy_timeout%%3D5000&_pragma=synchronous%%3DNORMAL&_pragma=foreign_keys%%3DON&_pragma=query_only%%3DON", path)
	readDB, err := sql.Open("sqlite", readDSN)
	if err != nil {
		_ = writeDB.Close()
		return nil, fmt.Errorf("failed to open sqlite read connection: %v", err)
	}
	readDB.SetMaxOpenConns(4)
	readDB.SetMaxIdleConns(4)
	readDB.SetConnMaxLifetime(0)

	return &DBPair{Read: readDB, Write: writeDB}, nil
}
