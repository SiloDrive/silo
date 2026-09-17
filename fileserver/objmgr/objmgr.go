// Package objmgr is one library's stored objects: its chunks, and the
// manifests, directories and commits that describe them.
//
// It is the join between the two halves that know nothing about each other.
// [store] is the format — pure, no I/O, shared byte-for-byte with the clients.
// [objstore] is the tier — bytes addressed by id, no idea what is in them.
// This package encodes and decodes format objects, puts and gets their bytes,
// and verifies that an id names what it claims to.
//
// # The three modes
//
// A Store is in one of three states, and which one it is decides what it can
// do rather than being a flag anybody checks:
//
//   - A plain library. Everything is readable, the server does the chunking.
//   - An E2EE library with the content key. Only a client is ever here.
//   - An E2EE library without it — the server's view. Bytes go in and out and
//     ids are verified, manifests give up their public chunk list, and
//     anything needing the key says so rather than half-working.
//
// The server is permanently in the third state for an E2EE library. That is
// not a degraded mode to be fixed later; it is the product.
package objmgr

import (
	"bytes"
	"errors"
	"fmt"
	"io"

	"github.com/dkam/silo/fileserver/objstore"
	"github.com/dkam/silo/fileserver/option"
	"github.com/dkam/silo/store"
)

// ErrNoContentKey reports an operation that needs the library's content key
// where this Store does not hold one. On a server this is the ordinary answer
// for an E2EE library, not a failure.
var ErrNoContentKey = errors.New("objmgr: operation needs the library's content key")

// ErrSealedChunks refuses to assemble a manifest from chunks already stored in
// an E2EE library.
//
// Not a permission rule — an impossibility, and worth writing down as one. A
// sealed chunk is opened with a key derived from its PLAINTEXT hash, and that
// hash lives only in the manifest's sealed section. So building the manifest
// needs the plaintext hashes, and getting the plaintext hashes needs the
// manifest. An encrypted client already holds both, which is why it builds its
// manifest itself and PUTs it by id.
var ErrSealedChunks = errors.New("objmgr: a manifest cannot be assembled from sealed chunks")

// ErrIDMismatch reports bytes that do not hash to the id they were offered
// under.
var ErrIDMismatch = errors.New("objmgr: content does not match its id")

// Config describes the library a Store is opened on.
type Config struct {
	// DataDir is the server's data directory.
	DataDir string
	// StoreID is where the objects live. Not always the library's own id — a
	// virtual library's objects live in its origin's store.
	StoreID string
	// E2EE is the library's type, from the catalog. It is stated rather than
	// inferred from CK, because "encrypted library, no key here" is a real
	// and permanent state and inferring would make it indistinguishable from
	// a plain library.
	E2EE bool
	// CK is the content key, or nil. Must be nil unless E2EE.
	CK []byte
	// Params is the library's chunker, from the catalog.
	Params store.Params
}

// Store is one library's objects.
type Store struct {
	storeID string
	e2ee    bool
	ck      []byte
	params  store.Params
	chunks  *objstore.ObjectStore
	objects *objstore.ObjectStore
}

// New opens a Store on a library.
//
// The seed check is the load-bearing one, and it is store.Params.ValidateFor
// rather than a check written out here. An E2EE library chunks under a seed
// derived from its content key and a plain library under the published
// constant, and the spec says in as many words that an E2EE library must never
// chunk under the plain seed — doing so turns the manifest's seal_hash, which
// is computed over plaintext hashes, into a content-confirmation oracle that
// needs no key. That is a rule about the format, so it lives with the format:
// every client is a second implementation of store/, and one that reimplements
// the codecs but not a rule enforced up here ships the oracle silently.
// chunker.json's params_refused is the same rule as bytes, for the ports that
// never read this file.
func New(cfg Config) (*Store, error) {
	if err := cfg.Params.ValidateFor(cfg.E2EE, cfg.CK); err != nil {
		return nil, err
	}
	if cfg.StoreID == "" {
		return nil, errors.New("objmgr: no store id")
	}

	s := &Store{
		storeID: cfg.StoreID,
		e2ee:    cfg.E2EE,
		ck:      cfg.CK,
		params:  cfg.Params,
		chunks:  objstore.New(cfg.DataDir, objstore.TypeChunks),
		objects: objstore.New(cfg.DataDir, objstore.TypeObjects),
	}
	return s, nil
}

