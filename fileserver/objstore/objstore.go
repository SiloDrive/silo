// Package objstore is the seam between everything that stores bytes and the
// tier those bytes land on.
//
// The interface is pack-shaped — write-and-seal, ranged read, list, delete —
// because that is what the storage tiers this is heading for actually offer.
// The local filesystem backend implements it degenerately: one object per
// "pack", sealing is the atomic publish it already did, and a ranged read is a
// seek. Real packs arrive as another implementation of this same interface
// (docs/storage.md), so the seam changes shape once, here, rather than once
// now and again later.
//
// ObjectStore is the per-object API over that seam: one object, addressed by
// its id, which is what every caller above this actually holds. It stays an
// adapter rather than the interface itself, because a real pack store answers
// the same question with a lookup and a byte range.
package objstore

import (
	"errors"
	"fmt"
	"hash"
	"io"
	"path/filepath"
	"time"
)

// ErrContentMismatch is returned by a verified write whose bytes do not hash
// to the id they were offered under. It is a sentinel because the caller's
// answer depends on who supplied the bytes: an ingest path handed a bad block
// by a remote client owes that client a 4xx, not the 500 an anonymous error
// would produce.
var ErrContentMismatch = errors.New("content does not match its object id")

// ErrNotFound reports a pack the backend does not hold.
//
// Normalised at the seam on purpose. A local backend says os.ErrNotExist, S3
// says 404, and a caller deciding whether to fetch from a durable tier has to
// ask that question of whichever backend it happens to be holding. One
// sentinel means the tiering logic is written once instead of per backend.
var ErrNotFound = errors.New("no such object")

// The two object types, and the directory each one's store occupies.
//
// Chunks are the large content objects — the ones packs exist for — and
// objects are the small ones that describe them: manifests, directories and
// commits. They are separate because their access patterns and their eventual
// packing are different: a chunk is read as a byte range out of whatever holds
// it, and an object is read whole.
//
// Exported because they are not private to this package in practice: gc walks
// the same directories to reclaim them and backup names them in its
// instructions. Both used to spell the strings out for themselves, and a
// mismatch would not have failed — gc treats a directory it cannot find as
// nothing to reclaim, so a renamed store would have turned "silo gc -delete"
// into a silent no-op that still reported success.
const (
	TypeChunks  = "chunks"
	TypeObjects = "objects"
)

// Types lists every object store a library has, for callers that must cover
// all of them.
var Types = []string{TypeChunks, TypeObjects}

// Root returns the directory holding every object store.
func Root(dataDir string) string {
	return filepath.Join(dataDir, "storage")
}

// TypeDir returns the directory holding one object type's stores.
func TypeDir(dataDir, objType string) string {
	return filepath.Join(Root(dataDir), objType)
}

// LibraryDir returns the directory holding one repository's objects of one type.
// The store id is not always the library's own id — a virtual library's objects live
// in its origin's store — so callers pass whichever they mean.
func LibraryDir(dataDir, objType, storeID string) string {
	return filepath.Join(TypeDir(dataDir, objType), storeID)
}

// ObjectStore is a container to access storage backend
type ObjectStore struct {
	// TypeChunks or TypeObjects
	ObjType string
	backend storageBackend
	// initErr is why there is no backend, if there is not. See New.
	initErr error
}

// writeOpts is how a pack is published.
type writeOpts struct {
	// sync makes the pack durable before write returns: the data is fsynced
	// before the publish, and the containing directory afterwards.
	sync bool
	// verify, when non-nil, is fed every byte written and its lowercase hex
	// digest must equal the pack id before the pack is published.
	//
	// A hash rather than a bool because the store holds both widths. Nothing
	// on the wire mints a SHA-1 any more — the routes pin their id variable to
	// sixty-four hex characters and ParseID refuses anything else, so a
	// 40-character id cannot arrive over HTTP — but the backend still reads
	// them, because the id's width is how an object on disk says which it is.
	// The backend does not need to know which, only that what it was handed
	// must agree with the name.
	verify hash.Hash
}

// storageBackend is the interface every storage tier implements.
//
// Pack-shaped, and specifically **sealed**-pack-shaped. A real pack has two
// lives: it is open while chunk frames are being appended to its end, and
// sealed once it reaches its target size, after which it is immutable
// forever. This interface only ever sees the second one.
//
// That split is not a simplification, it is forced. An open pack is appended
// to, and the durable tiers cannot do that — the S3 feature floor is PUT,
// ranged GET, DELETE and LIST, with no append and no multipart. So an open
// pack is local staging that lives above this interface, and a pack becomes a
// thing tiers store, replicate, evict and compact at the moment it seals.
// Where a pack holds exactly one object, as it does in the filesystem backend
// below, it is sealed the moment it is written.
//
// The mapping from chunk to (pack, offset, length) also lives above this
// interface, in the pack index. This layer stores addressable byte ranges and
// knows nothing about what is in them.
//
// Not in this interface, deliberately: anything that requires more of a
// backend than PUT, ranged GET, DELETE and LIST. Every extra verb assumed here
// is an object-storage clone this cannot run on.
// packInfo is one pack as a listing sees it. Lowercase because it is the
// backend's word; ObjectInfo is the same three fields in the vocabulary a
// caller of this package speaks.
type packInfo struct {
	id      string
	size    int64
	modTime time.Time
}

