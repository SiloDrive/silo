package objstore

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dkam/silo/fileserver/option"
)

// packWriting turns the cutover on for one test and puts it back afterwards.
// The flag is off by default because compaction does not exist yet, so nothing
// but these tests should ever see it on.
func packWriting(t *testing.T) {
	t.Helper()
	was := option.PackWrites
	option.PackWrites = true
	t.Cleanup(func() { option.PackWrites = was })
}

// writeStore is a store with the pack write path on, in its own data
// directory, sealed and shut down by the test's cleanup.
func writeStore(t *testing.T) (*ObjectStore, string) {
	t.Helper()
	packWriting(t)
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

	deadline := time.Now().Add(3 * time.Second)
	for {
		sealed, open := packFiles(t, dataDir, TypeChunks)
		if sealed == 1 && open == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the pack was not sealed on age: %d sealed, %d open — "+
				"without this rule the last chunks of a quiet day sit on one disk indefinitely", sealed, open)
		}
		time.Sleep(2 * time.Millisecond)
	}

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
	packWriting(t)
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
	packWriting(t)
	dataDir := filepath.Join(t.TempDir(), "storage-data")
	s := New(confPath, dataDir, TypeChunks)

	set, err := s.packs.set(libraryID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := set.writablePack(TypeDir(dataDir, TypeChunks), libraryID); err != nil {
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
	packWriting(t)
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

// With the flag off — which is every install — nothing changes at all.
func TestWithTheFlagOffNothingIsPacked(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "storage-data")
	s := New(confPath, dataDir, TypeChunks)
	body := "written the way every install writes"
	id := idOf([]byte(body))

	if err := s.WriteVerified(libraryID, id, strings.NewReader(body), true); err != nil {
		t.Fatal(err)
	}
	if sealed, open := packFiles(t, dataDir, TypeChunks); sealed != 0 || open != 0 {
		t.Errorf("%d sealed and %d open packs with PackWrites off, want none", sealed, open)
	}
	loose := filepath.Join(LibraryDir(dataDir, TypeChunks, libraryID), id[:2], id[2:])
	if _, err := os.Stat(loose); err != nil {
		t.Errorf("the object is not where the loose store puts it: %v", err)
	}
}
