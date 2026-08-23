package objstore

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
)

const (
	repoID = "b1f2ad61-9164-418a-a47f-ab805dbd5694"
	objID  = "0401fc662e3bc87a41f299a907c056aaf8322a27"
)

// Set from os.MkdirTemp in TestMain (t.TempDir needs a *testing.T, which
// TestMain has no access to) so this package's object store is its own and
// does not collide with the other packages' tests when they run in parallel.
// testFile lives under it too, rather than being written into the package dir.
var confPath string
var dataDir string
var testFile string

func createFile() error {
	outputFile, err := os.OpenFile(testFile, os.O_WRONLY|os.O_CREATE, 0666)
	if err != nil {
		return err
	}
	defer func() { _ = outputFile.Close() }()

	outputString := "hello world!\n"
	for i := 0; i < 10; i++ {
		_, _ = outputFile.WriteString(outputString)
	}

	return nil
}

func delFile() error {
	// testFile lives under confPath, so one RemoveAll covers both.
	err := os.RemoveAll(confPath)
	if err != nil {
		return err
	}

	return nil
}

func TestMain(m *testing.M) {
	var err error
	confPath, err = os.MkdirTemp("", "silo-objstore-test")
	if err != nil {
		fmt.Printf("Failed to create test dir : %v\n", err)
		os.Exit(1)
	}
	dataDir = filepath.Join(confPath, "storage-data")
	testFile = filepath.Join(confPath, "output.data")

	err = createFile()
	if err != nil {
		fmt.Printf("Failed to create test file : %v\n", err)
		os.Exit(1)
	}
	code := m.Run()
	err = delFile()
	if err != nil {
		fmt.Printf("Failed to remove test file : %v\n", err)
		os.Exit(1)
	}
	os.Exit(code)
}

func testWrite(t *testing.T) {
	inputFile, err := os.Open(testFile)
	if err != nil {
		t.Errorf("Failed to open test file : %v\n", err)
	}
	defer func() { _ = inputFile.Close() }()

	bend := New(confPath, dataDir, "commit")
	_ = bend.Write(repoID, objID, inputFile, true)
}

func testRead(t *testing.T) {
	outputFile, err := os.OpenFile(testFile, os.O_WRONLY, 0666)
	if err != nil {
		t.Errorf("Failed to open test file:%v\n", err)
	}
	defer func() { _ = outputFile.Close() }()

	bend := New(confPath, dataDir, "commit")
	err = bend.Read(repoID, objID, outputFile)
	if err != nil {
		t.Errorf("Failed to read backend : %s\n", err)
	}
}

func testExists(t *testing.T) {
	bend := New(confPath, dataDir, "commit")
	ret, _ := bend.Exists(repoID, objID)
	if !ret {
		t.Errorf("File is not exist\n")
	}

	filePath := path.Join(dataDir, "storage", "commit", repoID, objID[:2], objID[2:])
	fileInfo, _ := os.Stat(filePath)
	if fileInfo.Size() != 130 {
		t.Errorf("File is exist, but the size of file is incorrect.\n")
	}
}

func TestObjStore(t *testing.T) {
	testWrite(t)
	testRead(t)
	testExists(t)
}

// A zero-length object is the signature of a write that was published but
// never made durable — the failure mode the fsync in write() exists to
// prevent, and the one already on disk in stores written before it. Exists has
// to report it absent: /check-blocks answers the client from Exists, so
// calling it present tells the client the block is already uploaded and it is
// never sent again, which is what turns a lost write into permanent damage.
func TestObjStoreZeroLengthObjectIsAbsent(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "storage-data")
	bend := New(confPath, dataDir, "blocks")

	if err := bend.Write(repoID, objID, strings.NewReader(""), true); err != nil {
		t.Fatalf("Write() returned %v", err)
	}

	exists, err := bend.Exists(repoID, objID)
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
	bend := New(confPath, dataDir, "blocks")

	exists, err := bend.Exists(repoID, objID)
	if err != nil {
		t.Errorf("Exists() on a missing object returned error %v, want nil", err)
	}
	if exists {
		t.Error("Exists() = true for a missing object, want false")
	}
}

