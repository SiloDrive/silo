// Package objstore is the seam between everything that stores bytes and the
// tier those bytes land on.
//
// The interface is pack-shaped — write-and-seal, ranged read, list, delete —
// because that is what the storage tiers this is heading for actually offer.
// The local filesystem backend implements it degenerately: one object per
// "pack", sealing is the atomic publish it already did, and a ranged read is a
// seek. Phase 4 of the store-v2 plan adds real packs by writing another
// implementation of this same interface, so the seam changes shape once, here,
// rather than once now and again later.
//
// The per-object API on ObjectStore is what commitmgr, fsmgr and blockmgr
// still call. It is an adapter over the pack interface and goes away with
// them.
package objstore

import (
	"errors"
	"fmt"
	"hash"
	"io"
	"path/filepath"
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

// The three object types, and the directory each one's store occupies.
//
// Exported because they are not private to this package in practice: gc walks
// the same directories to reclaim them and backup names them in its
// instructions. Both used to spell the strings out for themselves, and a
// mismatch would not have failed — gc treats a directory it cannot find as
// nothing to reclaim, so a renamed store would have turned "silo gc -delete"
// into a silent no-op that still reported success.
const (
	TypeCommits = "commits"
	TypeFS      = "fs"
	TypeBlocks  = "blocks"
)

// Types lists every object store a repository has, for callers that must
// cover all of them.
var Types = []string{TypeCommits, TypeFS, TypeBlocks}

// Root returns the directory holding every object store.
func Root(seafileDataDir string) string {
	return filepath.Join(seafileDataDir, "storage")
}

// TypeDir returns the directory holding one object type's stores.
func TypeDir(seafileDataDir, objType string) string {
	return filepath.Join(Root(seafileDataDir), objType)
}

// RepoDir returns the directory holding one repository's objects of one type.
// The store id is not always the repo's own id — a virtual repo's objects live
// in its origin's store — so callers pass whichever they mean.
func RepoDir(seafileDataDir, objType, storeID string) string {
	return filepath.Join(TypeDir(seafileDataDir, objType), storeID)
}

// ObjectStore is a container to access storage backend
type ObjectStore struct {
	// one of TypeCommits, TypeFS or TypeBlocks
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
	// A hash rather than a bool because the hash function is changing:
	// today's block ids are SHA-1 of exactly the bytes stored, and store-v2's
	// chunk ids are SHA-256 of the frame. The backend does not need to know
	// which, only that what it was handed must agree with the name.
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
type storageBackend interface {
	// write stores r under packID and publishes it atomically. A reader
	// never sees a partial pack, and with opts.sync the pack is durable
	// before write returns.
	write(repoID, packID string, r io.Reader, opts writeOpts) error
	// readAt reads len(p) bytes from packID starting at off, with io.ReaderAt
	// semantics: a short read returns io.EOF.
	readAt(repoID, packID string, p []byte, off int64) (int, error)
	// read streams a whole pack into w.
	read(repoID, packID string, w io.Writer) error
	// stat returns a pack's size, or ErrNotFound.
	stat(repoID, packID string) (int64, error)
	// list calls fn for every pack the repo holds. fn's error stops the walk
	// and is returned.
	list(repoID string, fn func(packID string, size int64) error) error
	// remove deletes one pack. Removing a pack that is not there is not an
	// error: deletion is idempotent because compaction has to be
	// interruptible at every step.
	remove(repoID, packID string) error
	// removeRepo deletes every pack a repo holds.
	removeRepo(repoID string) error
}

// New returns a new object store for a given type of objects.
// objType is one of TypeCommits, TypeFS or TypeBlocks.
//
// A backend that cannot be created is recorded rather than returned, and every
// operation then reports it. The three managers that call this have Init
// functions returning nothing, and they are deleted by the end of the store
// cutover — so widening their signatures now costs a ripple through server.go
// for code with a known end date. What this does fix is the nil dereference
// the ignored error used to produce: the failure now says what happened.
func New(seafileConfPath string, seafileDataDir string, objType string) *ObjectStore {
	obj := &ObjectStore{ObjType: objType}
	backend, err := newFSBackend(seafileDataDir, objType)
	if err != nil {
		obj.initErr = fmt.Errorf("objstore: no %s store: %w", objType, err)
		return obj
	}
	obj.backend = backend
	return obj
}

func (s *ObjectStore) ready() error { return s.initErr }

// Read data from storage backends.
func (s *ObjectStore) Read(repoID string, objID string, w io.Writer) error {
	if err := s.ready(); err != nil {
		return err
	}
	return s.backend.read(repoID, objID, w)
}

// ReadAt reads len(p) bytes of an object starting at off, with io.ReaderAt
// semantics.
//
// The whole point of the reshape: a chunk read becomes a ranged read of the
// pack holding it, which is one ranged GET against object storage rather than
// a fetch of the pack. Here, where an object is its own pack, it is a seek.
func (s *ObjectStore) ReadAt(repoID string, objID string, p []byte, off int64) (int, error) {
	if err := s.ready(); err != nil {
		return 0, err
	}
	return s.backend.readAt(repoID, objID, p, off)
}

// Write data to storage backends.
func (s *ObjectStore) Write(repoID string, objID string, r io.Reader, sync bool) error {
	if err := s.ready(); err != nil {
		return err
	}
	return s.backend.write(repoID, objID, r, writeOpts{sync: sync})
}

// WriteVerified writes an object and publishes it only if its content hashes
// to objID.
//
// For blocks — the only object type whose id is the SHA-1 of exactly the bytes
// stored — this is the invariant of the store itself, so it is enforced here
// rather than at each caller. Commit and fs ids are computed over other
// representations and cannot use this.
//
// The check runs before the publish, not after the write, which matters: the
// object may already exist with the correct content, and a verify-then-delete
// would let one bad upload destroy a good block.
func (s *ObjectStore) WriteVerified(repoID string, objID string, r io.Reader, sync bool) error {
	if err := s.ready(); err != nil {
		return err
	}
	return s.backend.write(repoID, objID, r, writeOpts{sync: sync, verify: sha1Hash()})
}

// Exists reports whether an object is present and usable.
//
// A zero-length object counts as absent. No object type has a valid empty
// encoding, so a zero-length file is the signature of a write that was
// published but never made durable — the pre-fsync failure mode. Calling it
// present is what made that damage permanent: /check-blocks would answer that
// the client already uploaded the block, so it would never be sent again.
func (s *ObjectStore) Exists(repoID string, objID string) (bool, error) {
	size, err := s.Stat(repoID, objID)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return size > 0, nil
}

// Stat returns an object's size, or ErrNotFound.
func (s *ObjectStore) Stat(repoID string, objID string) (int64, error) {
	if err := s.ready(); err != nil {
		return -1, err
	}
	return s.backend.stat(repoID, objID)
}

// List calls fn for every object a repo holds, with its size. fn's error stops
// the walk and is returned.
func (s *ObjectStore) List(repoID string, fn func(objID string, size int64) error) error {
	if err := s.ready(); err != nil {
		return err
	}
	return s.backend.list(repoID, fn)
}

// Remove deletes one object. Removing an object that is not there is not an
// error.
func (s *ObjectStore) Remove(repoID string, objID string) error {
	if err := s.ready(); err != nil {
		return err
	}
	return s.backend.remove(repoID, objID)
}

// RemoveRepo deletes every object a repo holds.
func (s *ObjectStore) RemoveRepo(repoID string) error {
	if err := s.ready(); err != nil {
		return err
	}
	return s.backend.removeRepo(repoID)
}
