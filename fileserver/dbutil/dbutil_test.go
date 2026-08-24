package dbutil

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestInsertOrReplace(t *testing.T) {
	got := InsertOrReplace("LibraryHead", "library_id, branch_name")
	want := "INSERT OR REPLACE INTO LibraryHead (library_id, branch_name) VALUES (?, ?)"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestInsertOrIgnore(t *testing.T) {
	got := InsertOrIgnore("GarbageLibraries", "library_id")
	want := "INSERT OR IGNORE INTO GarbageLibraries (library_id) VALUES (?)"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestInsertOrIgnoreMultipleColumns(t *testing.T) {
	got := InsertOrIgnore("LibraryUserToken", "library_id, email, token")
	want := "INSERT OR IGNORE INTO LibraryUserToken (library_id, email, token) VALUES (?, ?, ?)"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestInsertOrReplaceMultipleColumns(t *testing.T) {
	got := InsertOrReplace("LibraryOwner", "library_id, owner_id")
	want := "INSERT OR REPLACE INTO LibraryOwner (library_id, owner_id) VALUES (?, ?)"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestOpenSQLite(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	pair, err := OpenSQLite(dbPath)
	if err != nil {
		t.Fatalf("OpenSQLite failed: %v", err)
	}
	defer func() { _ = pair.Close() }()

	if pair.Read == nil || pair.Write == nil {
		t.Fatal("expected non-nil Read and Write handles")
	}
	if pair.Read == pair.Write {
		t.Fatal("SQLite Read and Write should be separate connections")
	}

	// Verify WAL mode
	var mode string
	if err := pair.Write.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatalf("failed to query journal_mode: %v", err)
	}
	if mode != "wal" {
		t.Errorf("expected WAL mode, got %q", mode)
	}
}

func TestOpenSQLiteCreatesFile(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "new.db")

	pair, err := OpenSQLite(dbPath)
	if err != nil {
		t.Fatalf("OpenSQLite failed: %v", err)
	}
	_ = pair.Close()

	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		t.Fatal("expected database file to be created")
	}
}

func TestDBPairCloseShared(t *testing.T) {
	// Close guards against Read and Write aliasing the same handle, so a
	// pair built that way closes once and reports no error.
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "shared.db")

	pair, err := openTestDB(dbPath)
	if err != nil {
		t.Fatalf("failed to open test db: %v", err)
	}
	_ = pair.Read.Close()
	pair.Read = pair.Write

	if err := pair.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}
}

func TestSQLiteUpsertIntegration(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "upsert.db")

	pair, err := OpenSQLite(dbPath)
	if err != nil {
		t.Fatalf("OpenSQLite failed: %v", err)
	}
	defer func() { _ = pair.Close() }()

	// Create a test table
	if _, err := pair.Write.Exec("CREATE TABLE test_kv (key TEXT PRIMARY KEY, value TEXT)"); err != nil {
		t.Fatalf("failed to create table: %v", err)
	}

	// Insert a row
	sql := InsertOrReplace("test_kv", "key, value")
	if _, err := pair.Write.Exec(sql, "k1", "v1"); err != nil {
		t.Fatalf("initial insert failed: %v", err)
	}

	// Upsert should overwrite
	if _, err := pair.Write.Exec(sql, "k1", "v2"); err != nil {
		t.Fatalf("upsert failed: %v", err)
	}

	var val string
	if err := pair.Read.QueryRow("SELECT value FROM test_kv WHERE key=?", "k1").Scan(&val); err != nil {
		t.Fatalf("query failed: %v", err)
	}
	if val != "v2" {
		t.Errorf("expected v2 after upsert, got %q", val)
	}

	// Count should be 1, not 2
	var count int
	if err := pair.Read.QueryRow("SELECT COUNT(*) FROM test_kv").Scan(&count); err != nil {
		t.Fatalf("count query failed: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1 row after upsert, got %d", count)
	}
}

func TestSQLiteInsertOrIgnoreIntegration(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "ignore.db")

	pair, err := OpenSQLite(dbPath)
	if err != nil {
		t.Fatalf("OpenSQLite failed: %v", err)
	}
	defer func() { _ = pair.Close() }()

	if _, err := pair.Write.Exec("CREATE TABLE test_ids (id TEXT PRIMARY KEY)"); err != nil {
		t.Fatalf("failed to create table: %v", err)
	}

	sql := InsertOrIgnore("test_ids", "id")

	// First insert succeeds
	if _, err := pair.Write.Exec(sql, "abc"); err != nil {
		t.Fatalf("first insert failed: %v", err)
	}

	// Duplicate insert should not error
	if _, err := pair.Write.Exec(sql, "abc"); err != nil {
		t.Fatalf("duplicate insert should not error: %v", err)
	}

	// Should still be 1 row
	var count int
	if err := pair.Read.QueryRow("SELECT COUNT(*) FROM test_ids").Scan(&count); err != nil {
		t.Fatalf("count query failed: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1 row, got %d", count)
	}
}

func TestSQLiteReadWriteSplit(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "split.db")

	pair, err := OpenSQLite(dbPath)
	if err != nil {
		t.Fatalf("OpenSQLite failed: %v", err)
	}
	defer func() { _ = pair.Close() }()

	if _, err := pair.Write.Exec("CREATE TABLE nums (n INTEGER)"); err != nil {
		t.Fatalf("create table failed: %v", err)
	}
	if _, err := pair.Write.Exec("INSERT INTO nums VALUES (42)"); err != nil {
		t.Fatalf("insert failed: %v", err)
	}

	// Read connection should see the write (WAL mode)
	var n int
	if err := pair.Read.QueryRow("SELECT n FROM nums").Scan(&n); err != nil {
		t.Fatalf("read failed: %v", err)
	}
	if n != 42 {
		t.Errorf("expected 42, got %d", n)
	}

	// Write via read connection should fail (read-only)
	_, err = pair.Read.Exec("INSERT INTO nums VALUES (99)")
	if err == nil {
		t.Fatal("expected error writing via read-only connection")
	}
}

