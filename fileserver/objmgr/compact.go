// Scheduling compaction: which of a library's packs are worth rewriting, in
// what order, and how much work one run is allowed to do.
//
// docs/plans/compaction.md owns the decisions. The two that shape this file
// are Decision 1 — the mark is per store and packs are an attribution of it,
// so a library costs one walk however many packs it has — and the rule that
// PackStats is a cache: the plan is a scheduling input, and the rewrite
// re-verifies against the same marks before it drops a frame.
//
// Nothing here opens a frame. Planning reads footers; rewriting copies bytes
// it never decodes. Compaction runs on an E2EE library the server cannot read,
// which is the library most likely to be large.
package objmgr

import (
	"fmt"
	"sort"
	"time"

	"github.com/dkam/silo/fileserver/objstore"
	"github.com/dkam/silo/store"
)

// CompactionPlan is what one library's pass would do, and what it would leave.
//
// Three fields rather than one list, because "nothing to do" and "plenty to do
// and this run may not do it" are different states and an operator watching a
// disk has to be able to tell them apart. A run that stopped short reports as
// having stopped short.
type CompactionPlan struct {
	// Candidates are past the threshold, old enough to touch, and inside the
	// budget — in the order they should be run.
	Candidates []objstore.PackStat
	// TooYoung is how many were past the threshold but sealed too recently.
	TooYoung int
	// Held are past the threshold and old enough, but over this run's budget.
	Held []objstore.PackStat

	// marks is the walk the plan was drawn from, carried so that CompactPack
	// cannot be handed a different one. The rule that a pack is rewritten
	// against the set it was measured against is then structural rather than
	// something a doc comment asks the caller to observe.
	marks *marks
}

// DeadBytes is what running the plan would drop.
func (p CompactionPlan) DeadBytes() int64 {
	var n int64
	for _, c := range p.Candidates {
		n += c.DeadBytes()
	}
	return n
}

// PlanCompaction measures every sealed pack in a library and decides which are
// worth rewriting.
//
// The plan carries the marks it was drawn from, and that is the point of it
// being a separate call from CompactPack: one walk per library, handed to every
// rewrite the plan produced, so the set a pack is measured against is the set
// it is rewritten against. Two calls also let a dry run print the plan without
// touching anything, which is how every reclaimer here works.
//
// **The liveness answer is history, never head.** A rewrite that kept only what
// the head reaches would expire history without a retention policy having said
// so, and no other part of the system would report it having happened. What
// comes back is the same set the orphan sweep deletes against.
//
// **expired is retention, and it is the only way a packed store can apply one.**
// A commit inside a sealed pack cannot be deleted where it is, so the expiry
// pass leaves it on disk and still reachable; a walk that believed the disk
// would keep every expired history alive for ever, and no later run would ever
// free it. Naming the cut here lets the rewrite drop the commit and everything
// only it reached. Empty means "keep all of history", which is what a library
// with no retention policy asks for.
//
// threshold is the dead fraction at which a pack is worth the copy. sealedBefore
// is the age guard: a pack sealed after it is left alone, because a frame
// nothing reaches is indistinguishable from one a client is about to commit,
// and a pack's seal time is a lower bound on every frame's age in it. budget is
// live bytes this run may copy, or 0 for no cap.
//
// The marks are held on the plan rather than stored, and there is deliberately
// no catalog row yet: a row is for a later process to schedule from, and the
// plan here is drawn and acted on seconds apart with the marks in memory. The
// row arrives with the in-process scheduler, where a gc_id on it is what makes
// its staleness detectable.
//
// One walk, not the census's two. PackCensus takes a second one to split the
// head out of history, because df has a column for it; nothing here reads that
// column, and the stats come back with HeadBytes unfilled. A walk of every tree
// in the library to fill a number no decision consults is the largest thing
// this call could do for nothing.
func (s *Store) PlanCompaction(head store.ID, expired []store.ID, threshold float64, sealedBefore time.Time, budget int64) (CompactionPlan, error) {
	cut := make(map[store.ID]bool, len(expired))
	for _, id := range expired {
		cut[id] = true
	}
	all, err := s.reachable([]store.ID{head}, true, cut)
	if err != nil {
		return CompactionPlan{}, fmt.Errorf("walking the history: %w", err)
	}

	stats, err := s.packStats(nil, all)
	if err != nil {
		return CompactionPlan{}, err
	}
	plan := selectForCompaction(stats, threshold, sealedBefore, budget)
	plan.marks = all
	return plan, nil
}

