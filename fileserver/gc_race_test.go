package silod

import (
	"errors"
	"testing"
	"time"

	"github.com/dkam/silo/fileserver/libmgr"
	"github.com/dkam/silo/fileserver/objmgr"
	"github.com/dkam/silo/store"
)

// A write that was in flight when a sweep started loses its head move.
//
// The two halves of this were each tested and never joined.
// TestCommitThatRacedGCIsRejected proves updateBranch rejects a moved
// generation, but it moves the generation with a raw UPDATE.
// TestSweepBumpsTheGCGenerationBeforeMarking proves the sweep writes a new one,
// but nothing was racing it. Neither says the collector's bump is the thing
// updateBranch rejects on, which is the whole safety claim -- and a bump
// written to the wrong table, or to the library id where updateBranch reads the
// store id, would pass both of them.
//
// So: run the real sweep in the window between a real commit reading the
// generation and swapping the head.
func TestARealSweepRejectsAWriteThatWasAlreadyInFlight(t *testing.T) {
	libraryID, acct := testLibrary(t)
	library, err := libmgr.GetWithReason(libraryID)
	if err != nil {
		t.Fatal(err)
	}

	swept := false
	_, _, err = mutateTree(library, acct.Email, func(st *objmgr.Store, root store.ID, now int64) (store.ID, error) {
		if !swept {
			swept = true
			// Reporting only. Even a run that removes nothing has read the
			// store, and a client cannot tell a report from a collection.
			if _, err := sweepOrphans(libraryID, time.Hour, false); err != nil {
				t.Fatalf("sweepOrphans mid-write: %v", err)
			}
		}
		return st.Mkdir(root, "/raced", defaultDirMode, now)
	})
	if !errors.Is(err, ErrGCConflict) {
		t.Fatalf("a commit racing a real sweep returned %v, want ErrGCConflict", err)
	}

	// And the retry succeeds, which is what makes the refusal a retry rather
	// than a wall. A client that is told to try again and then cannot is worse
	// off than one that was never refused.
	if _, _, err := mutateTree(library, acct.Email, func(st *objmgr.Store, root store.ID, now int64) (store.ID, error) {
		return st.Mkdir(root, "/raced", defaultDirMode, now)
	}); err != nil {
		t.Fatalf("the retry after a sweep failed: %v", err)
	}
}

// Expiry bumps the generation too, and the same write loses to it.
func TestARealExpiryRejectsAWriteThatWasAlreadyInFlight(t *testing.T) {
	libraryID, acct := testLibrary(t)
	library, err := libmgr.GetWithReason(libraryID)
	if err != nil {
		t.Fatal(err)
	}

	// Give it history to expire, so the pass actually deletes something rather
	// than returning early with nothing to do and no bump.
	for _, name := range []string{"/one", "/two"} {
		if _, _, err := mutateTree(library, acct.Email, func(st *objmgr.Store, root store.ID, now int64) (store.ID, error) {
			return st.Mkdir(root, name, defaultDirMode, now)
		}); err != nil {
			t.Fatal(err)
		}
		library, err = libmgr.GetWithReason(libraryID)
		if err != nil {
			t.Fatal(err)
		}
	}

	expired := false
	_, _, err = mutateTree(library, acct.Email, func(st *objmgr.Store, root store.ID, now int64) (store.ID, error) {
		if !expired {
			expired = true
			if _, err := expireHistory(libraryID, time.Nanosecond, true); err != nil {
				t.Fatalf("expireHistory mid-write: %v", err)
			}
		}
		return st.Mkdir(root, "/raced", defaultDirMode, now)
	})
	if !errors.Is(err, ErrGCConflict) {
		t.Fatalf("a commit racing a real expiry returned %v, want ErrGCConflict", err)
	}
}

// Running a sweep twice is a no-op the second time, and running expiry twice
// finds a chain that already ends.
//
// Idempotence is what makes these safe to put on a timer. A second pass that
// found new work would mean the first left the store in a state it did not
// intend, and the way that shows up on a cron is a library that loses a commit
// a day.
func TestSweepAndExpiryAreIdempotent(t *testing.T) {
	sqliteTestDB(t)
	libraryID, ids := historyFixture(t, 40*24*time.Hour, 30*24*time.Hour, 1*24*time.Hour)

	first, err := expireHistory(libraryID, 14*24*time.Hour, true)
	if err != nil {
		t.Fatal(err)
	}
	if first.expired != 2 {
		t.Fatalf("first expiry took %d commits, want 2", first.expired)
	}
	second, err := expireHistory(libraryID, 14*24*time.Hour, true)
	if err != nil {
		t.Fatalf("second expiry: %v", err)
	}
	if second.expired != 0 {
		t.Errorf("second expiry took %d more commits, want 0", second.expired)
	}
	if !commitExists(t, libraryID, ids[2]) {
		t.Error("the head went on the second pass")
	}

	firstSweep, err := sweepOrphans(libraryID, 0, true)
	if err != nil {
		t.Fatal(err)
	}
	if firstSweep.removed == 0 {
		t.Fatal("the first sweep removed nothing; expiry should have left work")
	}
	secondSweep, err := sweepOrphans(libraryID, 0, true)
	if err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	if secondSweep.removed != 0 {
		t.Errorf("second sweep removed %d more objects, want 0", secondSweep.removed)
	}

	// And the library still reads.
	library := libmgr.Get(libraryID)
	if library == nil {
		t.Fatal("the library no longer loads")
	}
	head, err := store.ParseID(library.HeadCommitID)
	if err != nil {
		t.Fatal(err)
	}
	st, err := library.Store()
	if err != nil {
		t.Fatal(err)
	}
	c, err := st.Census(head)
	if err != nil {
		t.Fatalf("census after two of each: %v", err)
	}
	if c.Head.Bytes == 0 {
		t.Error("head is empty after the passes")
	}
	if c.History.Bytes != 0 || c.Unreferenced.Bytes != 0 {
		t.Errorf("history=%+v unreferenced=%+v, want both empty", c.History, c.Unreferenced)
	}
}

// A library with only a head has nothing to expire, at any window.
func TestExpireLeavesASingleCommitAlone(t *testing.T) {
	sqliteTestDB(t)
	libraryID, ids := historyFixture(t, 400*24*time.Hour)

	got, err := expireHistory(libraryID, time.Nanosecond, true)
	if err != nil {
		t.Fatalf("expireHistory: %v", err)
	}
	if got.expired != 0 {
		t.Errorf("expired %d commits from a library that has only a head", got.expired)
	}
	if !commitExists(t, libraryID, ids[0]) {
		t.Error("the only commit was deleted")
	}
}
