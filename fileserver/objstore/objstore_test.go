package objstore

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	libraryID = "b1f2ad61-9164-418a-a47f-ab805dbd5694"
	objID     = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
)

// Set from os.MkdirTemp in TestMain, because t.TempDir needs a *testing.T and
// TestMain has none. The tests in this package that do not build a store of
// their own share this directory, each under its own object type, so that the
// package's stores are its own and do not collide with the other packages'
// tests when they run in parallel.
var dataDir string

func TestMain(m *testing.M) {
	root, err := os.MkdirTemp("", "silo-objstore-test")
	if err != nil {
		fmt.Printf("no temp directory for the test store: %v\n", err)
		os.Exit(1)
	}
	dataDir = filepath.Join(root, "storage-data")

	code := m.Run()

	// Cleaned up here rather than in a defer, because os.Exit does not run
	// them -- and reported rather than exited on, since a directory left in
	// /tmp is not a reason to throw away the result of the run.
	if err := os.RemoveAll(root); err != nil {
		fmt.Printf("leaving %s behind: %v\n", root, err)
	}
	os.Exit(code)
}

// One object, written and then asked about every way this store can be asked.
//
// It replaces three functions that ran in sequence out of a fourth and shared a
// file on disk between them. Neither of the two that mattered could fail on
// what it was named for: the write discarded the error Write returned, and the
// read opened that same input file for writing and copied the object over it,
// comparing nothing. So a store that wrote nothing and read nothing back passed
// all three. What is asserted here is the round trip -- the bytes back, the
// size, and where on the disk they actually ended up.
func TestAnObjectIsWrittenReadAndFoundAgain(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "storage-data")
	s := New(dataDir, "commit")

	content := strings.Repeat("hello world!\n", 10)
	if err := s.Write(libraryID, objID, strings.NewReader(content), true); err != nil {
		t.Fatalf("Write: %v", err)
	}

	var got strings.Builder
	if err := s.Read(libraryID, objID, &got); err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got.String() != content {
		t.Errorf("Read gave %q, want the %d bytes written", got.String(), len(content))
	}

	exists, err := s.Exists(libraryID, objID)
	if err != nil || !exists {
		t.Errorf("Exists = (%v, %v), want (true, nil)", exists, err)
	}
	if size, err := s.Stat(libraryID, objID); err != nil || size != int64(len(content)) {
		t.Errorf("Stat = (%d, %v), want (%d, nil)", size, err, len(content))
	}

	// The store answers about the object; the disk holds a sealed frame around
	// it. Both halves are asserted, because the pair is the invariant.
	//
	// Since the cutover that frame lives inside a pack rather than in a file of
	// its own, so the loose path is asserted absent: a store that wrote both
	// would be storing everything twice.
	loose := filepath.Join(LibraryDir(dataDir, "commit", libraryID), objID[:2], objID[2:])
	if _, err := os.Stat(loose); !os.IsNotExist(err) {
		t.Errorf("the object is also in a file of its own at %s", loose)
	}
	if _, e, ok, err := s.packs.find(libraryID, objID); err != nil || !ok {
		t.Fatalf("the pack lookup does not hold the object: ok=%v err=%v", ok, err)
	} else if e.Length != int64(len(content)+frameOverhead) {
		t.Errorf("the frame in the pack is %d bytes, want %d", e.Length, len(content)+frameOverhead)
	}
}

// A zero-length object is the signature of a write that was published but
// never made durable — the failure mode the fsync in write() exists to
// prevent, and the one already on disk in stores written before it. Exists has
// to report it absent: /check-blocks answers the client from Exists, so
// calling it present tells the client the chunk is already uploaded and it is
// never sent again, which is what turns a lost write into permanent damage.
func TestObjStoreZeroLengthObjectIsAbsent(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "storage-data")
	bend := New(dataDir, "chunks")

	if err := bend.Write(libraryID, objID, strings.NewReader(""), true); err != nil {
		t.Fatalf("Write() returned %v", err)
	}

	exists, err := bend.Exists(libraryID, objID)
	if err != nil {
		t.Errorf("Exists() returned error %v, want nil", err)
	}
	if exists {
		t.Error("Exists() = true for a zero-length object, want false")
	}
}

