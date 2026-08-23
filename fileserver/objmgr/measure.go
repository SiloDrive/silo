package objmgr

import "github.com/dkam/silo/store"

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

// Add and Sub let a delta be applied to a total without either caller
// remembering that there are two numbers travelling together. A delta is an
// ordinary Usage with negative fields.
func (u Usage) Add(d Usage) Usage {
	return Usage{Size: u.Size + d.Size, FileCount: u.FileCount + d.FileCount}
}

func (u Usage) Sub(d Usage) Usage {
	return Usage{Size: u.Size - d.Size, FileCount: u.FileCount - d.FileCount}
}

// Measure totals a whole tree.
//
// This is the repair path and not the routine one: it reads every directory
// and every manifest, so it costs the size of the library rather than the size
// of a change. Use it when there is nothing to measure against — a library
// with no recorded total, or one whose recorded root is no longer in the store
// because history was cut out from under it. MeasureDelta is the steady state.
func (s *Store) Measure(root store.ID) (Usage, error) {
	var u Usage
	entries, err := s.publicEntries(root)
	if err != nil {
		return Usage{}, err
	}
	for _, e := range entries {
		d, err := s.measureEntry(e)
		if err != nil {
			return Usage{}, err
		}
		u = u.Add(d)
	}
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
	var u Usage
	if err := s.deltaDir(oldRoot, newRoot, &u); err != nil {
		return Usage{}, err
	}
	return u, nil
}

func (s *Store) deltaDir(oldID, newID store.ID, u *Usage) error {
	if oldID == newID {
		return nil
	}
	oldEntries, err := s.publicEntries(oldID)
	if err != nil {
		return err
	}
	newEntries, err := s.publicEntries(newID)
	if err != nil {
		return err
	}

	// Both lists are in strictly increasing bytewise order by stored name, so
	// this is a merge rather than a lookup per entry — the same property the
	// diff leans on, for the same reason.
	i, j := 0, 0
	for i < len(oldEntries) || j < len(newEntries) {
		switch {
		case j == len(newEntries):
			if err := s.applyEntry(oldEntries[i], u, -1); err != nil {
				return err
			}
			i++
		case i == len(oldEntries):
			if err := s.applyEntry(newEntries[j], u, +1); err != nil {
				return err
			}
			j++
		default:
			o, n := oldEntries[i], newEntries[j]
			switch cmp := compareNames(o.Name, n.Name); {
			case cmp < 0:
				if err := s.applyEntry(o, u, -1); err != nil {
					return err
				}
				i++
			case cmp > 0:
				if err := s.applyEntry(n, u, +1); err != nil {
					return err
				}
				j++
			default:
				if err := s.deltaEntry(o, n, u); err != nil {
					return err
				}
				i++
				j++
			}
		}
	}
	return nil
}

// deltaEntry accounts for two entries that share a name.
func (s *Store) deltaEntry(o, n store.DirEntry, u *Usage) error {
	if o.ChildID == n.ChildID && o.Type == n.Type {
		return nil
	}

	// A name that changed type is a removal and an addition, and has to be
	// accounted as both: the old thing's bytes stop being reachable whatever
	// the new thing is.
	if o.Type != n.Type {
		if err := s.applyEntry(o, u, -1); err != nil {
			return err
		}
		return s.applyEntry(n, u, +1)
	}

	if n.Type == store.NodeDir {
		return s.deltaDir(o.ChildID, n.ChildID, u)
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
func (s *Store) applyEntry(e store.DirEntry, u *Usage, sign int64) error {
	d, err := s.measureEntry(e)
	if err != nil {
		return err
	}
	u.Size += sign * d.Size
	u.FileCount += sign * d.FileCount
	return nil
}

// measureEntry totals one entry, and everything beneath it when it is a
// directory.
//
// Directories themselves weigh nothing. A library's size is the bytes it
// holds, and a directory holds none: charging for the object that records the
// names would make an empty tree cost something and make the number depend on
// how the client chose to arrange it.
func (s *Store) measureEntry(e store.DirEntry) (Usage, error) {
	if e.Type == store.NodeDir {
		return s.Measure(e.ChildID)
	}
	size, err := s.fileSize(e.ChildID)
	if err != nil {
		return Usage{}, err
	}
	return Usage{Size: size, FileCount: 1}, nil
}
