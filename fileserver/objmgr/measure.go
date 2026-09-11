package objmgr

import (
	"errors"
	"math"

	"github.com/dkam/silo/store"
)

// Usage is what a tree holds: the logical size of the files it reaches, and
// how many there are.
//
// Logical size is the sum of file_size over reachable manifests — what a user
// would get back by deleting the files. It is deliberately not stored bytes
// and not disk. Under content-defined chunking, dedup and deferred compaction
// those three numbers diverge by multiples in both directions, and only this
// one holds still while the server works: a compaction run must never change
// what somebody is charged.
//
// It costs no key to compute, in either library type, because file_size lives
// in a manifest's public section by design. One path covers plain and E2EE
// with no branch.
type Usage struct {
	Size      int64
	FileCount int64
}

// Add lets a delta be applied to a total without the caller remembering that
// there are two numbers travelling together. A delta is an ordinary Usage with
// negative fields.
func (u Usage) Add(d Usage) Usage {
	return Usage{Size: u.Size + d.Size, FileCount: u.FileCount + d.FileCount}
}

// ErrTotalTooLarge reports a tree whose total does not fit in the numbers that
// carry it.
//
// Not caution about large libraries: 2^63 bytes is more than any of them, and
// no honest tree gets near it. It is the DAG again. A file under a directory
// reached 2^40 times IS reachable 2^40 times, so the total is arithmetically
// right and does not fit — and the only wrong answer available at that point is
// the one that wraps. A wrapped total is a negative delta, and a negative delta
// walks through the quota check that a positive one would have stopped. Better
// to refuse the tree than to be charged a lie.
var ErrTotalTooLarge = errors.New("objmgr: the tree totals more than can be counted")

// accumulate adds sign*d to u, or refuses. It is used wherever subtree totals
// are summed; the ordinary small arithmetic — one manifest's size against
// another's — is left alone.
func accumulate(u *Usage, d Usage, sign int64) error {
	size, sizeOK := addWithinRange(u.Size, sign*d.Size)
	count, countOK := addWithinRange(u.FileCount, sign*d.FileCount)
	if !sizeOK || !countOK {
		return ErrTotalTooLarge
	}
	u.Size, u.FileCount = size, count
	return nil
}

func addWithinRange(a, b int64) (int64, bool) {
	if b > 0 && a > math.MaxInt64-b {
		return 0, false
	}
	if b < 0 && a < math.MinInt64-b {
		return 0, false
	}
	return a + b, true
}

// Measure totals a whole tree.
//
// This is the repair path and not the routine one: it reads every directory
// and every manifest, so it costs the size of the library rather than the size
// of a change. Use it when there is nothing to measure against — a library
// with no recorded total, or one whose recorded root is no longer in the store
// because history was cut out from under it. MeasureDelta is the steady state.
// A DAG bomb costs what its objects cost: see [walk]. A directory reached
// twice is two copies and weighs twice — skipping the repeat would be the
// wrong number — but what a subtree totals is a pure function of its id, so
// the answer is remembered and added again rather than recomputed.
func (s *Store) Measure(root store.ID) (Usage, error) {
	return s.measure(newMeasureWalk(), root)
}

func (s *Store) measure(w *walk, root store.ID) (Usage, error) {
	if u, ok := w.usage[root]; ok {
		return u, nil
	}

	var u Usage
	entries, err := w.listing(s, root)
	if err != nil {
		return Usage{}, err
	}
	for _, e := range entries {
		d, err := s.measureEntry(w, e)
		if err != nil {
			return Usage{}, err
		}
		if err := accumulate(&u, d, +1); err != nil {
			return Usage{}, err
		}
	}
	w.usage[root] = u
	return u, nil
}

// MeasureDelta reports how the totals change moving from one root to another.
//
// It is the same Merkle walk the diff is: a subtree whose id is unchanged
// contributes nothing, whatever it holds, so it is skipped without being read.
// That is what makes accounting affordable at every head move — a one-file
// change in a million-file library reads the directories along one path and
// the two manifests, and the totals stay exact.
//
// Exact is the word that matters. This is not an estimate that drifts and
// needs periodic correction; it is the difference between two trees, computed
// from the trees. A total maintained by adding these deltas is the same number
// Measure would produce, which is why Measure is only ever a repair.
func (s *Store) MeasureDelta(oldRoot, newRoot store.ID) (Usage, error) {
	w := newMeasureWalk()
	d, err := s.deltaDir(w, oldRoot, newRoot)
	if err != nil {
		return Usage{}, err
	}
	return d, nil
}

// deltaDir returns what one pair of directories contributes, memoised on the
// pair for the same reason Measure memoises on the id: a pair reached twice
// contributes twice, so the answer is remembered and added again.
func (s *Store) deltaDir(w *walk, oldID, newID store.ID) (Usage, error) {
	key := [2]store.ID{oldID, newID}
	if d, ok := w.deltas[key]; ok {
		return d, nil
	}

	var u Usage
	err := s.mergeDirs(w, oldID, newID,
		func(o store.DirEntry) error { return s.applyEntry(w, o, &u, -1) },
		func(n store.DirEntry) error { return s.applyEntry(w, n, &u, +1) },
		func(o, n store.DirEntry) error { return s.deltaEntry(w, o, n, &u) },
	)
	if err != nil {
		return Usage{}, err
	}
	w.deltas[key] = u
	return u, nil
}

// deltaEntry accounts for two entries that share a name.
func (s *Store) deltaEntry(w *walk, o, n store.DirEntry, u *Usage) error {
	if o.ChildID == n.ChildID && o.Type == n.Type {
		return nil
	}

	// A name that changed type is a removal and an addition, and has to be
	// accounted as both: the old thing's bytes stop being reachable whatever
	// the new thing is.
	if o.Type != n.Type {
		if err := s.applyEntry(w, o, u, -1); err != nil {
			return err
		}
		return s.applyEntry(w, n, u, +1)
	}

	if n.Type == store.NodeDir {
		d, err := s.deltaDir(w, o.ChildID, n.ChildID)
		if err != nil {
			return err
		}
		return accumulate(u, d, +1)
	}

	// A file whose content changed: its count is unchanged and only the
	// difference in size moves. A rename is invisible here, correctly — the
	// same id under a different name is the same bytes and charges the same.
	oldSize, err := s.fileSize(o.ChildID)
	if err != nil {
		return err
	}
	newSize, err := s.fileSize(n.ChildID)
	if err != nil {
		return err
	}
	u.Size += newSize - oldSize
	return nil
}

// applyEntry adds a whole entry to the running total, or takes it away.
func (s *Store) applyEntry(w *walk, e store.DirEntry, u *Usage, sign int64) error {
	d, err := s.measureEntry(w, e)
	if err != nil {
		return err
	}
	return accumulate(u, d, sign)
}

// measureEntry totals one entry, and everything beneath it when it is a
// directory.
//
// Directories themselves weigh nothing. A library's size is the bytes it
// holds, and a directory holds none: charging for the object that records the
// names would make an empty tree cost something and make the number depend on
// how the client chose to arrange it.
func (s *Store) measureEntry(w *walk, e store.DirEntry) (Usage, error) {
	if e.Type == store.NodeDir {
		return s.measure(w, e.ChildID)
	}
	size, err := s.fileSize(e.ChildID)
	if err != nil {
		return Usage{}, err
	}
	return Usage{Size: size, FileCount: 1}, nil
}
