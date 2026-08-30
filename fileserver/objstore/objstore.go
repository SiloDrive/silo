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
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"time"

	"github.com/dkam/silo/fileserver/option"
)

// ErrContentMismatch is returned by a verified write whose bytes do not hash
// to the id they were offered under. It is a sentinel because the caller's
// answer depends on who supplied the bytes: an ingest path handed a bad chunk
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

// ErrReclaimDeferred reports an object that cannot be deleted where it is, and
// whose space a later background rewrite reclaims instead.
//
// A sentinel because a caller has to be able to tell it from a failure. It is
// not "the delete went wrong": the object is exactly where it should be. A
// collector that treats this as an error stops; one that treats it as success
// reports space it did not free. Neither is right, and only a named error lets
// a caller pick the third answer.
//
// Named for the property rather than for the container, so that the caller does
// not have to learn what a pack is to count it — which is the invariant this
// package is built to keep. The wrapped text carries the specifics, and a
// second reason to defer a reclaim (an immutable object on a durable tier, say)
// arrives as more text rather than as a second special case at every collector.
var ErrReclaimDeferred = errors.New("objstore: the object cannot be deleted in place, and its space is reclaimed by a later rewrite")

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
	// key is storage.key: what every object in this store is sealed under.
	// See frame.go and key.go.
	key []byte
	// packs is the lookup: which pack, if any, holds an object. It sits above
	// the backend rather than inside one, because the seam takes whole sealed
	// packs and knows nothing about what is in them. See packstore.go.
	packs *packStore
	// packWrites is whether new objects go into a pack, resolved once here
	// rather than read per write. Which container a store writes into is a
	// property of the store: reading the global on each object would let two
	// writes to one open pack disagree, and would have a test that toggles the
	// flag mutating shared state under a live sealer.
	packWrites bool
	// backendErr is why there is no backend, if there is not. Everything fails
	// on it: with no backend there is nothing to ask.
	backendErr error
	// keyErr is why there is no storage key, if there is not — kept apart from
	// backendErr because far less depends on it than it first appears.
	//
	// The key opens and seals frames. It has nothing to do with how many bytes
	// a library occupies or with deleting them, so a store that cannot find its
	// key can still be measured, listed and reclaimed. That distinction is the
	// difference between a server that lost storage.key being unable to read
	// its objects — which is true and unavoidable — and being unable to free
	// the disk they are sitting on, which would be gratuitous and would bite at
	// exactly the worst moment.
	keyErr error
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
	// never sees a partial pack, and with sync the pack is durable before
	// write returns.
	write(libraryID, packID string, r io.Reader, sync bool) error
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
	// libraryUsage reports how many files a library occupies and how many
	// bytes, counting everything removeLibrary would delete.
	//
	// The pair matters more than either half. A dry run has to report what the
	// delete pass will actually free, so these two have to agree about what
	// "everything this library holds" means — including the debris a listing
	// deliberately skips, which removeLibrary deletes all the same.
	libraryUsage(libraryID string) (files int, bytes int64, err error)
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

	// The key is loaded after the backend because generating one refuses over
	// a store that already holds objects, and answering that question needs
	// the store directories to be the ones this backend will use.
	backend, err := newFSBackend(dataDir, objType)
	if err != nil {
		obj.backendErr = fmt.Errorf("objstore: no %s store: %w", objType, err)
		return obj
	}
	obj.backend = backend
	obj.packs = packStoreFor(TypeDir(dataDir, objType))
	obj.packWrites = option.PackWrites

	key, err := storageKeyFor(dataDir)
	if err != nil {
		obj.keyErr = fmt.Errorf("objstore: no %s store: %w", objType, err)
		return obj
	}
	obj.key = key
	return obj
}

// ready reports whether this store can move object bytes, which needs both the
// backend and the key.
func (s *ObjectStore) ready() error {
	if s.backendErr != nil {
		return s.backendErr
	}
	return s.keyErr
}

// present reports whether this store can answer questions about what it holds
// and remove things, which needs the backend alone.
//
// Sizes, listings and deletions never touch a frame's contents: a size is
// arithmetic or an index lookup, and a deletion is a path. So they are gated on
// the backend rather than on the key, and "silo gc" keeps working on a store
// whose key is gone.
func (s *ObjectStore) present() error { return s.backendErr }

