// Measuring a pack against a caller's answer about what is still reachable.
//
// This is the mark phase's other half. Liveness is global reachability from
// live commits — a tracing walk, never a refcount — and that walk lives above
// this package because it follows commits, trees and manifests, which are
// shapes objstore deliberately knows nothing about. What lives here is the
// other input: which objects are in which pack, and how many bytes each one
// occupies.
//
// So the caller supplies a function and gets back numbers. It never learns
// which objects are in which pack, and the pack id it does get back is opaque:
// something to store in a scheduling row and hand back to CompactPack, not
// something to parse, publish or reason about. That keeps docs/plans/packs.md's
// invariant true in the sense that matters — a pack is still not a thing any
// caller can act on except through this package.
//
// No frame is read and no key is needed. A sealed pack's footer already names
// every id it holds and how long each frame is, so this is an index walk.
package objstore

import "os"

// Reach is how far a caller's mark got to an object: not at all, through some
// commit in history, or from the head itself.
//
// The three are the same partition Census draws over a whole store, asked one
// object at a time. They are ordered, and the order is the point — an object
// reachable from head is also reachable from history, and is counted once,
// against the strongest thing that reaches it.
type Reach int

const (
	// Unreached: no commit reaches it. Reclaimable with no policy attached.
	Unreached Reach = iota
	// ReachedHistory: some older commit reaches it, but not the head. This is
	// what a retention policy would free.
	ReachedHistory
	// ReachedHead: the library holds it now.
	ReachedHead
)

// PackStat is one sealed pack, measured.
//
// FrameBytes rather than the file size is what the dead fraction is taken
// over, because it is the part a rewrite can shrink in proportion. The footer
// shrinks too — it is one record per object plus a filter sized from the
// count — but it shrinks as a consequence rather than as the thing being
// reclaimed. FileBytes is carried alongside so an operator can see the whole
// number and reconcile it against the disk.
type PackStat struct {
	// PackID is opaque above this package. See the file comment.
	PackID  string
	Objects int64

	FrameBytes int64
	LiveBytes  int64
	HeadBytes  int64
	FileBytes  int64
}

// DeadBytes is what a rewrite of this pack would drop.
func (p PackStat) DeadBytes() int64 { return p.FrameBytes - p.LiveBytes }

// DeadFraction is what the compaction threshold is compared against.
//
// Zero for an empty pack rather than undefined: a pack with no frames has
// nothing dead in it, and returning a NaN here would put one into a comparison
// that silently answers false.
func (p PackStat) DeadFraction() float64 {
	if p.FrameBytes <= 0 {
		return 0
	}
	return float64(p.DeadBytes()) / float64(p.FrameBytes)
}

// PackStats measures every sealed pack a library holds.
//
// Sealed only, and the open pack is deliberately absent. Its dead fraction is
// not a fact yet — it is still being appended to, so the number would describe
// a file that is a different size by the time anything acted on it — and
// nothing compacts an open pack in any case. What is in it becomes measurable
// when it seals, which is at most one age interval away.
//
// The result is a snapshot and is stale the moment it returns: an upload in
// flight is unreached and is about to be reached. That is inherent to a mark
// and is why this is a scheduling input rather than a licence to delete. The
// caller re-verifies before it acts.
func (s *ObjectStore) PackStats(libraryID string, reach func(objID string) Reach) ([]PackStat, error) {
	if err := s.present(); err != nil {
		return nil, err
	}
	set, err := s.packs.set(libraryID)
	if err != nil {
		return nil, err
	}

	_, sealed := set.packs()
	out := make([]PackStat, 0, len(sealed))
	for _, p := range sealed {
		stat := PackStat{PackID: p.id}
		for _, e := range p.entries() {
			stat.Objects++
			stat.FrameBytes += e.Length
			switch reach(e.ID) {
			case ReachedHead:
				stat.LiveBytes += e.Length
				stat.HeadBytes += e.Length
			case ReachedHistory:
				stat.LiveBytes += e.Length
			}
		}
		info, err := os.Stat(p.path)
		if err != nil {
			// Compacted or removed between the listing and the stat. A pack
			// that is gone is not one to report on.
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		stat.FileBytes = info.Size()
		out = append(out, stat)
	}
	return out, nil
}
