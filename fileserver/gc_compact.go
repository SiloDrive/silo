package silod

import (
	"fmt"
	"strings"
	"time"

	"github.com/dkam/silo/internal/format"
	storefmt "github.com/dkam/silo/store"
	log "github.com/sirupsen/logrus"
)

// packCompaction is what one library's compaction pass found and did.
type packCompaction struct {
	// planned is how many packs this run would rewrite or delete; dropped is
	// the frame bytes they hold that nothing reaches.
	planned int
	dropped int64
	// tooYoung and held are the two ways a pack past the threshold is left
	// alone. Reported rather than silently dropped, because a run that stopped
	// short and a run with nothing left to do are different states.
	tooYoung int
	held     int
	// pruned is the redundant packs an interrupted earlier rewrite left behind.
	pruned int
	// compacted is how many packs were actually rewritten or deleted, and
	// reclaimed is the file bytes that came back -- what df will change by,
	// not what the frames added up to.
	compacted int
	reclaimed int64
}

// add folds one library's result into a run's total.
func (c *packCompaction) add(o packCompaction) {
	c.planned += o.planned
	c.dropped += o.dropped
	c.tooYoung += o.tooYoung
	c.held += o.held
	c.pruned += o.pruned
	c.compacted += o.compacted
	c.reclaimed += o.reclaimed
}

// compactOpts is one compaction run's settings.
//
// A struct rather than six positional arguments, because four of them are
// numbers and a caller that swapped two would compile and misbehave quietly.
type compactOpts struct {
	// threshold is the dead fraction at which a pack is worth rewriting.
	threshold float64
	// minAge is the age guard: a pack sealed more recently is left alone.
	minAge time.Duration
	// budget is the live bytes one run may copy. 0 is no cap.
	budget int64
	// expire says the run also asked for history expiry, so the rewrite may
	// drop what retention no longer keeps. Off, compaction preserves every
	// commit on disk -- "-compact" alone reclaims garbage, never history.
	//
	// It is a separate switch and not implied by a retention policy existing,
	// because expiring history is irreversible and an operator who typed one
	// flag should not get the other one's consequences.
	expire bool
	// expireWindow overrides every library's policy, as -expire-window does for
	// expiry. Zero means each library's own policy.
	expireWindow time.Duration
	// del turns the plan into rewrites.
	del bool
}

// compactLibrary plans one library's compaction and, with del, runs it.
//
// The guards are the orphan sweep's, and for the same reasons:
//
// The **GC generation** is bumped before the mark, never after. A rewrite drops
// every frame the mark did not reach, and a frame nothing reaches is
// indistinguishable from one a client is about to commit. Bumping gc_id makes
// updateBranch refuse a head move that read the old generation (ErrGCConflict,
// a 503 with a retry), so the client re-uploads rather than publishing a commit
// whose chunks were rewritten out from under it.
//
// The **age guard** is on the pack rather than on the frame, because a pack's
// index carries no times. Every frame in a sealed pack was appended before the
// footer was written, so the pack's seal time is a lower bound on every frame's
// age -- which is the direction the guard needs.
//
// PruneRedundantPacks runs before the mark, so a pack left behind by an
// interrupted rewrite is not measured, scheduled and copied into a third
// version of the same frames. It deletes files, so it runs only under del.
func compactLibrary(libraryID string, opt compactOpts) (packCompaction, error) {
	var out packCompaction

	// A dry run rewrites nothing and marks nothing. A real one is a collection
	// from before its head is read until its last rewrite.
	st, head, end, err := openForCollection(libraryID, opt.del)
	if err != nil {
		return out, err
	}
	defer end()

	// The retention cut, as a decision rather than as a deletion. On a packed
	// store the expiry pass cannot delete a commit inside a sealed pack, so it
	// leaves it there and defers; naming the same set here is what lets the
	// rewrite finally drop it. On a loose store expiry already removed them and
	// this set is a no-op, which is the point -- the two agree.
	var expired []storefmt.ID
	if opt.expire {
		expired, err = retentionCut(st, libraryID, head, opt.expireWindow)
		if err != nil {
			return out, err
		}
	}

	if opt.del {
		pruned, err := st.PruneRedundantPacks()
		if err != nil {
			return out, err
		}
		out.pruned = pruned
	}

	plan, err := st.PlanCompaction(head, expired, opt.threshold, time.Now().Add(-opt.minAge), opt.budget)
	if err != nil {
		return out, err
	}
	out.planned = len(plan.Candidates)
	out.dropped = plan.DeadBytes()
	out.tooYoung = plan.TooYoung
	out.held = len(plan.Held)

	if !opt.del {
		return out, nil
	}
	for _, p := range plan.Candidates {
		done, err := st.CompactPack(p, plan)
		if err != nil {
			// One pack that cannot be rewritten must not strand the rest. The
			// old pack is the authority until the rename, so a failure here
			// leaves the library exactly as it was.
			log.Errorf("Failed to compact a pack of %s: %v", libraryID, err)
			continue
		}
		out.compacted++
		out.reclaimed += done.Reclaimed
	}
	return out, nil
}

