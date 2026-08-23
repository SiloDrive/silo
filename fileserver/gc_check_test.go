package silod

import (
	"errors"
	"testing"

	"github.com/dkam/silo/fileserver/objmgr"
	"github.com/dkam/silo/fileserver/repomgr"
	"github.com/dkam/silo/store"
)

// Every commit checks the GC generation, and it is structural rather than a
// convention now: mutateTree is the only way a head moves on this lane, and it
// reads the gc id before it writes anything and hands it to updateBranch. The
// gap this used to police — a handler that passed checkGC false because that
// was the default nobody revisited — cannot be reopened without deleting the
// read, which is one line in one function rather than a flag at each call site.
//
// What is still worth asserting is that the check rejects. Opting into a check
// that never fires is decoration.
func TestCommitThatRacedGCIsRejected(t *testing.T) {
	repoID, acct := storeV2Library(t)
	repo, err := repomgr.GetWithReason(repoID)
	if err != nil {
		t.Fatal(err)
	}

	dbExec(t, "INSERT INTO GCID (repo_id, gc_id) VALUES (?, ?)", repoID, "gc-before")

	// A collector ran after the request read the gc id: what mutateTree holds
	// no longer describes the store it is about to commit against. Simulated
	// by moving the id out from under it between the read and the swap.
	raced := false
	_, _, err = mutateTree(repo, acct.Email, func(st *objmgr.Store, root store.ID, now int64) (store.ID, error) {
		if !raced {
			raced = true
			dbExec(t, "UPDATE GCID SET gc_id = ? WHERE repo_id = ?", "gc-after", repoID)
		}
		return st.Mkdir(root, "/raced", defaultDirMode, now)
	})
	if !errors.Is(err, ErrGCConflict) {
		t.Fatalf("committing against a moved gc generation returned %v, want ErrGCConflict", err)
	}

	// The same mutation with a settled generation goes through, so the
	// rejection above is the gc check and not the commit path failing for some
	// unrelated reason.
	if _, _, err := mutateTree(repo, acct.Email, func(st *objmgr.Store, root store.ID, now int64) (store.ID, error) {
		return st.Mkdir(root, "/settled", defaultDirMode, now)
	}); err != nil {
		t.Fatalf("committing with the current gc id failed: %v", err)
	}
}
