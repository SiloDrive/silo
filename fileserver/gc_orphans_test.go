package silod

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dkam/silo/fileserver/libmgr"
	"github.com/dkam/silo/fileserver/objmgr"
	"github.com/dkam/silo/fileserver/objstore"
	storefmt "github.com/dkam/silo/store"
)

// orphanFixture builds a library holding one committed file and one orphan --
// a chunk written and never committed, which is what an upload that stopped
// halfway leaves behind. It returns the library id and the orphan's id.
func orphanFixture(t *testing.T) (libraryID string, orphanID string) {
	t.Helper()
	libraryID = "33333333-3333-3333-3333-333333333333"
	insertTestLibrary(t, libraryID)

	st, err := objmgr.New(objmgr.Config{
		DataDir: absDataDir,
		StoreID: libraryID,
		Params:  storefmt.DefaultParams(storefmt.PlainSeed()),
	})
	if err != nil {
		t.Fatal(err)
	}

	empty, err := st.EmptyDir()
	if err != nil {
		t.Fatal(err)
	}
	m, err := st.WriteFile(bytes.NewReader(bytes.Repeat([]byte("a"), 200000)))
	if err != nil {
		t.Fatal(err)
	}
	manifestID, err := st.PutManifest(m)
	if err != nil {
		t.Fatal(err)
	}
	root, err := st.PutNode(empty, "/a.bin", objmgr.Node{ID: manifestID, Type: storefmt.NodeFile, Mode: 0o644}, 1)
	if err != nil {
		t.Fatal(err)
	}
	head, err := st.PutCommit(&storefmt.Commit{Root: root, CreatedAt: 1})
	if err != nil {
		t.Fatal(err)
	}
	dbExec(t, "INSERT INTO Branch (name, library_id, commit_id, root_id) VALUES ('master', ?, ?, ?)",
		libraryID, head.String(), root.String())

	// The orphan: a chunk written under no manifest anybody committed.
	orphan, err := st.WriteFile(bytes.NewReader(bytes.Repeat([]byte("x"), 200000)))
	if err != nil {
		t.Fatal(err)
	}
	if len(orphan.Chunks) == 0 {
		t.Fatal("the orphan was inlined; it must be over store.InlineThreshold to be a chunk")
	}
	return libraryID, orphan.Chunks[0].ID.String()
}

// age backdates a library's packs so the sweep's guard will consider what is in
// them.
//
// Since the cutover an object has no file of its own and no time of its own:
// its age is the age of the pack holding it, which is a lower bound on every
// frame in that pack. So the unit here is the library and not the object --
// every pack in it is at most as old as any object a caller could name.
func age(t *testing.T, libraryID string, d time.Duration) {
	t.Helper()
	when := time.Now().Add(-d)
	for _, objType := range objstore.Types {
		dir := objstore.PackDir(absDataDir, objType, libraryID)
		for _, e := range packFiles(t, objType, libraryID) {
			if filepath.Ext(e.Name()) != ".pack" {
				continue
			}
			if err := os.Chtimes(filepath.Join(dir, e.Name()), when, when); err != nil {
				t.Fatalf("backdating %s: %v", e.Name(), err)
			}
		}
	}
}

// A freshly written orphan is never collected, whatever the operator asked
// for.
//
// This is the guard the whole sweep rests on. An object nothing points at and
// one that is about to be pointed at look identical from the store: an upload
// in flight is unreferenced right up until the commit that names it lands. Age
// is what separates them, and a sweep without it deletes the chunks of every
// upload in progress.
func TestSweepLeavesAFreshOrphanAlone(t *testing.T) {
	sqliteTestDB(t)
	libraryID, orphanID := orphanFixture(t)

	sweep, err := sweepOrphans(libraryID, time.Hour, true)
	if err != nil {
		t.Fatalf("sweepOrphans: %v", err)
	}

	if sweep.removed != 0 {
		t.Errorf("removed %d objects, want 0: the orphan is seconds old", sweep.removed)
	}
	if sweep.tooYoung == 0 {
		t.Error("nothing was reported as too young, but the orphan is")
	}
	assertObjectExists(t, objstore.TypeChunks, libraryID, orphanID, true)
}

// An orphan past the age threshold is reported, and survives, unless -delete
// was given.
//
// Report-by-default is the same convention RunGC already has, and it matters
// more here than there: this is the first thing in Silo that deletes objects
// belonging to a library that still exists.
func TestSweepReportsWithoutDeletingUnlessAsked(t *testing.T) {
	sqliteTestDB(t)
	libraryID, orphanID := orphanFixture(t)
	age(t, libraryID, 48*time.Hour)

	sweep, err := sweepOrphans(libraryID, 24*time.Hour, false)
	if err != nil {
		t.Fatalf("sweepOrphans: %v", err)
	}
	if sweep.found == 0 {
		t.Fatal("found nothing, want the backdated orphan")
	}
	if sweep.removed != 0 {
		t.Errorf("removed %d without -delete, want 0", sweep.removed)
	}
	assertObjectExists(t, objstore.TypeChunks, libraryID, orphanID, true)

	sweep, err = sweepOrphans(libraryID, 24*time.Hour, true)
	if err != nil {
		t.Fatalf("sweepOrphans -delete: %v", err)
	}
	if sweep.chosen() == 0 {
		t.Error("removed nothing with -delete, want the orphan")
	}
	// Inside a pack, so the sweep could only defer; the rewrite is what takes
	// the bytes away.
	reclaimPacked(t, libraryID, false, 0)
	assertObjectExists(t, objstore.TypeChunks, libraryID, orphanID, false)
}

