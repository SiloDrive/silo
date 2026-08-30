package objstore

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// packedStore builds an object store and puts some objects into a pack under
// it, which is the state step 4 will produce and step 3 only has to read.
//
// It hands back a *second* store opened over the same directory, because that
// is the honest scenario: a process that finds packs on disk and has to serve
// reads out of them.
func packedStore(t *testing.T, objType string, bodies []string, seal bool) (store *ObjectStore, ids []string, dataDir string) {
	t.Helper()
	dataDir = filepath.Join(t.TempDir(), "storage-data")
	first := New(confPath, dataDir, objType)
	if err := first.ready(); err != nil {
		t.Fatalf("opening a store: %v", err)
	}

	p, err := createPack(TypeDir(dataDir, objType), libraryID)
	if err != nil {
		t.Fatalf("createPack: %v", err)
	}
	for _, body := range bodies {
		id, frame := framed(t, first.key, body)
		if _, err := p.append(frame, id, true); err != nil {
			t.Fatalf("appending %s: %v", id, err)
		}
		ids = append(ids, id)
	}
	if seal {
		if err := p.seal(); err != nil {
			t.Fatalf("seal: %v", err)
		}
	}

	return New(confPath, dataDir, objType), ids, dataDir
}

// The assertion step 3 exists for: every read verb answers out of a pack, with
// nothing above objstore having said anything different.
func TestEveryReadVerbIsServedFromAPack(t *testing.T) {
	for _, sealed := range []bool{true, false} {
		name := "an open pack"
		if sealed {
			name = "a sealed pack"
		}
		t.Run(name, func(t *testing.T) {
			bodies := []string{"the first body", "the second, which is longer than the first", ""}
			s, ids, _ := packedStore(t, TypeChunks, bodies, sealed)

			for i, id := range ids {
				want := bodies[i]

				var buf bytes.Buffer
				if err := s.Read(libraryID, id, &buf); err != nil {
					t.Fatalf("Read %s: %v", id, err)
				}
				if buf.String() != want {
					t.Errorf("Read %s gave %q, want %q", id, buf.String(), want)
				}

				got, err := s.ReadInto(libraryID, id, nil)
				if err != nil {
					t.Fatalf("ReadInto %s: %v", id, err)
				}
				if string(got) != want {
					t.Errorf("ReadInto %s gave %q, want %q", id, got, want)
				}

				size, err := s.Stat(libraryID, id)
				if err != nil {
					t.Fatalf("Stat %s: %v", id, err)
				}
				if size != int64(len(want)) {
					t.Errorf("Stat %s gave %d, want %d", id, size, len(want))
				}

				exists, err := s.Exists(libraryID, id)
				if err != nil {
					t.Fatalf("Exists %s: %v", id, err)
				}
				// An empty object counts as absent everywhere else in this
				// store; a pack does not get to disagree.
				if exists != (len(want) > 0) {
					t.Errorf("Exists %s = %v, want %v", id, exists, len(want) > 0)
				}

				if len(want) > 2 {
					p := make([]byte, 2)
					n, err := s.ReadAt(libraryID, id, p, 1)
					if err != nil {
						t.Fatalf("ReadAt %s: %v", id, err)
					}
					if n != 2 || string(p) != want[1:3] {
						t.Errorf("ReadAt %s gave %q, want %q", id, p[:n], want[1:3])
					}
				}
			}
		})
	}
}

// A pack is asked before the loose store, and this is what that is for: during
// ingest an object exists in both places, and the pack is the copy that will
// still be there afterwards. The loose copy is damaged here so that reading it
// would fail — if the order were the other way round, this test would fail
// rather than pass by luck.
func TestAPackIsAskedBeforeTheLooseStore(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "storage-data")
	first := New(confPath, dataDir, TypeChunks)
	body := "this object exists in both places at once"

	if err := first.Write(libraryID, idOf([]byte(body)), strings.NewReader(body), true); err != nil {
		t.Fatalf("writing loose: %v", err)
	}
	id := idOf([]byte(body))
	loose := filepath.Join(LibraryDir(dataDir, TypeChunks, libraryID), id[:2], id[2:])
	if _, err := os.Stat(loose); err != nil {
		t.Fatalf("the loose copy is not where this test thinks: %v", err)
	}
	// Ruin the loose copy, so that anything reading it says so.
	if err := os.WriteFile(loose, []byte("not a frame at all, not even close"), 0o644); err != nil {
		t.Fatal(err)
	}

	p, err := createPack(TypeDir(dataDir, TypeChunks), libraryID)
	if err != nil {
		t.Fatal(err)
	}
	frame, err := sealFrame(first.key, id, []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.append(frame, id, true); err != nil {
		t.Fatal(err)
	}
	if err := p.seal(); err != nil {
		t.Fatal(err)
	}

	s := New(confPath, dataDir, TypeChunks)
	got, err := s.ReadInto(libraryID, id, nil)
	if err != nil {
		t.Fatalf("reading an object that is in a pack and loose: %v", err)
	}
	if string(got) != body {
		t.Errorf("read %q, want %q — the loose copy was preferred", got, body)
	}
}

// A store with no packs still reads everything it has, which is the state
// every install is in until step 4. This is the lane that lets packs land
// beside a working store rather than in place of one.
func TestALooseStoreIsUnaffectedByThePackLayer(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "storage-data")
	s := New(confPath, dataDir, TypeChunks)
	body := "no pack has ever been written here"
	id := idOf([]byte(body))

	if err := s.WriteVerified(libraryID, id, strings.NewReader(body), true); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := s.ReadInto(libraryID, id, nil)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != body {
		t.Errorf("read %q, want %q", got, body)
	}
	if _, err := s.ReadInto(libraryID, idOf([]byte("never stored")), nil); !errors.Is(err, ErrNotFound) {
		t.Errorf("reading something absent gave %v, want ErrNotFound", err)
	}
}

