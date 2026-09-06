package silod

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/dkam/silo/fileserver/libmgr"
	"github.com/dkam/silo/fileserver/objmgr"
	"github.com/dkam/silo/fileserver/objstore"
	"github.com/dkam/silo/fileserver/option"
	"github.com/dkam/silo/internal/format"
	storefmt "github.com/dkam/silo/store"
	log "github.com/sirupsen/logrus"
)

// orphanSweep is what one library's sweep found and what it did.
type orphanSweep struct {
	libraryID string
	// found and bytes are objects past the age threshold: the candidates.
	found int
	bytes int64
	// tooYoung is how many were skipped for being too recent. Reported rather
	// than silently dropped, because "nothing to collect" and "plenty to
	// collect, all of it too new to touch" are different states and an
	// operator watching a disk fill needs to tell them apart.
	tooYoung int
	removed  int
	freed    int64
	// deferred is how many the store could not delete in place, and whose
	// space a later background rewrite reclaims instead.
	//
	// Counted separately from removed and from an error, because it is
	// neither. Folding it into removed would report space that is still on the
	// disk; logging it as a failure would tell an operator something went
	// wrong when nothing did. What the store could not delete and why is the
	// store's business — this only has to not lie about the total.
	deferred      int
	deferredBytes int64
}

// chosen is how many objects the sweep decided to reclaim, whether or not the
// store was able to delete them here.
//
// Since packs became the write path that is almost always deferred rather than
// removed: an object inside a pack cannot be deleted where it is, and
// compaction is what takes the bytes away. The two counts stay separate --
// reporting deferred bytes as freed would quote space that is still on the
// disk -- but the decision itself is the sum.
func (s orphanSweep) chosen() int { return s.removed + s.deferred }

// sweepOrphans reclaims one library's unreferenced objects.
//
// Unreferenced means no commit reaches it — not the head, not any ancestor.
// Superseded content is history and is not this function's business: it is
// reclaimed by a retention policy somebody chose, and a sweep that could not
// tell the two apart would delete the past on the way to deleting garbage.
//
// Two guards, and they cover different failures.
//
// The **age threshold** is the primary one. It is what keeps an upload in
// flight out of the candidate set at all, and it works without coordinating
// with anything.
//
// The **GC generation** is the backstop for the case the age threshold cannot
// cover — a client whose commit names, by dedup, an object that has sat
// unreferenced for longer than the threshold: an upload that stalled and
// resumed, most often. Marking the generation makes updateBranch refuse every
// head move while the sweep runs (ErrGCConflict, answered as a 503 with a
// retry), and bumping it on the way out makes a generation read mid-sweep
// stale, so the client re-checks its blocks against the store as the sweep
// left it rather than publishing a commit whose objects were collected
// underneath it. See beginCollection for why one bump is not enough.
//
// A reporting run marks the generation too. It has read the store, a client
// cannot tell a report from a collection, and the cost is one client retry.
func sweepOrphans(libraryID string, minAge time.Duration, del bool) (orphanSweep, error) {
	sweep := orphanSweep{libraryID: libraryID}

	st, head, end, err := openForCollection(libraryID, true)
	if err != nil {
		return sweep, err
	}
	defer end()

	cutoff := time.Now().Add(-minAge)
	var doomed []objmgr.Orphan
	err = st.Unreferenced(head, func(o objmgr.Orphan) error {
		// The time comes from the listing that found it rather than from a
		// lookup of its own. An object that vanishes between the listing and
		// the removal needs no special handling here: RemoveOrphan treats a
		// missing object as success, because it has to -- deletion has always
		// had to be idempotent for compaction's sake.
		if o.ModTime.After(cutoff) {
			sweep.tooYoung++
			return nil
		}
		sweep.found++
		sweep.bytes += o.Size
		if del {
			doomed = append(doomed, o)
		}
		return nil
	})
	if err != nil {
		return sweep, err
	}

	// Collected after the walk rather than during it: removing objects from
	// under a directory listing that is still being read is the kind of thing
	// that works until the day the backend is not a filesystem.
	for _, o := range doomed {
		if err := st.RemoveOrphan(o); err != nil {
			if errors.Is(err, objstore.ErrReclaimDeferred) {
				sweep.deferred++
				sweep.deferredBytes += o.Size
				continue
			}
			log.Errorf("Failed to remove orphan %s from %s: %v", o.ID, libraryID, err)
			continue
		}
		sweep.removed++
		sweep.freed += o.Size
	}
	return sweep, nil
}

