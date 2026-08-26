package silod

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dkam/silo/fileserver/credential"
	"github.com/dkam/silo/fileserver/libmgr"
	"github.com/dkam/silo/fileserver/middleware"
)

const (
	damagedLibrary = "328be500-9164-418a-a47f-ab805dbd5694"
	goneLibrary    = "d3f4cf83-1111-2222-3333-444455556666"
	libraryOwner   = "owner@example.com"
)

// damagedHead is a well-formed head id for an object that was never written.
var damagedHead = strings.Repeat("ab", 32)

// damagedLibraryTestDB seeds a library that exists in every table and whose head
// commit object does not exist at all — a database that survived an object
// store that did not, which is the shape a half-restored backup, an
// interrupted rsync, a bad disk or an over-eager GC leaves behind.
func damagedLibraryTestDB(t *testing.T) {
	t.Helper()
	sqliteTestDB(t)

	insertTestLibrary(t, damagedLibrary)
	dbExec(t, "INSERT INTO Branch (name, library_id, commit_id, root_id) VALUES ('master', ?, ?, ?)",
		damagedLibrary, damagedHead, damagedHead)
	dbExec(t, "INSERT INTO LibraryHead (library_id, branch_name) VALUES (?, 'master')", damagedLibrary)
	dbExec(t, "INSERT INTO LibraryOwner (library_id, account_id) VALUES (?, ?)", damagedLibrary, mintAccount(t, libraryOwner).ID)
}

// authed builds a request carrying an unnarrowed credential for a test
// address, which is what entryLibrary now reads its permission from. The
// credential is unscoped and rw, so what it measures is the account's
// permission and the library lookup -- the ceiling has its own tests.
func authed(t *testing.T, email string) *http.Request {
	t.Helper()
	acct := acctFor(t, email)
	r := httptest.NewRequest(http.MethodGet, "/api/silo/v1/libraries", nil)
	return middleware.WithCredential(r, &credential.Credential{
		Kind: credential.KindSession, AccountID: acct.ID, Label: "test", Perm: "rw",
	}, acct)
}

// The bug, at the surface porter-fuse and the File Provider extension both
// read: a library whose head commit object is missing was reported as 404
// "Library not found". To a sync client a 404 is not "something went wrong", it
// is "this library was deleted, remove your copy" — and the copy it removes is
// the one that could have restored the missing object. 5xx is the truth:
// something is broken, nothing was deleted, do not act on it.
func TestEntryLibraryDoesNotReport404ForAMissingObject(t *testing.T) {
	damagedLibraryTestDB(t)

	w := httptest.NewRecorder()
	library := entryLibrary(w, authed(t, libraryOwner), damagedLibrary, "/", false)

	if library != nil {
		t.Fatal("entryLibrary returned a library whose head commit is missing")
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
func TestEntryLibraryStillReports404ForAnAbsentLibrary(t *testing.T) {
	damagedLibraryTestDB(t)

	// Owned, so the permission check passes and the lookup is what answers.
	dbExec(t, "INSERT INTO LibraryOwner (library_id, account_id) VALUES (?, ?)", goneLibrary, mintAccount(t, libraryOwner).ID)

	w := httptest.NewRecorder()
	if library := entryLibrary(w, authed(t, libraryOwner), goneLibrary, "/", false); library != nil {
		t.Fatal("entryLibrary returned a library that has no row")
	}
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
}

// GetWithReason is what carries the distinction; Get keeps the old signature
// for the many callers that only need the library or nil.
func TestGetStillReturnsNilForADamagedLibrary(t *testing.T) {
	damagedLibraryTestDB(t)

	if library := libmgr.Get(damagedLibrary); library != nil {
		t.Error("Get returned a library whose head commit is missing")
	}
}
