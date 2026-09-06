package objstore

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// writeStore is a store in its own data directory, sealed and shut down by the
// test's cleanup. Packs are the write path, so there is nothing to turn on.
func writeStore(t *testing.T) (*ObjectStore, string) {
	t.Helper()
	dataDir := filepath.Join(t.TempDir(), "storage-data")
	s := New(confPath, dataDir, TypeChunks)
	if err := s.ready(); err != nil {
		t.Fatalf("opening a store: %v", err)
	}
	t.Cleanup(func() {
		if err := s.packs.close(); err != nil {
			t.Errorf("closing the pack store: %v", err)
		}
	})
	return s, dataDir
}

// packFiles counts what is on disk for a library: how many sealed packs, and
// whether one is open.
func packFiles(t *testing.T, dataDir, objType string) (sealed int, open int) {
	t.Helper()
	entries, err := osReadDirNames(packDir(TypeDir(dataDir, objType), libraryID))
	if err != nil {
		t.Fatal(err)
	}
	sidecars := map[string]bool{}
	for _, name := range entries {
		if filepath.Ext(name) == ".idx" {
			sidecars[strings.TrimSuffix(name, ".idx")] = true
		}
	}
	for _, name := range entries {
		if filepath.Ext(name) != ".pack" {
			continue
		}
		if sidecars[strings.TrimSuffix(name, ".pack")] {
			open++
		} else {
			sealed++
		}
	}
	return sealed, open
}

// waitForPacks polls the chunk store's pack directory until it holds the
// counts wanted, or fails with why after a few seconds.
func waitForPacks(t *testing.T, dataDir string, wantSealed, wantOpen int, why string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		sealed, open := packFiles(t, dataDir, TypeChunks)
		if sealed == wantSealed && open == wantOpen {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d sealed and %d open, want %d and %d: %s", sealed, open, wantSealed, wantOpen, why)
		}
		time.Sleep(packSweep)
	}
}

// osReadDirNames lists a directory, treating a missing one as empty: a library
// that has never been packed and one whose packs were all removed are the same
// answer here.
func osReadDirNames(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names, nil
}