// Read data from storage backends.
func (s *ObjectStore) Read(libraryID string, objID string, w io.Writer) error {
	if err := s.ready(); err != nil {
		return err
	}
	obj, err := s.object(libraryID, objID, nil)
	if err != nil {
		return err
	}
	_, err = w.Write(obj)
	return err
}

// ReadInto reads a whole object into buf, growing it only when it is too
// small, and returns the object as a sub-slice of it.
//
// This is the shape every reader of this store actually wants, and the one
// that costs least. An object cannot be read in parts — see object — so a
// caller that hands its own buffer down a loop pays no allocation per object
// at all: the frame is read into a buffer sized from stat, and the plaintext
// is decrypted straight into buf. The returned slice aliases buf, and callers
// pass it back in on the next call.
func (s *ObjectStore) ReadInto(libraryID string, objID string, buf []byte) ([]byte, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	return s.object(libraryID, objID, buf)
}

// object reads one object and opens its frame, appending the bytes the caller
// stored to dst.
//
// Whole, not streamed, and that is the shape of GCM rather than a choice: the
// tag covers the entire ciphertext, so there is no prefix of a frame that can
// be trusted before the last byte has been read. A ranged read of a *pack* is
// a range over frames; there is no ranged read within one.
//
// How the frame is fetched, and why it is fetched in one pass, is frame's.
func (s *ObjectStore) object(libraryID string, objID string, dst []byte) ([]byte, error) {
	frame, err := s.frame(libraryID, objID)
	if err != nil {
		return nil, err
	}
	// Everything in the store is a frame. Anything that is not is corruption,
	// and openFrame says so — there is no reading of unsealed bytes here,
	// because a store that holds any is one that must not have started.
	return openFrame(s.key, objID, frame, dst)
}

// frame reads the sealed frame an object is stored in, from whichever of the
// two places holds it.
//
// A pack is asked first. That order is not an optimisation — it is what makes
// ingest safe to interrupt: ingest appends a frame to a pack and deletes the
// loose copy once the index is durable, so an object can exist in both places
// at once, and the pack is the copy that will still be there afterwards.
//
// The loose read is sized from stat and read in one pass rather than copied
// into a growing buffer. The growth is not free at these sizes — a 4 MiB chunk
// reallocates a dozen times and memcpys twice its own length before it is even
// decrypted — and the size is already one syscall away. A packed read needs no
// stat at all: the index already said how long the frame is.
func (s *ObjectStore) frame(libraryID string, objID string) ([]byte, error) {
	pack, e, ok, err := s.packs.find(libraryID, objID)
	if err != nil {
		return nil, err
	}
	if ok {
		return pack.readFrameAt(e)
	}

	size, err := s.backend.stat(libraryID, objID)
	if err != nil {
		return nil, err
	}
	frame := make([]byte, size)
	if _, err := s.backend.readAt(libraryID, objID, frame, 0); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return frame, nil
}

