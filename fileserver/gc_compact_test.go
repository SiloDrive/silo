package silod

import (
	"os"
	"testing"
	"time"

	"github.com/SiloDrive/silo/fileserver/objstore"
	"github.com/SiloDrive/silo/internal/format"
	storefmt "github.com/SiloDrive/silo/store"
)

// packedHistoryFixture is historyFixture with its packs sealed, which is the
// only shape compaction has anything to say about.
//
// Sealing is what makes the frames measurable: an open pack is deliberately not
// reported, because its dead fraction describes a file that is a different size
// by the time anything acts on it. It is what a clean shutdown does, and gc is
// an offline operation, so it is the state gc runs against.
func packedHistoryFixture(t *testing.T, ages ...time.Duration) (string, []string) {
	t.Helper()
	t.Cleanup(func() { _ = objstore.Close() })

	libraryID, ids := historyFixture(t, ages...)
	if err := objstore.Close(); err != nil {
		t.Fatalf("sealing: %v", err)
	}
	return libraryID, ids
}

// deadFramesWindow is the retention window these tests expire at: two of the
// fixture's three commits fall outside it, so the packs end up holding frames
// nothing reaches.
const deadFramesWindow = 14 * 24 * time.Hour

// deadFramesFixture is packedHistoryFixture with the two passes that run ahead
// of compaction already done -- history expired, orphans swept.
//
// Both of them defer on a packed store, because neither can delete out of a
// sealed pack, so what they leave behind is the only state compaction has
// anything to say about: sealed packs full of dead frames. Every test below
// wants that state and none of them is about how it was reached.
func deadFramesFixture(t *testing.T) (libraryID string, ids []string) {
	t.Helper()
	libraryID, ids = packedHistoryFixture(t, 40*24*time.Hour, 30*24*time.Hour, 1*24*time.Hour)
	if _, err := expireHistory(libraryID, deadFramesWindow, true); err != nil {
		t.Fatal(err)
	}
	if _, err := sweepOrphans(libraryID, 0, true); err != nil {
		t.Fatal(err)
	}
	return libraryID, ids
}

// packFiles is the entries of a library's pack directory in one store, or none
// when the store holds nothing for it.
//
// The layout is objstore's, and reaching past the seam is the point: the tests
// that use this are asking what is actually on the disk, which is the one
// question an objstore answer cannot settle. Written once so that the path and
// the "no directory yet" case are spelled in a single place.
func packFiles(t *testing.T, objType, libraryID string) []os.DirEntry {
	t.Helper()
	entries, err := os.ReadDir(objstore.PackDir(absDataDir, objType, libraryID))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return entries
}

// diskUsage is what the two object stores hold for a library, framing, footers
// and all -- the number a rewrite has to move for the reclaim to be real.
func diskUsage(t *testing.T, libraryID string) int64 {
	t.Helper()
	var total int64
	for _, st := range stores() {
		_, bytes, err := st.LibraryUsage(libraryID)
		if err != nil {
			t.Fatalf("measuring %s in the %s store: %v", libraryID, st.ObjType, err)
		}
		total += bytes
	}
	return total
}

// Dry run by default, like every other reclaimer here. A report that quietly
// rewrote a pack would be the one command in this file that acts without being
// told to.
func TestCompactionWithoutDeleteChangesNothing(t *testing.T) {
	sqliteTestDB(t)
	libraryID, _ := deadFramesFixture(t)

	before := diskUsage(t, libraryID)
	got, err := compactLibrary(libraryID, compactOpts{threshold: 0.01})
	if err != nil {
		t.Fatalf("compactLibrary: %v", err)
	}
	if got.planned == 0 {
		t.Fatal("nothing was planned, so this test asserts nothing about a dry run")
	}
	if got.compacted != 0 {
		t.Errorf("a dry run rewrote %d packs", got.compacted)
	}
	if after := diskUsage(t, libraryID); after != before {
		t.Errorf("the store held %s before the dry run and %s after",
			format.Bytes(before), format.Bytes(after))
	}
}

