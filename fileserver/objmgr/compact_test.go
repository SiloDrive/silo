package objmgr

import (
	"bytes"
	"testing"
	"time"

	"github.com/dkam/silo/fileserver/objstore"
	"github.com/dkam/silo/store"
)

// stat builds a PackStat with a given dead fraction, for the scheduling tests.
//
// The selection rules are arithmetic over numbers a walk produced, and building
// a real pack with a dead fraction of exactly 0.4 means arranging for the
// chunker to emit frames of chosen sizes — which tests the chunker, not the
// scheduler. The walk's own numbers are asserted against real packs by
// TestCompactionKeepsHistoryTheHeadDoesNotReach below.
func stat(id string, frames, live int64, sealed time.Time) objstore.PackStat {
	return objstore.PackStat{
		PackID: id, FrameBytes: frames, LiveBytes: live,
		FileBytes: frames, SealedAt: sealed,
	}
}

func packIDs(stats []objstore.PackStat) []string {
	out := make([]string, len(stats))
	for i, p := range stats {
		out[i] = p.PackID
	}
	return out
}

func TestAPackUnderTheThresholdIsNotACandidate(t *testing.T) {
	old := time.Now().Add(-time.Hour)
	cutoff := time.Now()

	forty := stat("forty", 1000, 600, old) // 0.4 dead
	sixty := stat("sixty", 1000, 400, old) // 0.6 dead
	stats := []objstore.PackStat{forty, sixty}

	plan := selectForCompaction(stats, 0.5, cutoff, 0)
	if got := packIDs(plan.Candidates); len(got) != 1 || got[0] != "sixty" {
		t.Errorf("at threshold 0.5 the candidates are %v, want just the 0.6-dead pack", got)
	}

	plan = selectForCompaction(stats, 0.3, cutoff, 0)
	if got := packIDs(plan.Candidates); len(got) != 2 {
		t.Errorf("at threshold 0.3 the candidates are %v, want both", got)
	}
}

// A pack nothing reaches is deleted rather than rewritten, so it copies no
// bytes. It goes first because it is the cheapest reclaim there is, and a
// library whose history has just been expired is mostly this case.
func TestAWhollyDeadPackGoesFirstAndCostsTheBudgetNothing(t *testing.T) {
	old := time.Now().Add(-time.Hour)
	stats := []objstore.PackStat{
		stat("half", 1000, 500, old),
		stat("dead", 1000, 0, old),
	}

	// A budget far too small to copy the half-dead pack's live bytes.
	plan := selectForCompaction(stats, 0.5, time.Now(), 100)
	got := packIDs(plan.Candidates)
	if len(got) == 0 || got[0] != "dead" {
		t.Fatalf("candidates are %v, want the wholly dead pack first", got)
	}
	if len(got) != 1 {
		t.Errorf("candidates are %v; the half-dead pack costs 500 live bytes against a budget of 100", got)
	}
	if len(plan.Held) != 1 || plan.Held[0].PackID != "half" {
		t.Errorf("held is %v, want the pack the budget could not fit", packIDs(plan.Held))
	}
}

// Among packs that must be copied, the order is dead fraction descending:
// reclaimed per byte copied, which is what a budget measured in bytes copied
// should be spent by.
func TestCandidatesAreOrderedByWhatTheyReclaimPerByteCopied(t *testing.T) {
	old := time.Now().Add(-time.Hour)
	stats := []objstore.PackStat{
		stat("sixty", 1000, 400, old),
		stat("ninety", 1000, 100, old),
		stat("seventy", 1000, 300, old),
	}
	plan := selectForCompaction(stats, 0.5, time.Now(), 0)
	want := []string{"ninety", "seventy", "sixty"}
	got := packIDs(plan.Candidates)
	for i := range want {
		if i >= len(got) || got[i] != want[i] {
			t.Fatalf("order is %v, want %v", got, want)
		}
	}
}