// Exists distinguishes "not there" from "could not tell": a missing object is
// not an error, and no failure reports the object as present.
func TestObjStoreExistsOnMissingObject(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "storage-data")
	bend := New(dataDir, "chunks")

	exists, err := bend.Exists(libraryID, objID)
	if err != nil {
		t.Errorf("Exists() on a missing object returned error %v, want nil", err)
	}
	if exists {
		t.Error("Exists() = true for a missing object, want false")
	}
}

// The durable write path creates the library and fan-out directories itself and
// fsyncs each one it had to create. Writing into a store that has never seen
// the library before is the case where those syncs run.
func TestObjStoreSyncWriteIntoNewLibraryDir(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "storage-data")

	for _, objType := range []string{"chunks", "commit", "fs"} {
		bend := New(dataDir, objType)
		// Loose, because what this asserts is the loose lane's publish-by-rename:
		// a pack is appended to rather than renamed into place, and has no temp
		// file to leave behind.
		writeLooseObject(t, bend, objID, "payload")

		exists, err := bend.Exists(libraryID, objID)
		if err != nil || !exists {
			t.Fatalf("Exists(%s) = (%v, %v), want (true, nil)", objType, exists, err)
		}

		var buf strings.Builder
		if err := bend.Read(libraryID, objID, &buf); err != nil {
			t.Fatalf("Read(%s) returned %v", objType, err)
		}
		if buf.String() != "payload" {
			t.Errorf("Read(%s) = %q, want %q", objType, buf.String(), "payload")
		}

		// The object is published by rename, so nothing partial may be left
		// beside it — a stray temp file is invisible to reads and to the GC,
		// which only walks well-formed object paths.
		fanoutDir := filepath.Join(dataDir, "storage", objType, libraryID, objID[:2])
		entries, err := os.ReadDir(fanoutDir)
		if err != nil {
			t.Fatalf("failed to read %s: %v", fanoutDir, err)
		}
		if len(entries) != 1 || entries[0].Name() != objID[2:] {
			var names []string
			for _, e := range entries {
				names = append(names, e.Name())
			}
			t.Errorf("%s contains %v, want just %q", fanoutDir, names, objID[2:])
		}
	}
}

// The store fans objects out as objID[:2]/objID[2:], which panics outright on
// an ID shorter than two characters, so every entry point must reject a
// malformed ID rather than slicing it.
func TestObjStoreRejectsInvalidObjectID(t *testing.T) {
	bad := []string{
		"",
		"a",
		"ab",
		"../../../etc/passwd",
		objID[:len(objID)-1],   // 63 chars
		objID + "0",            // 65 chars
		strings.ToUpper(objID), // uppercase hex
		"g" + objID[1:],        // non-hex
	}

	bend := New(dataDir, "commit")
	for _, id := range bad {
		// Each of these would panic rather than return if the guard were gone.
		if err := bend.Read(libraryID, id, io.Discard); err == nil {
			t.Errorf("Read(%q) returned nil error, want rejection", id)
		}
		if err := bend.Write(libraryID, id, strings.NewReader("data"), true); err == nil {
			t.Errorf("Write(%q) returned nil error, want rejection", id)
		}
		exists, err := bend.Exists(libraryID, id)
		if err == nil || exists {
			t.Errorf("Exists(%q) = (%v, %v), want (false, error)", id, exists, err)
		}
		if _, err := bend.Stat(libraryID, id); err == nil {
			t.Errorf("Stat(%q) returned nil error, want rejection", id)
		}
	}
}

// A second id, for the tests that need two distinct objects in one store.
const otherObjID = "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"

