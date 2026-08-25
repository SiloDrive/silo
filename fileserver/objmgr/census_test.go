package objmgr

import (
	"bytes"
	"testing"

	"github.com/dkam/silo/store"
)

// commitOn writes a commit for a root and returns its id.
func commitOn(t *testing.T, s *Store, root store.ID, parents ...store.ID) store.ID {
	t.Helper()
	id, err := s.PutCommit(&store.Commit{Root: root, Parents: parents, CreatedAt: 1})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// chunkCount is how many chunk objects the store holds. Tests assert it is
// non-zero because the file sizes here are the only thing keeping them above
// store.InlineThreshold: a file under it lives inside its manifest, no chunk
// is written, and a census test built from small files would walk no chunks at
// all while appearing to pass.
func chunkCount(t *testing.T, s *Store) int {
	t.Helper()
	n := 0
	if err := s.chunks.List(s.storeID, func(string, int64) error { n++; return nil }); err != nil {
		t.Fatal(err)
	}
	return n
}

func mustCensus(t *testing.T, s *Store, head store.ID) Census {
	t.Helper()
	c, err := s.Census(head)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// A library with one commit has everything at head and nothing anywhere else.
//
// The floor case, and it is the one that catches a walk that double-counts:
// the head's own numbers must not also appear under History, and a store with
// nothing dead in it must report zero rather than a small number nobody can
// account for.
func TestCensusPutsASingleCommitEntirelyAtHead(t *testing.T) {
	s := plainStore(t)
	root := put(t, s, mustEmpty(t, s), "/a.bin", bytes.Repeat([]byte("a"), 200000))
	head := commitOn(t, s, root)

	c := mustCensus(t, s, head)

	if c.Head.Bytes == 0 || c.Head.Objects == 0 {
		t.Errorf("head = %+v, want the tree's objects and bytes", c.Head)
	}
	if c.History != (Extent{}) {
		t.Errorf("history = %+v, want zero: there is only one commit", c.History)
	}
	if n := chunkCount(t, s); n == 0 {
		t.Fatal("no chunks were written; the file is under the inline threshold and this test walks no chunk path")
	}
	// One object is unreferenced and should be: EmptyDir writes an empty
	// directory, put replaces the root with a tree containing the file, and
	// nothing ever commits the empty one. It is a real orphan of exactly the
	// kind this column exists to surface -- the intermediate roots a tree
	// build leaves behind.
	if c.Unreferenced.Objects != 1 {
		t.Errorf("unreferenced = %+v, want exactly the superseded empty root", c.Unreferenced)
	}
	if c.Unreferenced.Bytes >= c.Head.Bytes {
		t.Errorf("unreferenced = %d bytes, want it tiny beside head's %d", c.Unreferenced.Bytes, c.Head.Bytes)
	}
}

// Overwriting a file leaves its old bytes reachable from the old commit only.
//
// This is the number the whole census exists to produce. Quota charges the
// head, so an account that rewrites one file a hundred times reports a
// constant usage while the disk grows every time -- and until this, nothing
// could say by how much.
func TestCensusChargesSupersededBytesToHistory(t *testing.T) {
	s := plainStore(t)
	const size = 200000

	root := put(t, s, mustEmpty(t, s), "/a.bin", bytes.Repeat([]byte("a"), size))
	first := commitOn(t, s, root)
	atFirst := mustCensus(t, s, first)

	root = put(t, s, root, "/a.bin", bytes.Repeat([]byte("b"), size))
	second := commitOn(t, s, root, first)
	c := mustCensus(t, s, second)

	if c.History.Bytes == 0 {
		t.Fatal("history = 0 after an overwrite, want the superseded chunks and objects")
	}
	// The old content is the bulk of it: the superseded chunk, its manifest,
	// the old root directory and the old commit.
	if c.History.Bytes < size {
		t.Errorf("history = %d bytes, want at least the %d superseded bytes", c.History.Bytes, size)
	}
	// Head must not have grown by anything like the file: the library still
	// holds one file of one size. It grows by exactly the id the second commit
	// carries as a parent -- a parentless commit object is 38 bytes and one
	// with a parent is 70 -- so the bound is the growth of the commit object,
	// not zero.
	if grew := c.Head.Bytes - atFirst.Head.Bytes; grew >= size {
		t.Errorf("head grew by %d, want far less than the %d-byte file: it still holds one file", grew, size)
	}
	if c.Head.Objects != atFirst.Head.Objects {
		t.Errorf("head = %d objects, want the same %d: one file replaced one file",
			c.Head.Objects, atFirst.Head.Objects)
	}
}

// Unchanged content is charged once, to head, however many commits reach it.
//
// The trap this pins is summing per-commit measurements. Successive commits
// share nearly all of their objects, so a census that added up what each
// commit reaches would report a history several times the size of the disk --
// and would grow every time somebody touched an unrelated file.
func TestCensusDoesNotChargeSharedObjectsTwice(t *testing.T) {
	s := plainStore(t)
	const size = 200000

	root := put(t, s, mustEmpty(t, s), "/keep.bin", bytes.Repeat([]byte("k"), size))
	first := commitOn(t, s, root)
	atFirst := mustCensus(t, s, first)

	// A second file, committed on top. keep.bin is untouched and is reachable
	// from both commits.
	root = put(t, s, root, "/other.bin", bytes.Repeat([]byte("o"), size))
	second := commitOn(t, s, root, first)
	c := mustCensus(t, s, second)

	// The only thing the first commit reaches that the second does not is the
	// first commit object and the old root directory -- both small. keep.bin's
	// chunk is shared and belongs to head.
	if c.History.Bytes >= size {
		t.Errorf("history = %d bytes, want well under %d: nothing of that size was superseded",
			c.History.Bytes, size)
	}
	if c.Head.Bytes <= atFirst.Head.Bytes {
		t.Errorf("head = %d, want more than %d: a second file was added",
			c.Head.Bytes, atFirst.Head.Bytes)
	}
}

// An object no commit reaches is unreferenced, and is counted apart from
// history.
//
// They are separated because they want different actions. History is
// reclaimable only by a retention policy somebody has to choose; an
// unreferenced object -- an interrupted upload, an abandoned commit -- is
// reclaimable now, with no decision attached.
func TestCensusCountsWhatNoCommitReaches(t *testing.T) {
	s := plainStore(t)

	root := put(t, s, mustEmpty(t, s), "/a.bin", bytes.Repeat([]byte("a"), 200000))
	head := commitOn(t, s, root)
	before := mustCensus(t, s, head)

	// A chunk written and never committed: the upload that stopped halfway.
	orphan := bytes.Repeat([]byte("x"), 200000)
	if _, err := s.WriteFile(bytes.NewReader(orphan)); err != nil {
		t.Fatal(err)
	}

	c := mustCensus(t, s, head)

	if c.Unreferenced.Bytes == 0 {
		t.Fatal("unreferenced = 0, want the orphaned chunk")
	}
	if c.Head != before.Head {
		t.Errorf("head = %+v, want it unchanged at %+v: nothing was committed",
			c.Head, before.Head)
	}
	if c.History != (Extent{}) {
		t.Errorf("history = %+v, want zero: the orphan is reachable from no commit at all", c.History)
	}
}

// The census works on a library the server cannot read.
//
// Every edge it follows is published in both library types -- a commit gives
// up its root and parents, a directory its entries, a manifest its chunk list
// -- so an E2EE library is measurable without its content key. That is the
// ordinary case for a server holding one, and a census that needed the key
// would be a census that never ran.
func TestCensusMeasuresAnEncryptedLibraryWithoutItsKey(t *testing.T) {
	dir := t.TempDir()
	params := store.DefaultParams(store.ChunkerSeed(testCK))
	s, err := New(Config{DataDir: dir, StoreID: testStoreID, E2EE: true, CK: testCK, Params: params})
	if err != nil {
		t.Fatal(err)
	}
	root := put(t, s, mustEmpty(t, s), "/a.bin", bytes.Repeat([]byte("a"), 200000))
	head := commitOn(t, s, root)

	// The same objects, opened by a server that was never given the key.
	blind, err := New(Config{DataDir: dir, StoreID: testStoreID, E2EE: true, Params: params})
	if err != nil {
		t.Fatal(err)
	}
	if blind.HasKey() {
		t.Fatal("the blind store has a key; the test is not testing what it says")
	}

	c, err := blind.Census(head)
	if err != nil {
		t.Fatalf("Census without a key: %v", err)
	}
	if c.Head.Bytes == 0 || c.Head.Objects == 0 {
		t.Errorf("head = %+v, want the tree's objects and bytes", c.Head)
	}
}

// Every byte on disk is in exactly one of the three columns.
//
// The property that makes the report trustworthy: the columns are a partition
// of the store, not three overlapping estimates. If they do not add up, either
// something is counted twice or something is invisible, and both are worse
// than a wrong total because neither shows.
func TestCensusColumnsPartitionTheStore(t *testing.T) {
	s := plainStore(t)

	root := put(t, s, mustEmpty(t, s), "/a.bin", bytes.Repeat([]byte("a"), 200000))
	first := commitOn(t, s, root)
	root = put(t, s, root, "/a.bin", bytes.Repeat([]byte("b"), 200000))
	root = put(t, s, root, "/c.bin", bytes.Repeat([]byte("c"), 200000))
	head := commitOn(t, s, root, first)
	if _, err := s.WriteFile(bytes.NewReader(bytes.Repeat([]byte("x"), 200000))); err != nil {
		t.Fatal(err)
	}

	c := mustCensus(t, s, head)

	var diskBytes, diskObjects int64
	for _, st := range s.stores() {
		if err := st.List(s.storeID, func(_ string, size int64) error {
			diskBytes += size
			diskObjects++
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}

	sum := c.Head.Bytes + c.History.Bytes + c.Unreferenced.Bytes
	if sum != diskBytes {
		t.Errorf("head+history+unreferenced = %d bytes, disk holds %d", sum, diskBytes)
	}
	sumObjects := c.Head.Objects + c.History.Objects + c.Unreferenced.Objects
	if sumObjects != diskObjects {
		t.Errorf("head+history+unreferenced = %d objects, disk holds %d", sumObjects, diskObjects)
	}
}