// A run that stopped short says so, rather than reporting as one that finished.
func TestTheBudgetHoldsWhatItCannotFit(t *testing.T) {
	old := time.Now().Add(-time.Hour)
	stats := []objstore.PackStat{
		stat("first", 1000, 400, old),
		stat("second", 1000, 400, old),
	}
	plan := selectForCompaction(stats, 0.5, time.Now(), 500)
	if len(plan.Candidates) != 1 {
		t.Errorf("%d candidates against a budget that fits one, want 1", len(plan.Candidates))
	}
	if len(plan.Held) != 1 {
		t.Errorf("%d held, want the one the budget could not fit", len(plan.Held))
	}
	if plan.Candidates[0].PackID == plan.Held[0].PackID {
		t.Error("the same pack is both a candidate and held")
	}
}

// The age guard, and it is the orphan sweep's for the same reason: a frame
// nothing reaches is indistinguishable from one a client is about to commit.
func TestAPackSealedTooRecentlyIsTooYoungRatherThanACandidate(t *testing.T) {
	stats := []objstore.PackStat{
		stat("fresh", 1000, 100, time.Now()),
		stat("stale", 1000, 100, time.Now().Add(-time.Hour)),
	}
	plan := selectForCompaction(stats, 0.5, time.Now().Add(-time.Minute), 0)
	if plan.TooYoung != 1 {
		t.Errorf("TooYoung is %d, want the one pack sealed inside the guard", plan.TooYoung)
	}
	if got := packIDs(plan.Candidates); len(got) != 1 || got[0] != "stale" {
		t.Errorf("candidates are %v, want only the pack old enough to touch", got)
	}
}

// The whole point of the mark being history rather than head: a rewrite that
// kept only what the head reaches would expire history without a retention
// policy having said so, and nothing else in the system would report it.
//
// This is the test that catches a head-only walk.
func TestCompactionKeepsHistoryTheHeadDoesNotReach(t *testing.T) {
	s := plainStore(t)

	root := put(t, s, mustEmpty(t, s), "/a.bin", bytes.Repeat([]byte("a"), 200000))
	first := commitOn(t, s, root)
	root = put(t, s, root, "/a.bin", bytes.Repeat([]byte("b"), 200000))
	head := commitOn(t, s, root, first)

	// Genuinely dead bytes for the rewrite to drop: chunks written and never
	// named by any manifest, which is what an upload that stopped halfway
	// leaves behind.
	if _, err := s.WriteFile(bytes.NewReader(bytes.Repeat([]byte("z"), 200000))); err != nil {
		t.Fatal(err)
	}
	seal(t)

	before := mustCensus(t, s, head)
	if before.History.Bytes == 0 {
		t.Fatal("no history, so a head-only walk would look identical to this one")
	}
	if before.Unreferenced.Bytes == 0 {
		t.Fatal("nothing dead, so there is nothing for a rewrite to drop")
	}

	plan, err := s.PlanCompaction(head, nil, 0.05, time.Now().Add(time.Hour), 0)
	if err != nil {
		t.Fatalf("PlanCompaction: %v", err)
	}
	if len(plan.Candidates) == 0 {
		t.Fatal("no pack is a candidate, so nothing is compacted and this asserts nothing")
	}
	for _, p := range plan.Candidates {
		if _, err := s.CompactPack(p, plan); err != nil {
			t.Fatalf("CompactPack(%s): %v", p.PackID, err)
		}
	}

	after := mustCensus(t, s, head)
	if after.Head != before.Head {
		t.Errorf("head was %+v and is now %+v — compaction dropped live data", before.Head, after.Head)
	}
	if after.History != before.History {
		t.Errorf("history was %+v and is now %+v — compaction expired history no policy asked to expire",
			before.History, after.History)
	}
	if after.Unreferenced.Bytes >= before.Unreferenced.Bytes {
		t.Errorf("unreferenced was %+v and is now %+v — nothing was reclaimed",
			before.Unreferenced, after.Unreferenced)
	}

	// And the superseded content is still readable, which is what "history" has
	// to mean for it to be worth keeping.
	if _, err := s.GetCommitPublic(first); err != nil {
		t.Errorf("the parent commit is gone after compaction: %v", err)
	}
}