func writeTestObject(t *testing.T, s *ObjectStore, id, content string) {
	t.Helper()
	if err := s.Write(libraryID, id, strings.NewReader(content), false); err != nil {
		t.Fatalf("Write(%s): %v", id, err)
	}
}

// writeLooseObject stores an object the way the write path did before packs
// became it: one frame, in a file of its own, under the two-character fan-out.
//
// The loose lane is read-only now and will be for the life of every store
// written before the cutover, so the tests about it have to be able to produce
// one. It goes through the backend rather than through Write, which is the
// whole point: Write packs.
func writeLooseObject(t *testing.T, s *ObjectStore, id, content string) {
	t.Helper()
	if err := s.ready(); err != nil {
		t.Fatal(err)
	}
	frame, err := sealFrame(s.key, id, []byte(content))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.backend.write(libraryID, id, bytes.NewReader(frame), true); err != nil {
		t.Fatalf("writing %s loose: %v", id, err)
	}
}

func TestObjectsRoundTripUnderTheirOwnIDs(t *testing.T) {
	s := New(dataDir, "widths")
	for _, id := range []string{objID, otherObjID} {
		writeTestObject(t, s, id, "content for "+id)
		var got strings.Builder
		if err := s.Read(libraryID, id, &got); err != nil {
			t.Fatalf("Read(%s): %v", id, err)
		}
		if got.String() != "content for "+id {
			t.Fatalf("Read(%s) = %q", id, got.String())
		}
	}
}

// The ranged read is the whole reason the seam changed shape: a chunk read
// becomes a read of one byte range out of the pack holding it.
func TestReadAtReturnsOneRange(t *testing.T) {
	s := New(dataDir, "readat")
	writeTestObject(t, s, objID, "0123456789abcdef")

	for _, tc := range []struct {
		off  int64
		n    int
		want string
	}{
		{0, 4, "0123"},
		{6, 4, "6789"},
		{12, 4, "cdef"},
		{15, 1, "f"},
	} {
		p := make([]byte, tc.n)
		got, err := s.ReadAt(libraryID, objID, p, tc.off)
		if err != nil {
			t.Fatalf("ReadAt(%d,%d): %v", tc.off, tc.n, err)
		}
		if got != tc.n || string(p) != tc.want {
			t.Fatalf("ReadAt(%d,%d) = %q (%d bytes), want %q", tc.off, tc.n, p[:got], got, tc.want)
		}
	}
}

// io.ReaderAt semantics, not Read's. A pack index asking for a frame at a
// known offset and length has to be able to tell "the pack is shorter than
// the index says" from "the read happened to be short".
func TestReadAtPastTheEndReportsEOF(t *testing.T) {
	s := New(dataDir, "readat-eof")
	writeTestObject(t, s, objID, "0123456789")

	p := make([]byte, 8)
	n, err := s.ReadAt(libraryID, objID, p, 6)
	if err != io.EOF {
		t.Fatalf("err = %v, want io.EOF", err)
	}
	if n != 4 || string(p[:n]) != "6789" {
		t.Fatalf("got %q (%d bytes), want %q", p[:n], n, "6789")
	}

	if _, err := s.ReadAt(libraryID, objID, p, 100); err != io.EOF {
		t.Fatalf("reading entirely past the end: err = %v, want io.EOF", err)
	}
}

// One sentinel for absence, whichever backend answered. The tiering logic
// above this asks "is it here" once rather than once per backend.
func TestAMissingObjectIsErrNotFound(t *testing.T) {
	s := New(dataDir, "notfound")
	missing := strings.Repeat("1", 2*sha256.Size)

	if _, err := s.Stat(libraryID, missing); !errors.Is(err, ErrNotFound) {
		t.Errorf("Stat: %v, want ErrNotFound", err)
	}
	if err := s.Read(libraryID, missing, io.Discard); !errors.Is(err, ErrNotFound) {
		t.Errorf("Read: %v, want ErrNotFound", err)
	}
	if _, err := s.ReadAt(libraryID, missing, make([]byte, 1), 0); !errors.Is(err, ErrNotFound) {
		t.Errorf("ReadAt: %v, want ErrNotFound", err)
	}
	// Exists is the one that maps it back to an answer rather than an error.
	exists, err := s.Exists(libraryID, missing)
	if exists || err != nil {
		t.Errorf("Exists = (%v, %v), want (false, nil)", exists, err)
	}
}