// E2EE reports the library's type.
func (s *Store) E2EE() bool { return s.e2ee }

// HasKey reports whether this Store can read and write the library's content.
// False on a server holding an E2EE library, which is the ordinary case.
func (s *Store) HasKey() bool { return len(s.ck) > 0 }

// Params returns the library's chunker parameters.
func (s *Store) Params() store.Params { return s.params }

// PutChunk stores one chunk under its id.
//
// data is the *stored* form: plaintext in a plain library, the sealed frame in
// an E2EE one. The store verifies it hashes to id before publishing, which is
// what makes this safe to call with bytes a client supplied — and is why the
// server can accept chunks for a library it cannot read without trusting the
// uploader about what they are.
// PutChunks stores several chunks as one unit of work.
//
// The batch is the point, not a convenience. Written one at a time, a file's
// chunks take the store's write lock separately and can interleave with a
// concurrent upload's — so two files end up shuffled together wherever they
// land, and nothing downstream can unshuffle them, because only this layer
// knows which chunks belong to which file. Written together they stay together.
//
// It also costs one durability barrier instead of one per chunk.
//
// Every chunk is verified against its id before any is stored, so a batch
// carrying one bad chunk stores none of itself.
func (s *Store) PutChunks(chunks []objstore.Object) error {
	if len(chunks) == 0 {
		return nil
	}
	if err := s.chunks.WriteBatch(s.storeID, chunks, option.SyncObjectWrites); err != nil {
		return fmt.Errorf("failed to store %d chunks: %w", len(chunks), err)
	}
	return nil
}

func (s *Store) PutChunk(id store.ID, data []byte) error {
	if err := s.chunks.WriteVerified(s.storeID, id.String(), bytes.NewReader(data), option.SyncObjectWrites); err != nil {
		if errors.Is(err, objstore.ErrContentMismatch) {
			return fmt.Errorf("%w: chunk %s", ErrIDMismatch, id)
		}
		return err
	}
	return nil
}

// GetChunk returns a chunk's stored bytes, still sealed in an E2EE library.
func (s *Store) GetChunk(id store.ID) ([]byte, error) {
	return s.chunks.ReadInto(s.storeID, id.String(), nil)
}

// HasChunk reports whether a chunk is present and usable.
func (s *Store) HasChunk(id store.ID) (bool, error) {
	return s.chunks.Exists(s.storeID, id.String())
}

// ChunkSize returns a chunk's plaintext length without reading it.
//
// This is the number a manifest records, and it is derived rather than read
// because storage framing is exactly the difference: a sealed chunk is
// TagSize longer on disk than the bytes it holds, and store.ChunkRef pins
// Size as plaintext so a client can map a read offset onto a chunk. Deriving
// it is the same rule putChunkData applies from the other direction.
//
// Callers that want a size and nothing else must use this rather than
// GetChunk. A 4 MiB chunk read in full to have its length measured is roughly
// eight hundred times the cost of asking the filesystem, and allocates the
// whole chunk to throw it away.
func (s *Store) ChunkSize(id store.ID) (int64, error) {
	size, err := s.ChunkStoredSize(id)
	if err != nil {
		return 0, err
	}
	if !s.e2ee {
		return size, nil
	}
	if size < store.TagSize {
		return 0, fmt.Errorf("%w: chunk %s is %d bytes, shorter than a sealed frame", ErrIDMismatch, id, size)
	}
	return size - store.TagSize, nil
}

// ChunkStoredSize returns what the chunk occupies on disk, tag included.
//
// The distinction from ChunkSize is the whole reason both exist: a caller
// answering "how many bytes does this take" wants this one, and a caller
// filling in a manifest wants the plaintext length. Under E2EE they differ by
// TagSize, and picking the wrong one is invisible in a plain library and wrong
// in an encrypted one — which is the worst way for a difference to be found.
func (s *Store) ChunkStoredSize(id store.ID) (int64, error) {
	return s.chunks.Stat(s.storeID, id.String())
}

// PutObject stores an already-encoded object under its id, verifying it.
//
// This is the server's ingest path for manifests, directories and commits in
// an E2EE library: it cannot decode them, and it does not need to. What it can
// do — and must — is refuse bytes that are not what their id says, because an
// id that names something else is the one corruption a content-addressed store
// can catch on its own.
func (s *Store) PutObject(id store.ID, encoded []byte) error {
	if err := s.objects.WriteVerified(s.storeID, id.String(), bytes.NewReader(encoded), option.SyncObjectWrites); err != nil {
		if errors.Is(err, objstore.ErrContentMismatch) {
			return fmt.Errorf("%w: object %s", ErrIDMismatch, id)
		}
		return err
	}
	return nil
}

