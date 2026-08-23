package silod

import (
	"errors"
	"fmt"
	"time"

	"github.com/dkam/silo/fileserver/objmgr"
	"github.com/dkam/silo/fileserver/repomgr"
	"github.com/dkam/silo/store"
)

// commitAttempts bounds the compare-and-swap retry loop.
//
// A retry is cheap and a livelock is not: each pass re-reads the head and
// re-applies one tree mutation onto the new root, and the content it is
// committing was stored before the loop began and is not rewritten. Five
// passes is far past what contention on one library produces; beyond that the
// honest answer is that the caller is losing a race it should be told about.
const commitAttempts = 5

// defaultFileMode is the mode a server-written regular file gets. The format
// carries permission bits only — file type lives in NodeType — so this is the
// ordinary 0644 and nothing more.
const defaultFileMode = 0o644

// defaultDirMode is the mode a server-created directory gets. Permission bits
// only — a store-v2 dirent records the entry's type in NodeType, so the
// S_IFDIR the Seafile lane ors in here has no place in it.
const defaultDirMode = 0o755

// mutateTree applies one change to a store-v2 library's tree and moves the
// head to a commit describing it, retrying if it loses the race for the head.
//
// This is the store-v2 replacement for GenNewCommit and its friends, and the
// difference worth naming is what it does NOT do. The Seafile lane, on losing
// the head, merged: it read both trees and reconciled them. That cannot exist
// here, because merging trees means reading names, and in an E2EE library the
// server cannot. So a lost race re-applies the same mutation to the new root
// and swaps again — which is the same loop a client runs against PUT head, and
// it is what "no server-side merge" means in practice rather than in the
// abstract.
//
// The mutation is a function of the root rather than a value because that is
// exactly what makes the retry correct: on the second pass it must be applied
// to the head that won, not re-proposed against the one that lost. It is
// handed the attempt's timestamp too, so the mtime a dirent records and the
// commit's created_at are the same instant rather than two calls to the clock.
func mutateTree(repo *repomgr.Repo, author string, mutate func(st *objmgr.Store, root store.ID, now int64) (store.ID, error)) (store.ID, error) {
	st, err := repomgr.OpenStore(repo.StoreID, repo.Format)
	if err != nil {
		return store.ID{}, err
	}

	head := repo
	for attempt := 0; attempt < commitAttempts; attempt++ {
		// Read before the mutation, so a GC that starts mid-write is caught by
		// the generation check below rather than racing the objects this is
		// about to publish.
		gcID, err := repomgr.GetCurrentGCID(head.StoreID)
		if err != nil {
			return store.ID{}, fmt.Errorf("failed to read gc id: %w", err)
		}

		oldRoot, err := store.ParseID(head.RootID)
		if err != nil {
			return store.ID{}, err
		}
		now := time.Now().Unix()
		newRoot, err := mutate(st, oldRoot, now)
		if err != nil {
			return store.ID{}, err
		}

		// A mutation that changed nothing mints no commit. The tree is
		// content-addressed, so an identical root is not merely equivalent to
		// the old one, it IS the old one — and a commit whose root equals its
		// parent's would make changes?since= report a modification that did
		// not happen.
		if newRoot == oldRoot {
			return oldRoot, nil
		}

		parent, err := store.ParseID(head.HeadCommitID)
		if err != nil {
			return store.ID{}, err
		}
		commitID, err := st.PutCommit(&store.Commit{
			Root:      newRoot,
			Parents:   []store.ID{parent},
			CreatedAt: now,
			Author:    author,
		})
		if err != nil {
			return store.ID{}, err
		}

		_, err = updateBranch(repo.ID, head.StoreID, headMove{
			CommitID: commitID.String(),
			RootID:   newRoot.String(),
			Author:   author,
			Ctime:    now,
		}, head.HeadCommitID, "", true, gcID)
		if err == nil {
			return newRoot, nil
		}
		if errors.Is(err, ErrGCConflict) {
			return store.ID{}, err
		}

		// Lost the head, or lost it to a GC generation change. Re-read and
		// rebuild on whatever won. The objects already written stay written:
		// they are addressed by content, so the next pass reuses them and the
		// ones the losing commit orphaned are the collector's business.
		head, err = repomgr.GetWithReason(repo.ID)
		if err != nil {
			return store.ID{}, err
		}
	}
	return store.ID{}, fmt.Errorf("gave up after %d attempts to move the head of %s", commitAttempts, repo.ID)
}