func TestListYieldsEveryObjectWithItsSize(t *testing.T) {
	s := New(dataDir, "list")
	want := map[string]int64{
		objID:           4,
		otherObjID:      11,
		"a" + objID[1:]: 2,
	}
	for id, size := range want {
		writeTestObject(t, s, id, strings.Repeat("x", int(size)))
	}

	got := map[string]int64{}
	if err := s.List(libraryID, func(o ObjectInfo) error {
		got[o.ID] = o.Size
		return nil
	}); err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("listed %d objects, want %d: %v", len(got), len(want), got)
	}
	for id, size := range want {
		if got[id] != size {
			t.Errorf("%s listed as %d bytes, want %d", id, got[id], size)
		}
	}
}

// A library with no objects and a library that never existed are the same answer,
// and neither is an error.
func TestListOfAnEmptyLibraryIsEmptyNotAnError(t *testing.T) {
	s := New(dataDir, "list-empty")
	n := 0
	if err := s.List("00000000-0000-0000-0000-000000000000", func(ObjectInfo) error {
		n++
		return nil
	}); err != nil {
		t.Fatalf("List of a library that was never written: %v", err)
	}
	if n != 0 {
		t.Fatalf("listed %d objects in an empty library", n)
	}
}

// The debris of an interrupted write shares a directory with fs and commit
// objects. It is not a pack: nothing references it, and reporting it as one
// would have the caller asking a pack index about an id it never held.
func TestListSkipsTheDebrisOfAnInterruptedWrite(t *testing.T) {
	s := New(dataDir, "list-debris")
	writeLooseObject(t, s, objID, "good")

	fanout := filepath.Join(TypeDir(dataDir, "list-debris"), libraryID, objID[:2])
	if err := os.WriteFile(filepath.Join(fanout, objID[2:]+".123456"), []byte("partial"), 0o644); err != nil {
		t.Fatal(err)
	}

	var ids []string
	if err := s.List(libraryID, func(o ObjectInfo) error {
		ids = append(ids, o.ID)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != objID {
		t.Fatalf("listed %v, want just %s", ids, objID)
	}
}

func TestListStopsOnTheCallbacksError(t *testing.T) {
	s := New(dataDir, "list-stop")
	writeTestObject(t, s, objID, "one")
	writeTestObject(t, s, otherObjID, "two")

	sentinel := errors.New("stop")
	seen := 0
	err := s.List(libraryID, func(ObjectInfo) error {
		seen++
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("List returned %v, want the callback's error", err)
	}
	if seen != 1 {
		t.Fatalf("callback ran %d times after returning an error", seen)
	}
}

// Deletion is idempotent because compaction has to be interruptible at every
// step: a retry that finds the pack already gone must carry on, not stop.
//
// Loose, because a packed object is not deletable in place at all -- Remove
// says so with ErrReclaimDeferred, and that is a different property with a test
// of its own.
func TestRemoveIsIdempotent(t *testing.T) {
	s := New(dataDir, "remove")
	writeLooseObject(t, s, objID, "doomed")

	for i := range 2 {
		if err := s.Remove(libraryID, objID); err != nil {
			t.Fatalf("Remove call %d: %v", i+1, err)
		}
	}
	if exists, err := s.Exists(libraryID, objID); exists || err != nil {
		t.Fatalf("after Remove: Exists = (%v, %v)", exists, err)
	}
	if err := s.Remove(libraryID, "bad"); err == nil {
		t.Error("Remove accepted a malformed id")
	}
}

func TestRemoveLibraryTakesEverythingAndIsIdempotent(t *testing.T) {
	s := New(dataDir, "remove-library")
	writeTestObject(t, s, objID, "one")
	writeTestObject(t, s, otherObjID, "two")

	for i := range 2 {
		if err := s.RemoveLibrary(libraryID); err != nil {
			t.Fatalf("RemoveLibrary call %d: %v", i+1, err)
		}
	}
	n := 0
	if err := s.List(libraryID, func(ObjectInfo) error { n++; return nil }); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("%d objects survived RemoveLibrary", n)
	}
}

// The backend that could not be built used to be a nil pointer every call
// dereferenced. It now says what happened, on whichever call reaches it first.
func TestAStoreWithNoBackendReportsWhy(t *testing.T) {
	// A data directory that cannot hold a store, because it is a file.
	blocked := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocked, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := New(blocked, "commits")

	if err := s.Read(libraryID, objID, io.Discard); err == nil {
		t.Error("Read on a store with no backend returned nil")
	}
	if err := s.Write(libraryID, objID, strings.NewReader("x"), false); err == nil {
		t.Error("Write on a store with no backend returned nil")
	}
	if _, err := s.Stat(libraryID, objID); err == nil {
		t.Error("Stat on a store with no backend returned nil")
	}
	if _, err := s.Exists(libraryID, objID); err == nil {
		t.Error("Exists on a store with no backend returned nil")
	}
	if err := s.List(libraryID, func(ObjectInfo) error { return nil }); err == nil {
		t.Error("List on a store with no backend returned nil")
	}
	if err := s.Remove(libraryID, objID); err == nil {
		t.Error("Remove on a store with no backend returned nil")
	}
	if err := s.RemoveLibrary(libraryID); err == nil {
		t.Error("RemoveLibrary on a store with no backend returned nil")
	}
	if _, err := s.ReadAt(libraryID, objID, make([]byte, 1), 0); err == nil {
		t.Error("ReadAt on a store with no backend returned nil")
	}
}

// Nothing mints a forty-character id any more. The routes pin their id
// variable to sixty-four hex characters, store.ParseID refuses anything
// narrower, and the format that produced SHA-1 commits, fs objects and chunks
// is gone. A backend that still builds a path from one is a second id parser
// waiting to disagree with the first.
func TestOnlyTheSHA256WidthIsStorable(t *testing.T) {
	const sha1ObjID = "0401fc662e3bc87a41f299a907c056aaf8322a27"

	s := New(dataDir, TypeChunks)
	if err := s.Write(libraryID, sha1ObjID, strings.NewReader("legacy"), false); err == nil {
		t.Errorf("Write(%s) accepted a 40-character id", sha1ObjID)
	}
	if err := s.Read(libraryID, sha1ObjID, io.Discard); err == nil {
		t.Errorf("Read(%s) accepted a 40-character id", sha1ObjID)
	}
	if exists, err := s.Exists(libraryID, sha1ObjID); err == nil || exists {
		t.Errorf("Exists(%s) = (%v, %v), want (false, error)", sha1ObjID, exists, err)
	}
	if _, err := s.Stat(libraryID, sha1ObjID); err == nil {
		t.Errorf("Stat(%s) accepted a 40-character id", sha1ObjID)
	}
}

// Types is what gc walks to reclaim a deleted library, so a name in it that no
// longer names a store is a directory gc looks for and never finds -- and a
// store missing from it is one gc leaves on disk forever. Two object stores
// exist: the chunks that hold content and the objects that describe it.
func TestTypesAreTheTwoLiveStores(t *testing.T) {
	want := []string{TypeChunks, TypeObjects}
	if len(Types) != len(want) {
		t.Fatalf("Types = %v, want %v", Types, want)
	}
	for i := range want {
		if Types[i] != want[i] {
			t.Fatalf("Types = %v, want %v", Types, want)
		}
	}
}