// GetChunkInto reads a chunk into buf, growing it only when it is too small,
// and returns the bytes read as a sub-slice of it.
//
// GetChunk allocates one chunk per call, which is invisible one chunk at a
// time and is not invisible on the batch-fetch path, where a single request
// moves up to 256 of them. One scratch buffer reused down the loop replaces
// those 256 allocations: the chunk is decrypted straight into it.
//
// The returned slice aliases buf. Callers pass it back in on the next call.
func (s *Store) GetChunkInto(id store.ID, buf []byte) ([]byte, error) {
	return s.chunks.ReadInto(s.storeID, id.String(), buf)
}

// ObjectSize returns an object's stored size without reading it.
//
// The sibling of ChunkStoredSize, and it exists for the same reason that one
// does: a HEAD wants a Content-Length, and reading a manifest to measure it
// costs the whole object. A 1 TiB file's manifest is some 36 MB.
func (s *Store) ObjectSize(id store.ID) (int64, error) {
	return s.objects.Stat(s.storeID, id.String())
}

// GetObject returns an object's encoded bytes.
func (s *Store) GetObject(id store.ID) ([]byte, error) {
	return s.objects.ReadInto(s.storeID, id.String(), nil)
}

// HasObject reports whether an object is present and usable.
func (s *Store) HasObject(id store.ID) (bool, error) {
	return s.objects.Exists(s.storeID, id.String())
}

// putEncoded stores encoded bytes under their own hash and returns the id.
func (s *Store) putEncoded(encoded []byte) (store.ID, error) {
	return s.storeObject(encoded)
}

// storeChunk and storeObject hash the bytes and write them under their own id.
//
// They exist so the internal write path is not SHA-256'd twice. PutChunk and
// PutObject go through objstore.WriteVerified, which hashes what it is given
// and refuses it unless it matches the id -- the right rule for the exported
// ingest of client-supplied bytes, where the id is a claim someone made about
// content the server did not produce. On this path the id was computed from
// the identical slice a few instructions earlier, so verifying it re-derives a
// number we already hold: measured on a 1 MiB chunk, about a third of the
// write.
//
// The invariant WriteVerified's comment defends -- that an object's id is the
// hash of exactly the bytes stored under it -- is not weakened, because these
// take no id to get wrong. A caller cannot pass a mismatched one; there is
// nothing to pass. That is a stronger guarantee than checking a caller's
// arithmetic after the fact, and it is why these hash rather than accepting a
// precomputed digest.
func (s *Store) storeChunk(data []byte) (store.ID, error) {
	id := store.ChunkID(data)
	if err := s.chunks.Write(s.storeID, id.String(), bytes.NewReader(data), option.SyncObjectWrites); err != nil {
		return store.ID{}, err
	}
	return id, nil
}

func (s *Store) storeObject(encoded []byte) (store.ID, error) {
	id := store.ObjectID(encoded)
	if err := s.objects.Write(s.storeID, id.String(), bytes.NewReader(encoded), option.SyncObjectWrites); err != nil {
		return store.ID{}, err
	}
	return id, nil
}

// PutManifest encodes a manifest for this library's type and stores it.
//
// Validate runs here rather than in the writers because this is the seam every
// manifest crosses on its way to becoming an id, and there are three ways to
// reach it: WriteFile from bytes, ManifestFromChunks from ids already stored,
// and a handler assembling one directly. A manifest that breaks the format's
// rules — inline-iff-small, chunk sizes summing to FileSize — must not get an
// id, because once it has one it is a real object and the failure surfaces on
// a client at read time, a long way from whoever wrote it.
func (s *Store) PutManifest(m *store.Manifest) (store.ID, error) {
	if err := m.Validate(); err != nil {
		return store.ID{}, err
	}
	encoded, err := s.encodeManifest(m)
	if err != nil {
		return store.ID{}, err
	}
	return s.putEncoded(encoded)
}

func (s *Store) encodeManifest(m *store.Manifest) ([]byte, error) {
	if !s.e2ee {
		return m.Encode()
	}
	if !s.HasKey() {
		return nil, ErrNoContentKey
	}
	return m.EncodeSealed(s.ck)
}

