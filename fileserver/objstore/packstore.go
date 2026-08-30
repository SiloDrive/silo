// The lookup: which pack, if any, holds an object.
//
// This is the piece that lets everything above objstore keep asking for an
// object by its id while the answer moves into a container. It sits above the
// backend seam, as docs/plans/packs.md § Decision 2 requires — the seam takes
// whole sealed packs and knows nothing about what is inside one, so the
// mapping from object to (pack, offset, length) has to live here.
//
// Three places are asked, in this order:
//
//  1. **The open pack**, from the index its writer is already holding in
//     memory. One map read, no filter and no disk, because it is one pack and
//     the writer has it.
//  2. **Every sealed pack**, by its bloom filter first. "No" is certain and
//     ends that pack in a few hundred nanoseconds; "yes" costs one binary
//     search over that pack's index to confirm.
//  3. **The loose store**, by falling through to the backend. That lane is
//     what makes packs land beside a working store rather than in place of
//     one, and it lives until ingest finishes (step 5).
package objstore

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// packReader is what the two kinds of pack have in common, which is exactly
// what a read needs: say whether you hold this object, and hand back its
// frame.
type packReader interface {
	lookup(objID string) (indexEntry, bool)
	readFrameAt(e indexEntry) ([]byte, error)
}

// packSet is one library's packs.
//
// A library rather than a store, because packs are per library — removeLibrary
// is a RemoveAll today and packs shared across libraries would make deleting
// one library a compaction of every pack it touched.
type packSet struct {
	mu     sync.RWMutex
	open   *openPack
	sealed []*sealedPack
}

// find asks the open pack and then the sealed ones.
func (s *packSet) find(objID string) (packReader, indexEntry, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.open != nil {
		if e, ok := s.open.lookup(objID); ok {
			return s.open, e, true
		}
	}
	for _, p := range s.sealed {
		if e, ok := p.lookup(objID); ok {
			return p, e, true
		}
	}
	return nil, indexEntry{}, false
}

// each calls fn for every object the set's packs hold, with the modification
// time of the pack holding it.
//
// A pack's mtime rather than a frame's, because a frame has no timestamp and
// adding one would put a field in the index that only this walk reads. What
// uses it is the collector's age guard — "has this been sitting here long
// enough to be safe to reclaim" — and a pack is always at least as young as
// the frames in it, so an object in one looks newer than it is. That is the
// conservative direction: the guard errs towards not collecting.
func (s *packSet) each(fn func(indexEntry, time.Time) error) error {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.open != nil {
		mt, err := fileModTime(s.open.path)
		if err != nil {
			return err
		}
		for _, e := range s.open.snapshot() {
			if err := fn(e, mt); err != nil {
				return err
			}
		}
	}
	for _, p := range s.sealed {
		mt, err := fileModTime(p.path)
		if err != nil {
			return err
		}
		for _, e := range p.entries() {
			if err := fn(e, mt); err != nil {
				return err
			}
		}
	}
	return nil
}

func fileModTime(path string) (time.Time, error) {
	info, err := os.Stat(path)
	if err != nil {
		return time.Time{}, err
	}
	return info.ModTime(), nil
}

// packStore holds every library's packs, loaded when a library is first asked
// about.
//
// Lazily, because libraries are discovered at runtime and a store with
// thousands of them should not read every pack footer at startup to answer a
// question about one. Once loaded a library stays loaded: the footers are the
// index, and re-reading them per lookup would defeat the point of having them
// in memory.
//
// The consequence is that this process's view of a library's packs is fixed at
// first touch, which is correct exactly while this process is the only writer.
// It is, and it has to be — an open pack is a file being appended to. Step 4,
// which makes packs the write path, is what registers a newly sealed pack here
// rather than leaving it to be discovered.
type packStore struct {
	objDir string
	mu     sync.Mutex
	libs   map[string]*packSet
}

func newPackStore(objDir string) *packStore {
	return &packStore{objDir: objDir, libs: map[string]*packSet{}}
}

func (ps *packStore) set(libraryID string) (*packSet, error) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	if s, ok := ps.libs[libraryID]; ok {
		return s, nil
	}
	s, err := loadPackSet(ps.objDir, libraryID)
	if err != nil {
		return nil, err
	}
	ps.libs[libraryID] = s
	return s, nil
}

// forget drops a library's cached packs, so the next lookup rediscovers them.
// Removing a library is what needs this; a store that kept the packs of a
// library it had just deleted would answer reads out of files that are gone.
func (ps *packStore) forget(libraryID string) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	delete(ps.libs, libraryID)
}

// each calls fn for every object a library's packs hold.
func (ps *packStore) each(libraryID string, fn func(indexEntry, time.Time) error) error {
	s, err := ps.set(libraryID)
	if err != nil {
		return err
	}
	return s.each(fn)
}

// find locates an object in a library's packs.
func (ps *packStore) find(libraryID, objID string) (packReader, indexEntry, bool, error) {
	s, err := ps.set(libraryID)
	if err != nil {
		return nil, indexEntry{}, false, err
	}
	r, e, ok := s.find(objID)
	return r, e, ok, nil
}

// loadPackSet reads what a library's pack directory holds.
//
// The sidecar is what tells the two kinds apart, and it is the only thing that
// does: sealing removes it, so a pack with one beside it was open when this
// store last stopped and a pack without one is sealed. No state is kept
// anywhere else that could disagree.
func loadPackSet(objDir, libraryID string) (*packSet, error) {
	dir := packDir(objDir, libraryID)
	dirEntries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		// A library that has never been packed, which is every library until
		// step 4. Not an error, and not worth a second syscall to confirm.
		return &packSet{}, nil
	}
	if err != nil {
		return nil, err
	}

	hasSidecar := map[string]bool{}
	for _, e := range dirEntries {
		if !e.IsDir() && filepath.Ext(e.Name()) == ".idx" {
			hasSidecar[strings.TrimSuffix(e.Name(), ".idx")] = true
		}
	}

	var openIDs, sealedIDs []string
	for _, e := range dirEntries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".pack" {
			continue
		}
		id := strings.TrimSuffix(e.Name(), ".pack")
		if !validPackID(id) {
			continue
		}
		if hasSidecar[id] {
			openIDs = append(openIDs, id)
		} else {
			sealedIDs = append(sealedIDs, id)
		}
	}
	sort.Strings(openIDs)
	sort.Strings(sealedIDs)

	set := &packSet{}
	// Two open packs in one library means two writers appended to it, which
	// this store cannot produce. Picking one would silently orphan the other's
	// frames, so it is reported instead.
	if len(openIDs) > 1 {
		return nil, fmt.Errorf("%w: library %s has %d open packs (%s) — it was written by more than one process",
			ErrPackCorrupt, libraryID, len(openIDs), strings.Join(openIDs, ", "))
	}
	if len(openIDs) == 1 {
		// Recovery truncates, so this writes during what may be a read. That
		// is right where it is: a pack that disagrees with its index has to be
		// made to agree before anything reads either, and doing it at first
		// touch is doing it before the first read. Step 4 moves it to startup,
		// where the writer is opened anyway.
		p, err := recoverPack(objDir, libraryID, openIDs[0])
		if err != nil {
			return nil, err
		}
		set.open = p
	}
	for _, id := range sealedIDs {
		p, err := openSealedPack(objDir, libraryID, id)
		if err != nil {
			return nil, err
		}
		set.sealed = append(set.sealed, p)
	}
	return set, nil
}