// A collection is two bumps of the generation and a marker in between.
//
// The generation is what updateBranch compares against the one a write read
// on its way in, and a bump refuses every write that read the old one. One
// bump before the mark is not enough. A write that reads the generation after
// that bump, while the mark is walking a head that does not include it, passes
// the comparison and publishes a commit whose objects the mark has already
// decided are dead -- and the age guard does not help, because the objects
// the commit found by dedup may be as old as it likes. So while a collection
// runs the generation carries a marker, and updateBranch refuses any head
// move at all rather than one with a stale stamp. And the collection bumps
// again on its way out, so a write that read the marked generation, or
// answered a check-blocks against the store as it was mid-collection, is stale
// by the time it can commit; one that reads the fresh generation sees the
// store as the collection left it.
//
// The marker outlives a collection that dies: head moves stay refused until
// the next `silo gc` clears it. That is the safe direction, and the message
// the client gets names the command.
const collectingPrefix = "!"

// beginCollection stamps the store as being collected. Before the head is
// read, never after: the head is the mark's root, and a commit that lands
// between the two is one the mark has not seen.
func beginCollection(storeID string) error {
	return stampGCID(storeID, collectingPrefix)
}

// endCollection gives the store a fresh generation with no marker. It is
// deferred by every reclaimer, and a failure leaves the marker in place, which
// refuses writes rather than admitting one against a store that changed.
func endCollection(storeID string) {
	if err := stampGCID(storeID, ""); err != nil {
		log.Errorf("Failed to end the collection of %s; head moves stay refused until the next gc: %v", storeID, err)
	}
}

// isCollecting says whether a generation stamp is the marked kind.
func isCollecting(gcID string) bool { return strings.HasPrefix(gcID, collectingPrefix) }

// stampGCID gives a store a new generation stamp.
//
// The value is opaque and only ever compared for equality — updateBranch asks
// whether it is the same one the write read on its way in. Random rather than
// a counter because a counter has to be read before it is written, and two
// sweeps racing on that read would mint the same "next" value and each think
// the other's generation was its own.
func stampGCID(storeID, prefix string) error {
	var b [5]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Errorf("failed to mint a gc id: %w", err)
	}
	id := prefix + hex.EncodeToString(b[:])

	ctx, cancel := option.WithDBTimeout(context.Background())
	defer cancel()
	_, err := siloPair.Write.ExecContext(ctx,
		"INSERT INTO GCID (library_id, gc_id) VALUES (?, ?) "+
			"ON CONFLICT(library_id) DO UPDATE SET gc_id = excluded.gc_id", storeID, id)
	if err != nil {
		return fmt.Errorf("failed to bump the gc id of %s: %w", storeID, err)
	}
	return nil
}

// gcAfterHeadRead is a test seam: called by every reclaimer once it has read
// the head it will mark from and before it does anything else. Nil in
// production. It exists so a test can land a commit in the window between a
// reclaimer reading the head and acting on it, which is the window the
// generation guard is supposed to close.
var gcAfterHeadRead func(libraryID string)

// openForCollection is how every reclaimer starts: mark the store as being
// collected, then read the head, in that order and never the other -- see
// beginCollection. The caller defers end, which lifts the marker after its
// last removal.
//
// mark is false for a dry run that deletes nothing, which is then a plain
// read. The orphan sweep passes true even when reporting: it has read the
// store, a client cannot tell a report from a collection, and the cost is one
// client retry.
//
// The library is loaded twice on a marked run. The first load is only for the
// store id the marker is stamped on, and the head has to be read after the
// stamp, so the second load is the one that counts.
func openForCollection(libraryID string, mark bool) (st *objmgr.Store, head storefmt.ID, end func(), err error) {
	end = func() {}
	if mark {
		library, err := libmgr.GetWithReason(libraryID)
		if err != nil {
			return nil, storefmt.ID{}, end, err
		}
		if err := beginCollection(library.StoreID); err != nil {
			return nil, storefmt.ID{}, end, err
		}
		end = func() { endCollection(library.StoreID) }
	}
	_, st, head, err = openLibraryAtHead(libraryID)
	if err != nil {
		end()
		return nil, storefmt.ID{}, func() {}, err
	}
	if gcAfterHeadRead != nil {
		gcAfterHeadRead(libraryID)
	}
	return st, head, end, nil
}

