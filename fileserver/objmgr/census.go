package objmgr

import (
	"errors"
	"fmt"
	"time"

	"github.com/dkam/silo/fileserver/objstore"
	"github.com/dkam/silo/store"
)

// Extent is a number of stored objects and the bytes they occupy on disk.
//
// Bytes here is what the filesystem holds — the stored form, after E2EE
// framing — and not the logical size of anything. That is the
// whole point of the census: quota answers "what do this account's files add
// up to", and this answers "what is that costing", which are different
// questions with different answers and only one of them can be acted on by
// deleting history.
type Extent struct {
	Bytes   int64
	Objects int64
}

func (e Extent) add(size int64) Extent {
	return Extent{Bytes: e.Bytes + size, Objects: e.Objects + 1}
}

// Census is a library's store divided into three, by what reaches each object.
//
// The three want three different actions, which is why they are three numbers
// and not one:
//
//   - Head is reachable from the head commit. It is what the library holds
//     now, and nothing reclaims it short of deleting files.
//   - History is reachable from some older commit but not from head. It is
//     what a retention policy would reclaim, and it is the number nobody could
//     see before this: quota charges the head, so an account that rewrites one
//     file every day reports a flat usage while the disk grows daily.
//   - Unreferenced is reachable from no commit at all — an upload that stopped
//     halfway, a commit that lost its race and was never published. It is
//     reclaimable now, with no policy decision attached, which is why it is
//     counted apart from history rather than folded into it.
//
// The three partition the store: every object on disk lands in exactly one,
// and the totals add up to what the filesystem holds. That is asserted by a
// test rather than assumed, because three overlapping estimates would look the
// same as three correct ones from the outside.
type Census struct {
	Head         Extent
	History      Extent
	Unreferenced Extent
}

// Census divides a library's stored objects by what reaches them.
//
// It is a set union over ids and deliberately not a sum of per-commit
// measurements. Successive commits share nearly all of their objects — that is
// what makes the store affordable — so adding up what each commit reaches
// would report a history several times the size of the disk, and would grow
// every time somebody touched an unrelated file. An object is counted once,
// against the earliest column that reaches it.
//
// The walk is key-free. A commit publishes its root and parents, a directory
// its entries, and a manifest its chunk list, in both library types — so a
// server holding an E2EE library it cannot read can still say what that
// library is costing it. A census that needed the content key would be a
// census that never ran on the libraries most likely to be large.
//
// Cost is one pass over the store's directory listing plus one walk per
// distinct tree in history, with every already-seen id short-circuiting. That
// is affordable for a report and is not something to put on the write path.
//
// An object the walk cannot read is an error and not a smaller number. A
// census is the tool somebody reaches for when they suspect a store is wrong,
// and one that silently reported the readable fraction as the total would
// answer the question it was asked with a number that means something else.
func (s *Store) Census(head store.ID) (Census, error) {
	live, err := s.reachable([]store.ID{head}, false)
	if err != nil {
		return Census{}, fmt.Errorf("walking the head commit: %w", err)
	}
	all, err := s.reachable([]store.ID{head}, true)
	if err != nil {
		return Census{}, fmt.Errorf("walking the history: %w", err)
	}

	var c Census
	assign := func(id string, size int64, isChunk bool) error {
		switch {
		case live.has(id, isChunk):
			c.Head = c.Head.add(size)
		case all.has(id, isChunk):
			c.History = c.History.add(size)
		default:
			c.Unreferenced = c.Unreferenced.add(size)
		}
		return nil
	}

	if err := s.objects.List(s.storeID, func(o objstore.ObjectInfo) error {
		return assign(o.ID, o.Size, false)
	}); err != nil {
		return Census{}, fmt.Errorf("listing objects: %w", err)
	}
	if err := s.chunks.List(s.storeID, func(o objstore.ObjectInfo) error {
		return assign(o.ID, o.Size, true)
	}); err != nil {
		return Census{}, fmt.Errorf("listing chunks: %w", err)
	}
	return c, nil
}

