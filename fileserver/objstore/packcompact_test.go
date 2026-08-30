package objstore

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// compactable builds a store with one sealed pack holding the given bodies,
// and returns the pack's id.
func compactable(t *testing.T, bodies []string) (*ObjectStore, string, string) {
	t.Helper()
	s, dataDir := writeStore(t)
	for _, body := range bodies {
		if err := s.WriteVerified(libraryID, idOf([]byte(body)), strings.NewReader(body), true); err != nil {
			t.Fatalf("writing %q: %v", body, err)
		}
	}
	if err := s.packs.close(); err != nil {
		t.Fatal(err)
	}
	stats, err := s.PackStats(libraryID, reachAll(ReachedHead))
	if err != nil {
		t.Fatal(err)
	}
	if len(stats) != 1 {
		t.Fatalf("%d sealed packs, want exactly 1 for this test", len(stats))
	}
	return s, dataDir, stats[0].PackID
}

// liveOnly reports true for exactly the bodies named.
func liveOnly(bodies ...string) func(string) bool {
	keep := map[string]bool{}
	for _, b := range bodies {
		keep[idOf([]byte(b))] = true
	}
	return func(id string) bool { return keep[id] }
}

func mustRead(t *testing.T, s *ObjectStore, body string) {
	t.Helper()
	got, err := s.ReadInto(libraryID, idOf([]byte(body)), nil)
	if err != nil {
		t.Fatalf("reading %q after compaction: %v", body, err)
	}
	if string(got) != body {
		t.Errorf("read %q, want %q", got, body)
	}
}

// The assertion the whole step exists for: what is live survives, what is dead
// goes, and the object-addressed API above never notices that the bytes moved.
func TestCompactionKeepsTheLiveAndDropsTheDead(t *testing.T) {
	bodies := []string{"keep me one", "drop me one", "keep me two", "drop me two", "keep me three"}
	s, dataDir, packID := compactable(t, bodies)

	before, err := s.PackStats(libraryID, reachAll(ReachedHead))
	if err != nil {
		t.Fatal(err)
	}

	got, err := s.CompactPack(libraryID, packID, liveOnly(bodies[0], bodies[2], bodies[4]))
	if err != nil {
		t.Fatalf("CompactPack: %v", err)
	}
	if got.Kept != 3 || got.Dropped != 2 {
		t.Errorf("kept %d and dropped %d, want 3 and 2", got.Kept, got.Dropped)
	}
	if got.NewPackID == "" {
		t.Fatal("no new pack: a rewrite with live frames must produce one")
	}
	if got.NewPackID == packID {
		t.Error("the rewrite reused the old pack's id")
	}
	if got.Reclaimed <= 0 {
		t.Errorf("reclaimed %d bytes, want more than none", got.Reclaimed)
	}

	// Everything live still reads, through the same API that served it before.
	for _, body := range []string{bodies[0], bodies[2], bodies[4]} {
		mustRead(t, s, body)
	}
	// And the dead are gone.
	for _, body := range []string{bodies[1], bodies[3]} {
		if _, err := s.ReadInto(libraryID, idOf([]byte(body)), nil); err == nil {
			t.Errorf("%q survived a compaction that dropped it", body)
		}
	}

	// One pack on disk, and it is the new one.
	after, err := s.PackStats(libraryID, reachAll(ReachedHead))
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 1 {
		t.Fatalf("%d packs after compaction, want 1", len(after))
	}
	if after[0].PackID != got.NewPackID {
		t.Errorf("the surviving pack is %s, want the rewrite %s", after[0].PackID, got.NewPackID)
	}
	if after[0].FileBytes >= before[0].FileBytes {
		t.Errorf("the rewrite is %d bytes and the original was %d — it did not shrink",
			after[0].FileBytes, before[0].FileBytes)
	}
	if n := packFileCount(t, dataDir); n != 1 {
		t.Errorf("%d pack files on disk, want 1 — the old one was not deleted", n)
	}
}