type storageBackend interface {
	// write stores r under packID and publishes it atomically. A reader
	// never sees a partial pack, and with opts.sync the pack is durable
	// before write returns.
	write(libraryID, packID string, r io.Reader, opts writeOpts) error
	// readAt reads len(p) bytes from packID starting at off, with io.ReaderAt
	// semantics: a short read returns io.EOF.
	readAt(libraryID, packID string, p []byte, off int64) (int, error)
	// read streams a whole pack into w.
	read(libraryID, packID string, w io.Writer) error
	// stat returns a pack's size, or ErrNotFound.
	stat(libraryID, packID string) (int64, error)
	// list calls fn for every pack the library holds. fn's error stops the walk
	// and is returned.
	list(libraryID string, fn func(packInfo) error) error
	// remove deletes one pack. Removing a pack that is not there is not an
	// error: deletion is idempotent because compaction has to be
	// interruptible at every step.
	remove(libraryID, packID string) error
	// removeLibrary deletes every pack a library holds.
	removeLibrary(libraryID string) error
}

// New returns a new object store for a given type of objects: TypeChunks or
// TypeObjects.
//
// A backend that cannot be created is recorded rather than returned, and every
// operation then reports it. Callers open their stores where there is nothing
// to return an error to, so the failure is carried until something asks it a
// question — which fixes the nil dereference an ignored error used to produce,
// and says what happened.
func New(confPath string, dataDir string, objType string) *ObjectStore {
	obj := &ObjectStore{ObjType: objType}
	backend, err := newFSBackend(dataDir, objType)
	if err != nil {
		obj.initErr = fmt.Errorf("objstore: no %s store: %w", objType, err)
		return obj
	}
	obj.backend = backend
	return obj
}

func (s *ObjectStore) ready() error { return s.initErr }

// Read data from storage backends.
func (s *ObjectStore) Read(libraryID string, objID string, w io.Writer) error {
	if err := s.ready(); err != nil {
		return err
	}
	return s.backend.read(libraryID, objID, w)
}

// ReadAt reads len(p) bytes of an object starting at off, with io.ReaderAt
// semantics.
//
// The whole point of the reshape: a chunk read becomes a ranged read of the
// pack holding it, which is one ranged GET against object storage rather than
// a fetch of the pack. Here, where an object is its own pack, it is a seek.
func (s *ObjectStore) ReadAt(libraryID string, objID string, p []byte, off int64) (int, error) {
	if err := s.ready(); err != nil {
		return 0, err
	}
	return s.backend.readAt(libraryID, objID, p, off)
}

// Write data to storage backends.
func (s *ObjectStore) Write(libraryID string, objID string, r io.Reader, sync bool) error {
	if err := s.ready(); err != nil {
		return err
	}
	return s.backend.write(libraryID, objID, r, writeOpts{sync: sync})
}

// WriteVerified writes an object and publishes it only if its content hashes
// to objID.
//
// Every object's id is the hash of exactly the bytes stored under it —
// chunks, manifests, directories and commits alike — so this is the invariant
// of the store itself, enforced here rather than at each caller.
//
// The digest is not the caller's to choose. See verifierFor.
//
// The check runs before the publish, not after the write, which matters: the
// object may already exist with the correct content, and a verify-then-delete
// would let one bad upload destroy a good block.
func (s *ObjectStore) WriteVerified(libraryID string, objID string, r io.Reader, sync bool) error {
	if err := s.ready(); err != nil {
		return err
	}
	return s.backend.write(libraryID, objID, r, writeOpts{sync: sync, verify: verifierFor(objID)})
}

// Exists reports whether an object is present and usable.
//
// A zero-length object counts as absent. No object type has a valid empty
// encoding, so a zero-length file is the signature of a write that was
// published but never made durable — the pre-fsync failure mode. Calling it
// present is what made that damage permanent: /check-blocks would answer that
// the client already uploaded the block, so it would never be sent again.
func (s *ObjectStore) Exists(libraryID string, objID string) (bool, error) {
	size, err := s.Stat(libraryID, objID)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return size > 0, nil
}

// Stat returns an object's size, or ErrNotFound.
func (s *ObjectStore) Stat(libraryID string, objID string) (int64, error) {
	if err := s.ready(); err != nil {
		return -1, err
	}
	return s.backend.stat(libraryID, objID)
}

// ObjectInfo is what a listing already knows about one object without opening
// it: what it is called, how big it is, and when it was last written.
//
// The modification time is here because the collector needs it and a listing
// has it in hand. An object nothing points at is indistinguishable from one
// that is about to be pointed at -- an upload in flight is unreferenced right
// up until the commit that names it -- and the only thing separating them is
// how long it has been sitting there. That makes the timestamp a safety input
// rather than a statistic, and asking for it separately meant a second lookup
// per object for something the first one had already read and discarded.
//
// A struct rather than an fs.FileInfo, because a backend that is not a
// filesystem still knows these three things and should not have to invent the
// rest of an fs.FileInfo to say so.
type ObjectInfo struct {
	ID      string
	Size    int64
	ModTime time.Time
}

// List calls fn for every object a library holds. fn's error stops the walk and
// is returned.
func (s *ObjectStore) List(libraryID string, fn func(ObjectInfo) error) error {
	if err := s.ready(); err != nil {
		return err
	}
	return s.backend.list(libraryID, func(p packInfo) error {
		return fn(ObjectInfo{ID: p.id, Size: p.size, ModTime: p.modTime})
	})
}

// Remove deletes one object. Removing an object that is not there is not an
// error.
func (s *ObjectStore) Remove(libraryID string, objID string) error {
	if err := s.ready(); err != nil {
		return err
	}
	return s.backend.remove(libraryID, objID)
}

// RemoveLibrary deletes every object a library holds.
func (s *ObjectStore) RemoveLibrary(libraryID string) error {
	if err := s.ready(); err != nil {
		return err
	}
	return s.backend.removeLibrary(libraryID)
}