// Stat answers out of the index and never opens the pack. Moving the pack file
// away after the store has loaded it is what makes that observable: the size
// still comes back, and the read that does need the bytes fails.
func TestStatOfAPackedObjectNeverReadsThePack(t *testing.T) {
	body := "a body whose length is known without reading it"
	s, ids, dataDir := packedStore(t, TypeChunks, []string{body}, true)
	id := ids[0]

	// Load the footer, which is what a real store does on first touch.
	if _, err := s.Stat(libraryID, id); err != nil {
		t.Fatalf("Stat: %v", err)
	}

	dir := packDir(TypeDir(dataDir, TypeChunks), libraryID)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	moved := 0
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".pack" {
			if err := os.Rename(filepath.Join(dir, e.Name()), filepath.Join(dir, e.Name()+".moved")); err != nil {
				t.Fatal(err)
			}
			moved++
		}
	}
	if moved != 1 {
		t.Fatalf("moved %d packs, want 1", moved)
	}

	size, err := s.Stat(libraryID, id)
	if err != nil {
		t.Fatalf("Stat after the pack was moved away: %v — it read the pack", err)
	}
	if size != int64(len(body)) {
		t.Errorf("Stat gave %d, want %d", size, len(body))
	}
	if _, err := s.ReadInto(libraryID, id, nil); err == nil {
		t.Error("a read succeeded with the pack moved away, so it read nothing")
	}
}

// --- the set ------------------------------------------------------------

// The open pack is asked first because its index is already in memory and it
// is one map read. Asserted at this level because both copies decrypt to the
// same bytes — the only observable difference is which pack answered.
func TestTheOpenPackIsAskedBeforeTheSealedOnes(t *testing.T) {
	objDir, lib := packScratch(t)
	key := packKey(t)
	id, frame := framed(t, key, "in two packs at once, which ingest can produce")

	old := filledPack(t, objDir, lib, [][]byte{frame}, []string{id})
	if err := old.seal(); err != nil {
		t.Fatal(err)
	}
	live := filledPack(t, objDir, lib, [][]byte{frame}, []string{id})
	defer live.close()

	set, err := loadPackSet(objDir, lib)
	if err != nil {
		t.Fatalf("loadPackSet: %v", err)
	}
	if set.open == nil {
		t.Fatal("the set found no open pack")
	}
	if len(set.sealed) != 1 {
		t.Fatalf("the set found %d sealed packs, want 1", len(set.sealed))
	}

	who, e, ok := set.find(id)
	if !ok {
		t.Fatal("the set does not hold an object both its packs hold")
	}
	if who != packReader(set.open) {
		t.Errorf("a sealed pack answered before the open one")
	}
	got, err := who.readFrameAt(e)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, frame) {
		t.Error("the answer does not point at the frame")
	}
}

// Two open packs in one library means two writers appended to it. Picking one
// would silently orphan the other's frames, so it is refused.
func TestTwoOpenPacksInOneLibraryAreRefused(t *testing.T) {
	objDir, lib := packScratch(t)
	key := packKey(t)
	for i := 0; i < 2; i++ {
		id, frame := framed(t, key, fmt.Sprintf("written by writer %d", i))
		p := filledPack(t, objDir, lib, [][]byte{frame}, []string{id})
		defer p.close()
	}

	if _, err := loadPackSet(objDir, lib); !errors.Is(err, ErrPackCorrupt) {
		t.Errorf("err = %v, want ErrPackCorrupt", err)
	}
}

func TestALibraryWithNoPacksLoadsAsEmpty(t *testing.T) {
	objDir, lib := packScratch(t)
	set, err := loadPackSet(objDir, lib)
	if err != nil {
		t.Fatalf("a library that has never been packed: %v", err)
	}
	if set.open != nil || len(set.sealed) != 0 {
		t.Errorf("an unpacked library loaded %v open and %d sealed", set.open != nil, len(set.sealed))
	}
	if _, _, ok := set.find(strings.Repeat("ab", 32)); ok {
		t.Error("an empty set claims to hold something")
	}
}

// A store that kept a deleted library's packs would answer reads out of files
// that are gone.
func TestRemovingALibraryForgetsItsPacks(t *testing.T) {
	s, ids, _ := packedStore(t, TypeChunks, []string{"about to be deleted"}, true)
	id := ids[0]

	if _, err := s.ReadInto(libraryID, id, nil); err != nil {
		t.Fatalf("reading before the delete: %v", err)
	}
	if err := s.RemoveLibrary(libraryID); err != nil {
		t.Fatalf("RemoveLibrary: %v", err)
	}
	if _, err := s.ReadInto(libraryID, id, nil); err == nil {
		t.Error("the object is still readable after its library was removed")
	}
	if exists, err := s.Exists(libraryID, id); err != nil || exists {
		t.Errorf("Exists = %v (err %v) after the library was removed, want false", exists, err)
	}
}

// A frame in a pack is the same frame the loose store holds — copied, never
// re-sealed — so it opens under the id it is stored at and under no other.
func TestAPackedFrameIsStillBoundToItsID(t *testing.T) {
	s, ids, _ := packedStore(t, TypeChunks, []string{"bound to its own id"}, true)
	other := idOf([]byte("some entirely different object"))

	if _, err := s.ReadInto(libraryID, other, nil); err == nil {
		t.Error("an id the pack does not hold read something")
	}
	if _, err := s.ReadInto(libraryID, ids[0], nil); err != nil {
		t.Errorf("the id the pack does hold did not read: %v", err)
	}
}
