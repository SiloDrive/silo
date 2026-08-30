package silod

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/dkam/silo/fileserver/libmgr"
	"github.com/dkam/silo/fileserver/objmgr"
	"github.com/dkam/silo/fileserver/objstore"
	"github.com/dkam/silo/fileserver/option"
	"github.com/dkam/silo/internal/format"
	storefmt "github.com/dkam/silo/store"
	log "github.com/sirupsen/logrus"
)

// DefaultOrphanAge is how long an unreferenced object must have sat there
// before a sweep will consider it.
//
// The number is a safety margin and not a tuning knob. An upload that stopped
// halfway and an upload still in progress leave the same trace — objects
// nothing points at — and the only thing separating them is elapsed time. A
// day is orders of magnitude longer than any client takes to go from its first
// chunk to its head move, including one on a bad connection retrying, and it
// is short enough that a server does not carry a week of dead uploads.
const DefaultOrphanAge = 24 * time.Hour

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
	// packed is how many were left where they are because a pack holds them.
	// A sealed pack is immutable, so there is no delete to perform: the bytes
	// come back when compaction rewrites the pack without them.
	//
	// Counted separately from removed and from an error, because it is
	// neither. Folding it into removed would report space that is still on the
	// disk; logging it as a failure would tell an operator something went
	// wrong when nothing did.
	packed      int
	packedBytes int64
}

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
// The **GC generation** is the backstop for the pathological case the age
// threshold cannot cover — a client that uploaded its objects and then stalled
// for longer than the threshold before moving the head. Bumping gc_id makes
// updateBranch refuse that head move (ErrGCConflict, answered as a 503 with a
// retry), so the client re-uploads rather than publishing a commit whose
// objects were collected underneath it. The bump happens **before** the mark,
// because a bump afterwards leaves precisely the window it exists to close.
//
// A reporting run bumps the generation too. It has read the store, a client
// cannot tell a report from a collection, and the cost of an unnecessary bump
// is one client retry.
func sweepOrphans(libraryID string, minAge time.Duration, del bool) (orphanSweep, error) {
	sweep := orphanSweep{libraryID: libraryID}

	library, st, head, err := openLibraryAtHead(libraryID)
	if err != nil {
		return sweep, err
	}

	// Before the mark, never after. See above.
	if err := bumpGCID(library.StoreID); err != nil {
		return sweep, err
	}

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
			if errors.Is(err, objstore.ErrInPack) {
				sweep.packed++
				sweep.packedBytes += o.Size
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

// bumpGCID gives a store a new generation stamp.
//
// The value is opaque and only ever compared for equality — updateBranch asks
// whether it is the same one the write read on its way in. Random rather than
// a counter because a counter has to be read before it is written, and two
// sweeps racing on that read would mint the same "next" value and each think
// the other's generation was its own.
func bumpGCID(storeID string) error {
	var b [5]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Errorf("failed to mint a gc id: %w", err)
	}
	id := hex.EncodeToString(b[:])

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

	var found, removed, tooYoung, packed int
	var bytes, freed, packedBytes int64
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
		packed += sweep.packed
		packedBytes += sweep.packedBytes
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
	if packed > 0 {
		// Named rather than left inside "removed", because these bytes are
		// still on the disk. An operator who ran this to free space has to be
		// told that this much of it will not come back until compaction runs.
		fmt.Printf("%d unreferenced objects, %s, are inside packs and were left there; "+
			"a pack is immutable, and compaction is what reclaims them.\n", packed, format.Bytes(packedBytes))
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
