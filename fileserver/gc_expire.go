package silod

import (
	"errors"
	"fmt"
	"time"

	"github.com/dkam/silo/fileserver/libmgr"
	"github.com/dkam/silo/fileserver/objmgr"
	"github.com/dkam/silo/fileserver/objstore"
	"github.com/dkam/silo/internal/format"
	storefmt "github.com/dkam/silo/store"
	log "github.com/sirupsen/logrus"
)

// historyExpiry is what one library's expiry pass found and did.
type historyExpiry struct {
	libraryID string
	// expired is how many commit objects are past the window. kept is how many
	// are inside it, the head included.
	expired int
	kept    int
	// deferred is how many were past the window but could not be removed where
	// they are, because a sealed pack is immutable — their space comes back
	// when compaction rewrites the pack without them.
	//
	// Counted apart from expired, and this is the whole reason the field
	// exists: expired used to be assigned from the candidate list before a
	// single delete had been attempted, so a store where every delete was
	// refused still reported the history as truncated. It was not, the commits
	// are still readable, and the bytes are still on the disk the operator ran
	// this to free.
	deferred int
	// freed is the space the expired commits held that nothing else reaches --
	// what a following sweep will actually reclaim. The commit objects
	// themselves are tens of bytes; this is the number an operator cares about.
	freed int64
}

// chosen is how many commits the cut takes, whether or not the store was able
// to remove them here.
//
// Since packs became the write path that is almost always deferred rather than
// expired: a commit inside a sealed pack cannot be deleted where it is, and
// compaction is what carries the decision out. The two counts stay separate --
// telling an operator their history was truncated when the bytes are still on
// the disk is exactly the lie this struct exists to avoid -- but the decision
// itself is the sum, and that is what a test of the cut asserts against.
func (e historyExpiry) chosen() int { return e.expired + e.deferred }

// expireHistory drops the commit objects older than a retention window.
//
// This is the first operation in Silo that deletes something a commit reaches,
// and it is irreversible: the content of an expired commit is gone once the
// sweep behind it runs. Everything about the design is aimed at making the set
// it chooses obvious.
//
// **It deletes commit objects and nothing else.** The bulk of a library's
// space is chunks, and those are reclaimed by `gc -orphans`, which already
// exists and is already guarded by age and the GC generation. Expiry moves
// bytes from the census's history column to its unreferenced column and stops.
// Two small steps that can each be checked, rather than one large one that
// cannot.
//
// **The cut is a prefix, walked from the head.** History is a linked list, so
// deleting a commit cuts everything behind it whether or not those were meant
// to go. The walk stops at the first commit outside the window and expires it
// and all its ancestors. That matters because commit timestamps come from
// clients and need not decrease along the chain -- clock skew is enough to put
// an old commit between two new ones -- and the invariant every reader depends
// on is that what remains is a walkable prefix from the head.
//
// **The head is never expired.** A library nobody has written to in a year is
// ordinary, not a candidate. Dropping its head would leave the Branch row
// naming a commit the store does not hold, which libmgr reports as corruption
// and no client can recover from.
//
// Nothing else needs teaching about the boundary. walkHistory already ends a
// listing at a commit it cannot read, changes?since= already answers 410 for
// one, and objmgr's mark phase treats a missing parent as the end of that
// branch. Retention was designed into those three before it existed.
func expireHistory(libraryID string, keep time.Duration, del bool) (historyExpiry, error) {
	out := historyExpiry{libraryID: libraryID}

	// A dry run deletes nothing and marks nothing. A real one is a collection
	// from before its head is read until its last removal.
	st, head, end, err := openForCollection(libraryID, del)
	if err != nil {
		return out, err
	}
	defer end()

	// Measured before, because after the commits are gone there is nothing
	// left to attribute the space to.
	before, err := st.Census(head)
	if err != nil {
		return out, err
	}

	doomed, kept, err := commitsBehindTheWindow(st, head, time.Now().Add(-keep))
	if err != nil {
		return out, err
	}
	out.kept = kept
	if len(doomed) == 0 {
		return out, nil
	}

	if !del {
		// A dry run reports candidates, which is what it is for. Whether each
		// one can actually be removed is not knowable without trying.
		out.expired = len(doomed)
		out.freed = before.History.Bytes
		return out, nil
	}

	for _, id := range doomed {
		if err := st.RemoveOrphan(objmgr.Orphan{ID: id.String()}); err != nil {
			if errors.Is(err, objstore.ErrReclaimDeferred) {
				// Inside a sealed pack, which is immutable. Not a failure and
				// not an expiry: the commit is still there and still readable,
				// and its space comes back when compaction rewrites the pack.
				out.deferred++
				continue
			}
			log.Errorf("Failed to expire commit %s of %s: %v", id, libraryID, err)
			continue
		}
		out.expired++
	}

	after, err := st.Census(head)
	if err != nil {
		return out, err
	}
	out.freed = after.Unreferenced.Bytes - before.Unreferenced.Bytes
	return out, nil
}

