package silod

import (
	"bytes"
	"testing"
	"time"

	"github.com/dkam/silo/fileserver/libmgr"
	"github.com/dkam/silo/fileserver/objmgr"
	"github.com/dkam/silo/fileserver/objstore"
	"github.com/dkam/silo/fileserver/option"
	"github.com/dkam/silo/internal/format"
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
	if got.chosen() != 2 {
		t.Errorf("the cut took %d commits, want the 2 outside the window", got.chosen())
	}
	// Every one of them is inside a pack, so expiry could only defer; the
	// rewrite is what carries the decision out.
	reclaimPacked(t, libraryID, true, 14*24*time.Hour)
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
	if got.chosen() != 1 {
		t.Errorf("the cut took %d, want only the one commit that is not the head", got.chosen())
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
	reclaimPacked(t, libraryID, true, 14*24*time.Hour)

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
	// The commits are in packs, so expiry deferred every one of them and the
	// rewrite is what moves the bytes. What this test is about is unchanged:
	// the space leaves history and lands where a collector can have it.
	reclaimPacked(t, libraryID, true, 14*24*time.Hour)

	after, err := st.Census(head)
	if err != nil {
		t.Fatal(err)
	}
	if after.History.Bytes != 0 {
		t.Errorf("history = %d after expiry, want 0", after.History.Bytes)
	}
	// Reclaimed outright by the rewrite rather than left for a later sweep,
	// which is what compaction does: it drops the frame instead of unlinking a
	// file for somebody else to find.
	if after.Unreferenced.Bytes != 0 {
		t.Errorf("unreferenced = %d after the rewrite, want nothing left", after.Unreferenced.Bytes)
	}
	if after.Head != before.Head {
		t.Errorf("head = %+v, want it untouched at %+v", after.Head, before.Head)
	}
}

// A library's own retention wins over the server default, and no row means the
// default applies.
//
// The precedence has to run this way round for the same reason UserQuota's
// does: the config sets the floor for everybody and the per-library setting is
// the exception. A default that overrode the exceptions would make the
// exceptions unsettable, and one that only applied to libraries created after
// it was set would make it unpredictable.
func TestRetentionPrefersTheLibrarysOwnSettingOverTheDefault(t *testing.T) {
	sqliteTestDB(t)
	libraryID, _ := historyFixture(t, 1*time.Hour)

	restore := option.DefaultKeepDays
	defer func() { option.DefaultKeepDays = restore }()

	option.DefaultKeepDays = 30
	if got, err := libmgr.RetentionDays(libraryID); err != nil || got != 30 {
		t.Errorf("RetentionDays with no row = %d (%v), want the default 30", got, err)
	}

	if err := libmgr.SetRetentionDays(libraryID, 7); err != nil {
		t.Fatal(err)
	}
	if got, err := libmgr.RetentionDays(libraryID); err != nil || got != 7 {
		t.Errorf("RetentionDays after setting 7 = %d (%v), want 7", got, err)
	}

	// Zero is a real setting: keep everything, even where the server says not
	// to. A library that must retain every commit is a choice somebody makes,
	// and it has to survive a default that disagrees.
	if err := libmgr.SetRetentionDays(libraryID, 0); err != nil {
		t.Fatal(err)
	}
	if got, err := libmgr.RetentionDays(libraryID); err != nil || got != 0 {
		t.Errorf("RetentionDays after setting 0 = %d (%v), want 0 -- keep everything", got, err)
	}

	// And clearing it goes back to following the default.
	if err := libmgr.ClearRetentionDays(libraryID); err != nil {
		t.Fatal(err)
	}
	if got, err := libmgr.RetentionDays(libraryID); err != nil || got != 30 {
		t.Errorf("RetentionDays after clearing = %d (%v), want the default 30 again", got, err)
	}
}

// A library set to keep everything is never expired, whatever the pass was
// asked for.
func TestExpireSkipsALibraryThatKeepsEverything(t *testing.T) {
	sqliteTestDB(t)
	libraryID, ids := historyFixture(t, 400*24*time.Hour, 1*time.Hour)

	restore := option.DefaultKeepDays
	defer func() { option.DefaultKeepDays = restore }()
	option.DefaultKeepDays = 1

	if err := libmgr.SetRetentionDays(libraryID, 0); err != nil {
		t.Fatal(err)
	}

	got, err := expireHistoryByPolicy(libraryID, true)
	if err != nil {
		t.Fatalf("expireHistoryByPolicy: %v", err)
	}
	if got.expired != 0 {
		t.Errorf("expired %d commits from a library set to keep everything", got.expired)
	}
	if !commitExists(t, libraryID, ids[0]) {
		t.Error("the old commit was deleted despite keep-everything")
	}
}

// With the default set and no per-library row, the policy pass expires.
func TestExpireByPolicyUsesTheServerDefault(t *testing.T) {
	sqliteTestDB(t)
	libraryID, ids := historyFixture(t, 400*24*time.Hour, 1*time.Hour)

	restore := option.DefaultKeepDays
	defer func() { option.DefaultKeepDays = restore }()
	option.DefaultKeepDays = 14

	got, err := expireHistoryByPolicy(libraryID, true)
	if err != nil {
		t.Fatalf("expireHistoryByPolicy: %v", err)
	}
	if got.chosen() != 1 {
		t.Errorf("the cut took %d commits, want the 1 outside the 14-day default", got.chosen())
	}
	reclaimPacked(t, libraryID, true, 0)
	if commitExists(t, libraryID, ids[0]) {
		t.Error("the 400-day-old commit survived a 14-day default")
	}
}

// The default of zero means a server that was upgraded does not start deleting.
func TestExpireByPolicyDoesNothingWhenNothingIsConfigured(t *testing.T) {
	sqliteTestDB(t)
	libraryID, ids := historyFixture(t, 400*24*time.Hour, 1*time.Hour)

	restore := option.DefaultKeepDays
	defer func() { option.DefaultKeepDays = restore }()
	option.DefaultKeepDays = 0

	got, err := expireHistoryByPolicy(libraryID, true)
	if err != nil {
		t.Fatalf("expireHistoryByPolicy: %v", err)
	}
	if got.expired != 0 {
		t.Errorf("expired %d commits with no retention configured anywhere", got.expired)
	}
	for i, id := range ids {
		if !commitExists(t, libraryID, id) {
			t.Errorf("commit %d was deleted with no policy set", i)
		}
	}
}

// expired counts what was actually removed, not what was eligible.
//
// The two diverge the moment a commit lives somewhere it cannot be deleted
// from: a sealed pack is immutable, so Remove refuses with ErrReclaimDeferred
// and the object stays exactly where it was. Reporting it as expired tells an
// operator their history was truncated when it was not — and the bytes are
// still on the disk they ran this to free.
func TestExpireDoesNotReportCommitsItCouldNotRemove(t *testing.T) {
	sqliteTestDB(t)
	t.Cleanup(func() { _ = objstore.Close() })

	libraryID, ids := historyFixture(t, 40*24*time.Hour, 30*24*time.Hour, 1*24*time.Hour)
	// Seal, so the commits are inside immutable packs rather than in an open
	// one this process could still be appending to.
	if err := objstore.Close(); err != nil {
		t.Fatalf("sealing: %v", err)
	}

	got, err := expireHistory(libraryID, 14*24*time.Hour, true)
	if err != nil {
		t.Fatalf("expireHistory: %v", err)
	}

	if got.expired != 0 {
		t.Errorf("reported %d commits expired, but every one of them is in a sealed pack and could not be removed",
			got.expired)
	}
	if got.deferred != 2 {
		t.Errorf("reported %d commits deferred, want the 2 that a later rewrite reclaims", got.deferred)
	}
	if got.freed != 0 {
		t.Errorf("reported %s freed, but nothing was deleted", format.Bytes(got.freed))
	}
	// And they really are still readable, which is why claiming otherwise
	// matters: this is live data, not a bookkeeping detail.
	for _, id := range ids[:2] {
		if _, err := objstore.New("", absDataDir, objstore.TypeObjects).
			ReadInto(libraryID, id, nil); err != nil {
			t.Errorf("commit %s is not readable after a supposedly failed expiry: %v", id, err)
		}
	}
}