// A pack nothing reaches is deleted rather than rewritten into an empty one.
// A library whose history has just been expired produces exactly this.
func TestAWhollyDeadPackIsDroppedRatherThanRewritten(t *testing.T) {
	bodies := []string{"nothing", "reaches", "any of this"}
	s, dataDir, packID := compactable(t, bodies)

	got, err := s.CompactPack(libraryID, packID, liveOnly())
	if err != nil {
		t.Fatalf("CompactPack: %v", err)
	}
	if got.NewPackID != "" {
		t.Errorf("a wholly dead pack was rewritten into %s", got.NewPackID)
	}
	if got.Kept != 0 || got.Dropped != int64(len(bodies)) {
		t.Errorf("kept %d dropped %d, want 0 and %d", got.Kept, got.Dropped, len(bodies))
	}
	if got.Reclaimed <= 0 {
		t.Error("reclaimed nothing from a pack that was entirely dead")
	}
	if n := packFileCount(t, dataDir); n != 0 {
		t.Errorf("%d pack files left, want none", n)
	}
	for _, body := range bodies {
		if _, err := s.ReadInto(libraryID, idOf([]byte(body)), nil); err == nil {
			t.Errorf("%q is still readable after its pack was dropped", body)
		}
	}
}

// A pack with nothing dead in it is left exactly alone. Rewriting would copy
// the whole thing to produce an identical pack under a new name.
func TestAFullyLivePackIsNotRewritten(t *testing.T) {
	bodies := []string{"all", "of", "this", "is", "live"}
	s, dataDir, packID := compactable(t, bodies)
	before := packFileNames(t, dataDir)

	got, err := s.CompactPack(libraryID, packID, liveOnly(bodies...))
	if err != nil {
		t.Fatalf("CompactPack: %v", err)
	}
	if got.NewPackID != "" || got.Dropped != 0 {
		t.Errorf("a fully live pack was rewritten: %+v", got)
	}
	if after := packFileNames(t, dataDir); !equalStrings(before, after) {
		t.Errorf("the files changed: %v then %v", before, after)
	}
	for _, body := range bodies {
		mustRead(t, s, body)
	}
}

// --- crash behaviour ----------------------------------------------------

// An interrupted rewrite leaves debris under a name nothing loads, and the old
// pack untouched. Rolling back is available precisely because of that.
func TestAnInterruptedRewriteLeavesTheOldPackAuthoritative(t *testing.T) {
	bodies := []string{"live one", "dead one", "live two"}
	s, dataDir, packID := compactable(t, bodies)

	// The crash: a temporary pack part-written, exactly as a rewrite that
	// stopped before its rename would leave.
	dir := packDir(TypeDir(dataDir, TypeChunks), libraryID)
	tmp, err := createTmpPack(TypeDir(dataDir, TypeChunks), libraryID)
	if err != nil {
		t.Fatal(err)
	}
	_, frame := framed(t, s.key, bodies[0])
	if _, err := tmp.append(frame, idOf([]byte(bodies[0])), true); err != nil {
		t.Fatal(err)
	}
	if err := tmp.close(); err != nil {
		t.Fatal(err)
	}
	if n := tmpFileCount(t, dir); n == 0 {
		t.Fatal("the simulated crash left no debris, so this test asserts nothing")
	}

	// A fresh store, as a restart would build. The debris must not be loaded
	// as a pack, and above all must not be seen as a second open pack.
	forgetPackStore(TypeDir(dataDir, TypeChunks))
	restarted := New(confPath, dataDir, TypeChunks)
	t.Cleanup(func() { _ = restarted.packs.close() })
	stats, err := restarted.PackStats(libraryID, reachAll(ReachedHead))
	if err != nil {
		t.Fatalf("a store with rewrite debris did not load: %v", err)
	}
	if len(stats) != 1 || stats[0].PackID != packID {
		t.Fatalf("loaded %+v, want only the original pack %s", stats, packID)
	}
	for _, body := range bodies {
		mustRead(t, restarted, body)
	}

	// And the cleanup throws it away rather than trying to finish it.
	if _, err := restarted.PruneRedundantPacks(libraryID); err != nil {
		t.Fatalf("PruneRedundantPacks: %v", err)
	}
	if n := tmpFileCount(t, dir); n != 0 {
		t.Errorf("%d temporary files survived the prune", n)
	}
	for _, body := range bodies {
		mustRead(t, restarted, body)
	}
}