// The cutover: with the flag on, a write goes into a pack and comes back out
// through the same object-addressed API that served it loose.
func TestWritesGoIntoAPackAndReadBack(t *testing.T) {
	s, dataDir := writeStore(t)

	bodies := []string{"first", "second, a little longer", "third"}
	var ids []string
	for _, body := range bodies {
		id := idOf([]byte(body))
		ids = append(ids, id)
		if err := s.WriteVerified(libraryID, id, strings.NewReader(body), true); err != nil {
			t.Fatalf("write %q: %v", body, err)
		}
	}

	// Nothing loose: the fan-out is untouched, which is what "packs are the
	// write path" has to mean.
	looseDir := LibraryDir(dataDir, TypeChunks, libraryID)
	names, err := osReadDirNames(looseDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range names {
		if len(n) == 2 {
			t.Errorf("a fan-out directory %q exists, so something was written loose", n)
		}
	}

	_, open := packFiles(t, dataDir, TypeChunks)
	if open != 1 {
		t.Fatalf("%d open packs after three writes, want 1", open)
	}

	for i, id := range ids {
		got, err := s.ReadInto(libraryID, id, nil)
		if err != nil {
			t.Fatalf("reading %s back out of the pack: %v", id, err)
		}
		if string(got) != bodies[i] {
			t.Errorf("read %q, want %q", got, bodies[i])
		}
		size, err := s.Stat(libraryID, id)
		if err != nil || size != int64(len(bodies[i])) {
			t.Errorf("Stat %s = %d (err %v), want %d", id, size, err, len(bodies[i]))
		}
	}
}

// A verified write still refuses content that does not hash to its id, and
// refuses it before anything reaches the pack — otherwise a bad upload would
// leave a frame in an append-only file that nothing can remove.
func TestAVerifiedWriteRefusesBadContentBeforeItReachesThePack(t *testing.T) {
	s, dataDir := writeStore(t)

	err := s.WriteVerified(libraryID, idOf([]byte("what was promised")), strings.NewReader("what arrived"), true)
	if err == nil {
		t.Fatal("a write whose content does not match its id was accepted")
	}
	if _, open := packFiles(t, dataDir, TypeChunks); open != 0 {
		t.Errorf("%d packs were opened for a write that was refused", open)
	}
}

// Seal on size. packTarget is shrunk rather than half a gigabyte being
// written.
func TestAPackSealsWhenItFills(t *testing.T) {
	s, dataDir := writeStore(t)

	wasTarget := packTarget
	packTarget = int64(frameOverhead)*3 + 60
	t.Cleanup(func() { packTarget = wasTarget })

	var ids []string
	for i := 0; i < 9; i++ {
		body := fmt.Sprintf("body number %d", i)
		id := idOf([]byte(body))
		ids = append(ids, id)
		if err := s.WriteVerified(libraryID, id, strings.NewReader(body), true); err != nil {
			t.Fatal(err)
		}
	}

	sealed, _ := packFiles(t, dataDir, TypeChunks)
	if sealed < 2 {
		t.Errorf("%d sealed packs after nine writes past the target, want at least 2", sealed)
	}
	// Everything is still readable, across the seal boundaries.
	for i, id := range ids {
		got, err := s.ReadInto(libraryID, id, nil)
		if err != nil {
			t.Fatalf("reading %s after its pack sealed: %v", id, err)
		}
		if want := fmt.Sprintf("body number %d", i); string(got) != want {
			t.Errorf("read %q, want %q", got, want)
		}
	}
}

// Seal on age: the rule that bounds the single-copy window in time rather than
// leaving it to depend on future traffic arriving.
func TestAPackSealsOnAgeWithNoFurtherWrites(t *testing.T) {
	// Set before the store exists: a packStore copies these at creation, so
	// the sweeper reads its own fields rather than these variables.
	wasAge, wasSweep := packMaxAge, packSweep
	packMaxAge = 20 * time.Millisecond
	packSweep = 5 * time.Millisecond
	t.Cleanup(func() { packMaxAge, packSweep = wasAge, wasSweep })

	s, dataDir := writeStore(t)

	body := "the last chunk of a quiet day"
	id := idOf([]byte(body))
	if err := s.WriteVerified(libraryID, id, strings.NewReader(body), true); err != nil {
		t.Fatal(err)
	}
	if _, open := packFiles(t, dataDir, TypeChunks); open != 1 {
		t.Fatalf("the write did not open a pack")
	}

	waitForPacks(t, dataDir, 1, 0, "the pack was not sealed on age; "+
		"without this rule the last chunks of a quiet day sit on one disk indefinitely")

	got, err := s.ReadInto(libraryID, id, nil)
	if err != nil {
		t.Fatalf("reading after the age seal: %v", err)
	}
	if string(got) != body {
		t.Errorf("read %q, want %q", got, body)
	}
}

// Seal at shutdown, so a clean stop leaves nothing half-open.
func TestShutdownSealsTheOpenPack(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "storage-data")
	s := New(confPath, dataDir, TypeChunks)

	body := "written just before the server stopped"
	id := idOf([]byte(body))
	if err := s.WriteVerified(libraryID, id, strings.NewReader(body), true); err != nil {
		t.Fatal(err)
	}
	if err := s.packs.close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	sealed, open := packFiles(t, dataDir, TypeChunks)
	if sealed != 1 || open != 0 {
		t.Errorf("after shutdown: %d sealed, %d open, want 1 and 0", sealed, open)
	}
}

// An open pack that never held a frame is removed rather than sealed. Sealing
// it would leave a footer and a filter that every later lookup asks and every
// listing walks, permanently, for a pack that held nothing.
func TestShutdownDiscardsAnEmptyPack(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "storage-data")
	s := New(confPath, dataDir, TypeChunks)

	set, err := s.packs.set(libraryID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := set.writablePack(); err != nil {
		t.Fatal(err)
	}
	if _, open := packFiles(t, dataDir, TypeChunks); open != 1 {
		t.Fatal("no pack was opened")
	}

	if err := s.packs.close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	sealed, open := packFiles(t, dataDir, TypeChunks)
	if sealed != 0 || open != 0 {
		t.Errorf("an empty pack left %d sealed and %d open behind, want none", sealed, open)
	}
}

