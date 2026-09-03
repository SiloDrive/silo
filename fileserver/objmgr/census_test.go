package objmgr

import (
	"bytes"
	"testing"

	"github.com/dkam/silo/fileserver/objstore"
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
	if err := s.chunks.List(s.storeID, func(objstore.ObjectInfo) error { n++; return nil }); err != nil {
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
		if err := st.List(s.storeID, func(o objstore.ObjectInfo) error {
			diskBytes += o.Size
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

// Unreferenced yields exactly what the census counts in its third column, and
// yields it as objects a caller can act on rather than as a total.
//
// The two have to agree or the report and the collector are describing
// different stores -- the failure where somebody reads a number, runs the
// thing that acts on it, and gets a different set.
func TestUnreferencedYieldsWhatTheCensusCounts(t *testing.T) {
	s := plainStore(t)

	root := put(t, s, mustEmpty(t, s), "/a.bin", bytes.Repeat([]byte("a"), 200000))
	head := commitOn(t, s, root)
	if _, err := s.WriteFile(bytes.NewReader(bytes.Repeat([]byte("x"), 200000))); err != nil {
		t.Fatal(err)
	}

	c := mustCensus(t, s, head)

	var got Extent
	if err := s.Unreferenced(head, func(o Orphan) error {
		got = got.add(o.Size)
		return nil
	}); err != nil {
		t.Fatalf("Unreferenced: %v", err)
	}

	if got != c.Unreferenced {
		t.Errorf("Unreferenced yielded %+v, census counted %+v", got, c.Unreferenced)
	}
	if got.Objects == 0 {
		t.Fatal("nothing was yielded; the test is asserting two zeros are equal")
	}
}

// Nothing reachable is ever yielded, including from an older commit.
//
// The property the collector's safety rests on. A chunk that only the previous
// commit reaches is history, not garbage: it is reclaimed by a retention
// policy somebody chose, never by a sweep that could not tell the difference.
func TestUnreferencedNeverYieldsSomethingACommitReaches(t *testing.T) {
	s := plainStore(t)
	const size = 200000

	root := put(t, s, mustEmpty(t, s), "/a.bin", bytes.Repeat([]byte("a"), size))
	first := commitOn(t, s, root)
	root = put(t, s, root, "/a.bin", bytes.Repeat([]byte("b"), size))
	head := commitOn(t, s, root, first)

	reachable, err := s.reachable([]store.ID{head}, true, nil)
	if err != nil {
		t.Fatal(err)
	}

	var yielded int
	if err := s.Unreferenced(head, func(o Orphan) error {
		if reachable.has(o.ID, o.IsChunk) {
			t.Errorf("Unreferenced yielded %s, which a commit reaches", o.ID[:12])
		}
		yielded++
		return nil
	}); err != nil {
		t.Fatalf("Unreferenced: %v", err)
	}

	// The superseded chunk from the first commit must NOT be in there: it is
	// history. Only the empty root that was never committed should be.
	c := mustCensus(t, s, head)
	if c.History.Bytes < size {
		t.Fatalf("history = %d, want the superseded %d bytes; the fixture is wrong", c.History.Bytes, size)
	}
	if yielded != int(c.Unreferenced.Objects) {
		t.Errorf("yielded %d objects, census counted %d", yielded, c.Unreferenced.Objects)
	}
}

// A commit object that is gone ends that branch of the walk instead of failing
// it, because that is what a retention boundary looks like from here.
//
// The distinction this pins is which missing object is an error. A commit the
// store no longer holds is the ordinary result of history being expired --
// walkHistory and changes?since= already treat it that way -- so a census that
// failed on it would start erroring the day retention first ran, on every
// library it had touched. A missing *directory or manifest* is the opposite:
// nothing collects those without collecting the commit that reaches them
// first, so one that has gone missing is damage and must not be reported as a
// smaller store.
func TestCensusTreatsAMissingCommitAsTheEndOfHistory(t *testing.T) {
	s := plainStore(t)
	const size = 200000

	root := put(t, s, mustEmpty(t, s), "/a.bin", bytes.Repeat([]byte("a"), size))
	first := commitOn(t, s, root)
	root = put(t, s, root, "/a.bin", bytes.Repeat([]byte("b"), size))
	head := commitOn(t, s, root, first)

	before := mustCensus(t, s, head)
	if before.History.Bytes < size {
		t.Fatalf("history = %d, want the superseded %d; the fixture is wrong", before.History.Bytes, size)
	}

	// Expire the first commit, which is what retention plus a compaction run
	// does. Remove alone cannot: the commit is inside a pack.
	expunge(t, s, first)

	after, err := s.Census(head)
	if err != nil {
		t.Fatalf("Census after the oldest commit was expired: %v", err)
	}
	if after.Head != before.Head {
		t.Errorf("head = %+v, want it unchanged at %+v", after.Head, before.Head)
	}
	// What the expired commit exclusively reached is now reachable from
	// nothing, so it moves from history to unreferenced -- where the sweep
	// will collect it.
	if after.History.Bytes != 0 {
		t.Errorf("history = %+v, want zero: the only old commit is gone", after.History)
	}
	if after.Unreferenced.Bytes < size {
		t.Errorf("unreferenced = %d, want at least the %d bytes the expired commit held",
			after.Unreferenced.Bytes, size)
	}
}

// A missing directory is damage and is reported as an error, not as a store
// that got smaller.
func TestCensusFailsOnAMissingDirectory(t *testing.T) {
	s := plainStore(t)
	root := put(t, s, mustEmpty(t, s), "/a.bin", bytes.Repeat([]byte("a"), 200000))
	head := commitOn(t, s, root)

	expunge(t, s, root)

	if _, err := s.Census(head); err == nil {
		t.Error("Census succeeded with the root directory missing; damage must not read as a smaller store")
	}
}

// A file small enough to live inside its manifest is counted once, in the
// manifest's own size, and contributes no chunk.
//
// Every other test here uses 200 KB files specifically to get past
// store.InlineThreshold and exercise the chunk path. That makes the common
// case -- most files in most libraries are under 64 KiB -- the one nothing
// covered. An inlined manifest that was also credited with a chunk would
// double-count, and a walk that expected chunks and found none could as easily
// have skipped the object.
func TestCensusCountsAnInlinedFileWithoutAChunk(t *testing.T) {
	s := plainStore(t)
	root := put(t, s, mustEmpty(t, s), "/small.txt", bytes.Repeat([]byte("s"), 100))
	head := commitOn(t, s, root)

	if n := chunkCount(t, s); n != 0 {
		t.Fatalf("%d chunks written for a 100-byte file; it should be inlined", n)
	}

	c := mustCensus(t, s, head)
	if c.Head.Bytes == 0 || c.Head.Objects == 0 {
		t.Errorf("head = %+v, want the commit, the root and the inlined manifest", c.Head)
	}
	// The partition still has to hold with one of the two stores empty.
	var disk int64
	for _, st := range s.stores() {
		if err := st.List(s.storeID, func(o objstore.ObjectInfo) error { disk += o.Size; return nil }); err != nil {
			t.Fatal(err)
		}
	}
	if sum := c.Head.Bytes + c.History.Bytes + c.Unreferenced.Bytes; sum != disk {
		t.Errorf("columns sum to %d, disk holds %d", sum, disk)
	}
}

// A merge commit's second parent is reached by the mark, which is what keeps
// the collector honest if merges ever arrive.
//
// Nothing in this server writes a commit with more than one parent today, and
// the history walks take the first parent by design. This walk does not: it
// follows every parent, because the question it answers is "what is still
// referenced", and a second-parent branch that the mark could not see would be
// offered to the sweep for deletion while a commit still pointed at it. The
// asymmetry between the two walks is deliberate, and this is the test that
// says so out loud.
func TestReachableFollowsEveryParentOfAMerge(t *testing.T) {
	s := plainStore(t)
	const size = 200000

	base := mustEmpty(t, s)
	left := put(t, s, base, "/left.bin", bytes.Repeat([]byte("l"), size))
	leftCommit := commitOn(t, s, left)
	right := put(t, s, base, "/right.bin", bytes.Repeat([]byte("r"), size))
	rightCommit := commitOn(t, s, right)

	merged := put(t, s, left, "/right.bin", bytes.Repeat([]byte("r"), size))
	head := commitOn(t, s, merged, leftCommit, rightCommit)

	m, err := s.reachable([]store.ID{head}, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !m.has(rightCommit.String(), false) {
		t.Error("the second parent was not reached; the sweep would offer it for deletion")
	}
	if !m.has(leftCommit.String(), false) {
		t.Error("the first parent was not reached")
	}

	// Which means nothing in a merged history is ever a sweep candidate.
	if err := s.Unreferenced(head, func(o Orphan) error {
		if o.ID == rightCommit.String() || o.ID == leftCommit.String() {
			t.Errorf("%s was offered for deletion but a merge still reaches it", o.ID[:12])
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