// The check is that space comes back, not that the code runs.
func TestCompactionFreesWhatItReported(t *testing.T) {
	sqliteTestDB(t)
	libraryID, _ := deadFramesFixture(t)

	before := diskUsage(t, libraryID)
	got, err := compactLibrary(libraryID, compactOpts{threshold: 0.01, del: true})
	if err != nil {
		t.Fatalf("compactLibrary: %v", err)
	}
	if got.compacted == 0 {
		t.Fatal("nothing was compacted; expiry and the sweep should have left dead frames behind")
	}
	if got.reclaimed <= 0 {
		t.Fatalf("reported %s reclaimed", format.Bytes(got.reclaimed))
	}

	after := diskUsage(t, libraryID)
	if want := before - got.reclaimed; after != want {
		t.Errorf("the store held %s, reported %s reclaimed, and now holds %s — want %s",
			format.Bytes(before), format.Bytes(got.reclaimed), format.Bytes(after), format.Bytes(want))
	}
}

// The head survives a compaction, which is the thing that would be worst to get
// wrong and cheapest to check.
func TestCompactionLeavesTheLibraryReadable(t *testing.T) {
	sqliteTestDB(t)
	libraryID, ids := deadFramesFixture(t)
	if _, err := compactLibrary(libraryID, compactOpts{threshold: 0.01, del: true}); err != nil {
		t.Fatal(err)
	}

	_, st, head, err := openLibraryAtHead(libraryID)
	if err != nil {
		t.Fatalf("the library does not open after compaction: %v", err)
	}
	c, err := st.Census(head)
	if err != nil {
		t.Fatalf("census after compaction: %v", err)
	}
	if c.Head.Bytes == 0 {
		t.Error("the head is empty after compaction")
	}
	// Through the store rather than by looking for a file: the commit is
	// inside a pack, where objectOnDisk would never find it.
	headID, err := storefmt.ParseID(ids[len(ids)-1])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetCommitPublic(headID); err != nil {
		t.Errorf("the head commit is not readable after compaction: %v", err)
	}
}

// A pack sealed inside the age guard is left alone and reported as such, for
// the same reason the orphan sweep has one: a frame nothing reaches and a frame
// a client is about to commit look identical.
func TestCompactionSkipsPacksSealedTooRecently(t *testing.T) {
	sqliteTestDB(t)
	libraryID, _ := deadFramesFixture(t)

	before := diskUsage(t, libraryID)
	got, err := compactLibrary(libraryID, compactOpts{threshold: 0.01, minAge: time.Hour, del: true})
	if err != nil {
		t.Fatalf("compactLibrary: %v", err)
	}
	if got.tooYoung == 0 {
		t.Error("no pack was reported too young, but every one of them was sealed a moment ago")
	}
	if got.compacted != 0 {
		t.Errorf("%d packs were rewritten inside the age guard", got.compacted)
	}
	if after := diskUsage(t, libraryID); after != before {
		t.Errorf("the store held %s and now holds %s, with the guard refusing every pack",
			format.Bytes(before), format.Bytes(after))
	}
}

// The third mechanism's idempotence. A sibling of TestSweepAndExpiryAreIdempotent
// rather than an extension of it, because that one runs on a loose store where
// there is no pack to compact and the assertion would be vacuous.
func TestExpirySweepAndCompactionAreIdempotent(t *testing.T) {
	sqliteTestDB(t)
	libraryID, _ := deadFramesFixture(t)

	// The same window the expiry pass was given. On a packed store expiry
	// could not delete a single commit -- they are inside a sealed pack -- so
	// the cut is carried here, and this run is the one that applies it.
	opt := compactOpts{threshold: 0.01, expire: true, expireWindow: deadFramesWindow, del: true}
	first, err := compactLibrary(libraryID, opt)
	if err != nil {
		t.Fatal(err)
	}
	if first.compacted == 0 {
		t.Fatal("the first run compacted nothing, so the second proves nothing")
	}
	second, err := compactLibrary(libraryID, opt)
	if err != nil {
		t.Fatalf("second compaction: %v", err)
	}
	if second.compacted != 0 {
		t.Errorf("the second run rewrote %d more packs, want 0", second.compacted)
	}
	if second.reclaimed != 0 {
		t.Errorf("the second run reclaimed %s, want nothing", format.Bytes(second.reclaimed))
	}

	_, st, head, err := openLibraryAtHead(libraryID)
	if err != nil {
		t.Fatal(err)
	}
	c, err := st.Census(head)
	if err != nil {
		t.Fatal(err)
	}
	if c.Head.Bytes == 0 {
		t.Error("head is empty after two of each")
	}
	if c.History.Bytes != 0 || c.Unreferenced.Bytes != 0 {
		t.Errorf("history=%+v unreferenced=%+v, want both empty", c.History, c.Unreferenced)
	}
}

