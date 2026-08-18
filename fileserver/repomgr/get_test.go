package repomgr

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dkam/silo/fileserver/commitmgr"
	"github.com/dkam/silo/fileserver/dbutil"
	"github.com/dkam/silo/fileserver/objstore"
	"github.com/dkam/silo/fileserver/option"
)

const (
	testRepoID  = "328be500-1111-2222-3333-444455556666"
	absentRepo  = "99999999-8888-7777-6666-555555555555"
	testOwner   = "owner@example.com"
	missingHead = "ac9b78b5c6f7b1905277627bafef0fe99beb15ff"
)

// getTestStore wires a SQLite database and a real on-disk object store, and
// returns the data directory so a test can take an object back out of it.
func getTestStore(t *testing.T) string {
	t.Helper()

	option.LoadFileServerOptions("") // defaults, incl. a non-zero DBOpTimeout
	dbutil.DBEngine = dbutil.EngineSQLite

	dir := t.TempDir()
	pair, err := dbutil.OpenSQLite(filepath.Join(dir, "seafile.db"))
	if err != nil {
		t.Fatalf("open seafile db: %v", err)
	}
	t.Cleanup(func() { _ = pair.Close() })
	if err := dbutil.CreateSeafileTables(pair.Write); err != nil {
		t.Fatalf("create seafile tables: %v", err)
	}

	dataDir := filepath.Join(dir, "seafile-data")
	Init(pair.Read, pair.Write)
	commitmgr.Init(dir, dataDir)

	// Faults are suppressed per (repo, kind) for five minutes, and the map is
	// package state that outlives one test.
	t.Cleanup(func() {
		clearFaults(testRepoID)
		clearFaults(absentRepo)
	})

	return dataDir
}

// A library with no row is the one condition that is a statement about the
// request, and the only one a client may act on by forgetting the library.
func TestGetWithReasonMissingRowIsNotFound(t *testing.T) {
	getTestStore(t)

	repo, err := GetWithReason(absentRepo)
	if repo != nil {
		t.Fatal("got a repo for an id that was never created")
	}
	if !errors.Is(err, ErrRepoNotFound) {
		t.Fatalf("err = %v, want ErrRepoNotFound", err)
	}
	if code, _ := StatusFor(err); code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", code, http.StatusNotFound)
	}
}

// The bug: a `git filter-branch` removed the object store out from under a
// running server while the database survived, and every endpoint that resolved
// the library answered 404 "Repo not found" — which tells a sync client the
// library was deleted, and the copy it would then delete is the one that could
// have restored the object.
func TestGetWithReasonMissingCommitIsCorruptedNotNotFound(t *testing.T) {
	dataDir := getTestStore(t)

	repoID, err := CreateRepo("Porter Test", testOwner)
	if err != nil {
		t.Fatalf("CreateRepo: %v", err)
	}

	repo, err := GetWithReason(repoID)
	if err != nil {
		t.Fatalf("a freshly created repo did not load: %v", err)
	}
	head := repo.HeadCommitID

	// Exactly the damage the accident caused: the row stays, the object goes.
	if err := os.RemoveAll(objstore.RepoDir(dataDir, objstore.TypeCommits, repoID)); err != nil {
		t.Fatalf("remove commit store: %v", err)
	}
	t.Cleanup(func() { clearFaults(repoID) })

	repo, err = GetWithReason(repoID)
	if repo != nil {
		t.Fatal("got a repo whose head commit is gone")
	}
	if errors.Is(err, ErrRepoNotFound) {
		t.Fatalf("a missing object reported the library as absent: %v", err)
	}
	if !errors.Is(err, ErrRepoCorrupted) {
		t.Fatalf("err = %v, want ErrRepoCorrupted", err)
	}
	code, msg := StatusFor(err)
	if code != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d", code, http.StatusInternalServerError)
	}
	if msg == "" {
		t.Error("the 500 carries no explanation for the client")
	}
	// The detail has to survive the wrapping, or nothing in the log says which
	// object went missing.
	if !strings.Contains(err.Error(), head) {
		t.Errorf("err = %q, does not name the head commit %s", err, head)
	}

	// Get keeps its old signature for the callers that only need the repo.
	if got := Get(repoID); got != nil {
		t.Error("Get returned a repo whose head commit is gone")
	}
}

// A Branch row with an empty commit id is a corrupt row, not a missing one.
func TestGetWithReasonEmptyHeadIsCorrupted(t *testing.T) {
	getTestStore(t)

	if _, err := seafileWriteDB.Exec("INSERT INTO Repo (repo_id) VALUES (?)", testRepoID); err != nil {
		t.Fatalf("seed Repo: %v", err)
	}
	if _, err := seafileWriteDB.Exec(
		"INSERT INTO Branch (name, repo_id, commit_id) VALUES ('master', ?, '')", testRepoID); err != nil {
		t.Fatalf("seed Branch: %v", err)
	}

	_, err := GetWithReason(testRepoID)
	if !errors.Is(err, ErrRepoCorrupted) {
		t.Fatalf("err = %v, want ErrRepoCorrupted", err)
	}
	if code, _ := StatusFor(err); code != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d", code, http.StatusInternalServerError)
	}
}

// A database that cannot be read says nothing about whether the library
// exists, so it must not answer for it.
func TestGetWithReasonDeadDatabaseIsUnavailable(t *testing.T) {
	getTestStore(t)

	// Closing the read handle is the closest stand-in for the database being
	// away that does not need one to be running.
	if err := seafileDB.Close(); err != nil {
		t.Fatalf("close read handle: %v", err)
	}

	_, err := GetWithReason(testRepoID)
	if !errors.Is(err, ErrRepoUnavailable) {
		t.Fatalf("err = %v, want ErrRepoUnavailable", err)
	}
	code, _ := StatusFor(err)
	if code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", code, http.StatusServiceUnavailable)
	}
}

// A persistent fault is rediscovered by every retrying client. Reporting it
// once per interval is what keeps one server fault from becoming twelve log
// lines and twelve error reports — and a repair has to re-arm it, or a
// recurrence would be silent.
func TestFaultIsReportedOnceUntilRepaired(t *testing.T) {
	getTestStore(t)

	if !firstReport(testRepoID, ErrRepoCorrupted) {
		t.Fatal("the first occurrence was suppressed")
	}
	if firstReport(testRepoID, ErrRepoCorrupted) {
		t.Error("the same fault was reported twice")
	}
	// A different fault on the same library is a different thing to say.
	if !firstReport(testRepoID, ErrRepoUnavailable) {
		t.Error("an unrelated fault was suppressed as a repeat")
	}
	// As is the same fault on a different library.
	if !firstReport(absentRepo, ErrRepoCorrupted) {
		t.Error("a fault on another library was suppressed as a repeat")
	}

	clearFaults(testRepoID)
	if !firstReport(testRepoID, ErrRepoCorrupted) {
		t.Error("a recurrence after a repair was suppressed")
	}
}