// The post-rename window: both packs exist and hold the same frames. Nothing
// is lost, and the rule that closes it needs no journal — a pack every one of
// whose ids lives elsewhere is redundant.
func TestARedundantPackIsPrunedAndItsFramesStayReadable(t *testing.T) {
	bodies := []string{"in both packs", "also in both", "only in the first"}
	s, dataDir := writeStore(t)
	objDir := TypeDir(dataDir, TypeChunks)

	// Pack one: everything.
	for _, body := range bodies {
		if err := s.WriteVerified(libraryID, idOf([]byte(body)), strings.NewReader(body), true); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.packs.close(); err != nil {
		t.Fatal(err)
	}
	// Pack two: a subset, built directly, as a completed-but-unswept rewrite
	// would have left it.
	second, err := createPack(objDir, libraryID)
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range bodies[:2] {
		id, frame := framed(t, s.key, body)
		if _, err := second.append(frame, id, true); err != nil {
			t.Fatal(err)
		}
	}
	if err := second.seal(); err != nil {
		t.Fatal(err)
	}

	// A restart, which is what makes the second pack visible: within one
	// process the registry is the authority on which packs exist, and nothing
	// in production builds one behind its back.
	forgetPackStore(objDir)
	restarted := New(confPath, dataDir, TypeChunks)
	t.Cleanup(func() { _ = restarted.packs.close() })
	if n := packFileCount(t, dataDir); n != 2 {
		t.Fatalf("%d packs on disk, want 2", n)
	}

	// The subset is not redundant — it holds nothing the other does not, but
	// the other holds something it does not, so neither may go.
	pruned, err := restarted.PruneRedundantPacks(libraryID)
	if err != nil {
		t.Fatal(err)
	}
	if pruned != 1 {
		t.Errorf("pruned %d packs, want the 1 whose ids are all held elsewhere", pruned)
	}
	// Whichever went, every object is still readable. That is the property
	// that matters, and it is stronger than naming which pack survived.
	for _, body := range bodies {
		mustRead(t, restarted, body)
	}
}

// Two packs holding exactly the same ids: one goes, one stays. Judging both
// redundant would delete every copy, which is the failure this rule has to not
// have.
func TestTwoIdenticalPacksLeaveOneStanding(t *testing.T) {
	bodies := []string{"duplicated one", "duplicated two"}
	s, dataDir := writeStore(t)
	objDir := TypeDir(dataDir, TypeChunks)

	for round := 0; round < 2; round++ {
		p, err := createPack(objDir, libraryID)
		if err != nil {
			t.Fatal(err)
		}
		for _, body := range bodies {
			id, frame := framed(t, s.key, body)
			if _, err := p.append(frame, id, true); err != nil {
				t.Fatal(err)
			}
		}
		if err := p.seal(); err != nil {
			t.Fatal(err)
		}
	}

	forgetPackStore(objDir)
	restarted := New(confPath, dataDir, TypeChunks)
	t.Cleanup(func() { _ = restarted.packs.close() })
	pruned, err := restarted.PruneRedundantPacks(libraryID)
	if err != nil {
		t.Fatal(err)
	}
	if pruned != 1 {
		t.Errorf("pruned %d of 2 identical packs, want exactly 1", pruned)
	}
	if n := packFileCount(t, dataDir); n != 1 {
		t.Errorf("%d packs left, want 1", n)
	}
	for _, body := range bodies {
		mustRead(t, restarted, body)
	}
}

// A store with one pack has nothing redundant in it, however the packs look.
func TestPruningASinglePackStoreDoesNothing(t *testing.T) {
	bodies := []string{"alone"}
	s, dataDir, _ := compactable(t, bodies)
	pruned, err := s.PruneRedundantPacks(libraryID)
	if err != nil {
		t.Fatal(err)
	}
	if pruned != 0 {
		t.Errorf("pruned %d packs from a store holding one", pruned)
	}
	if n := packFileCount(t, dataDir); n != 1 {
		t.Errorf("%d packs left, want 1", n)
	}
	mustRead(t, s, bodies[0])
}

// Compaction copies frames and never opens one, so a store whose key is gone
// can still reclaim its disk — the same reason measuring and deleting do not
// need the key.
func TestCompactionNeedsNoStorageKey(t *testing.T) {
	// Built by hand in a directory that never had a key, because the key is
	// cached per data directory for the life of the process: a store that ever
	// loaded one keeps it, so deleting the file proves nothing. This is also
	// the real scenario — a server starting over a store whose key is missing.
	dataDir := filepath.Join(t.TempDir(), "storage-data")
	objDir := TypeDir(dataDir, TypeChunks)
	key := packKey(t)
	bodies := []string{"live", "dead"}

	p, err := createPack(objDir, libraryID)
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range bodies {
		id, frame := framed(t, key, body)
		if _, err := p.append(frame, id, true); err != nil {
			t.Fatal(err)
		}
	}
	if err := p.seal(); err != nil {
		t.Fatal(err)
	}
	packID := p.id
	forgetPackStore(objDir)

	keyless := New(confPath, dataDir, TypeChunks)
	t.Cleanup(func() { _ = keyless.packs.close() })
	if keyless.keyErr == nil {
		t.Fatal("the store found a key it should not have — this test is not testing what it claims")
	}

	got, err := keyless.CompactPack(libraryID, packID, liveOnly(bodies[0]))
	if err != nil {
		t.Fatalf("compacting without a key: %v", err)
	}
	if got.Kept != 1 || got.Dropped != 1 {
		t.Errorf("kept %d dropped %d, want 1 and 1", got.Kept, got.Dropped)
	}
	// The frame moved intact: it still stats as the object it was, which is
	// answered out of the rewritten pack's index.
	if size, err := keyless.Stat(libraryID, idOf([]byte(bodies[0]))); err != nil || size != int64(len(bodies[0])) {
		t.Errorf("Stat after a keyless compaction = %d (err %v), want %d", size, err, len(bodies[0]))
	}
	if size, err := keyless.Stat(libraryID, idOf([]byte(bodies[1]))); err == nil {
		t.Errorf("the dropped object still stats at %d bytes", size)
	}
}

func TestCompactingAPackThatIsNotThereIsRefused(t *testing.T) {
	s, _, _ := compactable(t, []string{"something"})
	if _, err := s.CompactPack(libraryID, strings.Repeat("ab", 32), liveOnly()); err == nil {
		t.Error("compacting a pack the library does not hold was accepted")
	}
}

// --- helpers ------------------------------------------------------------

func packFileNames(t *testing.T, dataDir string) []string {
	t.Helper()
	dir := packDir(TypeDir(dataDir, TypeChunks), libraryID)
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".pack" {
			out = append(out, e.Name())
		}
	}
	return out
}

func packFileCount(t *testing.T, dataDir string) int {
	t.Helper()
	return len(packFileNames(t, dataDir))
}

func tmpFileCount(t *testing.T, dir string) int {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range entries {
		if filepath.Ext(e.Name()) == packTmpSuffix {
			n++
		}
	}
	return n
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