// A pack found open at startup is not appended to. Its frames have been
// outside any sealed pack for at least as long as the process was down, which
// is the window the age rule exists to bound — so it is sealed rather than
// kept open until it happens to fill.
func TestAPackInheritedFromAPreviousRunIsSealedRatherThanReused(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "storage-data")
	objDir := TypeDir(dataDir, TypeChunks)

	first := New(confPath, dataDir, TypeChunks)
	oldBody := "written by the previous run"
	oldID := idOf([]byte(oldBody))
	if err := first.WriteVerified(libraryID, oldID, strings.NewReader(oldBody), true); err != nil {
		t.Fatal(err)
	}
	// Stop without sealing: a kill rather than a clean shutdown. Drop the
	// process-wide registry so the next store rediscovers from disk.
	set, err := first.packs.set(libraryID)
	if err != nil {
		t.Fatal(err)
	}
	if err := set.open.close(); err != nil {
		t.Fatal(err)
	}
	forgetPackStore(objDir)

	if _, open := packFiles(t, dataDir, TypeChunks); open != 1 {
		t.Fatal("the killed run left no open pack")
	}

	second := New(confPath, dataDir, TypeChunks)
	t.Cleanup(func() { _ = second.packs.close() })
	newBody := "written by the run that came after"
	newID := idOf([]byte(newBody))
	if err := second.WriteVerified(libraryID, newID, strings.NewReader(newBody), true); err != nil {
		t.Fatal(err)
	}

	sealed, open := packFiles(t, dataDir, TypeChunks)
	if sealed != 1 || open != 1 {
		t.Errorf("%d sealed and %d open, want the inherited pack sealed and a fresh one open", sealed, open)
	}
	for id, want := range map[string]string{oldID: oldBody, newID: newBody} {
		got, err := second.ReadInto(libraryID, id, nil)
		if err != nil {
			t.Fatalf("reading %s across the restart: %v", id, err)
		}
		if string(got) != want {
			t.Errorf("read %q, want %q", got, want)
		}
	}
}

// A store written before the cutover keeps reading, which is what makes the flip
// a change to the write path alone. The lookup asks the open pack, then the
// sealed packs, then the loose file — so an existing install carries its old
// objects forward unpacked and writes its new ones packed, which is what
// packs.md § 5 settled on instead of an ingest.
func TestALooseObjectStillReadsAndNewOnesArePacked(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "storage-data")
	s := New(confPath, dataDir, TypeChunks)
	if err := s.ready(); err != nil {
		t.Fatalf("opening a store: %v", err)
	}
	t.Cleanup(func() { _ = s.packs.close() })

	// Put one object where the pre-cutover write path put it: a frame, in a
	// file of its own, under the two-character fan-out.
	oldBody := "written before packs were the write path"
	oldID := idOf([]byte(oldBody))
	writeLooseObject(t, s, oldID, oldBody)
	loose := filepath.Join(LibraryDir(dataDir, TypeChunks, libraryID), oldID[:2], oldID[2:])
	if _, err := os.Stat(loose); err != nil {
		t.Fatalf("the loose object is not where this test put it: %v", err)
	}

	newBody := "written after"
	newID := idOf([]byte(newBody))
	if err := s.WriteVerified(libraryID, newID, strings.NewReader(newBody), true); err != nil {
		t.Fatal(err)
	}
	if sealed, open := packFiles(t, dataDir, TypeChunks); sealed != 0 || open != 1 {
		t.Errorf("%d sealed and %d open packs, want a single open one for the new object", sealed, open)
	}
	if _, err := os.Stat(loose); err != nil {
		t.Errorf("the loose object was disturbed by a packed write: %v", err)
	}

	for id, want := range map[string]string{oldID: oldBody, newID: newBody} {
		got, err := s.ReadInto(libraryID, id, nil)
		if err != nil {
			t.Fatalf("reading %s: %v", id, err)
		}
		if string(got) != want {
			t.Errorf("read %q, want %q", got, want)
		}
	}
}