// commitsBehindTheWindow walks back from the head and returns the commits to
// expire, and how many are kept.
//
// The head is always kept, whatever its timestamp. Everything from the first
// commit older than the cutoff is expired, ancestors included, without
// consulting their own timestamps -- see expireHistory for why the cut has to
// be a prefix.
func commitsBehindTheWindow(st *objmgr.Store, head storefmt.ID, cutoff time.Time) ([]storefmt.ID, int, error) {
	var doomed []storefmt.ID
	kept := 0
	seen := map[storefmt.ID]bool{}

	id, cutting := head, false
	// Revisiting an id is impossible in a Merkle DAG, since a commit's id
	// covers its parents. Bounded anyway: this walk decides what to delete, and
	// an unbounded loop here is not a hung request, it is a list of things to
	// remove that never stops growing.
	for !seen[id] {
		seen[id] = true

		c, err := st.GetCommitPublic(id)
		if err != nil {
			if id == head {
				return nil, 0, fmt.Errorf("head commit %s: %w", id, err)
			}
			// Already expired, by an earlier run. The chain ends here.
			break
		}

		switch {
		case cutting:
			doomed = append(doomed, id)
		case id != head && time.Unix(c.CreatedAt, 0).Before(cutoff):
			cutting = true
			doomed = append(doomed, id)
		default:
			kept++
		}

		if len(c.Parents) == 0 {
			break
		}
		// First parent, matching walkHistory. Nothing writes a merge today.
		id = c.Parents[0]
	}
	return doomed, kept, nil
}

// retentionCut is the commits a library's retention policy no longer keeps.
//
// The same set expireHistory deletes, produced without deleting anything, so
// that compaction can apply a retention decision the expiry pass was unable to
// carry out. That is not a corner case: a commit inside a sealed pack cannot be
// deleted where it is, so on a packed store *every* expiry defers, and a
// compaction that did not know the cut would copy the expired commits forward
// for ever.
//
// A zero window means the library's own policy, and a library with no policy
// anywhere keeps everything -- the same reading expireHistoryByPolicy takes,
// because it is the same function that takes it.
func retentionCut(st *objmgr.Store, libraryID string, head storefmt.ID, window time.Duration) ([]storefmt.ID, error) {
	keep, err := retentionWindow(libraryID, window)
	if err != nil || keep <= 0 {
		return nil, err
	}
	doomed, _, err := commitsBehindTheWindow(st, head, time.Now().Add(-keep))
	return doomed, err
}

// retentionWindow is how far back a library keeps history: the override if one
// was given, otherwise its stored policy, and zero for "keep everything".
//
// One function rather than the rule written out at each site that needs it.
// Two passes now ask it -- expiry, which deletes, and compaction, which drops
// what expiry could not -- and a cut those two disagreed about would show up as
// history that comes back from the dead on the next run.
//
// A library with no policy anywhere keeps everything, which is what makes
// upgrading a server safe: nothing starts deleting because somebody installed a
// new binary.
func retentionWindow(libraryID string, override time.Duration) (time.Duration, error) {
	if override > 0 {
		return override, nil
	}
	days, err := libmgr.RetentionDays(libraryID)
	if err != nil || days <= 0 {
		return 0, err
	}
	return time.Duration(days) * 24 * time.Hour, nil
}

// runHistoryExpiry expires every live library and prints what it found.
//
// A zero window means follow each library's own retention policy, which is the
// ordinary case and the one a scheduler would use. A non-zero window overrides
// every policy at once: useful for a one-off reclaim on a full disk, and
// dangerous enough that it is a separate flag rather than the default reading
// of a number.
func runHistoryExpiry(keep time.Duration, del bool, quiet bool) error {
	ids, err := liveLibraryIDs()
	if err != nil {
		return err
	}

	var expired, kept, deferred int
	var freed int64
	for _, id := range ids {
		var e historyExpiry
		var err error
		if keep > 0 {
			e, err = expireHistory(id, keep, del)
		} else {
			e, err = expireHistoryByPolicy(id, del)
		}
		if err != nil {
			log.Errorf("Failed to expire the history of %s: %v", id, err)
			continue
		}
		expired += e.expired
		kept += e.kept
		deferred += e.deferred
		freed += e.freed
		if !quiet && (e.expired > 0 || e.deferred > 0) {
			verb := "would expire"
			if del {
				verb = "expired"
			}
			fmt.Printf("%s %s: %d commits, %s", verb, id, e.expired, format.Bytes(e.freed))
			if e.deferred > 0 {
				fmt.Printf(" (%d more left where they are)", e.deferred)
			}
			fmt.Println()
		}
	}

	if del {
		fmt.Printf("Expired %d commits, %s now collectable. Run gc -orphans -delete to reclaim it.\n",
			expired, format.Bytes(freed))
		if deferred > 0 {
			// Named rather than folded into the count above, because these
			// commits are still there and still readable. An operator told
			// their history was truncated when it was not would go looking for
			// the space in the wrong place.
			fmt.Printf("%d commits past retention are inside sealed packs and were left there; "+
				"run with -compact to reclaim them.\n", deferred)
		}
	} else {
		fmt.Printf("%d commits past retention, holding %s. Re-run with -delete to expire them.\n",
			expired, format.Bytes(freed))
	}
	return nil
}

// expireHistoryByPolicy expires one library according to its stored retention,
// falling back to the server default.
//
// This is the entry point a scheduler would call. It takes no window, so
// whatever runs it — an operator, a cron, or one day a timer inside the
// server — cannot accidentally impose one the library never agreed to.
// retentionWindow says what "no policy" means, and says it to compaction too.
func expireHistoryByPolicy(libraryID string, del bool) (historyExpiry, error) {
	keep, err := retentionWindow(libraryID, 0)
	if err != nil || keep <= 0 {
		return historyExpiry{libraryID: libraryID}, err
	}
	return expireHistory(libraryID, keep, del)
}
