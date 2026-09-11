package objmgr

import (
	"bytes"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/dkam/silo/store"
)

// dagBomb builds a chain of directories where each level names the level below
// it twice, under two different names.
//
// It is a legal tree by every rule the format has: every object is
// well-formed, every id verifies, every name is unique within its directory,
// and the whole thing is a few kilobytes on disk. It is not a legal tree by the
// only rule that matters to a walk — that the thing being walked is a tree.
// A DAG's paths multiply where its objects do not, so n levels of this is n
// objects and 2^n paths through them.
//
// A client can build one with ordinary PUTs, because content addressing means
// naming a child twice costs nothing and the server has no reason to refuse it.
func dagBomb(t *testing.T, s *Store, levels int) store.ID {
	t.Helper()

	id, err := s.EmptyDir()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < levels; i++ {
		d := &store.Directory{Entries: []store.DirEntry{
			{ChildID: id, Type: store.NodeDir, Name: []byte("a" + strconv.Itoa(i)), Mode: 0o755},
			{ChildID: id, Type: store.NodeDir, Name: []byte("b" + strconv.Itoa(i)), Mode: 0o755},
		}}
		id, err = s.PutDirectory(d)
		if err != nil {
			t.Fatal(err)
		}
	}
	return id
}

// withinTheDeadline runs fn and fails the test if it has not returned in time.
// A walk that is still going is the finding: nothing bounds it, so it does not
// end, and the only way to report that is to stop waiting.
func withinTheDeadline(t *testing.T, what string, fn func()) {
	t.Helper()

	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s was still running after 5s on a 40-object tree", what)
	}
}

// Forty objects, one PUT of the head, and a core is gone until the process is.
// The walk must be bounded by the objects a tree holds rather than by the paths
// through it.
func TestVerifyDeltaSurvivesADAGBomb(t *testing.T) {
	s := plainStore(t)
	empty := mustEmpty(t, s)
	bomb := dagBomb(t, s, 40)

	withinTheDeadline(t, "VerifyDelta", func() {
		if err := s.VerifyDelta(empty, bomb); err != nil {
			t.Errorf("VerifyDelta on a complete tree: %v", err)
		}
	})
}

func TestMeasureSurvivesADAGBomb(t *testing.T) {
	s := plainStore(t)
	bomb := dagBomb(t, s, 40)

	withinTheDeadline(t, "Measure", func() {
		if _, err := s.Measure(bomb); err != nil {
			t.Errorf("Measure on a complete tree: %v", err)
		}
	})
}

func TestDiffSurvivesADAGBomb(t *testing.T) {
	s := plainStore(t)
	empty := mustEmpty(t, s)
	bomb := dagBomb(t, s, 40)

	withinTheDeadline(t, "Diff", func() {
		// A refusal, not a truncation. There is no listing of this tree that
		// is both complete and finite, and a short answer presented as a
		// complete one would have a client delete files it still has.
		_, err := s.Diff(empty, bomb)
		var tooLarge *TooLargeError
		if !errors.As(err, &tooLarge) {
			t.Errorf("Diff on a DAG bomb returned %v, want a TooLargeError", err)
		}
	})
}

// The walk that was hanging now returns, and the number it returns has to be
// one the quota check can use. A file under a directory reached 2^63 times is
// counted 2^63 times — correctly — and that does not fit. Wrapping would turn
// the bomb into a negative delta, which is a quota check that admits it.
func TestMeasureRefusesATotalThatDoesNotFit(t *testing.T) {
	s := plainStore(t)
	root := mustEmpty(t, s)
	root = put(t, s, root, "/f.txt", bytes.Repeat([]byte("f"), 100))

	id := root
	var err error
	for i := 0; i < 63; i++ {
		d := &store.Directory{Entries: []store.DirEntry{
			{ChildID: id, Type: store.NodeDir, Name: []byte("a" + strconv.Itoa(i)), Mode: 0o755},
			{ChildID: id, Type: store.NodeDir, Name: []byte("b" + strconv.Itoa(i)), Mode: 0o755},
		}}
		id, err = s.PutDirectory(d)
		if err != nil {
			t.Fatal(err)
		}
	}

	u, err := s.Measure(id)
	if !errors.Is(err, ErrTotalTooLarge) {
		t.Fatalf("Measure returned %+v, %v; want ErrTotalTooLarge", u, err)
	}

	// And the same through the delta lane, which is the one the head move
	// actually charges against.
	if _, err := s.MeasureDelta(mustEmpty(t, s), id); !errors.Is(err, ErrTotalTooLarge) {
		t.Errorf("MeasureDelta returned %v, want ErrTotalTooLarge", err)
	}
}