// A dry run is only useful if it can be produced without acting, so the plan
// and the rewrite are two calls. The marks handed back are the ones the plan
// was drawn from, which is what makes them one walk rather than two.
func TestPlanningCompactsNothing(t *testing.T) {
	s := plainStore(t)
	root := put(t, s, mustEmpty(t, s), "/a.bin", bytes.Repeat([]byte("a"), 200000))
	head := commitOn(t, s, root)
	if _, err := s.WriteFile(bytes.NewReader(bytes.Repeat([]byte("z"), 200000))); err != nil {
		t.Fatal(err)
	}
	seal(t)

	before := mustCensus(t, s, head)
	if _, err := s.PlanCompaction(head, nil, 0.0, time.Now().Add(time.Hour), 0); err != nil {
		t.Fatalf("PlanCompaction: %v", err)
	}
	if after := mustCensus(t, s, head); after != before {
		t.Errorf("the census was %+v before planning and %+v after — planning removed something", before, after)
	}
}

// A store with nothing sealed has no packs to plan against, and that is an
// empty plan rather than an error -- which is also what a store written before
// the cutover reports, for ever.
func TestPlanningAnUnsealedStoreFindsNothing(t *testing.T) {
	s := plainStore(t)
	root := put(t, s, mustEmpty(t, s), "/a.bin", bytes.Repeat([]byte("a"), 200000))
	head := commitOn(t, s, root)

	plan, err := s.PlanCompaction(head, nil, 0.0, time.Now(), 0)
	if err != nil {
		t.Fatalf("PlanCompaction on a loose store: %v", err)
	}
	if len(plan.Candidates) != 0 || len(plan.Held) != 0 || plan.TooYoung != 0 {
		t.Errorf("plan is %+v, want nothing at all", plan)
	}
}

// PruneRedundantPacks is the crash cleanup, and it has to cover both stores:
// an interrupted rewrite of an object pack leaves exactly the same debris as
// one of a chunk pack.
func TestPruneRunsOverBothStores(t *testing.T) {
	s := plainStore(t)
	root := put(t, s, mustEmpty(t, s), "/a.bin", bytes.Repeat([]byte("a"), 200000))
	head := commitOn(t, s, root)
	seal(t)

	if _, err := s.PruneRedundantPacks(); err != nil {
		t.Fatalf("PruneRedundantPacks: %v", err)
	}
	// Nothing was duplicated, so nothing may be removed: the rule deletes a
	// pack only when every one of its ids lives in another.
	if c := mustCensus(t, s, head); c.Head.Objects == 0 {
		t.Error("the head is empty after a prune that had nothing to prune")
	}
	if _, err := s.GetCommitPublic(head); err != nil {
		t.Errorf("the head commit is gone after a prune: %v", err)
	}
}

// Retention as a decision the disk has not yet carried out. A commit inside a
// sealed pack cannot be deleted where it is, so on a packed store the expiry
// pass leaves it there; naming it here is the only way its content is ever
// released.
func TestAnExpiredCommitIsNotMarked(t *testing.T) {
	s := plainStore(t)

	root := put(t, s, mustEmpty(t, s), "/a.bin", bytes.Repeat([]byte("a"), 200000))
	first := commitOn(t, s, root)
	root = put(t, s, root, "/a.bin", bytes.Repeat([]byte("b"), 200000))
	head := commitOn(t, s, root, first)
	seal(t)

	before := mustCensus(t, s, head)
	if before.History.Bytes == 0 {
		t.Fatal("no history to expire")
	}

	plan, err := s.PlanCompaction(head, []store.ID{first}, 0.0, time.Now().Add(time.Hour), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Candidates) == 0 {
		t.Fatal("nothing to compact, so the cut is never applied")
	}
	for _, p := range plan.Candidates {
		if _, err := s.CompactPack(p, plan); err != nil {
			t.Fatalf("CompactPack(%s): %v", p.PackID, err)
		}
	}

	after := mustCensus(t, s, head)
	if after.History.Bytes != 0 {
		t.Errorf("history is %+v after the expired commit was cut, want nothing", after.History)
	}
	if after.Head != before.Head {
		t.Errorf("head was %+v and is now %+v — the cut took live data with it", before.Head, after.Head)
	}
	if _, err := s.GetCommitPublic(first); err == nil {
		t.Error("the expired commit is still readable, so the rewrite kept it")
	}
}
