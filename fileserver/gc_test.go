package silod

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SiloDrive/silo/fileserver/account"
	"github.com/SiloDrive/silo/fileserver/credential"
	"github.com/SiloDrive/silo/fileserver/dbutil"
	"github.com/SiloDrive/silo/fileserver/libmgr"
	"github.com/SiloDrive/silo/fileserver/objstore"
	"github.com/SiloDrive/silo/fileserver/option"
	"github.com/SiloDrive/silo/fileserver/serversecret"
)

// sqliteTestDB points the package's database connection at a throwaway SQLite
// file and its object stores at a throwaway directory, and restores whatever
// was there when the test finishes.
//
// It initialises libmgr as well as the package globals because every caller
// did, on the next line, with a temp dir of its own — and one of them handed
// libmgr a different directory from the one it set absDataDir to, which is
// the failure this shape exists to make impossible. A test that wants to know
// where the objects went reads absDataDir.
func sqliteTestDB(t *testing.T) {
	t.Helper()

	// RunGC loads it from config; these tests call the internals directly, and
	// a zero timeout makes every query fail as "context deadline exceeded".
	origTimeout := option.DBOpTimeout
	option.DBOpTimeout = 5 * time.Second

	pair, err := dbutil.OpenSQLite(filepath.Join(t.TempDir(), "silo.db"))
	if err != nil {
		t.Fatalf("failed to open test database: %v", err)
	}
	if err := dbutil.Prepare(pair.Write); err != nil {
		t.Fatalf("failed to create test tables: %v", err)
	}

	origPair := siloPair
	origDataDir := absDataDir
	siloPair = pair
	absDataDir = t.TempDir()
	account.Init(pair.Read, pair.Write)
	credential.Init(pair.Read, pair.Write)
	serversecret.Init(pair.Read, pair.Write)
	libmgr.Init(pair.Read, pair.Write, absDataDir)

	t.Cleanup(func() {
		// The packs opened under this data directory are sealed before the
		// directory goes: objstore's registry is process-wide, so an open pack
		// left behind here is one the next test's Close trips over.
		_ = objstore.Close()
		siloPair = origPair
		absDataDir = origDataDir
		option.DBOpTimeout = origTimeout
		_ = pair.Read.Close()
		_ = pair.Write.Close()
	})
}

// mintAccount gives a test an account to own libraries and hold tokens. The
// tests still name people by address, which is what a reader recognises; the
// rows below them hold ids.
func mintAccount(t *testing.T, email string) *account.Account {
	t.Helper()
	ctx, cancel := option.WithDBTimeout(context.Background())
	defer cancel()
	// Create is idempotent — it hands back the existing row when the address
	// is already claimed — so minting and resolving differ only in whether
	// the account has to exist beforehand. acctFor does the reading part.
	if _, _, err := account.Create(ctx, email, "", account.RoleUser); err != nil {
		t.Fatalf("create account %s: %v", email, err)
	}
	return acctFor(t, email)
}

func dbExec(t *testing.T, query string, args ...interface{}) {
	t.Helper()
	if _, err := siloPair.Write.Exec(query, args...); err != nil {
		t.Fatalf("failed to exec %q: %v", query, err)
	}
}

// The store-sharing cases are the ones that destroy live data: libmgr sets a
// virtual library's StoreID to its origin's ID, so a directory named for a dead
// library can still hold a live library's only copy.
func TestUnsafeToReclaim(t *testing.T) {
	sqliteTestDB(t)

	const (
		clean    = "11111111-1111-1111-1111-111111111111"
		resurrec = "22222222-2222-2222-2222-222222222222"
		branched = "33333333-3333-3333-3333-333333333333"
		origin   = "44444444-4444-4444-4444-444444444444"
		virtual  = "55555555-5555-5555-5555-555555555555"
		liveVirt = "66666666-6666-6666-6666-666666666666"
	)

	insertTestLibrary(t, resurrec)
	dbExec(t, "INSERT INTO Branch (name, library_id, commit_id) VALUES ('master', ?, ?)",
		branched, "0401fc662e3bc87a41f299a907c056aaf8322a27")
	// A live virtual library whose objects are written into the dead origin's store.
	dbExec(t, "INSERT INTO VirtualLibrary (library_id, origin_library, path, base_commit) VALUES (?, ?, '/sub', ?)",
		liveVirt, origin, "0401fc662e3bc87a41f299a907c056aaf8322a27")
	// A dead library that is itself virtual: it owns no store directory.
	dbExec(t, "INSERT INTO VirtualLibrary (library_id, origin_library, path, base_commit) VALUES (?, ?, '/sub', ?)",
		virtual, "77777777-7777-7777-7777-777777777777", "0401fc662e3bc87a41f299a907c056aaf8322a27")

	cases := []struct {
		name      string
		libraryID string
		wantFor   string // substring of the expected reason; "" means reclaimable
	}{
		{"deleted and unreferenced", clean, ""},
		{"still present in Library", resurrec, "Library"},
		{"still has a branch head", branched, "Branch"},
		{"origin of a live virtual library", origin, "origin"},
		{"is itself a virtual library", virtual, "virtual"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := unsafeToReclaim(context.Background(), c.libraryID)
			if err != nil {
				t.Fatalf("unsafeToReclaim returned error: %v", err)
			}
			if c.wantFor == "" {
				if got != "" {
					t.Errorf("expected %s to be reclaimable, got skip %q", c.libraryID, got)
				}
				return
			}
			if got == "" {
				t.Fatalf("expected %s to be skipped for %q, but GC would have deleted it", c.libraryID, c.wantFor)
			}
			if !strings.Contains(got, c.wantFor) {
				t.Errorf("skip reason = %q, want it to mention %q", got, c.wantFor)
			}
		})
	}
}