// A store with nothing sealed yet has nothing to compact, and says so rather
// than failing -- which is also a store written before the cutover, for ever.
func TestCompactingAnUnsealedStoreDoesNothing(t *testing.T) {
	sqliteTestDB(t)
	libraryID, _ := historyFixture(t, 40*24*time.Hour, 1*24*time.Hour)
	t.Cleanup(func() { _ = objstore.Close() })

	got, err := compactLibrary(libraryID, compactOpts{del: true})
	if err != nil {
		t.Fatalf("compacting a store with no sealed packs: %v", err)
	}
	if got.planned != 0 || got.compacted != 0 || got.tooYoung != 0 {
		t.Errorf("%+v with nothing sealed, want nothing", got)
	}
}

// The budget is live bytes copied, and a run that stopped short says so rather
// than reporting as one that finished.
func TestTheBudgetStopsARunShortAndSaysSo(t *testing.T) {
	sqliteTestDB(t)
	libraryID, _ := deadFramesFixture(t)

	// One byte of budget, which no pack holding anything live can fit.
	got, err := compactLibrary(libraryID, compactOpts{threshold: 0.01, budget: 1})
	if err != nil {
		t.Fatalf("compactLibrary: %v", err)
	}
	if got.held == 0 && got.planned == 0 {
		t.Fatal("nothing was eligible, so the budget was never consulted")
	}
	if got.held == 0 {
		t.Errorf("%d packs planned and none held against a budget of one byte", got.planned)
	}
}

// -compact alone reclaims garbage; it does not expire history. The two are
// separate flags because expiring history is irreversible, and an operator who
// typed one should not get the other's consequences.
func TestCompactionWithoutExpiryKeepsHistory(t *testing.T) {
	sqliteTestDB(t)
	libraryID, _ := packedHistoryFixture(t, 40*24*time.Hour, 30*24*time.Hour, 1*24*time.Hour)

	_, st, head, err := openLibraryAtHead(libraryID)
	if err != nil {
		t.Fatal(err)
	}
	before, err := st.Census(head)
	if err != nil {
		t.Fatal(err)
	}
	if before.History.Bytes == 0 {
		t.Fatal("no history, so this asserts nothing")
	}

	if _, err := compactLibrary(libraryID, compactOpts{threshold: 0.0, del: true}); err != nil {
		t.Fatal(err)
	}

	after, err := st.Census(head)
	if err != nil {
		t.Fatal(err)
	}
	if after.History != before.History {
		t.Errorf("history was %+v and is now %+v — a compaction that was not asked to expire anything did",
			before.History, after.History)
	}
}

// The gap this closes: on a packed store the expiry pass cannot delete a single
// commit, because a sealed pack is immutable. Without the cut being carried into
// the mark, every expired history would be copied forward by every rewrite and
// no run would ever free it.
func TestCompactionAppliesTheCutExpiryCouldNotCarryOut(t *testing.T) {
	sqliteTestDB(t)
	libraryID, _ := packedHistoryFixture(t, 40*24*time.Hour, 30*24*time.Hour, 1*24*time.Hour)

	got, err := expireHistory(libraryID, deadFramesWindow, true)
	if err != nil {
		t.Fatal(err)
	}
	if got.expired != 0 || got.deferred == 0 {
		t.Fatalf("expiry reported %d expired and %d deferred; on a packed store every one of them defers",
			got.expired, got.deferred)
	}

	before := diskUsage(t, libraryID)
	if _, err := compactLibrary(libraryID, compactOpts{
		threshold: 0.0, expire: true, expireWindow: deadFramesWindow, del: true,
	}); err != nil {
		t.Fatal(err)
	}
	if after := diskUsage(t, libraryID); after >= before {
		t.Errorf("the store held %s and now holds %s — the deferred expiry was never carried out",
			format.Bytes(before), format.Bytes(after))
	}

	_, st, head, err := openLibraryAtHead(libraryID)
	if err != nil {
		t.Fatal(err)
	}
	c, err := st.Census(head)
	if err != nil {
		t.Fatal(err)
	}
	if c.History.Bytes != 0 {
		t.Errorf("history is %+v after the cut was applied, want nothing", c.History)
	}
	if c.Head.Bytes == 0 {
		t.Error("the head is empty")
	}
}