// The durable write path creates the repo and fan-out directories itself and
// fsyncs each one it had to create. Writing into a store that has never seen
// the repo before is the case where those syncs run.
func TestObjStoreSyncWriteIntoNewRepoDir(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "storage-data")

	for _, objType := range []string{"blocks", "commit", "fs"} {
		bend := New(confPath, dataDir, objType)
		if err := bend.Write(repoID, objID, strings.NewReader("payload"), true); err != nil {
			t.Fatalf("Write(%s) into a new repo dir returned %v", objType, err)
		}

		exists, err := bend.Exists(repoID, objID)
		if err != nil || !exists {
			t.Fatalf("Exists(%s) = (%v, %v), want (true, nil)", objType, exists, err)
		}

		var buf strings.Builder
		if err := bend.Read(repoID, objID, &buf); err != nil {
			t.Fatalf("Read(%s) returned %v", objType, err)
		}
		if buf.String() != "payload" {
			t.Errorf("Read(%s) = %q, want %q", objType, buf.String(), "payload")
		}

		// The object is published by rename, so nothing partial may be left
		// beside it — a stray temp file is invisible to reads and to the GC,
		// which only walks well-formed object paths.
		fanoutDir := filepath.Join(dataDir, "storage", objType, repoID, objID[:2])
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
		"0401fc662e3bc87a41f299a907c056aaf8322a2",   // 39 chars
		"0401fc662e3bc87a41f299a907c056aaf8322a277", // 41 chars
		"0401FC662E3BC87A41F299A907C056AAF8322A27",  // uppercase hex
		"0401fc662e3bc87a41f299a907c056aaf8322g27",  // non-hex
	}

	bend := New(confPath, dataDir, "commit")
	for _, id := range bad {
		// Each of these would panic rather than return if the guard were gone.
		if err := bend.Read(repoID, id, io.Discard); err == nil {
			t.Errorf("Read(%q) returned nil error, want rejection", id)
		}
		if err := bend.Write(repoID, id, strings.NewReader("data"), true); err == nil {
			t.Errorf("Write(%q) returned nil error, want rejection", id)
		}
		exists, err := bend.Exists(repoID, id)
		if err == nil || exists {
			t.Errorf("Exists(%q) = (%v, %v), want (false, error)", id, exists, err)
		}
		if _, err := bend.Stat(repoID, id); err == nil {
			t.Errorf("Stat(%q) returned nil error, want rejection", id)
		}
	}
}

// A 64-character id is a store-v2 chunk or pack. Both widths have to work at
// once: the old objects are still here while the new ones start arriving.
const sha256ObjID = "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"

func writeTestObject(t *testing.T, s *ObjectStore, id, content string) {
	t.Helper()
	if err := s.Write(repoID, id, strings.NewReader(content), false); err != nil {
		t.Fatalf("Write(%s): %v", id, err)
	}
}