// Reclaiming must remove every store directory for the dead library and clear its
// GarbageLibraries row, and must leave a live library's store alone.
func TestReclaimRemovesOnlyTheDeadLibrary(t *testing.T) {
	sqliteTestDB(t)

	const (
		dead = "11111111-1111-1111-1111-111111111111"
		live = "22222222-2222-2222-2222-222222222222"
	)

	for _, libraryID := range []string{dead, live} {
		for _, objType := range objstore.Types {
			objDir := filepath.Join(objstore.LibraryDir(absDataDir, objType, libraryID), "04")
			if err := os.MkdirAll(objDir, 0700); err != nil {
				t.Fatalf("failed to create %s: %v", objDir, err)
			}
			obj := filepath.Join(objDir, "01fc662e3bc87a41f299a907c056aaf8322a27")
			if err := os.WriteFile(obj, []byte("payload"), 0600); err != nil {
				t.Fatalf("failed to write %s: %v", obj, err)
			}
		}
	}

	dbExec(t, "INSERT INTO GarbageLibraries (library_id) VALUES (?)", dead)
	insertTestLibrary(t, live)

	libraries, err := collectGarbageLibraries()
	if err != nil {
		t.Fatalf("collectGarbageLibraries returned error: %v", err)
	}
	if len(libraries) != 1 {
		t.Fatalf("expected 1 garbage library, got %d", len(libraries))
	}
	r := libraries[0]
	if r.skip != "" {
		t.Fatalf("expected %s to be reclaimable, got skip %q", dead, r.skip)
	}
	if len(r.stores) != len(objstore.Types) {
		t.Errorf("expected %d stores to reclaim from, got %d", len(objstore.Types), len(r.stores))
	}
	if r.files != len(objstore.Types) {
		t.Errorf("counted %d objects, want %d", r.files, len(objstore.Types))
	}
	if r.bytes != int64(len("payload")*len(objstore.Types)) {
		t.Errorf("measured %d bytes, want %d", r.bytes, len("payload")*len(objstore.Types))
	}

	if err := reclaim(r); err != nil {
		t.Fatalf("reclaim returned error: %v", err)
	}

	for _, objType := range objstore.Types {
		deadDir := objstore.LibraryDir(absDataDir, objType, dead)
		if _, err := os.Stat(deadDir); !os.IsNotExist(err) {
			t.Errorf("%s survived reclaim", deadDir)
		}
		liveDir := objstore.LibraryDir(absDataDir, objType, live)
		if _, err := os.Stat(liveDir); err != nil {
			t.Errorf("live library's %s was removed: %v", liveDir, err)
		}
	}

	var remaining int
	if err := siloPair.Read.QueryRow("SELECT COUNT(*) FROM GarbageLibraries").Scan(&remaining); err != nil {
		t.Fatalf("failed to count GarbageLibraries: %v", err)
	}
	if remaining != 0 {
		t.Errorf("GarbageLibraries still has %d rows after reclaim", remaining)
	}
}

// A dead origin whose virtual library is still live must survive collection
// entirely: no directories measured, so a -delete pass has nothing to remove.
func TestCollectSkipsOriginOfLiveVirtualLibrary(t *testing.T) {
	sqliteTestDB(t)

	const (
		origin  = "11111111-1111-1111-1111-111111111111"
		virtual = "22222222-2222-2222-2222-222222222222"
	)

	objDir := filepath.Join(absDataDir, "storage", "fs", origin, "04")
	if err := os.MkdirAll(objDir, 0700); err != nil {
		t.Fatalf("failed to create %s: %v", objDir, err)
	}
	obj := filepath.Join(objDir, "01fc662e3bc87a41f299a907c056aaf8322a27")
	if err := os.WriteFile(obj, []byte("the virtual library's only copy"), 0600); err != nil {
		t.Fatalf("failed to write %s: %v", obj, err)
	}

	dbExec(t, "INSERT INTO GarbageLibraries (library_id) VALUES (?)", origin)
	insertTestLibrary(t, virtual)
	dbExec(t, "INSERT INTO VirtualLibrary (library_id, origin_library, path, base_commit) VALUES (?, ?, '/sub', ?)",
		virtual, origin, "0401fc662e3bc87a41f299a907c056aaf8322a27")

	libraries, err := collectGarbageLibraries()
	if err != nil {
		t.Fatalf("collectGarbageLibraries returned error: %v", err)
	}
	if len(libraries) != 1 {
		t.Fatalf("expected 1 garbage library, got %d", len(libraries))
	}
	if libraries[0].skip == "" {
		t.Fatal("GC would delete the store backing a live virtual library")
	}
	if len(libraries[0].stores) != 0 {
		t.Errorf("skipped library still had %d stores queued for removal",
			len(libraries[0].stores))
	}

	if _, err := os.Stat(obj); err != nil {
		t.Errorf("the virtual library's object was disturbed: %v", err)
	}
}

// insertTestLibrary creates the catalog row a library needs to exist, in the
// default server-readable format. The format columns have no DEFAULT — a
// creation path that forgets them should fail — so tests write them too.
func insertTestLibrary(t *testing.T, libraryID string) {
	t.Helper()
	f := libmgr.DefaultFormat(false)
	dbExec(t, "INSERT INTO Library (library_id, chunker, chunk_min, chunk_target, chunk_max, chunk_norm, e2ee) VALUES (?, ?, ?, ?, ?, ?, ?)",
		libraryID, f.Chunker, f.MinSize, f.TargetSize, f.MaxSize, f.Normalization, f.E2EE)
}
