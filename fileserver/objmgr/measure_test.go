package objmgr

import (
	"bytes"
	"testing"

	"github.com/dkam/silo/store"
)

func mustEmpty(t *testing.T, s *Store) store.ID {
	t.Helper()
	root, err := s.EmptyDir()
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func mustMeasure(t *testing.T, s *Store, root store.ID) Usage {
	t.Helper()
	u, err := s.Measure(root)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestMeasureTotalsTheFilesATreeReaches(t *testing.T) {
	s := plainStore(t)
	root := mustEmpty(t, s)

	if u := mustMeasure(t, s, root); u != (Usage{}) {
		t.Fatalf("empty library measures %+v, want zero", u)
	}

	root = put(t, s, root, "/a.txt", bytes.Repeat([]byte("a"), 100))
	var err error
	root, err = s.Mkdir(root, "/sub", 0o755, 1)
	if err != nil {
		t.Fatal(err)
	}
	root = put(t, s, root, "/sub/b.txt", bytes.Repeat([]byte("b"), 250))

	// The directory itself weighs nothing: 350 bytes in two files.
	if u := mustMeasure(t, s, root); u != (Usage{Size: 350, FileCount: 2}) {
		t.Fatalf("measured %+v, want 350 bytes in 2 files", u)
	}
}

// The delta between two roots must be exactly the difference between their
// totals, whatever the shape of the change. That equality is the whole
// contract: a total maintained by adding deltas has to stay the number a walk
// would produce, or accounting drifts and nobody notices until a user is
// charged for a file they deleted.
func TestDeltaAgreesWithAFullWalk(t *testing.T) {
	s := plainStore(t)
	root := mustEmpty(t, s)
	root = put(t, s, root, "/keep.txt", bytes.Repeat([]byte("k"), 10))
	root = put(t, s, root, "/edit.txt", bytes.Repeat([]byte("e"), 100))
	var err error
	root, err = s.Mkdir(root, "/sub", 0o755, 1)
	if err != nil {
		t.Fatal(err)
	}
	root = put(t, s, root, "/sub/gone.txt", bytes.Repeat([]byte("g"), 40))
	root, err = s.Mkdir(root, "/sub/deep", 0o755, 1)
	if err != nil {
		t.Fatal(err)
	}
	root = put(t, s, root, "/sub/deep/x.txt", bytes.Repeat([]byte("x"), 7))
	base := root

	// Grow one file, shrink another, add one, delete one, and rename a third.
	root = put(t, s, root, "/edit.txt", bytes.Repeat([]byte("E"), 30))
	root = put(t, s, root, "/new.txt", bytes.Repeat([]byte("n"), 500))
	root, err = s.Remove(root, "/sub/gone.txt", 2)
	if err != nil {
		t.Fatal(err)
	}
	root, err = s.Rename(root, "/keep.txt", "/kept.txt", 2)
	if err != nil {
		t.Fatal(err)
	}

	before := mustMeasure(t, s, base)
	after := mustMeasure(t, s, root)
	delta, err := s.MeasureDelta(base, root)
	if err != nil {
		t.Fatal(err)
	}
	if got := before.Add(delta); got != after {
		t.Fatalf("base %+v plus delta %+v is %+v, but a walk of the new tree says %+v", before, delta, got, after)
	}

	// And it runs the other way, which is what a revert costs.
	back, err := s.MeasureDelta(root, base)
	if err != nil {
		t.Fatal(err)
	}
	if got := after.Add(back); got != before {
		t.Fatalf("reverse delta %+v does not undo: got %+v, want %+v", back, got, before)
	}
}

// A rename charges nothing. The same id under a different name is the same
// bytes, and a client that reorganises a library must not watch its usage move.
func TestARenameIsFree(t *testing.T) {
	s := plainStore(t)
	root := mustEmpty(t, s)
	root = put(t, s, root, "/a.txt", bytes.Repeat([]byte("a"), 64))
	base := root

	root, err := s.Rename(root, "/a.txt", "/b.txt", 2)
	if err != nil {
		t.Fatal(err)
	}
	delta, err := s.MeasureDelta(base, root)
	if err != nil {
		t.Fatal(err)
	}
	if delta != (Usage{}) {
		t.Fatalf("rename charged %+v, want nothing", delta)
	}
}

// The same content stored twice under two names is charged twice. Dedup is not
// a discount: the file is there twice as far as the user is concerned, and
// deleting one must give back its bytes.
func TestTwoCopiesOfOneFileAreChargedTwice(t *testing.T) {
	s := plainStore(t)
	root := mustEmpty(t, s)
	content := bytes.Repeat([]byte("dup"), 1000)
	root = put(t, s, root, "/one.txt", content)
	root = put(t, s, root, "/two.txt", content)

	if u := mustMeasure(t, s, root); u != (Usage{Size: 6000, FileCount: 2}) {
		t.Fatalf("measured %+v, want both copies charged", u)
	}
}

// A directory replaced by a file, and the reverse: the old thing's bytes stop
// being reachable whatever the new thing is, so both sides have to be
// accounted even though the name did not change.
func TestATypeChangeAccountsBothSides(t *testing.T) {
	s := plainStore(t)
	root := mustEmpty(t, s)
	var err error
	root, err = s.Mkdir(root, "/x", 0o755, 1)
	if err != nil {
		t.Fatal(err)
	}
	root = put(t, s, root, "/x/inner.txt", bytes.Repeat([]byte("i"), 900))
	base := root

	root, err = s.Remove(root, "/x", 2)
	if err != nil {
		t.Fatal(err)
	}
	root = put(t, s, root, "/x", bytes.Repeat([]byte("f"), 5))

	before := mustMeasure(t, s, base)
	after := mustMeasure(t, s, root)
	delta, err := s.MeasureDelta(base, root)
	if err != nil {
		t.Fatal(err)
	}
	if got := before.Add(delta); got != after {
		t.Fatalf("type change: %+v plus %+v is %+v, want %+v", before, delta, got, after)
	}
	if after != (Usage{Size: 5, FileCount: 1}) {
		t.Fatalf("after the swap: %+v", after)
	}
}

// The server bills an encrypted library it cannot read. file_size is public by
// design, so this is the same code path with no branch — and it has to be,
// because a quota that only worked on libraries the server can open would make
// encryption the way to store for free.
func TestTheServerCanBillALibraryItCannotRead(t *testing.T) {
	dir := storeDir(t)
	client, err := New(Config{
		DataDir: dir, StoreID: testStoreID, E2EE: true, CK: testCK,
		Params: store.DefaultParams(store.ChunkerSeed(testCK)),
	})
	if err != nil {
		t.Fatal(err)
	}
	root := mustEmpty(t, client)
	root = put(t, client, root, "/secret.txt", bytes.Repeat([]byte("s"), 4096))
	base := root
	root = put(t, client, root, "/more.txt", bytes.Repeat([]byte("m"), 1024))

	server := serverView(t, dir)
	u, err := server.Measure(root)
	if err != nil {
		t.Fatalf("the server view could not measure an encrypted library: %v", err)
	}
	if u != (Usage{Size: 5120, FileCount: 2}) {
		t.Fatalf("measured %+v, want the plaintext sizes the manifests declare", u)
	}
	delta, err := server.MeasureDelta(base, root)
	if err != nil {
		t.Fatal(err)
	}
	if delta != (Usage{Size: 1024, FileCount: 1}) {
		t.Fatalf("delta %+v, want one 1024-byte file", delta)
	}
}
