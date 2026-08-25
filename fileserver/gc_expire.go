package silod

import (
	"fmt"
	"time"

	"github.com/dkam/silo/fileserver/libmgr"
	"github.com/dkam/silo/fileserver/objmgr"
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
	// freed is the space the expired commits held that nothing else reaches --
	// what a following sweep will actually reclaim. The commit objects
	// themselves are tens of bytes; this is the number an operator cares about.
	freed int64
}

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

	library, err := libmgr.GetWithReason(libraryID)
	if err != nil {
		return out, err
	}
	st, err := library.Store()
	if err != nil {
		return out, err
	}
	head, err := storefmt.ParseID(library.HeadCommitID)
	if err != nil {
		return out, fmt.Errorf("head commit %q: %w", library.HeadCommitID, err)
	}

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
	out.expired, out.kept = len(doomed), kept
	if len(doomed) == 0 {
		return out, nil
	}

	if !del {
		out.freed = before.History.Bytes
		return out, nil
	}

	// Bump before deleting, for the same reason the sweep does: a write that
	// read the old generation must lose its head move rather than land on a
	// history that changed under it.
	if err := bumpGCID(library.StoreID); err != nil {
		return out, err
	}
	for _, id := range doomed {
		if err := st.RemoveOrphan(objmgr.Orphan{ID: id.String()}); err != nil {
			log.Errorf("Failed to expire commit %s of %s: %v", id, libraryID, err)
			continue
		}
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
	for {
		if seen[id] {
			// Impossible in a Merkle DAG, since a commit's id covers its
			// parents. Bounded anyway: this walk decides what to delete, and
			// an unbounded loop here is not a hung request, it is a list of
			// things to remove that never stops growing.
			break
		}
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

	var expired, kept int
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
		freed += e.freed
		if !quiet && e.expired > 0 {
			verb := "would expire"
			if del {
				verb = "expired"
			}
			fmt.Printf("%s %s: %d commits, %s\n", verb, id, e.expired, format.Bytes(e.freed))
		}
	}

	if del {
		fmt.Printf("Expired %d commits, %s now collectable. Run gc -orphans -delete to reclaim it.\n",
			expired, format.Bytes(freed))
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
// server — cannot accidentally impose one the library never agreed to. A
// library with no policy anywhere keeps everything, which is what makes
// upgrading a server safe: nothing starts deleting because somebody installed
// a new binary.
func expireHistoryByPolicy(libraryID string, del bool) (historyExpiry, error) {
	days, err := libmgr.RetentionDays(libraryID)
	if err != nil {
		return historyExpiry{libraryID: libraryID}, err
	}
	if days <= 0 {
		return historyExpiry{libraryID: libraryID}, nil
	}
	return expireHistory(libraryID, time.Duration(days)*24*time.Hour, del)
}
