package silod

import (
	"testing"
	"time"

	"github.com/dkam/silo/fileserver/libmgr"
)

const (
	originLibrary = "aaaa1111-2222-3333-4444-555555555555"
	childLibrary  = "bbbb1111-2222-3333-4444-555555555555"
	otherLibrary  = "cccc1111-2222-3333-4444-555555555555"
)

func seedLibrary(t *testing.T, libraryID, email, token string) {
	t.Helper()
	insertTestLibrary(t, libraryID)
	dbExec(t, "INSERT INTO Branch (name, library_id, commit_id) VALUES (?, ?, ?)",
		"master", libraryID, "0401fc662e3bc87a41f299a907c056aaf8322a27")
	dbExec(t, "INSERT INTO LibraryHead (library_id, branch_name) VALUES (?, ?)", libraryID, "master")
	id := mintAccount(t, email).ID
	dbExec(t, "INSERT INTO LibraryOwner (library_id, account_id) VALUES (?, ?)", libraryID, id)
	dbExec(t, "INSERT INTO LibraryUserToken (library_id, account_id, token, ctime) VALUES (?, ?, ?, ?)",
		libraryID, id, token, time.Now().Unix())
}

// Deleting an origin removed its children's VirtualLibrary rows but left their
// Library, Branch and LibraryUserToken rows behind. Each child survived as an
// apparently ordinary library whose StoreID still pointed at the origin's
// object store — which GC had just reclaimed — so a client kept syncing
// against an empty store and nothing ever cleaned the rows up.
func TestDeleteLibraryCascadesToVirtualLibraries(t *testing.T) {
	sqliteTestDB(t)

	seedLibrary(t, originLibrary, "owner@example.com", "1111aaaa1111aaaa1111aaaa1111aaaa1111aaaa")
	seedLibrary(t, childLibrary, "owner@example.com", "2222bbbb2222bbbb2222bbbb2222bbbb2222bbbb")
	seedLibrary(t, otherLibrary, "owner@example.com", "3333cccc3333cccc3333cccc3333cccc3333cccc")
	dbExec(t, "INSERT INTO VirtualLibrary (library_id, origin_library, path, base_commit) VALUES (?, ?, ?, ?)",
		childLibrary, originLibrary, "/sub", "0401fc662e3bc87a41f299a907c056aaf8322a27")

	if err := libmgr.DeleteLibrary(originLibrary); err != nil {
		t.Fatalf("DeleteLibrary returned %v", err)
	}

	for _, table := range []string{"Library", "Branch", "LibraryHead", "LibraryOwner", "LibraryUserToken"} {
		if n := countRows(t, "SELECT COUNT(*) FROM "+table+" WHERE library_id = ?", childLibrary); n != 0 {
			t.Errorf("%s still holds %d row(s) for the orphaned virtual library", table, n)
		}
		if n := countRows(t, "SELECT COUNT(*) FROM "+table+" WHERE library_id = ?", originLibrary); n != 0 {
			t.Errorf("%s still holds %d row(s) for the deleted origin", table, n)
		}
		// An unrelated library must be untouched.
		if n := countRows(t, "SELECT COUNT(*) FROM "+table+" WHERE library_id = ?", otherLibrary); n != 1 {
			t.Errorf("%s holds %d row(s) for an unrelated library, want 1", table, n)
		}
	}

	if n := countRows(t, "SELECT COUNT(*) FROM VirtualLibrary WHERE library_id = ?", childLibrary); n != 0 {
		t.Error("the VirtualLibrary row survived")
	}

	// Both have to reach GC, or the child's storage directory leaks forever.
	for _, libraryID := range []string{originLibrary, childLibrary} {
		if n := countRows(t, "SELECT COUNT(*) FROM GarbageLibraries WHERE library_id = ?", libraryID); n != 1 {
			t.Errorf("%s was not recorded in GarbageLibraries", libraryID)
		}
	}
}

// A library listed as its own origin is corrupt data, not a reason to recurse
// until the stack runs out.
func TestDeleteLibrarySurvivesSelfReferencingVirtualLibrary(t *testing.T) {
	sqliteTestDB(t)

	seedLibrary(t, originLibrary, "owner@example.com", "1111aaaa1111aaaa1111aaaa1111aaaa1111aaaa")
	dbExec(t, "INSERT INTO VirtualLibrary (library_id, origin_library, path, base_commit) VALUES (?, ?, ?, ?)",
		originLibrary, originLibrary, "/", "0401fc662e3bc87a41f299a907c056aaf8322a27")

	done := make(chan error, 1)
	go func() { done <- libmgr.DeleteLibrary(originLibrary) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("DeleteLibrary returned %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("DeleteLibrary did not finish; probable infinite recursion")
	}

	if n := countRows(t, "SELECT COUNT(*) FROM Library WHERE library_id = ?", originLibrary); n != 0 {
		t.Error("the library was not deleted")
	}
}