// GetManifest reads a manifest, opening its sealed section.
func (s *Store) GetManifest(id store.ID) (*store.Manifest, error) {
	encoded, err := s.GetObject(id)
	if err != nil {
		return nil, err
	}
	if !s.e2ee {
		return store.DecodeManifest(encoded)
	}
	if !s.HasKey() {
		return nil, ErrNoContentKey
	}
	return store.DecodeSealedManifest(encoded, s.ck)
}

// GetManifestPublic reads a manifest's public chunk list without needing the
// content key. This is the garbage collector's path, and the only way a server
// can enumerate an E2EE library's chunks — see store.DecodeManifestPublic for
// why a client must not use it.
func (s *Store) GetManifestPublic(id store.ID) (*store.PublicManifest, error) {
	encoded, err := s.GetObject(id)
	if err != nil {
		return nil, err
	}
	return store.DecodeManifestPublic(encoded)
}

// GetDirectoryPublic reads a directory's edges without a content key: which
// objects it points at and what kind each one is. Under E2EE the names come
// back as ciphertext and the mtimes and modes come back zero, because they are
// sealed.
//
// This is the server's half of a tree walk, and the only half it will ever
// have on an encrypted library. It is what the tracing collector marks from.
func (s *Store) GetDirectoryPublic(id store.ID) (*store.PublicDirectory, error) {
	encoded, err := s.GetObject(id)
	if err != nil {
		return nil, err
	}
	return store.DecodeDirectoryPublic(encoded)
}

// GetCommitPublic reads a commit's root, parents and timestamp without a
// content key. It is where every server-side walk starts.
func (s *Store) GetCommitPublic(id store.ID) (*store.PublicCommit, error) {
	encoded, err := s.GetObject(id)
	if err != nil {
		return nil, err
	}
	return store.DecodeCommitPublic(encoded)
}

// PutDirectory encodes a directory object for this library's type and stores it.
func (s *Store) PutDirectory(d *store.Directory) (store.ID, error) {
	var encoded []byte
	var err error
	switch {
	case !s.e2ee:
		encoded, err = d.Encode()
	case s.HasKey():
		encoded, err = d.EncodeSealed(s.ck)
	default:
		return store.ID{}, ErrNoContentKey
	}
	if err != nil {
		return store.ID{}, err
	}
	return s.putEncoded(encoded)
}

// GetDirectory reads a directory object.
func (s *Store) GetDirectory(id store.ID) (*store.Directory, error) {
	encoded, err := s.GetObject(id)
	if err != nil {
		return nil, err
	}
	switch {
	case !s.e2ee:
		return store.DecodeDirectory(encoded)
	case s.HasKey():
		return store.DecodeSealedDirectory(encoded, s.ck)
	default:
		return nil, ErrNoContentKey
	}
}

// PutCommit encodes a commit for this library's type and stores it.
func (s *Store) PutCommit(c *store.Commit) (store.ID, error) {
	var encoded []byte
	var err error
	switch {
	case !s.e2ee:
		encoded, err = c.Encode()
	case s.HasKey():
		encoded, err = c.EncodeSealed(s.ck)
	default:
		return store.ID{}, ErrNoContentKey
	}
	if err != nil {
		return store.ID{}, err
	}
	return s.putEncoded(encoded)
}

// GetCommit reads a commit.
func (s *Store) GetCommit(id store.ID) (*store.Commit, error) {
	encoded, err := s.GetObject(id)
	if err != nil {
		return nil, err
	}
	switch {
	case !s.e2ee:
		return store.DecodeCommit(encoded)
	case s.HasKey():
		return store.DecodeSealedCommit(encoded, s.ck)
	default:
		return nil, ErrNoContentKey
	}
}

