package objstore

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// A second process on one data directory is refused before it can touch a
// live open pack.
//
// This is the damage the lock exists to prevent, and it is worth stating in
// full because nothing about it is visible while it happens. A second
// instance reaches loadPackSet, sees the first's sidecar, and recoverPack
// truncates the pack to its last indexed record -- while the first is
// mid-append. The second then seals it: a footer at that offset, and the
// sidecar the first still holds open unlinked. The first goes on appending
// past the footer. On the next start there is no sidecar, so the pack reads
// as sealed, the trailer check fails, and the library is ErrPackCorrupt --
// including every frame the first server acknowledged to a client.
//
// The refusal has to come before loadPackSet rather than inside it, because
// by the time a pack set is being read the truncation is the read.
func TestSecondProcessCannotAdoptALiveOpenPack(t *testing.T) {
	dataDir := t.TempDir()
	objDir := filepath.Join(dataDir, "objects")
	const lib = "lib-under-test"
	key := packKey(t)

	// The first server: holding the directory, with an open pack it is still
	// appending to.
	first, err := LockDataDir(dataDir)
	if err != nil {
		t.Fatalf("the first holder was refused: %v", err)
	}
	id, frame := framed(t, key, "acknowledged to a client before the second server started")
	live := filledPack(t, objDir, lib, [][]byte{frame}, []string{id})
	defer live.close()

	// The second server, on the same directory.
	if _, err := LockDataDir(dataDir); err == nil {
		t.Fatal("a second holder of the data directory was allowed in")
	}

	// And the first's frames are still there to be read, which is the whole
	// point of refusing the second.
	set, err := loadPackSet(objDir, lib)
	if err != nil {
		t.Fatalf("loadPackSet after the refusal: %v", err)
	}
	if set.open == nil {
		t.Fatal("the open pack is gone")
	}
	if _, _, ok := set.find(id); !ok {
		t.Error("an acknowledged frame is no longer in the pack")
	}

	if err := first.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
}

// The refusal names the process holding the directory.
//
// "Already locked" sends an operator to look for a stale lockfile. A pid
// sends them to `ps`, which is where the answer is: either the server is
// running and they meant to stop it, or it is not and the file is theirs to
// remove.
func TestTheDataDirRefusalNamesTheHolder(t *testing.T) {
	dataDir := t.TempDir()

	held, err := LockDataDir(dataDir)
	if err != nil {
		t.Fatalf("LockDataDir: %v", err)
	}
	defer func() { _ = held.Release() }()

	_, err = LockDataDir(dataDir)
	if err == nil {
		t.Fatal("a second holder was allowed in")
	}
	if pid := strconv.Itoa(os.Getpid()); !strings.Contains(err.Error(), pid) {
		t.Errorf("the refusal is %q; it must name the holder's pid %s", err, pid)
	}
}

// Releasing lets the next process in, so a clean shutdown needs no cleanup
// and a stale file is not a lock.
func TestReleasingTheDataDirLetsTheNextProcessIn(t *testing.T) {
	dataDir := t.TempDir()

	first, err := LockDataDir(dataDir)
	if err != nil {
		t.Fatalf("LockDataDir: %v", err)
	}
	if err := first.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}

	second, err := LockDataDir(dataDir)
	if err != nil {
		t.Fatalf("the directory stayed locked after Release: %v", err)
	}
	if err := second.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
}

// A lock file left behind by a process that is gone is not a lock.
//
// flock is held by an open file description, so it dies with the process that
// held it however the process died -- kill -9, OOM, a power cut. The file
// surviving is expected, and an operator must never have to reason about
// whether one is stale.
func TestALeftoverLockFileIsNotALock(t *testing.T) {
	dataDir := t.TempDir()

	if err := os.WriteFile(filepath.Join(dataDir, lockFileName), []byte("999999\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	held, err := LockDataDir(dataDir)
	if err != nil {
		t.Fatalf("a lock file nobody holds refused a start: %v", err)
	}
	if err := held.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
}
