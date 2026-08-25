package silod

import (
	"reflect"
	"testing"
)

// df and the collector report on the same set of libraries.
//
// They had two functions for this, reading two different tables: df listed
// LibraryOwner, the collector listed Library. Both rows are written in one
// transaction and deleted in another, so on a healthy server the two agree and
// the divergence is invisible -- which is the problem. The moment they do not
// agree, df is silently measuring a different server than gc is sweeping, and
// the operator reading the two outputs together is the one who pays for it.
//
// A library with no owner row is the shape that separates them. It should not
// happen; a census is what somebody runs when things that should not happen
// have.
func TestDFAndTheCollectorSeeTheSameLibraries(t *testing.T) {
	libraryID, _ := storeV2Library(t)
	dbExec(t, "DELETE FROM LibraryOwner WHERE library_id = ?", libraryID)

	forDF, err := libraryIDsForDF(nil)
	if err != nil {
		t.Fatalf("libraryIDsForDF: %v", err)
	}
	forGC, err := liveLibraryIDs()
	if err != nil {
		t.Fatalf("liveLibraryIDs: %v", err)
	}

	if !reflect.DeepEqual(forDF, forGC) {
		t.Errorf("df would measure %v and the collector would sweep %v; "+
			"the two commands must not disagree about which libraries exist", forDF, forGC)
	}
	if len(forDF) != 1 || forDF[0] != libraryID {
		t.Errorf("df's list is %v, want just %s -- a library that still exists went unmeasured",
			forDF, libraryID)
	}
}

// A named library is taken as given, without a catalog lookup.
//
// Naming one is how an operator measures a library the catalog has lost, so
// the argument must not be filtered by the very table that might be missing it.
func TestDFTakesANamedLibraryAsGiven(t *testing.T) {
	sqliteTestDB(t)

	ids, err := libraryIDsForDF([]string{"not-in-any-table"})
	if err != nil {
		t.Fatalf("libraryIDsForDF: %v", err)
	}
	if len(ids) != 1 || ids[0] != "not-in-any-table" {
		t.Errorf("got %v, want the id as passed", ids)
	}
}