// runCompaction compacts every live library and prints what it found.
//
// One library at a time, and one mark per library: the walk is per store, and
// the packs are an attribution of it, so a library with a hundred packs costs
// the same walk as one with a single pack.
func runCompaction(opt compactOpts, quiet bool) error {
	ids, err := liveLibraryIDs()
	if err != nil {
		return err
	}
	if len(ids) == 0 {
		fmt.Println("No live libraries to compact.")
		return nil
	}

	var total packCompaction
	for _, id := range ids {
		got, err := compactLibrary(id, opt)
		if err != nil {
			// One library that cannot be read must not strand the others, the
			// same as the sweep. The generation was already bumped for it, so
			// the worst outcome is a client retry.
			log.Errorf("Failed to compact %s: %v", id, err)
			continue
		}
		total.add(got)

		if quiet || (got.planned == 0 && got.tooYoung == 0 && got.held == 0 && got.pruned == 0) {
			continue
		}
		verb := "would rewrite"
		if opt.del {
			verb = "rewrote"
		}
		fmt.Printf("%s %s: %d packs, %s dropped", verb, id, got.planned, format.Bytes(got.dropped))
		var notes []string
		if got.tooYoung > 0 {
			notes = append(notes, fmt.Sprintf("%d sealed too recently", got.tooYoung))
		}
		if got.held > 0 {
			notes = append(notes, fmt.Sprintf("%d over budget", got.held))
		}
		if got.pruned > 0 {
			notes = append(notes, fmt.Sprintf("%d redundant packs removed", got.pruned))
		}
		if len(notes) > 0 {
			fmt.Printf(" (%s)", strings.Join(notes, ", "))
		}
		fmt.Println()
	}

	if opt.del {
		// File bytes rather than frame bytes: this is the number df changes by,
		// and an operator who ran this to free space is watching a disk.
		fmt.Printf("Rewrote %d packs, %s freed.\n", total.compacted, format.Bytes(total.reclaimed))
	} else {
		fmt.Printf("%d packs past the %.0f%% threshold, holding %s of dead frames. Re-run with -delete to rewrite them.\n",
			total.planned, opt.threshold*100, format.Bytes(total.dropped))
	}
	if total.held > 0 {
		// Named, because a run that stopped short otherwise looks exactly like
		// a run with nothing left to do.
		fmt.Printf("%d packs were left for a later run: this one's budget was spent.\n", total.held)
	}
	if total.tooYoung > 0 {
		fmt.Printf("%d packs were sealed less than %s ago and were not considered; "+
			"they may hold uploads in progress.\n", total.tooYoung, opt.minAge)
	}
	return nil
}

// parseCompactBudget reads the -compact-budget flag: a size, or nothing.
//
// Zero means no cap rather than "do nothing", which is the reading a cron wants:
// a scheduled compaction with no budget set should catch up rather than fall
// further behind every night. An operator who wants throttling sets one.
//
// The size parser is the quota command's, so that a number typed at silo means
// the same thing everywhere it is typed -- including the words for "no
// ceiling", which it already answers through remove and which are not
// respelled here.
func parseCompactBudget(s string) (int64, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "0":
		return 0, nil
	}
	n, remove, err := parseQuotaSize(s)
	if err != nil {
		return 0, fmt.Errorf("-compact-budget: %w", err)
	}
	if remove {
		return 0, nil
	}
	return n, nil
}