// ReadAt reads len(p) bytes of an object starting at off, with io.ReaderAt
// semantics.
//
// The whole point of the reshape: a chunk read becomes a ranged read of the
// pack holding it, which is one ranged GET against object storage rather than
// a fetch of the pack. Here, where an object is its own pack, it is a seek.
//
// Ranged in what it returns, never in what it costs: a range of one frame is
// the whole frame read and the whole object decrypted, because the tag covers
// all of it. Reading one object in k ranges therefore costs k times reading
// it once. ReadInto is the call for reading a whole object, and it is what
// every reader in this repository uses.
func (s *ObjectStore) ReadAt(libraryID string, objID string, p []byte, off int64) (int, error) {
	if err := s.ready(); err != nil {
		return 0, err
	}
	obj, err := s.object(libraryID, objID, nil)
	if err != nil {
		return 0, err
	}
	if off < 0 {
		return 0, fmt.Errorf("objstore: negative offset %d", off)
	}
	if off >= int64(len(obj)) {
		return 0, io.EOF
	}
	n := copy(p, obj[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

// Write data to storage backends.
func (s *ObjectStore) Write(libraryID string, objID string, r io.Reader, sync bool) error {
	if err := s.ready(); err != nil {
		return err
	}
	return s.write(libraryID, objID, r, sync, false)
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
// would let one bad upload destroy a good chunk.
func (s *ObjectStore) WriteVerified(libraryID string, objID string, r io.Reader, sync bool) error {
	if err := s.ready(); err != nil {
		return err
	}
	return s.write(libraryID, objID, r, sync, true)
}

// write seals an object into a frame and hands the frame to the backend.
//
// The verification is over the bytes the caller offered, never over the frame.
// That ordering is the whole of what WriteVerified promises: an id names the
// stored object, and a frame is a container the store puts around it. Hashing
// the frame instead would still pass — a frame is self-consistent — while
// checking nothing anybody asked about.
//
// Buffered whole, for the reason object() is: GCM is one-shot. A 1 MiB chunk
// is nothing; the manifest of a 1 TiB file is some 36 MB, and splitting a
// large object across several frames is a pack-format question that arrives
// with packs.
func (s *ObjectStore) write(libraryID string, objID string, r io.Reader, sync bool, verify bool) error {
	plain, err := readWhole(r)
	if err != nil {
		return err
	}
	if verify {
		h := verifier()
		h.Write(plain)
		// Checked before the frame is built, not after the write: the object
		// may already be there with the right content, and a
		// verify-then-delete would let one bad upload destroy a good chunk.
		if got := hex.EncodeToString(h.Sum(nil)); got != objID {
			return fmt.Errorf("object %s/%s hashes to %s: %w", libraryID, objID, got, ErrContentMismatch)
		}
	}
	frame, err := sealFrame(s.key, objID, plain)
	if err != nil {
		return err
	}
	if s.packWrites {
		// The frame is the same bytes either way. That is the property that
		// makes ingest a copy rather than a re-seal, and it is why this is a
		// choice of container at the last moment rather than two write paths.
		return s.packs.append(libraryID, objID, frame, sync)
	}
	return s.backend.write(libraryID, objID, bytes.NewReader(frame), sync)
}

// readWhole reads r to EOF, sized up front when r can say how much it holds.
//
// Both callers of Write and WriteVerified hand this a bytes.Reader over a
// slice they are already holding, so io.ReadAll's growth — a dozen
// reallocations and twice the object copied, for a 4 MiB chunk — buys
// nothing. Anything that genuinely streams still works; it just does not know
// its length, and falls through.
func readWhole(r io.Reader) ([]byte, error) {
	sized, ok := r.(interface{ Len() int })
	if !ok {
		return io.ReadAll(r)
	}
	buf := make([]byte, sized.Len())
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

// Exists reports whether an object is present and usable.
//
// A zero-length object counts as absent. No object type has a valid empty
// encoding, so a zero-length file is the signature of a write that was
// published but never made durable — the pre-fsync failure mode. Calling it
// present is what made that damage permanent: /check-blocks would answer that
// the client already uploaded the chunk, so it would never be sent again.
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
//
// The size of the object, not of the file holding it. Every caller of this is
// asking about the bytes it stored — ChunkStoredSize and ObjectSize turn it
// into a Content-Length, and GetChunkInto sizes a buffer with it — so
// answering with the file size would be wrong by exactly the frame overhead,
// on the wire, silently.
func (s *ObjectStore) Stat(libraryID string, objID string) (int64, error) {
	if err := s.present(); err != nil {
		return -1, err
	}
	// Out of the index when a pack holds it, which keeps this one lookup and
	// no read — the same property the fixed-width frame header was built to
	// give the loose store, arrived at the other way round.
	_, e, ok, err := s.packs.find(libraryID, objID)
	if err != nil {
		return -1, err
	}
	if ok {
		return e.plaintextLen(), nil
	}

	size, err := s.backend.stat(libraryID, objID)
	if err != nil {
		return -1, err
	}
	return objectSize(size), nil
}

// objectSize turns a file size into the size of the object inside it.
//
// Arithmetic, and no read: the frame's widths are all fixed precisely so that
// this question costs one stat. That is why ct_len is eight bytes rather than
// a varint.
func objectSize(fileSize int64) int64 {
	if fileSize <= int64(frameOverhead) {
		// Nothing a frame that size could hold: the torn write, caught by the
		// same rule that has always called a zero-length object absent.
		return 0
	}
	return fileSize - int64(frameOverhead)
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

// List calls fn for every object a library holds, packed or loose. fn's error
// stops the walk and is returned.
//
// Each object is reported once even while it is in both places. Ingest appends
// a frame to a pack and deletes the loose copy only once the index is durable,
// so the two overlap by design, and a census that counted such an object twice
// would report bytes that are not there — which is exactly the number an
// operator reads before deciding to reclaim.
//
// The set of packed ids is held for the duration, which is the cost of that
// guarantee: a full store is millions of ids. It is the same shape the census
// above this already builds to answer reachability, so it is not a new order
// of memory — but it is why this is a walk rather than a stream that could
// forget what it had seen.
func (s *ObjectStore) List(libraryID string, fn func(ObjectInfo) error) error {
	if err := s.present(); err != nil {
		return err
	}

	packed := map[string]struct{}{}
	err := s.packs.each(libraryID, func(e indexEntry, modTime time.Time) error {
		if _, seen := packed[e.ID]; seen {
			// The same object in two packs, which ingest can produce and
			// compaction resolves. One report, not two.
			return nil
		}
		packed[e.ID] = struct{}{}
		return fn(ObjectInfo{ID: e.ID, Size: e.plaintextLen(), ModTime: modTime})
	})
	if err != nil {
		return err
	}

	return s.backend.list(libraryID, func(p packInfo) error {
		if _, seen := packed[p.id]; seen {
			return nil
		}
		return fn(ObjectInfo{ID: p.id, Size: objectSize(p.size), ModTime: p.modTime})
	})
}

// Remove deletes one object. Removing an object that is not there is not an
// error.
//
// An object inside a pack is refused rather than deleted, and that refusal is
// the point of ErrReclaimDeferred being a sentinel. A pack is immutable once
// sealed, so there is no delete to perform: the bytes come back when compaction
// rewrites the pack without them (silo#19). The dangerous version of this
// function is the one that does not check — the loose path is already gone, os.Remove on a
// missing file is deliberately not an error here, so it would return success
// and let "silo gc -delete" report bytes reclaimed that are still on the disk.
func (s *ObjectStore) Remove(libraryID string, objID string) error {
	if err := s.present(); err != nil {
		return err
	}
	_, _, packed, err := s.packs.find(libraryID, objID)
	if err != nil {
		return err
	}
	if packed {
		return fmt.Errorf("%s/%s is inside a sealed pack, which is immutable; compaction reclaims it: %w",
			libraryID, objID, ErrReclaimDeferred)
	}
	return s.backend.remove(libraryID, objID)
}

// LibraryUsage reports how many files a library occupies and how many bytes,
// counting everything RemoveLibrary would delete.
//
// Deliberately not the same question as List. A listing answers about
// *objects* — well-formed ids, plaintext sizes, one entry per object — because
// that is what a census and a collector need. This answers about *storage*:
// every byte the delete pass frees, including a pack's footer and filter, the
// frame overhead around each object, and the debris of an interrupted write
// that a listing skips on purpose.
//
// Reporting the listing's number instead would make "silo gc" quote a figure
// smaller than the space that came back, every time, and an operator
// reconciling that against df would have nothing to find.
func (s *ObjectStore) LibraryUsage(libraryID string) (files int, bytes int64, err error) {
	if err := s.present(); err != nil {
		return 0, 0, err
	}
	return s.backend.libraryUsage(libraryID)
}

// RemoveLibrary deletes every object a library holds.
func (s *ObjectStore) RemoveLibrary(libraryID string) error {
	if err := s.present(); err != nil {
		return err
	}
	// The cached packs go first. They are footers in memory and file paths,
	// and keeping them past the deletion would leave this store answering
	// reads out of files that are no longer there.
	s.packs.forget(libraryID)
	return s.backend.removeLibrary(libraryID)
}
