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
	// Which library this is, and where its packs live. Held rather than passed
	// in, so that "these methods only ever touch this library" is structural
	// instead of something every call site has to keep getting right.
	objDir    string
	libraryID string

	// mu guards which packs exist. Readers take it for a moment to look; it is
	// never held across an fsync or across a caller's callback.
	mu     sync.RWMutex
	open   *openPack
	sealed []*sealedPack
	// writeMu serialises appends and the rotation between packs. Separate from
	// mu on purpose — see appendFrame.
	writeMu sync.Mutex
	// compactMu serialises rewrites against each other, and deliberately not
	// against writeMu: a rewrite copies a whole pack, and holding the write
	// path's lock for that long would stall every upload. Compaction touches
	// only sealed packs, which the writer never writes to.
	compactMu sync.Mutex
}

// current is the open pack, or nil. One accessor rather than the same three
// lines wherever the question is asked.
func (s *packSet) current() *openPack {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.open
}

// packs copies out what the set holds, so a walk can stat and read without
// holding the lock across any of it. Same argument openPack.snapshot makes one
// level down: a whole-library listing runs for millions of objects, and a Go
// RWMutex blocks new readers behind a waiting writer, so holding this would
// stall pack rotation and every lookup behind it.
func (s *packSet) packs() (*openPack, []*sealedPack) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sealed := make([]*sealedPack, len(s.sealed))
	copy(sealed, s.sealed)
	return s.open, sealed
}

// find asks the open pack and then the sealed ones.
//
// The id is decoded once here rather than inside each pack. A lookup fans out
// over every sealed pack — twelve thousand of them on a full store — and each
// one needs the same thirty-two bytes, so decoding per pack would be twelve
// thousand identical parses and allocations for one question.
func (s *packSet) find(objID string) (packReader, indexEntry, bool) {
	raw, err := frameID(objID)
	if err != nil {
		return nil, indexEntry{}, false
	}

	open, sealed := s.packs()
	if open != nil {
		if e, ok := open.lookup(objID); ok {
			return open, e, true
		}
	}
	for _, p := range sealed {
		if e, ok := p.lookupRaw(raw); ok {
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
	open, sealed := s.packs()

	walk := func(path string, entries []indexEntry) error {
		mt, err := fileModTime(path)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if err := fn(e, mt); err != nil {
				return err
			}
		}
		return nil
	}

	if open != nil {
		if err := walk(open.path, open.snapshot()); err != nil {
			return err
		}
	}
	for _, p := range sealed {
		if err := walk(p.path, p.entries()); err != nil {
			return err
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

	// The age sealer, brought up the first time anything writes a pack. See
	// packwrite.go.
	sealerOnce sync.Once
	stopOnce   sync.Once
	stop       chan struct{}
	wg         sync.WaitGroup
	// maxAge and sweep are copied from the package variables at creation, so
	// the sweeper goroutine reads fields nobody else writes. See packwrite.go.
	maxAge time.Duration
	sweep  time.Duration
}

func newPackStore(objDir string) *packStore {
	return &packStore{
		objDir: objDir,
		libs:   map[string]*packSet{},
		// Created here rather than by the sealer, so that closing it is a
		// sync.Once over a channel that always exists — no nil check, no
		// second flag, and no lock to keep the two in step.
		stop:   make(chan struct{}),
		maxAge: packMaxAge,
		sweep:  packSweep,
	}
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
//
// The open pack is closed on the way out. Sealed packs hold no descriptor, so
// there is nothing to release there — but an open one does, and on Linux a
// RemoveAll over a file somebody still holds unlinks the name and leaves the
// handle live, so a writer that was never told would go on appending to a file
// nothing can reach.
func (ps *packStore) forget(libraryID string) {
	ps.mu.Lock()
	set := ps.libs[libraryID]
	delete(ps.libs, libraryID)
	ps.mu.Unlock()

	if set == nil {
		return
	}
	set.writeMu.Lock()
	defer set.writeMu.Unlock()
	set.mu.Lock()
	p := set.open
	set.open, set.sealed = nil, nil
	set.mu.Unlock()
	if p != nil {
		_ = p.close()
	}
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
		return &packSet{objDir: objDir, libraryID: libraryID}, nil
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

	set := &packSet{objDir: objDir, libraryID: libraryID}
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
		// touch is doing it before the first read. The writer moves it to
		// startup, where the pack is opened anyway.
		p, err := recoverPack(objDir, libraryID, openIDs[0])
		if err != nil {
			return nil, err
		}
		// Inherited from a previous run rather than opened by this one, which
		// the writer needs to know: its frames have been outside any sealed
		// pack for at least as long as the process was down.
		p.recovered = true
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