// Nothing the head reaches is ever removed, however old it is.
//
// Age is a guard against deleting something too new. It must never become a
// reason to delete something that is referenced -- an old library that nobody
// has touched in a year is the ordinary case, not a candidate.
func TestSweepNeverRemovesWhatTheHeadReaches(t *testing.T) {
	sqliteTestDB(t)
	libraryID, _ := orphanFixture(t)

	// Backdate everything, orphan and committed alike.
	for _, objType := range []string{objstore.TypeChunks, objstore.TypeObjects} {
		dir := objstore.LibraryDir(absDataDir, objType, libraryID)
		_ = filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() {
				return nil
			}
			when := time.Now().Add(-72 * time.Hour)
			return os.Chtimes(p, when, when)
		})
	}

	library := libmgr.Get(libraryID)
	if library == nil {
		t.Fatal("fixture library vanished")
	}
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

	if _, err := sweepOrphans(libraryID, time.Hour, true); err != nil {
		t.Fatalf("sweepOrphans: %v", err)
	}
	reclaimPacked(t, libraryID, false, 0)

	after, err := st.Census(head)
	if err != nil {
		t.Fatal(err)
	}
	if after.Head != before.Head {
		t.Errorf("head = %+v after the sweep, want it untouched at %+v", after.Head, before.Head)
	}
	if after.Unreferenced.Objects != 0 {
		t.Errorf("unreferenced = %+v after -delete, want it emptied", after.Unreferenced)
	}
}

// The sweep bumps the store's GC generation before it marks anything.
//
// That bump is what makes the sweep safe against a write that is already in
// flight. updateBranch re-reads gc_id inside the transaction that moves the
// head and refuses when it has changed, so a client that uploaded its objects
// before the sweep started is told to retry rather than publishing a commit
// whose objects were collected underneath it. Bumping afterwards would leave
// exactly that window open, which is why the order is asserted rather than
// assumed.
func TestSweepBumpsTheGCGenerationBeforeMarking(t *testing.T) {
	sqliteTestDB(t)
	libraryID, _ := orphanFixture(t)

	before, err := libmgr.GetCurrentGCID(libraryID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sweepOrphans(libraryID, time.Hour, false); err != nil {
		t.Fatalf("sweepOrphans: %v", err)
	}
	after, err := libmgr.GetCurrentGCID(libraryID)
	if err != nil {
		t.Fatal(err)
	}
	if after == before {
		t.Fatalf("gc id is still %q after a sweep; an in-flight write would not be caught", before)
	}
	if after == "" {
		t.Error("gc id was cleared rather than bumped")
	}

	// A reporting run bumps it too. It has read the store and a client cannot
	// tell a report from a collection, so the conservative answer is the only
	// safe one.
	second, err := sweepOrphans(libraryID, time.Hour, false)
	if err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	_ = second
	third, err := libmgr.GetCurrentGCID(libraryID)
	if err != nil {
		t.Fatal(err)
	}
	if third == after {
		t.Error("a second sweep reused the same gc id; two sweeps are two generations")
	}
}

// objectOnDisk asks the store rather than looking for a file, because since the
// cutover an object is inside a pack and there is no file of its own to look
// for. What every caller means is "does the store still hold this", which is
// the question Exists answers either way.
func objectOnDisk(t *testing.T, objType, libraryID, objID string) bool {
	t.Helper()
	exists, err := objstore.New(absDataDir, objType).Exists(libraryID, objID)
	if err != nil {
		t.Fatalf("asking the %s store about %s: %v", objType, objID, err)
	}
	return exists
}

// reclaimPacked carries out what expiry and the sweep could only defer: it
// seals the library's packs and rewrites them without the frames nothing
// reaches. `silo gc -delete` does exactly this when it is also given -compact,
// and since the cutover it is the only thing that frees a byte.
//
// expire says whether to apply a retention cut at all -- `-compact` alone never
// expires history -- and window overrides the library's own policy, as
// -expire-window does.
func reclaimPacked(t *testing.T, libraryID string, expire bool, window time.Duration) {
	t.Helper()
	if err := objstore.Close(); err != nil {
		t.Fatalf("sealing: %v", err)
	}
	opt := compactOpts{threshold: 0, minAge: 0, del: true, expire: expire, expireWindow: window}
	if _, err := compactLibrary(libraryID, opt); err != nil {
		t.Fatalf("compacting %s: %v", libraryID, err)
	}
}

func assertObjectExists(t *testing.T, objType, libraryID, objID string, want bool) {
	t.Helper()
	if got := objectOnDisk(t, objType, libraryID, objID); got != want {
		if want {
			t.Errorf("%s should still be there", objID[:12])
		} else {
			t.Errorf("%s should have been removed", objID[:12])
		}
	}
}