// WriteFile chunks r, stores every chunk, and returns the manifest describing
// it. The manifest is not stored — the caller decides where it belongs in a
// tree first, and an orphaned manifest is garbage the collector has to reason
// about.
//
// A file under the inline threshold becomes a manifest carrying its own bytes
// and no chunks at all. Whether that happens is decided by the size and never
// by the caller: two writers disagreeing about a 30 KB file would mint two
// manifest ids for identical content, and every reader downstream would see a
// modification that did not happen.
func (s *Store) WriteFile(r io.Reader) (*store.Manifest, error) {
	if s.e2ee && !s.HasKey() {
		return nil, ErrNoContentKey
	}

	// Read up to the inline threshold before deciding. A short file never
	// reaches the chunker at all; a long one is chunked from the start, with
	// what was read already put back in front of it.
	head := make([]byte, store.InlineThreshold)
	n, err := io.ReadFull(r, head)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return nil, err
	}
	head = head[:n]
	if n < store.InlineThreshold {
		// Cloned, not sliced. head is a store.InlineThreshold array and the
		// manifest is the return value, so slicing would hand the caller 64 KiB
		// of live backing array per file however few bytes the file holds.
		return &store.Manifest{FileSize: int64(n), Inline: bytes.Clone(head)}, nil
	}

	m := &store.Manifest{}
	chunker, err := store.NewChunker(s.params, io.MultiReader(bytes.NewReader(head), r))
	if err != nil {
		return nil, err
	}
	for {
		ch, err := chunker.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		ref, err := s.putChunkData(ch.Data)
		if err != nil {
			return nil, err
		}
		m.FileSize += ref.Size
		m.Chunks = append(m.Chunks, ref)
	}
	return m, nil
}

// ManifestFromChunks builds the manifest for a file whose chunks are already
// stored, named in order.
//
// This is WriteFile's other half: the same verb — content in, manifest out —
// for a resumable upload, where the bytes arrived one chunk at a time through
// the chunk surface and all that is left is to say what order they go in. It
// lives beside WriteFile because it answers the same format questions, and
// answering them twice is how two writers come to disagree about a 30 KB file
// and mint two ids for identical content.
//
// Every occurrence gets a ref, including a repeat: a file that repeats a chunk
// — a run of zeroes, a duplicated section — is one object on disk and two
// entries in the manifest, and the size counts it twice because the repeat is
// a real part of the file. A missing id is reported once however often it
// appears, so a client is never told to upload the same bytes twice.
//
// A non-empty missing list is not an error: the request was well formed and
// the content it names is simply not here yet, which is a different thing from
// a request that can never succeed. The caller gets the list so it can say
// which chunks to send.
func (s *Store) ManifestFromChunks(ids []store.ID) (*store.Manifest, []store.ID, error) {
	if s.e2ee {
		return nil, nil, ErrSealedChunks
	}

	var missing []store.ID
	seen := make(map[store.ID]bool, len(ids))
	sizes := make(map[store.ID]int64, len(ids))
	refs := make([]store.ChunkRef, 0, len(ids))
	var size int64

	for _, id := range ids {
		sz, known := sizes[id]
		if !known {
			var err error
			sz, err = s.ChunkSize(id)
			if err != nil {
				if errors.Is(err, objstore.ErrNotFound) {
					if !seen[id] {
						seen[id] = true
						missing = append(missing, id)
					}
					continue
				}
				return nil, nil, fmt.Errorf("chunk %s: %w", id, err)
			}
			sizes[id] = sz
		}
		refs = append(refs, store.ChunkRef{ID: id, Size: sz})
		size += sz
	}
	if len(missing) > 0 {
		return nil, missing, nil
	}

	// Whether a file inlines is decided by its size and never by a writer, so a
	// small file assembled from chunks becomes an inline manifest here — which
	// means reading those chunks back to put the bytes in. WriteFile decides it
	// the same way from the other direction.
	if !store.Inlined(size) {
		return &store.Manifest{FileSize: size, Chunks: refs}, nil, nil
	}
	inline := make([]byte, 0, size)
	for i, ref := range refs {
		data, err := s.openChunk(ref, i, len(refs))
		if err != nil {
			return nil, nil, err
		}
		inline = append(inline, data...)
	}
	return &store.Manifest{FileSize: size, Inline: inline}, nil, nil
}

// putChunkData stores one chunk of plaintext, sealing it first in an E2EE
// library, and returns the manifest entry for it.
//
// Size is the chunk's plaintext length in both library types. The stored
// object is sixteen bytes longer under E2EE, and that difference is storage
// framing the manifest deliberately does not carry — a client mapping a read
// offset to a chunk needs plaintext lengths.
func (s *Store) putChunkData(data []byte) (store.ChunkRef, error) {
	size := int64(len(data))
	if !s.e2ee {
		id, err := s.storeChunk(data)
		if err != nil {
			return store.ChunkRef{}, err
		}
		return store.ChunkRef{ID: id, Size: size}, nil
	}

	sealed, err := store.SealChunk(s.ck, data)
	if err != nil {
		return store.ChunkRef{}, err
	}
	// storeChunk returns ChunkID(sealed.Frame), which is what SealChunk
	// already put in sealed.ID -- an E2EE chunk is addressed by its sealed
	// bytes. TestTheSealedChunkIsStoredUnderTheIdSealChunkGave pins that.
	id, err := s.storeChunk(sealed.Frame)
	if err != nil {
		return store.ChunkRef{}, err
	}
	return store.ChunkRef{ID: id, Size: size, PlaintextHash: sealed.PlaintextHash}, nil
}