// A batch lands as a contiguous run even when another batch is being written
// at the same time. That is the property the whole batched write path exists
// for, and it is only observable under concurrency: written one after the
// other, chunks land contiguously however they are stored.
//
// Interleaved, a file's chunks are scattered through the pack and nothing
// downstream can gather them — only the layer that knows which chunks arrived
// together could, and it is above this one. Compaction preserves the order it
// finds, so the order has to be right here.
//
// The assertion is exact for the batched path: each batch takes the write lock
// once, so there are exactly two runs. Against a per-chunk writer it is
// overwhelmingly likely to fail rather than certain, which is why the batches
// are large.
func TestABatchLandsContiguouslyAgainstAConcurrentWriter(t *testing.T) {
	s, dataDir := writeStore(t)

	const n = 60
	batches := make([][]Object, 2)
	for b := range batches {
		batches[b] = make([]Object, n)
		for i := range batches[b] {
			body := fmt.Sprintf("file %d, chunk %03d", b, i)
			batches[b][i] = Object{ID: idOf([]byte(body)), Data: []byte(body)}
		}
	}

	var wg sync.WaitGroup
	errs := make([]error, len(batches))
	start := make(chan struct{})
	for b := range batches {
		wg.Add(1)
		go func(b int) {
			defer wg.Done()
			<-start
			errs[b] = s.WriteBatch(libraryID, batches[b], true)
		}(b)
	}
	close(start)
	wg.Wait()
	for b, err := range errs {
		if err != nil {
			t.Fatalf("batch %d: %v", b, err)
		}
	}
	if err := s.packs.close(); err != nil {
		t.Fatal(err)
	}

	sealed, err := openSealedPack(TypeDir(dataDir, TypeChunks), libraryID, onlyPackID(t, dataDir))
	if err != nil {
		t.Fatal(err)
	}
	entries := sealed.entries()
	sort.Slice(entries, func(i, j int) bool { return entries[i].Offset < entries[j].Offset })
	if len(entries) != 2*n {
		t.Fatalf("the pack holds %d frames, want %d", len(entries), 2*n)
	}

	owner := map[string]int{}
	for b := range batches {
		for _, o := range batches[b] {
			owner[o.ID] = b
		}
	}

	// Which batch each frame belongs to, in physical order. Two runs, not
	// stripes.
	runs := 1
	for i := 1; i < len(entries); i++ {
		if owner[entries[i].ID] != owner[entries[i-1].ID] {
			runs++
		}
	}
	if runs != 2 {
		t.Errorf("the two batches are broken into %d runs in the pack, want 2 — they interleaved", runs)
	}

	// And each batch kept its own order within its run.
	for b := range batches {
		var got []string
		for _, e := range entries {
			if owner[e.ID] == b {
				got = append(got, e.ID)
			}
		}
		for i, o := range batches[b] {
			if got[i] != o.ID {
				t.Errorf("batch %d frame %d is out of order", b, i)
				break
			}
		}
	}
}

// onlyPackID is the id of the single sealed pack a test expects to exist.
func onlyPackID(t *testing.T, dataDir string) string {
	t.Helper()
	names := packFileNames(t, dataDir)
	if len(names) != 1 {
		t.Fatalf("%d sealed packs, want exactly 1", len(names))
	}
	return strings.TrimSuffix(names[0], ".pack")
}

// A seal must never make an object unfindable, even for the length of an
// fsync. rotate used to take the open pack out of the set before sealing it
// and put the sealed pack in afterwards, and a lookup in between missed every
// object in it: Read said ErrNotFound and Exists said no, for bytes that were
// on the disk and acknowledged. The seam fires inside the seal, between the
// two, which is the only way to land a lookup there on purpose.
func TestNoLookupMissesDuringASeal(t *testing.T) {
	s, _ := writeStore(t)

	var ids []string
	for i := 0; i < 3; i++ {
		body := fmt.Sprintf("sealing around %d", i)
		id := idOf([]byte(body))
		ids = append(ids, id)
		if err := s.WriteVerified(libraryID, id, strings.NewReader(body), true); err != nil {
			t.Fatal(err)
		}
	}

	var misses []string
	packSealHook = func() error {
		for _, id := range ids {
			if _, err := s.ReadInto(libraryID, id, nil); err != nil {
				misses = append(misses, fmt.Sprintf("read %s: %v", id[:12], err))
			}
			if ok, err := s.Exists(libraryID, id); err != nil || !ok {
				misses = append(misses, fmt.Sprintf("exists %s: ok=%v err=%v", id[:12], ok, err))
			}
		}
		return nil
	}
	t.Cleanup(func() { packSealHook = nil })

	set, err := s.packs.set(libraryID)
	if err != nil {
		t.Fatal(err)
	}
	if err := set.sealOpen(); err != nil {
		t.Fatal(err)
	}
	if len(misses) > 0 {
		t.Fatalf("%d lookups missed acknowledged objects during the seal; first: %s", len(misses), misses[0])
	}
	for _, id := range ids {
		if _, err := s.ReadInto(libraryID, id, nil); err != nil {
			t.Fatalf("reading %s after the seal: %v", id[:12], err)
		}
	}
}

