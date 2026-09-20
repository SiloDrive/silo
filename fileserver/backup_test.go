package silod

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/SiloDrive/silo/fileserver/dbutil"
)

// backupTestDataDir builds a data directory holding the SQLite database with
// a row committed but deliberately not checkpointed, and isolates the
// process-global path state RunBackupDB reads and writes.
func backupTestDataDir(t *testing.T) string {
	t.Helper()

	// resolvePaths falls back to the XDG config directory looking for a real
	// silo.conf, which on a developer's machine would point at a real data
	// directory.
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("SILO_DATA_DIR", "")

	origDataDir, origConfigFile, origAbsDataDir := dataDir, configFile, absDataDir
	t.Cleanup(func() {
		dataDir, configFile, absDataDir = origDataDir, origConfigFile, origAbsDataDir
	})

	dir := t.TempDir()
	pair, err := dbutil.OpenSQLite(filepath.Join(dir, DatabaseName))
	if err != nil {
		t.Fatalf("failed to create %s: %v", DatabaseName, err)
	}
	if _, err := pair.Write.Exec("CREATE TABLE Marker (v TEXT)"); err != nil {
		t.Fatalf("failed to create table: %v", err)
	}
	if _, err := pair.Write.Exec("INSERT INTO Marker (v) VALUES (?)", backupMarker); err != nil {
		t.Fatalf("failed to insert: %v", err)
	}
	// No checkpoint: the row lives in the -wal file, which is exactly the
	// state a `cp silo.db` backup silently loses.
	if err := pair.Close(); err != nil {
		t.Fatalf("failed to close %s: %v", DatabaseName, err)
	}
	return dir
}

const backupMarker = "committed-but-not-checkpointed"

// The snapshot has to contain writes that were committed but not yet
// checkpointed into the main database file — that is the whole reason the
// command exists rather than a documented cp.
func TestBackupDBCapturesUncheckpointedWrites(t *testing.T) {
	dir := backupTestDataDir(t)
	dest := filepath.Join(t.TempDir(), "snapshot")

	if err := RunBackupDB([]string{"-d", dir, dest}); err != nil {
		t.Fatalf("RunBackupDB returned %v", err)
	}

	copied := filepath.Join(dest, DatabaseName)
	if _, err := os.Stat(copied); err != nil {
		t.Fatalf("%s was not created: %v", copied, err)
	}
	// A snapshot is checkpointed by construction, so it must stand alone.
	for _, sidecar := range []string{DatabaseName + "-wal", DatabaseName + "-shm"} {
		if _, err := os.Stat(filepath.Join(dest, sidecar)); err == nil {
			t.Errorf("%s exists; the snapshot should need no sidecar files", sidecar)
		}
	}

	pair, err := dbutil.OpenSQLite(copied)
	if err != nil {
		t.Fatalf("failed to open snapshot %s: %v", copied, err)
	}
	var got string
	err = pair.Read.QueryRow("SELECT v FROM Marker").Scan(&got)
	_ = pair.Close()
	if err != nil {
		t.Fatalf("failed to read snapshot %s: %v", copied, err)
	}
	if got != backupMarker {
		t.Errorf("snapshot contains %q, want %q", got, backupMarker)
	}
}

// Overwriting last night's good backup with today's is the failure that makes
// a backup tool worse than none, so it takes an explicit flag.
func TestBackupDBRefusesToOverwriteWithoutForce(t *testing.T) {
	dir := backupTestDataDir(t)
	dest := filepath.Join(t.TempDir(), "snapshot")

	if err := RunBackupDB([]string{"-d", dir, dest}); err != nil {
		t.Fatalf("first RunBackupDB returned %v", err)
	}
	if err := RunBackupDB([]string{"-d", dir, dest}); err == nil {
		t.Error("second RunBackupDB overwrote the destination, want an error")
	}
	if err := RunBackupDB([]string{"-d", dir, "-f", dest}); err != nil {
		t.Errorf("RunBackupDB -f returned %v", err)
	}
}

// Writing the snapshot into the data directory would leave the copies where
// the next backup treats them as databases to snapshot.
func TestBackupDBRejectsDataDirAsDestination(t *testing.T) {
	dir := backupTestDataDir(t)

	if err := RunBackupDB([]string{"-d", dir, dir}); err == nil {
		t.Error("RunBackupDB accepted the data directory as its destination, want an error")
	}
}
