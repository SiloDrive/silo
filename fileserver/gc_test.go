package silod

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dkam/silo/fileserver/account"
	"github.com/dkam/silo/fileserver/dbutil"
	"github.com/dkam/silo/fileserver/objstore"
	"github.com/dkam/silo/fileserver/option"
)

// sqliteTestDB points the package's seafile connection at a throwaway SQLite file
// and restores whatever was there when the test finishes.
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
	if err := dbutil.CreateSiloTables(pair.Write); err != nil {
		t.Fatalf("failed to create test tables: %v", err)
	}

	origPair := siloPair
	origDataDir := absDataDir
	siloPair = pair
	account.Init(pair.Read, pair.Write)

	t.Cleanup(func() {
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
	if _, _, err := account.Create(ctx, email, "", false); err != nil {
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

// The store-sharing cases are the ones that destroy live data: repomgr sets a
// virtual repo's StoreID to its origin's ID, so a directory named for a dead
// repo can still hold a live repo's only copy.
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

	dbExec(t, "INSERT INTO Repo (repo_id) VALUES (?)", resurrec)
	dbExec(t, "INSERT INTO Branch (name, repo_id, commit_id) VALUES ('master', ?, ?)",
		branched, "0401fc662e3bc87a41f299a907c056aaf8322a27")
	// A live virtual repo whose objects are written into the dead origin's store.
	dbExec(t, "INSERT INTO VirtualRepo (repo_id, origin_repo, path, base_commit) VALUES (?, ?, '/sub', ?)",
		liveVirt, origin, "0401fc662e3bc87a41f299a907c056aaf8322a27")
	// A dead repo that is itself virtual: it owns no store directory.
	dbExec(t, "INSERT INTO VirtualRepo (repo_id, origin_repo, path, base_commit) VALUES (?, ?, '/sub', ?)",
		virtual, "77777777-7777-7777-7777-777777777777", "0401fc662e3bc87a41f299a907c056aaf8322a27")

	cases := []struct {
		name    string
		repoID  string
		wantFor string // substring of the expected reason; "" means reclaimable
	}{
		{"deleted and unreferenced", clean, ""},
		{"still present in Repo", resurrec, "Repo"},
		{"still has a branch head", branched, "Branch"},
		{"origin of a live virtual repo", origin, "origin"},
		{"is itself a virtual repo", virtual, "virtual"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := unsafeToReclaim(context.Background(), c.repoID)
			if err != nil {
				t.Fatalf("unsafeToReclaim returned error: %v", err)
			}
			if c.wantFor == "" {
				if got != "" {
					t.Errorf("expected %s to be reclaimable, got skip %q", c.repoID, got)
				}
				return
			}
			if got == "" {
				t.Fatalf("expected %s to be skipped for %q, but GC would have deleted it", c.repoID, c.wantFor)
			}
			if !strings.Contains(got, c.wantFor) {
				t.Errorf("skip reason = %q, want it to mention %q", got, c.wantFor)
			}
		})
	}
}

// Reclaiming must remove every store directory for the dead repo and clear its
// GarbageRepos row, and must leave a live repo's store alone.
func TestReclaimRemovesOnlyTheDeadRepo(t *testing.T) {
	sqliteTestDB(t)

	const (
		dead = "11111111-1111-1111-1111-111111111111"
		live = "22222222-2222-2222-2222-222222222222"
	)

	absDataDir = t.TempDir()
	for _, repoID := range []string{dead, live} {
		for _, objType := range objstore.Types {
			objDir := filepath.Join(objstore.RepoDir(absDataDir, objType, repoID), "04")
			if err := os.MkdirAll(objDir, 0700); err != nil {
				t.Fatalf("failed to create %s: %v", objDir, err)
			}
			obj := filepath.Join(objDir, "01fc662e3bc87a41f299a907c056aaf8322a27")
			if err := os.WriteFile(obj, []byte("payload"), 0600); err != nil {
				t.Fatalf("failed to write %s: %v", obj, err)
			}
		}
	}

	dbExec(t, "INSERT INTO GarbageRepos (repo_id) VALUES (?)", dead)
	dbExec(t, "INSERT INTO Repo (repo_id) VALUES (?)", live)

	repos, err := collectGarbageRepos()
	if err != nil {
		t.Fatalf("collectGarbageRepos returned error: %v", err)
	}
	if len(repos) != 1 {
		t.Fatalf("expected 1 garbage repo, got %d", len(repos))
	}
	r := repos[0]
	if r.skip != "" {
		t.Fatalf("expected %s to be reclaimable, got skip %q", dead, r.skip)
	}
	if len(r.dirs) != len(objstore.Types) {
		t.Errorf("expected %d store dirs, got %d: %v", len(objstore.Types), len(r.dirs), r.dirs)
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
		deadDir := objstore.RepoDir(absDataDir, objType, dead)
		if _, err := os.Stat(deadDir); !os.IsNotExist(err) {
			t.Errorf("%s survived reclaim", deadDir)
		}
		liveDir := objstore.RepoDir(absDataDir, objType, live)
		if _, err := os.Stat(liveDir); err != nil {
			t.Errorf("live repo's %s was removed: %v", liveDir, err)
		}
	}

	var remaining int
	if err := siloPair.Read.QueryRow("SELECT COUNT(*) FROM GarbageRepos").Scan(&remaining); err != nil {
		t.Fatalf("failed to count GarbageRepos: %v", err)
	}
	if remaining != 0 {
		t.Errorf("GarbageRepos still has %d rows after reclaim", remaining)
	}
}

// A dead origin whose virtual repo is still live must survive collection
// entirely: no directories measured, so a -delete pass has nothing to remove.
func TestCollectSkipsOriginOfLiveVirtualRepo(t *testing.T) {
	sqliteTestDB(t)

	const (
		origin  = "11111111-1111-1111-1111-111111111111"
		virtual = "22222222-2222-2222-2222-222222222222"
	)

	absDataDir = t.TempDir()
	objDir := filepath.Join(absDataDir, "storage", "fs", origin, "04")
	if err := os.MkdirAll(objDir, 0700); err != nil {
		t.Fatalf("failed to create %s: %v", objDir, err)
	}
	obj := filepath.Join(objDir, "01fc662e3bc87a41f299a907c056aaf8322a27")
	if err := os.WriteFile(obj, []byte("the virtual repo's only copy"), 0600); err != nil {
		t.Fatalf("failed to write %s: %v", obj, err)
	}

	dbExec(t, "INSERT INTO GarbageRepos (repo_id) VALUES (?)", origin)
	dbExec(t, "INSERT INTO Repo (repo_id) VALUES (?)", virtual)
	dbExec(t, "INSERT INTO VirtualRepo (repo_id, origin_repo, path, base_commit) VALUES (?, ?, '/sub', ?)",
		virtual, origin, "0401fc662e3bc87a41f299a907c056aaf8322a27")

	repos, err := collectGarbageRepos()
	if err != nil {
		t.Fatalf("collectGarbageRepos returned error: %v", err)
	}
	if len(repos) != 1 {
		t.Fatalf("expected 1 garbage repo, got %d", len(repos))
	}
	if repos[0].skip == "" {
		t.Fatal("GC would delete the store backing a live virtual repo")
	}
	if len(repos[0].dirs) != 0 {
		t.Errorf("skipped repo still had %d directories queued for removal: %v",
			len(repos[0].dirs), repos[0].dirs)
	}

	if _, err := os.Stat(obj); err != nil {
		t.Errorf("the virtual repo's object was disturbed: %v", err)
	}
}
