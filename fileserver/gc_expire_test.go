package silod

import (
	"bytes"
	"testing"
	"time"

	"github.com/dkam/silo/fileserver/libmgr"
	"github.com/dkam/silo/fileserver/objmgr"
	"github.com/dkam/silo/fileserver/objstore"
	storefmt "github.com/dkam/silo/store"
)

// historyFixture builds a library with commits at the given ages, oldest
// first, each overwriting the same path so every commit holds content only it
// reaches. It returns the library id and the commit ids in the same order.
func historyFixture(t *testing.T, ages ...time.Duration) (string, []string) {
	t.Helper()
	libraryID := "44444444-4444-4444-4444-444444444444"
	insertTestLibrary(t, libraryID)

	st, err := objmgr.New(objmgr.Config{
		DataDir: absDataDir,
		StoreID: libraryID,
		Params:  storefmt.DefaultParams(storefmt.PlainSeed()),
	})
	if err != nil {
		t.Fatal(err)
	}

	root, err := st.EmptyDir()
	if err != nil {
		t.Fatal(err)
	}

	var ids []string
	var parents []storefmt.ID
	for i, age := range ages {
		content := bytes.Repeat([]byte{byte('a' + i)}, 200000)
		m, err := st.WriteFile(bytes.NewReader(content))
		if err != nil {
			t.Fatal(err)
		}
		manifestID, err := st.PutManifest(m)
		if err != nil {
			t.Fatal(err)
		}
		root, err = st.PutNode(root, "/a.bin", objmgr.Node{ID: manifestID, Type: storefmt.NodeFile, Mode: 0o644}, int64(i+1))
		if err != nil {
			t.Fatal(err)
		}
		id, err := st.PutCommit(&storefmt.Commit{
			Root:      root,
			Parents:   parents,
			CreatedAt: time.Now().Add(-age).Unix(),
		})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id.String())
		parents = []storefmt.ID{id}
	}

	head := ids[len(ids)-1]
	dbExec(t, "INSERT INTO Branch (name, library_id, commit_id, root_id) VALUES ('master', ?, ?, ?)",
		libraryID, head, root.String())
	return libraryID, ids
}

func commitExists(t *testing.T, libraryID, commitID string) bool {
	t.Helper()
	return objectOnDisk(t, objstore.TypeObjects, libraryID, commitID)
}

// Commits inside the retention window are kept; the ones behind it go.
func TestExpireKeepsTheWindowAndDropsWhatIsBehindIt(t *testing.T) {
	sqliteTestDB(t)
	// 40 and 30 days old are outside a 14-day window; 5 and 1 are inside.
	libraryID, ids := historyFixture(t,
		40*24*time.Hour, 30*24*time.Hour, 5*24*time.Hour, 1*24*time.Hour)

	got, err := expireHistory(libraryID, 14*24*time.Hour, true)
	if err != nil {
		t.Fatalf("expireHistory: %v", err)
	}
	if got.expired != 2 {
		t.Errorf("expired %d commits, want the 2 outside the window", got.expired)
	}
	for i, id := range ids {
		want := i >= 2
		if commitExists(t, libraryID, id) != want {
			t.Errorf("commit %d (%s) exists=%v, want %v", i, id[:12], !want, want)
		}
	}
}