// TestSQLiteReadPreparedStatement guards against regressing the original
// d6ec679 workaround that removed read-only mode "because it caused issues
// with prepared statements". The current implementation uses
// PRAGMA query_only=ON (applied via the _pragma= DSN parameter on every new
// pool connection) instead of URI mode=ro, so Prepare/repeated exec must
// work reliably on pair.Read. If we ever switch back to mode=ro without
// solving the shm/prepared-statement interaction, this test will catch it.
func TestSQLiteReadPreparedStatement(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "prepared.db")

	pair, err := OpenSQLite(dbPath)
	if err != nil {
		t.Fatalf("OpenSQLite failed: %v", err)
	}
	defer func() { _ = pair.Close() }()

	if _, err := pair.Write.Exec("CREATE TABLE kv (k TEXT PRIMARY KEY, v INTEGER)"); err != nil {
		t.Fatalf("create table failed: %v", err)
	}
	for i := 0; i < 20; i++ {
		if _, err := pair.Write.Exec("INSERT INTO kv VALUES (?, ?)", fmt.Sprintf("key-%d", i), i); err != nil {
			t.Fatalf("insert %d failed: %v", i, err)
		}
	}

	// Prepare once on the read pool and reuse across many calls. The pool
	// has 4 connections; executing 40 calls guarantees multiple underlying
	// connections are exercised, so if _pragma=query_only isn't applied
	// to every new conn, at least one call will hit a conn without the
	// pragma and the follow-up write check would succeed incorrectly.
	stmt, err := pair.Read.Prepare("SELECT v FROM kv WHERE k = ?")
	if err != nil {
		t.Fatalf("prepare on read conn failed: %v", err)
	}
	defer func() { _ = stmt.Close() }()

	for i := 0; i < 40; i++ {
		key := fmt.Sprintf("key-%d", i%20)
		var v int
		if err := stmt.QueryRow(key).Scan(&v); err != nil {
			t.Fatalf("prepared query %d failed: %v", i, err)
		}
		if v != i%20 {
			t.Errorf("call %d: got %d, want %d", i, v, i%20)
		}
	}

	// Concurrent readers should all succeed — WAL lets multiple read
	// connections coexist with the single writer.
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				var v int
				if err := stmt.QueryRow("key-5").Scan(&v); err != nil {
					t.Errorf("concurrent read failed: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()

	// And a fresh Exec on pair.Read must still be rejected — proving
	// query_only is in force on the pool connections touched above.
	if _, err := pair.Read.Exec("INSERT INTO kv VALUES ('late', 999)"); err == nil {
		t.Fatal("expected write via read pool to fail after prepared reads")
	}
}

// TestSQLiteWriteTransactionNoHang is a general lock-contention canary for
// the write pool. It does NOT directly reproduce the FOR UPDATE hang fixed
// in d6ec679 — that bug lived in fileop.go's updateBranch(), not in
// dbutil. What it does catch: any future change to OpenSQLite (pool sizing,
// busy_timeout, WAL pragmas) that makes a plain BEGIN / SELECT / UPDATE /
// COMMIT deadlock on the single writer. The deadline ensures a hang fails
// the test instead of stalling the suite.
func TestSQLiteWriteTransactionNoHang(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "tx.db")

	pair, err := OpenSQLite(dbPath)
	if err != nil {
		t.Fatalf("OpenSQLite failed: %v", err)
	}
	defer func() { _ = pair.Close() }()

	if _, err := pair.Write.Exec("CREATE TABLE branch (library TEXT PRIMARY KEY, commit_id TEXT)"); err != nil {
		t.Fatalf("create table failed: %v", err)
	}
	if _, err := pair.Write.Exec("INSERT INTO branch VALUES ('r1', 'c0')"); err != nil {
		t.Fatalf("seed failed: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		tx, err := pair.Write.BeginTx(ctx, nil)
		if err != nil {
			done <- fmt.Errorf("begin: %w", err)
			return
		}
		var head string
		if err := tx.QueryRowContext(ctx, "SELECT commit_id FROM branch WHERE library = ?", "r1").Scan(&head); err != nil {
			_ = tx.Rollback()
			done <- fmt.Errorf("select in tx: %w", err)
			return
		}
		if head != "c0" {
			_ = tx.Rollback()
			done <- fmt.Errorf("unexpected head %q", head)
			return
		}
		if _, err := tx.ExecContext(ctx, "UPDATE branch SET commit_id = ? WHERE library = ? AND commit_id = ?", "c1", "r1", head); err != nil {
			_ = tx.Rollback()
			done <- fmt.Errorf("update in tx: %w", err)
			return
		}
		if err := tx.Commit(); err != nil {
			done <- fmt.Errorf("commit: %w", err)
			return
		}
		done <- nil
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("write transaction failed: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("write transaction hung — probable FOR UPDATE / locking regression")
	}

	// And verify the committed state is visible via the read pool.
	var head string
	if err := pair.Read.QueryRow("SELECT commit_id FROM branch WHERE library = ?", "r1").Scan(&head); err != nil {
		t.Fatalf("post-commit read failed: %v", err)
	}
	if head != "c1" {
		t.Errorf("expected c1, got %q", head)
	}
}

func openTestDB(path string) (*DBPair, error) {
	return OpenSQLite(path)
}
