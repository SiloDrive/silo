// An open pack: frames appended to one file, with its index accumulating in a
// sidecar beside it until sealing folds that into a footer.
//
// This file is the write half and the recovery rule. Sealing, the footer and
// reading a sealed pack are the next step and are deliberately not here —
// docs/plans/packs.md § Work order says why the sidecar comes first, which is
// that everything else reads what it wrote.
//
// What is not here and is not an oversight: nothing above objstore learns that
// a pack exists. The exported API stays object-addressed, and this is a
// container the package uses to hold what it was already holding.
package objstore

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
)

// A pack opens with its own magic and version, and — once sealed — closes with
// the magic again.
//
// The closing copy earns its four bytes. A truncated pack is the ordinary
// outcome of an unclean shutdown here rather than an exotic corruption, so
// "does this file end where a pack ends" is a question worth being able to ask
// in one read, and worth being able to ask before trusting a footer offset
// read out of the same tail.
const (
	packMagic      = "SILP"
	packVersion    = 1
	packHeaderSize = len(packMagic) + 1 + 3 // magic, version, three reserved
)

// packTarget is when a pack is full enough to seal. 512 MB, sized in
// storage.md by compaction-rewrite granularity rather than by anything about
// the filesystem.
//
// It is a variable so a test can seal a pack without writing half a gigabyte.
var packTarget int64 = 512 << 20

// ErrPackCorrupt reports a pack whose header is not one. A short tail is not
// this: that is what recovery is for.
var ErrPackCorrupt = errors.New("objstore: pack file is not readable")

// packDirName holds a library's packs, beside the two-character fan-out
// directories its loose objects live in.
//
// The name is longer than two characters on purpose. backend_fs.list walks the
// fan-out and skips any entry whose name is not exactly two, so packs are
// invisible to it without a line being changed there — which is what lets this
// step land beside a working loose store rather than in place of one.
const packDirName = "packs"

func packDir(objDir, libraryID string) string {
	return filepath.Join(objDir, libraryID, packDirName)
}

// newPackID mints a pack's name: 32 random bytes, hex.
//
// Random rather than derived, because a pack is not content-addressed. Its
// bytes are not known when it is named, it grows after it is named, and
// compaction rewrites its contents under a new name — so there is nothing to
// hash. It is the same width and alphabet an object id has, which means
// validPackID and the fan-out arithmetic need no second case for it.
func newPackID() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("naming a pack: %v", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// openPack is a pack being written to, and the index of what is in it.
//
// The mutex is the one genuinely new piece of coordination packs introduce.
// Writes to distinct loose paths needed none; an append to a shared file needs
// an allocated offset, and two writers that both believed they were at the end
// would index each other's bytes. It is held across the append and the index
// update rather than only the append, because a record naming an offset the
// writer no longer owns is worse than a slow write.
//
// The batch surface is what keeps this cheap. POST chunks carries up to 256
// frames, so a request takes this once and pays one fsync, where the loose
// store paid 256 temp files, fsyncs and renames.
type openPack struct {
	id      string
	path    string
	mu      sync.Mutex
	f       *os.File
	side    *sidecar
	size    int64
	entries []indexEntry
	byID    map[string]indexEntry
}

func packPaths(objDir, libraryID, packID string) (pack string, side string) {
	dir := packDir(objDir, libraryID)
	return filepath.Join(dir, packID+".pack"), filepath.Join(dir, packID+".idx")
}

// createPack starts a new empty pack and its sidecar.
func createPack(objDir, libraryID string) (*openPack, error) {
	id, err := newPackID()
	if err != nil {
		return nil, err
	}
	dir := packDir(objDir, libraryID)
	if err := os.MkdirAll(dir, os.ModePerm); err != nil {
		return nil, err
	}
	packPath, sidePath := packPaths(objDir, libraryID, id)

	f, err := os.OpenFile(packPath, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return nil, err
	}
	header := make([]byte, packHeaderSize)
	copy(header, packMagic)
	header[len(packMagic)] = packVersion
	if _, err := f.Write(header); err != nil {
		_ = f.Close()
		return nil, err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return nil, err
	}

	side, err := createSidecar(sidePath)
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	// The directory entry for both, so a crash cannot lose the pack itself
	// while leaving what referenced it.
	if err := syncDir(dir); err != nil {
		_ = f.Close()
		_ = side.close()
		return nil, err
	}

	return &openPack{
		id:   id,
		path: packPath,
		f:    f,
		side: side,
		size: int64(packHeaderSize),
		byID: map[string]indexEntry{},
	}, nil
}

// append writes one frame and indexes it.
//
// The order is the whole of the crash story, and it is the order storage.md
// gives for the same reason the loose store's write path gives its own: the
// branch head lives in SQLite, which fsyncs its own WAL, so a head commit can
// outlive the objects it references unless those are durable first. Nothing
// repairs that afterwards — the client believes it has already uploaded them.
//
//	append the frame → fsync the pack → append the record → fsync the sidecar
//
// A crash between any two of those leaves the pack with a tail that no record
// points at, which recoverPack truncates. It can never leave a record pointing
// at bytes that are not durable, which is the direction that would lose data
// rather than waste it.
//
// sync is honoured the way the loose store honours it: a caller that does not
// need durability before its own return does not pay for it, and the frame is
// still indexed in memory and on its way to disk.
func (p *openPack) append(frame []byte, objID string, sync bool) (indexEntry, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	offset := p.size
	if _, err := p.f.Write(frame); err != nil {
		// The pack may now hold a partial frame. Nothing indexes it, so the
		// next recovery truncates it away; what must not happen is indexing
		// it, so this returns before the sidecar is touched.
		return indexEntry{}, fmt.Errorf("appending to pack %s: %v", p.id, err)
	}
	if sync {
		if err := p.f.Sync(); err != nil {
			return indexEntry{}, fmt.Errorf("syncing pack %s: %v", p.id, err)
		}
	}

	e := indexEntry{ID: objID, Offset: offset, Length: int64(len(frame))}
	if err := p.side.append(e); err != nil {
		return indexEntry{}, fmt.Errorf("indexing %s in pack %s: %v", objID, p.id, err)
	}
	if sync {
		if err := p.side.sync(); err != nil {
			return indexEntry{}, fmt.Errorf("syncing the index of pack %s: %v", p.id, err)
		}
	}

	p.size = offset + int64(len(frame))
	p.entries = append(p.entries, e)
	p.byID[objID] = e
	return e, nil
}

// lookup answers from memory. An open pack has no bloom filter and needs none:
// it is one pack, the writer is already holding its index, and asking it is a
// map read rather than anything on disk.
func (p *openPack) lookup(objID string) (indexEntry, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byID[objID]
	return e, ok
}

// readFrameAt reads one frame out of the pack by its index entry.
func (p *openPack) readFrameAt(e indexEntry) ([]byte, error) {
	buf := make([]byte, e.Length)
	if _, err := p.f.ReadAt(buf, e.Offset); err != nil {
		return nil, fmt.Errorf("reading %s from pack %s: %v", e.ID, p.id, err)
	}
	return buf, nil
}

// full reports whether this pack has reached the size it should be sealed at.
func (p *openPack) full() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.size >= packTarget
}