// The head is kept however old it is.
//
// A library nobody has written to for a year is the ordinary case, not a
// candidate for deletion. Expiring its head would leave a library whose
// Branch row names a commit the store does not hold -- which libmgr reports as
// corruption, and which no client can recover from.
func TestExpireNeverDropsTheHeadHoweverOldItIs(t *testing.T) {
	sqliteTestDB(t)
	libraryID, ids := historyFixture(t, 400*24*time.Hour, 380*24*time.Hour)

	got, err := expireHistory(libraryID, 24*time.Hour, true)
	if err != nil {
		t.Fatalf("expireHistory: %v", err)
	}

	head := ids[len(ids)-1]
	if !commitExists(t, libraryID, head) {
		t.Fatal("the head commit was expired; the library is now unreadable")
	}
	if got.expired != 1 {
		t.Errorf("expired %d, want only the one commit that is not the head", got.expired)
	}

	// And the library still loads, which is the property that actually matters.
	library := libmgr.Get(libraryID)
	if library == nil {
		t.Fatal("the library no longer loads after expiry")
	}
	st, err := library.Store()
	if err != nil {
		t.Fatal(err)
	}
	headID, err := storefmt.ParseID(library.HeadCommitID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Census(headID); err != nil {
		t.Errorf("the library cannot be measured after expiry: %v", err)
	}
}

// Nothing is deleted without -delete.
func TestExpireReportsWithoutDeletingUnlessAsked(t *testing.T) {
	sqliteTestDB(t)
	libraryID, ids := historyFixture(t, 40*24*time.Hour, 1*24*time.Hour)

	got, err := expireHistory(libraryID, 14*24*time.Hour, false)
	if err != nil {
		t.Fatalf("expireHistory: %v", err)
	}
	if got.expired != 1 {
		t.Errorf("reported %d expirable commits, want 1", got.expired)
	}
	if !commitExists(t, libraryID, ids[0]) {
		t.Error("a commit was deleted without -delete")
	}
}

// Expiry drops a contiguous run ending at the oldest commit, never a hole in
// the middle.
//
// History is a linked list walked from the head, so a commit deleted out of
// the middle cuts everything behind it whether or not those were meant to go.
// Timestamps come from clients and need not decrease along the chain -- a
// clock skew, or a client backdating a commit, is enough to produce an old
// commit sitting between two new ones. Cutting at the first commit outside the
// window keeps the invariant "what remains is a walkable prefix from the
// head", which is the thing every reader depends on.
func TestExpireCutsAPrefixEvenWhenTimestampsAreNotMonotonic(t *testing.T) {
	sqliteTestDB(t)
	// Third-from-head is older than the window; the two behind it are not.
	// Walking from the head, the cut has to start at the offender and take
	// everything behind it regardless of their own ages.
	libraryID, ids := historyFixture(t,
		1*24*time.Hour, 2*24*time.Hour, 40*24*time.Hour, 1*24*time.Hour, 1*24*time.Hour)

	if _, err := expireHistory(libraryID, 14*24*time.Hour, true); err != nil {
		t.Fatalf("expireHistory: %v", err)
	}

	// ids[0..2] are behind the cut and must all be gone, including the two
	// that are individually young enough to keep.
	for i := 0; i <= 2; i++ {
		if commitExists(t, libraryID, ids[i]) {
			t.Errorf("commit %d survived; the cut must take everything behind the offender", i)
		}
	}
	for i := 3; i < len(ids); i++ {
		if !commitExists(t, libraryID, ids[i]) {
			t.Errorf("commit %d was expired, but it is in front of the cut", i)
		}
	}
}

// What an expired commit exclusively held becomes collectable, and the sweep
// that already exists is what collects it.
//
// This is the whole reason expiry only deletes commit objects. The bulk of the
// space is in chunks, and reclaiming those is the sweep's job -- already
// written, already guarded by age and the GC generation. Expiry moves bytes
// from "history" to "unreferenced" and stops.
func TestExpireMakesItsBytesCollectableBySweep(t *testing.T) {
	sqliteTestDB(t)
	libraryID, _ := historyFixture(t, 40*24*time.Hour, 1*24*time.Hour)

	library := libmgr.Get(libraryID)
	st, err := library.Store()
	if err != nil {
		t.Fatal(err)
	}
	head, err := storefmt.ParseID(library.HeadCommitID)
	if err != nil {
		t.Fatal(err)
	}

	before, err := st.Census(head)
	if err != nil {
		t.Fatal(err)
	}
	if before.History.Bytes < 200000 {
		t.Fatalf("history = %d before expiry, want the old commit's content", before.History.Bytes)
	}

	if _, err := expireHistory(libraryID, 14*24*time.Hour, true); err != nil {
		t.Fatal(err)
	}

	after, err := st.Census(head)
	if err != nil {
		t.Fatal(err)
	}
	if after.History.Bytes != 0 {
		t.Errorf("history = %d after expiry, want 0", after.History.Bytes)
	}
	if after.Unreferenced.Bytes < before.History.Bytes-1000 {
		t.Errorf("unreferenced = %d, want roughly the %d that history held",
			after.Unreferenced.Bytes, before.History.Bytes)
	}
	if after.Head != before.Head {
		t.Errorf("head = %+v, want it untouched at %+v", after.Head, before.Head)
	}
}