// ReadFileRange writes n bytes of a manifest's file content to w, starting at
// off. A negative or over-long n means "to the end of the file".
//
// The whole point of the manifest carrying PLAINTEXT chunk sizes is here: the
// range maps to a run of chunks by arithmetic on those sizes alone, so a read
// at an offset fetches only the chunks it overlaps rather than the file. The
// stored size differs (+16 for the content tag under E2EE), which is exactly
// why the plaintext size is the one pinned in the format — client arithmetic
// must never depend on storage framing.
//
// An inline manifest is sliced directly; there are no chunks to walk, and a
// file small enough to inline is small enough that this is the whole cost.
func (s *Store) ReadFileRange(m *store.Manifest, off, n int64, w io.Writer) error {
	if err := m.Validate(); err != nil {
		return err
	}
	if off < 0 {
		return fmt.Errorf("objmgr: negative read offset %d", off)
	}
	if off >= m.FileSize {
		return nil
	}
	end := m.FileSize
	if n >= 0 && off+n < end {
		end = off + n
	}

	if store.Inlined(m.FileSize) {
		_, err := w.Write(m.Inline[off:end])
		return err
	}
	if s.e2ee && !s.HasKey() {
		return ErrNoContentKey
	}

	var pos int64
	for i, ref := range m.Chunks {
		next := pos + ref.Size
		// Wholly before the range, or wholly after it. The second case ends
		// the walk rather than continuing it — the chunks are ordered, so
		// nothing later can overlap either.
		if next <= off {
			pos = next
			continue
		}
		if pos >= end {
			break
		}
		data, err := s.openChunk(ref, i, len(m.Chunks))
		if err != nil {
			return err
		}
		lo := int64(0)
		if off > pos {
			lo = off - pos
		}
		hi := ref.Size
		if end < next {
			hi = end - pos
		}
		if _, err := w.Write(data[lo:hi]); err != nil {
			return err
		}
		pos = next
	}
	return nil
}

// ReadFile writes a manifest's file content to w.
//
// The whole file is the range starting at nothing and running to the end, and
// saying so is the entire implementation: Manifest.Validate guarantees the
// inline slice is the file and that the chunk sizes sum to FileSize, so every
// chunk of that range is taken whole. Written out separately it was a second
// copy of the E2EE-without-key refusal and the chunk walk, three lines apart,
// and the two would have drifted the first time either grew a check.
func (s *Store) ReadFile(m *store.Manifest, w io.Writer) error {
	return s.ReadFileRange(m, 0, -1, w)
}

// openChunk fetches one chunk of a manifest and returns its plaintext, having
// checked that it is the chunk the manifest named and the length the manifest
// claimed. i and total are for the error message only — "chunk 3 of 900" says
// where a file went wrong, which a bare id does not.
func (s *Store) openChunk(ref store.ChunkRef, i, total int) ([]byte, error) {
	stored, err := s.GetChunk(ref.ID)
	if err != nil {
		return nil, fmt.Errorf("chunk %d of %d (%s): %w", i+1, total, ref.ID, err)
	}
	data := stored
	if s.e2ee {
		data, err = store.OpenChunk(s.ck, ref.PlaintextHash, stored)
		if err != nil {
			return nil, fmt.Errorf("chunk %d of %d (%s): %w", i+1, total, ref.ID, err)
		}
	} else if store.ChunkID(data) != ref.ID {
		// A plain library has no tag to catch a substituted chunk, so the
		// id is the check, and it is worth making rather than assuming:
		// the bytes came off a disk the threat model does not trust.
		return nil, fmt.Errorf("chunk %d of %d: %w: %s", i+1, total, ErrIDMismatch, ref.ID)
	}
	if int64(len(data)) != ref.Size {
		return nil, fmt.Errorf("chunk %d of %d (%s): %d bytes, manifest says %d",
			i+1, total, ref.ID, len(data), ref.Size)
	}
	return data, nil
}