func (p *openPack) close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	err := p.f.Close()
	if sErr := p.side.close(); err == nil {
		err = sErr
	}
	return err
}

// recoverPack reopens a pack that was open when the process stopped, and makes
// the file agree with its index again.
//
// One rule, and it runs the same way whether the last shutdown was clean, was
// a kill, or was a power cut: **the sidecar is the authority on what the pack
// holds, and the pack is truncated to the end of its last record.**
//
// That is sound because of the order append writes in. A record exists only
// after the frame it names was fsynced, so every record describes durable
// bytes; and bytes past the last record were never acknowledged to anybody, so
// discarding them loses nothing a client believes it has stored.
//
// The one thing it will not do is repair a pack *shorter* than its index. That
// direction cannot be produced by the write order, so finding it means the
// file was damaged or replaced underneath the store, and truncating further
// would turn a detectable problem into a silent one.
func recoverPack(objDir, libraryID, packID string) (*openPack, error) {
	packPath, sidePath := packPaths(objDir, libraryID, packID)

	entries, err := readSidecar(sidePath)
	if err != nil {
		return nil, err
	}

	f, err := os.OpenFile(packPath, os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	closeOnErr := func(e error) (*openPack, error) {
		_ = f.Close()
		return nil, e
	}

	header := make([]byte, packHeaderSize)
	if _, err := io.ReadFull(f, header); err != nil {
		return closeOnErr(fmt.Errorf("%w: %s is too short to hold a header", ErrPackCorrupt, packPath))
	}
	if string(header[:len(packMagic)]) != packMagic {
		return closeOnErr(fmt.Errorf("%w: %s does not begin %q", ErrPackCorrupt, packPath, packMagic))
	}
	if header[len(packMagic)] != packVersion {
		return closeOnErr(fmt.Errorf("%w: %s is version %d, and this build reads %d",
			ErrPackCorrupt, packPath, header[len(packMagic)], packVersion))
	}

	indexed := int64(packHeaderSize)
	if n := len(entries); n > 0 {
		indexed = entries[n-1].end()
	}

	info, err := f.Stat()
	if err != nil {
		return closeOnErr(err)
	}
	switch {
	case info.Size() < indexed:
		return closeOnErr(fmt.Errorf(
			"%w: %s is %d bytes but its index describes %d — the pack is shorter than what was written to it, "+
				"which the write order cannot produce, so it was damaged or replaced rather than interrupted",
			ErrPackCorrupt, packPath, info.Size(), indexed))
	case info.Size() > indexed:
		// The ordinary interrupted-write case: a frame that got as far as the
		// pack and no further.
		if err := f.Truncate(indexed); err != nil {
			return closeOnErr(fmt.Errorf("truncating %s to its last indexed frame: %v", packPath, err))
		}
		if err := f.Sync(); err != nil {
			return closeOnErr(err)
		}
	}

	if _, err := f.Seek(indexed, io.SeekStart); err != nil {
		return closeOnErr(err)
	}

	side, err := openSidecar(sidePath)
	if err != nil {
		return closeOnErr(err)
	}

	p := &openPack{
		id:      packID,
		path:    packPath,
		f:       f,
		side:    side,
		size:    indexed,
		entries: entries,
		byID:    make(map[string]indexEntry, len(entries)),
	}
	for _, e := range entries {
		p.byID[e.ID] = e
	}
	return p, nil
}

// findOpenPack names the pack a library was writing to, if it was writing to
// one. A sealed pack has no sidecar — sealing folds it into the footer and
// removes it — so a sidecar on disk is exactly the marker for "this one was
// open", with no state kept anywhere else to disagree.
func findOpenPack(objDir, libraryID string) (string, error) {
	dir := packDir(objDir, libraryID)
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || filepath.Ext(name) != ".idx" {
			continue
		}
		id := name[:len(name)-len(".idx")]
		if !validPackID(id) {
			continue
		}
		return id, nil
	}
	return "", nil
}