func TestBothIDWidthsAreStorable(t *testing.T) {
	s := New(confPath, dataDir, "widths")
	for _, id := range []string{objID, sha256ObjID} {
		writeTestObject(t, s, id, "content for "+id)
		var got strings.Builder
		if err := s.Read(repoID, id, &got); err != nil {
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
	s := New(confPath, dataDir, "readat")
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
		got, err := s.ReadAt(repoID, objID, p, tc.off)
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
	s := New(confPath, dataDir, "readat-eof")
	writeTestObject(t, s, objID, "0123456789")

	p := make([]byte, 8)
	n, err := s.ReadAt(repoID, objID, p, 6)
	if err != io.EOF {
		t.Fatalf("err = %v, want io.EOF", err)
	}
	if n != 4 || string(p[:n]) != "6789" {
		t.Fatalf("got %q (%d bytes), want %q", p[:n], n, "6789")
	}

	if _, err := s.ReadAt(repoID, objID, p, 100); err != io.EOF {
		t.Fatalf("reading entirely past the end: err = %v, want io.EOF", err)
	}
}

// One sentinel for absence, whichever backend answered. The tiering logic
// above this asks "is it here" once rather than once per backend.
func TestAMissingObjectIsErrNotFound(t *testing.T) {
	s := New(confPath, dataDir, "notfound")
	missing := "1111111111111111111111111111111111111111"

	if _, err := s.Stat(repoID, missing); !errors.Is(err, ErrNotFound) {
		t.Errorf("Stat: %v, want ErrNotFound", err)
	}
	if err := s.Read(repoID, missing, io.Discard); !errors.Is(err, ErrNotFound) {
		t.Errorf("Read: %v, want ErrNotFound", err)
	}
	if _, err := s.ReadAt(repoID, missing, make([]byte, 1), 0); !errors.Is(err, ErrNotFound) {
		t.Errorf("ReadAt: %v, want ErrNotFound", err)
	}
	// Exists is the one that maps it back to an answer rather than an error.
	exists, err := s.Exists(repoID, missing)
	if exists || err != nil {
		t.Errorf("Exists = (%v, %v), want (false, nil)", exists, err)
	}
}

func TestListYieldsEveryObjectWithItsSize(t *testing.T) {
	s := New(confPath, dataDir, "list")
	want := map[string]int64{
		objID:           4,
		sha256ObjID:     11,
		"a" + objID[1:]: 2,
	}
	for id, size := range want {
		writeTestObject(t, s, id, strings.Repeat("x", int(size)))
	}

	got := map[string]int64{}
	if err := s.List(repoID, func(id string, size int64) error {
		got[id] = size
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

// A repo with no objects and a repo that never existed are the same answer,
// and neither is an error.
func TestListOfAnEmptyRepoIsEmptyNotAnError(t *testing.T) {
	s := New(confPath, dataDir, "list-empty")
	n := 0
	if err := s.List("00000000-0000-0000-0000-000000000000", func(string, int64) error {
		n++
		return nil
	}); err != nil {
		t.Fatalf("List of a repo that was never written: %v", err)
	}
	if n != 0 {
		t.Fatalf("listed %d objects in an empty repo", n)
	}
}

// The debris of an interrupted write shares a directory with fs and commit
// objects. It is not a pack: nothing references it, and reporting it as one
// would have the caller asking a pack index about an id it never held.
func TestListSkipsTheDebrisOfAnInterruptedWrite(t *testing.T) {
	s := New(confPath, dataDir, "list-debris")
	writeTestObject(t, s, objID, "good")

	fanout := filepath.Join(TypeDir(dataDir, "list-debris"), repoID, objID[:2])
	if err := os.WriteFile(filepath.Join(fanout, objID[2:]+".123456"), []byte("partial"), 0o644); err != nil {
		t.Fatal(err)
	}

	var ids []string
	if err := s.List(repoID, func(id string, _ int64) error {
		ids = append(ids, id)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != objID {
		t.Fatalf("listed %v, want just %s", ids, objID)
	}
}

func TestListStopsOnTheCallbacksError(t *testing.T) {
	s := New(confPath, dataDir, "list-stop")
	writeTestObject(t, s, objID, "one")
	writeTestObject(t, s, sha256ObjID, "two")

	sentinel := errors.New("stop")
	seen := 0
	err := s.List(repoID, func(string, int64) error {
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
func TestRemoveIsIdempotent(t *testing.T) {
	s := New(confPath, dataDir, "remove")
	writeTestObject(t, s, objID, "doomed")

	for i := range 2 {
		if err := s.Remove(repoID, objID); err != nil {
			t.Fatalf("Remove call %d: %v", i+1, err)
		}
	}
	if exists, err := s.Exists(repoID, objID); exists || err != nil {
		t.Fatalf("after Remove: Exists = (%v, %v)", exists, err)
	}
	if err := s.Remove(repoID, "bad"); err == nil {
		t.Error("Remove accepted a malformed id")
	}
}

func TestRemoveRepoTakesEverythingAndIsIdempotent(t *testing.T) {
	s := New(confPath, dataDir, "remove-repo")
	writeTestObject(t, s, objID, "one")
	writeTestObject(t, s, sha256ObjID, "two")

	for i := range 2 {
		if err := s.RemoveRepo(repoID); err != nil {
			t.Fatalf("RemoveRepo call %d: %v", i+1, err)
		}
	}
	n := 0
	if err := s.List(repoID, func(string, int64) error { n++; return nil }); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("%d objects survived RemoveRepo", n)
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
	s := New(confPath, blocked, "commits")

	if err := s.Read(repoID, objID, io.Discard); err == nil {
		t.Error("Read on a store with no backend returned nil")
	}
	if err := s.Write(repoID, objID, strings.NewReader("x"), false); err == nil {
		t.Error("Write on a store with no backend returned nil")
	}
	if _, err := s.Stat(repoID, objID); err == nil {
		t.Error("Stat on a store with no backend returned nil")
	}
	if _, err := s.Exists(repoID, objID); err == nil {
		t.Error("Exists on a store with no backend returned nil")
	}
	if err := s.List(repoID, func(string, int64) error { return nil }); err == nil {
		t.Error("List on a store with no backend returned nil")
	}
	if err := s.Remove(repoID, objID); err == nil {
		t.Error("Remove on a store with no backend returned nil")
	}
	if err := s.RemoveRepo(repoID); err == nil {
		t.Error("RemoveRepo on a store with no backend returned nil")
	}
	if _, err := s.ReadAt(repoID, objID, make([]byte, 1), 0); err == nil {
		t.Error("ReadAt on a store with no backend returned nil")
	}
}