// selectForCompaction is the scheduling decision on its own: threshold, age,
// order and budget over numbers a walk has already produced.
//
// Separate from the walk because it is arithmetic and can be tested as such.
// Building a real pack with a dead fraction of exactly 0.4 tests the chunker.
func selectForCompaction(stats []objstore.PackStat, threshold float64, sealedBefore time.Time, budget int64) CompactionPlan {
	var plan CompactionPlan

	var eligible []objstore.PackStat
	for _, p := range stats {
		if p.DeadFraction() < threshold {
			continue
		}
		if p.SealedAt.After(sealedBefore) {
			plan.TooYoung++
			continue
		}
		eligible = append(eligible, p)
	}

	// Wholly dead packs first: they are deleted rather than rewritten, so they
	// cost no I/O at all, and a library whose history has just been expired is
	// mostly this case. Among the rest, dead fraction descending — reclaimed
	// per byte copied, which is what a budget measured in bytes copied should
	// be spent by. Pack id breaks a tie so that two runs over an unchanged
	// store agree about the order.
	sort.SliceStable(eligible, func(i, j int) bool {
		a, b := eligible[i], eligible[j]
		if (a.LiveBytes == 0) != (b.LiveBytes == 0) {
			return a.LiveBytes == 0
		}
		if a.DeadFraction() != b.DeadFraction() {
			return a.DeadFraction() > b.DeadFraction()
		}
		return a.PackID < b.PackID
	})

	var spent int64
	for i, p := range eligible {
		// The first pack the budget cannot cover is held, and so is everything
		// behind it: the order is a priority order, and skipping past a held
		// pack to a cheaper one further down would spend the budget on the
		// less valuable rewrite.
		if budget > 0 && spent+p.LiveBytes > budget {
			plan.Held = append(plan.Held, eligible[i:]...)
			break
		}
		spent += p.LiveBytes
		plan.Candidates = append(plan.Candidates, p)
	}
	return plan
}

// CompactPack rewrites one pack from a plan, against the plan's own marks.
//
// The stat says which of the two stores holds it, which is why PackStat carries
// its ObjType: an id is only unique within a store, and a chunk pack's id
// offered to the object store names no pack it holds.
//
// The closure is the whole re-verification the plan promised. It is the same
// set the plan was drawn from rather than a fresh walk, which is what makes the
// two calls one mark: a walk taken again here would be a second answer, and the
// pack would be measured against one and rewritten against the other. Taking
// the plan rather than a set of marks is what makes that impossible to get
// wrong from outside.
func (s *Store) CompactPack(p objstore.PackStat, plan CompactionPlan) (objstore.Compaction, error) {
	st, isChunk := s.storeFor(p.ObjType)
	return st.CompactPack(s.storeID, p.PackID, func(objID string) bool {
		return plan.marks.has(objID, isChunk)
	})
}

// storeFor picks the store a pack or an object belongs to, and says which of
// the two marks answers for it. One place that reads objstore's type name, so
// that the mapping cannot drift between the callers that need it.
func (s *Store) storeFor(objType string) (*objstore.ObjectStore, bool) {
	if objType == objstore.TypeChunks {
		return s.chunks, true
	}
	return s.objects, false
}

// PruneRedundantPacks clears the debris of an interrupted rewrite, in both
// stores, and deletes any pack whose every id lives in another.
//
// It runs before the mark rather than after it, so that a redundant pack is not
// measured, scheduled, and rewritten into a third copy of the same frames.
//
// It deletes files, so it belongs to a run that was asked to delete. A dry run
// that quietly removed a pack would be the one reclaimer here that acts without
// being told to.
func (s *Store) PruneRedundantPacks() (int, error) {
	removed := 0
	for _, st := range s.stores() {
		n, err := st.PruneRedundantPacks(s.storeID)
		if err != nil {
			return removed, fmt.Errorf("pruning redundant packs: %w", err)
		}
		removed += n
	}
	return removed, nil
}

// packStats is PackCensus with the walk already done, so that a caller needing
// both the marks and the stats pays for one walk rather than two.
//
// live may be nil, which is a caller saying it wants history and has no use for
// the head column: nothing is then reported as ReachedHead, HeadBytes stays
// zero, and no second walk was taken to fill it.
func (s *Store) packStats(live, all *marks) ([]objstore.PackStat, error) {
	var out []objstore.PackStat
	for _, st := range s.stores() {
		_, isChunk := s.storeFor(st.ObjType)
		stats, err := st.PackStats(s.storeID, func(objID string) objstore.Reach {
			switch {
			case live.has(objID, isChunk):
				return objstore.ReachedHead
			case all.has(objID, isChunk):
				return objstore.ReachedHistory
			default:
				return objstore.Unreached
			}
		})
		if err != nil {
			return nil, fmt.Errorf("measuring packs: %w", err)
		}
		out = append(out, stats...)
	}
	return out, nil
}