// runOrphanSweep sweeps every live library and prints what it found.
//
// It runs alongside the dead-library reclaim rather than instead of it because
// they are the same operator question — "give me my disk back" — asked about
// two different piles, and an operator who has to know which pile their space
// is in before choosing a command does not have the information to choose.
func runOrphanSweep(minAge time.Duration, del bool, quiet bool) error {
	ids, err := liveLibraryIDs()
	if err != nil {
		return err
	}
	if len(ids) == 0 {
		fmt.Println("No live libraries to sweep.")
		return nil
	}

	var found, removed, tooYoung, deferred int
	var bytes, freed, deferredBytes int64
	for _, id := range ids {
		sweep, err := sweepOrphans(id, minAge, del)
		if err != nil {
			// One library the sweep cannot read must not strand the others.
			// The generation was already bumped for it, so the worst outcome
			// is a client retry.
			log.Errorf("Failed to sweep %s: %v", id, err)
			continue
		}
		found += sweep.found
		bytes += sweep.bytes
		tooYoung += sweep.tooYoung
		removed += sweep.removed
		freed += sweep.freed
		deferred += sweep.deferred
		deferredBytes += sweep.deferredBytes
		if !quiet && (sweep.found > 0 || sweep.tooYoung > 0) {
			verb := "would remove"
			if del {
				verb = "removed"
			}
			fmt.Printf("%s %s: %d unreferenced objects, %s", verb, id, sweep.found, format.Bytes(sweep.bytes))
			if sweep.tooYoung > 0 {
				fmt.Printf(" (%d more too recent to touch)", sweep.tooYoung)
			}
			fmt.Println()
		}
	}

	if del {
		fmt.Printf("Removed %d unreferenced objects, %s.\n", removed, format.Bytes(freed))
	} else {
		fmt.Printf("%d unreferenced objects, %s. Re-run with -delete to remove.\n", found, format.Bytes(bytes))
	}
	if deferred > 0 {
		// Named rather than left inside "removed", because these bytes are
		// still on the disk. An operator who ran this to free space has to be
		// told that this much of it will not come back yet.
		fmt.Printf("%d unreferenced objects, %s, could not be deleted in place and were left; "+
			"they are inside sealed packs -- run with -compact to reclaim them.\n",
			deferred, format.Bytes(deferredBytes))
	}
	if tooYoung > 0 {
		// Said out loud rather than left to be inferred from a total that does
		// not add up to what silo df reported. The two commands disagree by
		// exactly this number, and an operator chasing that difference should
		// find it named rather than have to work it out.
		fmt.Printf("%d unreferenced objects are newer than %s and were not considered; "+
			"they may be uploads in progress.\n", tooYoung, minAge)
	}
	return nil
}

// openLibraryAtHead loads a library, opens its store, and parses its head.
//
// The three commands that walk a library -- df's census, the orphan sweep and
// history expiry -- all need exactly these three things before they can do
// anything, and all three had their own copy of getting them. Each copy also
// had to spell the error for an unreadable head commit, and three spellings of
// one error is how they come to disagree about it.
func openLibraryAtHead(libraryID string) (*libmgr.Library, *objmgr.Store, storefmt.ID, error) {
	library, err := libmgr.GetWithReason(libraryID)
	if err != nil {
		return nil, nil, storefmt.ID{}, err
	}
	st, err := library.Store()
	if err != nil {
		return nil, nil, storefmt.ID{}, err
	}
	head, err := storefmt.ParseID(library.HeadCommitID)
	if err != nil {
		return nil, nil, storefmt.ID{}, fmt.Errorf("head commit %q: %w", library.HeadCommitID, err)
	}
	return library, st, head, nil
}

// liveLibraryIDs is every library that still exists, by store id.
func liveLibraryIDs() ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), option.DBOpTimeout*2)
	defer cancel()

	rows, err := siloPair.Read.QueryContext(ctx, "SELECT library_id FROM Library")
	if err != nil {
		return nil, fmt.Errorf("failed to list libraries: %v", err)
	}
	defer func() { _ = rows.Close() }()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("failed to scan a library row: %v", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to list libraries: %v", err)
	}
	sort.Strings(ids)
	return ids, nil
}