// PackCensus measures every sealed pack in a library against the same walk
// Census uses, so the two answer with one another rather than approximately.
//
// The mark is one walk of history plus one of head — the same pair Census
// takes — and the packs are an attribution of it. No pack's contents are read:
// a sealed pack's footer already names every id it holds and how long each
// frame is, so this costs one index walk per pack on top of a walk the census
// was doing anyway.
//
// Both stores are measured. Packs exist for chunks, but objects are packed by
// the same writer under the same rules, and a compaction scheduler that could
// only see half the store would leave the other half growing.
//
// The numbers are stale by construction and are a scheduling input, never a
// licence to delete: an object nothing reaches is indistinguishable from one
// about to be committed. Whatever acts on these re-verifies first.
func (s *Store) PackCensus(head store.ID) ([]objstore.PackStat, error) {
	live, err := s.reachable([]store.ID{head}, false)
	if err != nil {
		return nil, fmt.Errorf("walking the head commit: %w", err)
	}
	all, err := s.reachable([]store.ID{head}, true)
	if err != nil {
		return nil, fmt.Errorf("walking the history: %w", err)
	}

	var out []objstore.PackStat
	for _, st := range []struct {
		store   *objstore.ObjectStore
		isChunk bool
	}{{s.chunks, true}, {s.objects, false}} {
		stats, err := st.store.PackStats(s.storeID, func(objID string) objstore.Reach {
			switch {
			case live.has(objID, st.isChunk):
				return objstore.ReachedHead
			case all.has(objID, st.isChunk):
				return objstore.ReachedHistory
			default:
				return objstore.Unreached
			}
		})
		if err != nil {
			return nil, fmt.Errorf("measuring packs: %w", err)
		}
		out = append(out, stats...)
	}
	return out, nil
}

// Orphan is one stored object that no commit reaches.
//
// The id is as objstore spells it -- hex -- because what a caller does with
// one is hand it back to objstore, and IsChunk says which of the two stores to
// hand it to. There is no store.ID here on purpose: an id on disk that will
// not parse is still an object taking up space, and forcing it through
// ParseID first would drop exactly the debris most worth reporting.
type Orphan struct {
	ID      string
	IsChunk bool
	Size    int64
	// ModTime is when the object was last written, carried from the listing
	// that found it. It is the collector's age guard, and it is a field rather
	// than a lookup because the listing had already read it: asking for it
	// separately cost a second stat per orphan for a value the walk was
	// throwing away.
	ModTime time.Time
}

// Unreferenced calls fn for every stored object no commit reaches.
//
// It is the third column of Census as a stream rather than a total, and it is
// deliberately the same walk: a report and a collector that computed their
// candidate sets separately would eventually disagree, and the way that shows
// up is somebody reading a number, running the thing that acts on it, and
// getting a different set.
//
// What this yields is safe to reclaim in the sense that nothing in the store
// points at it. That is not the same as safe to delete, and a caller must not
// treat it as such: an object uploaded a moment ago and not yet committed is
// unreferenced and is about to be referenced. Deciding that is the caller's,
// and the guards are age and the GCID generation -- see RunGC.
func (s *Store) Unreferenced(head store.ID, fn func(Orphan) error) error {
	all, err := s.reachable([]store.ID{head}, true)
	if err != nil {
		return fmt.Errorf("walking the history: %w", err)
	}
	for _, st := range []struct {
		store   *objstore.ObjectStore
		isChunk bool
	}{{s.objects, false}, {s.chunks, true}} {
		if err := st.store.List(s.storeID, func(o objstore.ObjectInfo) error {
			if all.has(o.ID, st.isChunk) {
				return nil
			}
			return fn(Orphan{ID: o.ID, IsChunk: st.isChunk, Size: o.Size, ModTime: o.ModTime})
		}); err != nil {
			return err
		}
	}
	return nil
}

// RemoveOrphan deletes one orphan. It is here rather than on objstore so that
// a caller holding an Orphan cannot send a chunk id to the object store, which
// would silently do nothing and report success. The name is not Remove because
// that one removes a path from a tree, and the two must never be confused.
func (s *Store) RemoveOrphan(o Orphan) error {
	if o.IsChunk {
		return s.chunks.Remove(s.storeID, o.ID)
	}
	return s.objects.Remove(s.storeID, o.ID)
}

// marks is the set of ids a walk has reached.
//
// Two sets rather than one, because chunks and objects are separate stores and
// an id is only unique within one of them. Collapsing them would be correct
// today — both are content hashes and a collision across them would mean the
// same bytes — but it would make the census silently wrong the first time the
// two stores held different framings of one payload.
type marks struct {
	objects map[store.ID]struct{}
	chunks  map[store.ID]struct{}
}

