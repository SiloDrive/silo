package silod

import (
	"bytes"
	"errors"
	"testing"

	"github.com/SiloDrive/silo/fileserver/libmgr"
	"github.com/SiloDrive/silo/fileserver/objmgr"
	"github.com/SiloDrive/silo/fileserver/objstore"
	"github.com/SiloDrive/silo/store"
)

// stalledUpload leaves a file's chunks in the store with nothing referencing
// them -- the trace an upload leaves when it stops before its head move -- and
// seals the pack they landed in, so every reclaimer can see them. It returns
// the manifest that names them, unwritten, for a later commit to publish.
func stalledUpload(t *testing.T, st *objmgr.Store) (*store.Manifest, store.ID) {
	t.Helper()
	m, err := st.WriteFile(bytes.NewReader(bytes.Repeat([]byte("stalled"), 40000)))
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Chunks) == 0 {
		t.Fatal("the fixture file was inlined, so there is no chunk to lose")
	}
	if err := objstore.Close(); err != nil {
		t.Fatalf("sealing: %v", err)
	}
	return m, m.Chunks[0].ID
}

// publishOver is the resumed upload: check-blocks said the chunks are there,
// so the client writes only the manifest and moves the head.
func publishOver(library *libmgr.Library, author string, m *store.Manifest) error {
	_, _, err := mutateTree(library, author, func(st *objmgr.Store, root store.ID, now int64) (store.ID, error) {
		id, err := st.PutManifest(m)
		if err != nil {
			return store.ID{}, err
		}
		return st.PutNode(root, "/resumed.bin", objmgr.Node{ID: id, Type: store.NodeFile, Mode: 0o644}, now)
	})
	return err
}

// commitDuring runs one reclaimer over a library holding a stalled upload,
// and lands that upload's commit in the window after the reclaimer has read
// the head. It returns the store and the upload's chunk, or refused when the
// guard turned the commit away -- which is one of the two right answers, and
// the caller has nothing left to check.
func commitDuring(t *testing.T, run func(libraryID string) error) (st *objmgr.Store, chunk store.ID, refused bool) {
	t.Helper()
	libraryID, acct := testLibrary(t)
	library, err := libmgr.GetWithReason(libraryID)
	if err != nil {
		t.Fatal(err)
	}
	st, err = library.Store()
	if err != nil {
		t.Fatal(err)
	}
	m, chunk := stalledUpload(t, st)

	var landed error
	fired := false
	gcAfterHeadRead = func(string) {
		if fired {
			return
		}
		fired = true
		landed = publishOver(library, acct.Email, m)
	}
	t.Cleanup(func() { gcAfterHeadRead = nil })

	if err := run(libraryID); err != nil {
		t.Fatal(err)
	}
	if !fired {
		t.Fatal("the seam never fired")
	}
	if errors.Is(landed, ErrGCConflict) {
		return st, chunk, true
	}
	if landed != nil {
		t.Fatalf("the commit failed for a reason other than the guard: %v", landed)
	}
	return st, chunk, false
}

// A commit that starts after a collection has begun can reference, by dedup,
// an object the collection has already decided is dead. The generation guard
// is documented as the backstop for exactly this -- a client that uploaded and
// stalled -- and it only catches a client that read the generation before the
// bump. One that reads it afterwards, while the mark is walking a head that
// does not include it, passes the check and publishes a commit whose chunks
// are about to go.
//
// Either outcome is acceptable: the commit is refused, or the collection saw
// it. What is not acceptable is a commit that lands and a collection that
// takes its chunks.
func TestACommitThatStartsDuringASweepCannotLoseItsChunksToIt(t *testing.T) {
	var sweep orphanSweep
	st, chunk, refused := commitDuring(t, func(libraryID string) (err error) {
		sweep, err = sweepOrphans(libraryID, 0, true)
		return err
	})
	if refused {
		return
	}
	if sweep.chosen() != 0 {
		t.Fatalf("the commit landed and the sweep still chose %d objects (%d deferred) from a head that reaches them", sweep.chosen(), sweep.deferred)
	}
	if ok, err := st.HasChunk(chunk); err != nil || !ok {
		t.Fatalf("chunk %s: present=%v err=%v after a sweep the commit referencing it survived", chunk, ok, err)
	}
}

// The same window in compaction, which is what actually removes bytes on a
// packed store. Here the wrong answer is not a count but a missing chunk.
func TestACommitThatStartsDuringACompactionCannotLoseItsChunksToIt(t *testing.T) {
	st, chunk, refused := commitDuring(t, func(libraryID string) error {
		_, err := compactLibrary(libraryID, compactOpts{threshold: 0, minAge: 0, del: true})
		return err
	})
	if refused {
		return
	}
	if ok, err := st.HasChunk(chunk); err != nil || !ok {
		t.Fatalf("chunk %s: present=%v err=%v after a compaction the commit referencing it survived", chunk, ok, err)
	}
}

// A client that read the generation while a collection was running, and
// commits after it has finished, is committing against a store it has not
// seen: its check-blocks answers predate the deletions. The generation it
// holds must therefore be stale by the time the collection ends, which means
// the collection has to bump on the way out as well as on the way in.
func TestAGenerationReadDuringACollectionIsStaleWhenItEnds(t *testing.T) {
	libraryID, acct := testLibrary(t)
	library, err := libmgr.GetWithReason(libraryID)
	if err != nil {
		t.Fatal(err)
	}

	var seen string
	gcAfterHeadRead = func(string) {
		var err error
		seen, err = libmgr.GetCurrentGCID(library.StoreID)
		if err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { gcAfterHeadRead = nil })

	if _, err := sweepOrphans(libraryID, 0, true); err != nil {
		t.Fatal(err)
	}
	if seen == "" {
		t.Fatal("the seam never fired, or saw no generation")
	}

	// Mint a commit the honest way, then move the head with the generation
	// the client saw mid-collection.
	st, err := library.Store()
	if err != nil {
		t.Fatal(err)
	}
	root, err := store.ParseID(library.RootID)
	if err != nil {
		t.Fatal(err)
	}
	newRoot, err := st.Mkdir(root, "/late", defaultDirMode, 1)
	if err != nil {
		t.Fatal(err)
	}
	parent, err := store.ParseID(library.HeadCommitID)
	if err != nil {
		t.Fatal(err)
	}
	commitID, err := st.PutCommit(&store.Commit{Root: newRoot, Parents: []store.ID{parent}, CreatedAt: 1, Author: acct.Email})
	if err != nil {
		t.Fatal(err)
	}
	err = updateBranch(library.ID, library.StoreID, headMove{
		CommitID: commitID.String(), RootID: newRoot.String(), Author: acct.Email, Ctime: 1,
	}, library.HeadCommitID, seen)
	if !errors.Is(err, ErrGCConflict) {
		t.Fatalf("a head move with a generation read mid-collection returned %v, want ErrGCConflict", err)
	}
}
