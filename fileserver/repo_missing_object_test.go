package silod

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dkam/silo/fileserver/repomgr"
)

const (
	damagedRepo = "328be500-9164-418a-a47f-ab805dbd5694"
	goneRepo    = "d3f4cf83-1111-2222-3333-444455556666"
	repoOwner   = "owner@example.com"
)

// damagedHead is a well-formed head id for an object that was never written.
var damagedHead = strings.Repeat("ab", 32)

// damagedRepoTestDB seeds a library that exists in every table and whose head
// commit object does not exist at all — a database that survived an object
// store that did not, which is the shape a half-restored backup, an
// interrupted rsync, a bad disk or an over-eager GC leaves behind.
func damagedRepoTestDB(t *testing.T) {
	t.Helper()
	sqliteTestDB(t)

	insertTestRepo(t, damagedRepo)
	dbExec(t, "INSERT INTO Branch (name, repo_id, commit_id, root_id) VALUES ('master', ?, ?, ?)",
		damagedRepo, damagedHead, damagedHead)
	dbExec(t, "INSERT INTO RepoHead (repo_id, branch_name) VALUES (?, 'master')", damagedRepo)
	dbExec(t, "INSERT INTO RepoOwner (repo_id, account_id) VALUES (?, ?)", damagedRepo, mintAccount(t, repoOwner).ID)
}

// The bug, at the surface porter-fuse and the File Provider extension both
// read: a library whose head commit object is missing was reported as 404
// "Repo not found". To a sync client a 404 is not "something went wrong", it
// is "this library was deleted, remove your copy" — and the copy it removes is
// the one that could have restored the missing object. 5xx is the truth:
// something is broken, nothing was deleted, do not act on it.
func TestEntryRepoDoesNotReport404ForAMissingObject(t *testing.T) {
	damagedRepoTestDB(t)

	w := httptest.NewRecorder()
	repo := entryRepo(w, damagedRepo, acctFor(t, repoOwner).ID, false)

	if repo != nil {
		t.Fatal("entryRepo returned a repo whose head commit is missing")
	}
	if w.Code == http.StatusNotFound {
		t.Fatal("a missing object told the client the library was deleted")
	}
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d", w.Code, http.StatusInternalServerError)
	}
	if w.Body.Len() == 0 {
		t.Error("the 500 carries no explanation for the client")
	}
}

// The 404 still has to work, or a library deleted from the server never
// propagates and clients keep a copy of something that is genuinely gone.
func TestEntryRepoStillReports404ForAnAbsentLibrary(t *testing.T) {
	damagedRepoTestDB(t)

	// Owned, so the permission check passes and the lookup is what answers.
	dbExec(t, "INSERT INTO RepoOwner (repo_id, account_id) VALUES (?, ?)", goneRepo, mintAccount(t, repoOwner).ID)

	w := httptest.NewRecorder()
	if repo := entryRepo(w, goneRepo, acctFor(t, repoOwner).ID, false); repo != nil {
		t.Fatal("entryRepo returned a repo that has no row")
	}
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
}

// GetWithReason is what carries the distinction; Get keeps the old signature
// for the many callers that only need the repo or nil.
func TestGetStillReturnsNilForADamagedLibrary(t *testing.T) {
	damagedRepoTestDB(t)

	if repo := repomgr.Get(damagedRepo); repo != nil {
		t.Error("Get returned a repo whose head commit is missing")
	}
}