// A seal that fails -- a full disk at the footer, typically -- must leave the
// pack where it was: open, findable, and appendable or re-sealable. It used to
// leave it in neither list, so every acknowledged frame in it read as absent,
// the next write opened a second pack, and the next start refused the library
// for having two.
func TestASealFailureKeepsAcknowledgedObjectsReadable(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "storage-data")
	objDir := TypeDir(dataDir, TypeChunks)
	s := New(confPath, dataDir, TypeChunks)

	var ids []string
	for i := 0; i < 3; i++ {
		body := fmt.Sprintf("before the failed seal %d", i)
		id := idOf([]byte(body))
		ids = append(ids, id)
		if err := s.WriteVerified(libraryID, id, strings.NewReader(body), true); err != nil {
			t.Fatal(err)
		}
	}

	injected := errors.New("no space left on device")
	packSealHook = func() error { return injected }
	t.Cleanup(func() { packSealHook = nil })

	set, err := s.packs.set(libraryID)
	if err != nil {
		t.Fatal(err)
	}
	if err := set.sealOpen(); !errors.Is(err, injected) {
		t.Fatalf("sealOpen returned %v, want the injected failure", err)
	}
	for _, id := range ids {
		if _, err := s.ReadInto(libraryID, id, nil); err != nil {
			t.Fatalf("reading %s after a failed seal: %v", id[:12], err)
		}
	}

	// The disk comes back. The next write must not open a second pack beside
	// the one that could not seal; it seals it and carries on.
	packSealHook = nil
	late := "after the disk came back"
	lateID := idOf([]byte(late))
	if err := s.WriteVerified(libraryID, lateID, strings.NewReader(late), true); err != nil {
		t.Fatalf("writing after the failed seal: %v", err)
	}
	if _, open := packFiles(t, dataDir, TypeChunks); open > 1 {
		t.Fatalf("%d open packs after a failed seal and a write, want at most 1", open)
	}
	if err := s.packs.close(); err != nil {
		t.Fatal(err)
	}
	forgetPackStore(objDir)

	again := New(confPath, dataDir, TypeChunks)
	t.Cleanup(func() { _ = again.packs.close() })
	for _, id := range append(ids, lateID) {
		if _, err := again.ReadInto(libraryID, id, nil); err != nil {
			t.Fatalf("reading %s after a restart: %v", id[:12], err)
		}
	}
}

// A pack found open at startup has frames that have sat outside a sealed pack
// for at least as long as the process was down. The age sweeper must seal it
// like any other, rather than waiting for a write that a read-only library
// never makes.
func TestARecoveredPackIsSealedByTheSweeper(t *testing.T) {
	wasAge, wasSweep := packMaxAge, packSweep
	packMaxAge = 20 * time.Millisecond
	packSweep = 5 * time.Millisecond
	t.Cleanup(func() { packMaxAge, packSweep = wasAge, wasSweep })

	dataDir := filepath.Join(t.TempDir(), "storage-data")

	first := New(confPath, dataDir, TypeChunks)
	body := "left open by a kill"
	id := idOf([]byte(body))
	if err := first.WriteVerified(libraryID, id, strings.NewReader(body), true); err != nil {
		t.Fatal(err)
	}
	// Stop without sealing, the way a kill does: the sealer goes and the
	// descriptors are released, and nothing writes a footer. Then drop the
	// registry entry so the next store rediscovers the directory from disk.
	first.packs.discard()
	forgetPackStore(TypeDir(dataDir, TypeChunks))

	second := New(confPath, dataDir, TypeChunks)
	t.Cleanup(func() { _ = second.packs.close() })
	// A read, and only a read: it loads the library's packs and never writes.
	if _, err := second.ReadInto(libraryID, id, nil); err != nil {
		t.Fatal(err)
	}

	waitForPacks(t, dataDir, 1, 0, "the recovered pack was never sealed")
	if _, err := second.ReadInto(libraryID, id, nil); err != nil {
		t.Fatalf("reading after the sweeper sealed the recovered pack: %v", err)
	}
}
