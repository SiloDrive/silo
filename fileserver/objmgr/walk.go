package objmgr

import (
	"fmt"

	"github.com/dkam/silo/store"
)

// A tree is not a tree.
//
// Every walk in this package was written against the picture in the name: a
// root, its children, their children, each object reached once on the way
// down. What the format actually gives is a DAG. An id names bytes, so two
// entries holding the same id name the same object, and nothing in the format
// forbids it — nothing should, because it is what makes a copied directory
// free and a rename invisible.
//
// The cost is that paths multiply where objects do not. A chain of forty
// directories, each naming the one below it twice, is forty objects, a few
// kilobytes on disk, and 2^40 paths through them. Every object verifies, every
// name is unique in its directory, every rule the codec has is satisfied. A
// client builds it with ordinary PUTs and moves the head onto it, and a walk
// that follows paths never comes back.
//
// So each walk here is bounded, and the three of them are bounded differently,
// because what they compute differs in whether a repeat visit is redundant:
//
//   - Verifying is idempotent. A subtree that is complete is complete however
//     many paths reach it, so a visited set is exact and costs nothing.
//   - Measuring is not — the same directory under two names is two copies and
//     weighs twice — but the total for an id is a pure function of the id, so
//     the answer is memoised and added as often as it is reached. Also exact.
//   - Diffing is neither. Each path is a distinct line of output, so there is
//     nothing to dedupe and nothing to memoise: an honest answer for a bomb is
//     exponentially long. That one is capped, and refuses rather than truncates.
//
// The first two end up bounded by the number of distinct objects a library
// holds, which is the bound that belongs on them: a real library with ten
// million objects still measures, and a bomb with forty costs forty.
type walk struct {
	// nodes is the set of subtrees already fully handled, for walks where
	// handling one twice is redundant.
	nodes map[store.ID]struct{}
	// pairs is the same for the two-tree walks, keyed on both ids.
	pairs map[[2]store.ID]struct{}
	// usage memoises what a subtree totals, for the walk that must count a
	// repeat rather than skip it.
	usage map[store.ID]Usage
	// deltas is the same for a pair of subtrees.
	deltas map[[2]store.ID]Usage
	// entries caches directory listings. Only the capped walk keeps one: for
	// the others the dedupe already prevents the second read, and holding
	// every directory of a large library in memory to prove it would cost
	// more than the walk.
	entries map[store.ID][]store.DirEntry

	steps int
	limit int
	what  string
}

// maxDiffSteps bounds the walk that cannot dedupe.
//
// It is a ceiling on the work, not a page size — the changes endpoint pages
// its answer, but it computes the whole diff to do it, so the diff is what has
// to be finite. Two million is far past any real change set (a client
// enumerating a fresh library from empty pays one step per path, and a library
// with two million paths has other problems with this endpoint) and far short
// of what a bomb wants, which is unbounded.
const maxDiffSteps = 2_000_000

// TooLargeError reports a walk that ran past its ceiling. It is a client error:
// the tree is the client's, and no answer of a workable size exists for it.
type TooLargeError struct {
	What  string
	Limit int
}

func (e *TooLargeError) Error() string {
	return fmt.Sprintf("%s reached %d steps and stopped: the tree it was given has more paths through it than objects in it", e.What, e.Limit)
}

// newVerifyWalk bounds by dedupe: a subtree verified once is verified.
func newVerifyWalk() *walk {
	return &walk{
		nodes: make(map[store.ID]struct{}),
		pairs: make(map[[2]store.ID]struct{}),
	}
}

// newMeasureWalk bounds by memo: a repeat visit still counts, but it costs a
// map lookup rather than a subtree.
func newMeasureWalk() *walk {
	return &walk{
		usage:  make(map[store.ID]Usage),
		deltas: make(map[[2]store.ID]Usage),
	}
}

// newDiffWalk bounds by ceiling, since neither dedupe nor memo applies to a
// listing keyed on path. The listing cache is what makes hitting the ceiling
// cheap: forty reads rather than two million.
func newDiffWalk() *walk {
	return &walk{
		entries: make(map[store.ID][]store.DirEntry),
		limit:   maxDiffSteps,
		what:    "the changes walk",
	}
}

// step charges one unit of work. A walk with no limit is one whose dedupe or
// memo has already made it finite, and counting there would only invent a
// second ceiling for a library to grow into.
func (w *walk) step() error {
	if w.limit == 0 {
		return nil
	}
	w.steps++
	if w.steps > w.limit {
		return &TooLargeError{What: w.what, Limit: w.limit}
	}
	return nil
}

// enterNode reports whether this subtree still needs walking, and marks it
// walked. Only the dedupe walk has a set; the others always say yes.
func (w *walk) enterNode(id store.ID) bool {
	if w.nodes == nil {
		return true
	}
	if _, done := w.nodes[id]; done {
		return false
	}
	w.nodes[id] = struct{}{}
	return true
}

// enterPair is enterNode for the two-tree walks.
func (w *walk) enterPair(oldID, newID store.ID) bool {
	if w.pairs == nil {
		return true
	}
	key := [2]store.ID{oldID, newID}
	if _, done := w.pairs[key]; done {
		return false
	}
	w.pairs[key] = struct{}{}
	return true
}

// listing reads a directory's public entries, through the walk's cache when it
// keeps one.
func (w *walk) listing(s *Store, id store.ID) ([]store.DirEntry, error) {
	if w.entries != nil {
		if e, ok := w.entries[id]; ok {
			return e, nil
		}
	}
	e, err := s.publicEntries(id)
	if err != nil {
		return nil, err
	}
	if w.entries != nil {
		w.entries[id] = e
	}
	return e, nil
}