func newMarks() *marks {
	return &marks{objects: map[store.ID]struct{}{}, chunks: map[store.ID]struct{}{}}
}

// has reports whether an id — as spelled by objstore.List, in hex — was
// reached. An id the walk never produced cannot be parsed into the set, so an
// unparseable name on disk is unreferenced, which is what it is.
func (m *marks) has(id string, isChunk bool) bool {
	parsed, err := store.ParseID(id)
	if err != nil {
		return false
	}
	set := m.objects
	if isChunk {
		set = m.chunks
	}
	_, ok := set[parsed]
	return ok
}

// seen adds an id and reports whether it was already there, which is what
// stops the walk revisiting a subtree every commit that shares it.
func (m *marks) seen(set map[store.ID]struct{}, id store.ID) bool {
	if _, ok := set[id]; ok {
		return true
	}
	set[id] = struct{}{}
	return false
}

// reachable marks everything the given commits reach. withParents follows
// commit history; without it the walk stops at the commits it was handed,
// which is what makes "reachable from head alone" askable.
//
// A parent commit the store no longer holds ends that branch instead of
// failing the walk. That is the retention boundary, not damage: expiring
// history is exactly "delete the oldest commit objects", and walkHistory and
// changes?since= already read a missing commit the same way. A census that
// failed on one would start erroring the day retention first ran.
//
// The head is the exception. It is the commit the caller named, and a store
// that cannot produce it is not a library with short history -- it is a
// library whose head is gone, which is the corruption libmgr has a whole error
// for. Silently reporting an empty store would be the worst available answer.
//
// Missing directories and manifests are always errors, wherever they are
// found. Nothing collects those without first collecting the commit that
// reaches them, so one that has gone missing is damage.
func (s *Store) reachable(commits []store.ID, withParents bool) (*marks, error) {
	m := newMarks()
	isHead := make(map[store.ID]bool, len(commits))
	for _, id := range commits {
		isHead[id] = true
	}
	pending := append([]store.ID(nil), commits...)
	for len(pending) > 0 {
		id := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if m.seen(m.objects, id) {
			continue
		}
		c, err := s.GetCommitPublic(id)
		if err != nil {
			if isHead[id] || !errors.Is(err, objstore.ErrNotFound) {
				return nil, fmt.Errorf("commit %s: %w", id, err)
			}
			// Expired. This branch of history ends here.
			continue
		}
		if err := s.markTree(m, c.Root); err != nil {
			return nil, err
		}
		if withParents {
			pending = append(pending, c.Parents...)
		}
	}
	return m, nil
}

// markTree marks a directory object and everything below it.
//
// A subtree whose id was already reached is skipped unread — the same Merkle
// short-circuit MeasureDelta relies on, and the reason a hundred commits of a
// large library cost little more than one.
func (s *Store) markTree(m *marks, dirID store.ID) error {
	if m.seen(m.objects, dirID) {
		return nil
	}
	d, err := s.GetDirectoryPublic(dirID)
	if err != nil {
		return fmt.Errorf("directory %s: %w", dirID, err)
	}
	for _, e := range d.Entries {
		if e.Type == store.NodeDir {
			if err := s.markTree(m, e.ChildID); err != nil {
				return err
			}
			continue
		}
		// NodeFile and NodeSymlink both name a manifest; a symlink's target is
		// its content, so it is stored and counted like any other small file.
		if err := s.markManifest(m, e.ChildID); err != nil {
			return err
		}
	}
	return nil
}

// markManifest marks a manifest and the chunks it names.
//
// An inlined manifest names none: its bytes are in the object itself, already
// counted by the object's own size, and adding a chunk for it would be
// counting a chunk that does not exist.
func (s *Store) markManifest(m *marks, id store.ID) error {
	if m.seen(m.objects, id) {
		return nil
	}
	man, err := s.GetManifestPublic(id)
	if err != nil {
		return fmt.Errorf("manifest %s: %w", id, err)
	}
	for _, ch := range man.Chunks {
		m.seen(m.chunks, ch.ID)
	}
	return nil
}

// storeFor names the two object stores a census walks, so a caller totalling
// the disk for itself reads the same two this does.
func (s *Store) stores() []*objstore.ObjectStore { return []*objstore.ObjectStore{s.objects, s.chunks} }
