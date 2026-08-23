// Package objmgr is one library's store-v2 objects: its chunks, and the
// manifests, directories and commits that describe them.
//
// It is the join between the two halves that know nothing about each other.
// [store] is the format — pure, no I/O, shared byte-for-byte with the clients.
// [objstore] is the tier — bytes addressed by id, no idea what is in them.
// This package encodes and decodes format objects, puts and gets their bytes,
// and verifies that an id names what it claims to.
//
// It replaces fsmgr, blockmgr and commitmgr, and for now it lives alongside
// them: nothing here is wired into a request path yet.
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
	"github.com/dkam/silo/store"
)

// ErrNoContentKey reports an operation that needs the library's content key
// where this Store does not hold one. On a server this is the ordinary answer
// for an E2EE library, not a failure.
var ErrNoContentKey = errors.New("objmgr: operation needs the library's content key")

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
// The seed check is the load-bearing one. An E2EE library chunks under a seed
// derived from its content key and a plain library under the published
// constant, and the spec says in as many words that an E2EE library must never
// chunk under the plain seed — doing so would turn the manifest's seal_hash,
// which is computed over plaintext hashes, into a content-confirmation oracle
// that needs no key. Catching a mismatched seed here means it cannot be
// reached by assembling a Config wrong.
func New(cfg Config) (*Store, error) {
	if err := cfg.Params.Validate(); err != nil {
		return nil, err
	}
	if !cfg.E2EE && len(cfg.CK) > 0 {
		return nil, errors.New("objmgr: a plain library has no content key")
	}
	if cfg.StoreID == "" {
		return nil, errors.New("objmgr: no store id")
	}

	switch {
	case len(cfg.CK) > 0:
		if want := store.ChunkerSeed(cfg.CK); cfg.Params.Seed != want {
			return nil, errors.New("objmgr: chunker seed is not this library's content key's")
		}
	case !cfg.E2EE:
		if cfg.Params.Seed != store.PlainSeed() {
			return nil, errors.New("objmgr: a plain library must chunk under the published seed")
		}
	}

	s := &Store{
		storeID: cfg.StoreID,
		e2ee:    cfg.E2EE,
		ck:      cfg.CK,
		params:  cfg.Params,
		chunks:  objstore.New("", cfg.DataDir, objstore.TypeChunks),
		objects: objstore.New("", cfg.DataDir, objstore.TypeObjects),
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
func (s *Store) PutChunk(id store.ID, data []byte) error {
	if err := s.chunks.WriteVerified(s.storeID, id.String(), bytes.NewReader(data), true); err != nil {
		if errors.Is(err, objstore.ErrContentMismatch) {
			return fmt.Errorf("%w: chunk %s", ErrIDMismatch, id)
		}
		return err
	}
	return nil
}

// GetChunk returns a chunk's stored bytes, still sealed in an E2EE library.
func (s *Store) GetChunk(id store.ID) ([]byte, error) {
	var buf bytes.Buffer
	if err := s.chunks.Read(s.storeID, id.String(), &buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// HasChunk reports whether a chunk is present and usable.
func (s *Store) HasChunk(id store.ID) (bool, error) {
	return s.chunks.Exists(s.storeID, id.String())
}

// PutObject stores an already-encoded object under its id, verifying it.
//
// This is the server's ingest path for manifests, directories and commits in
// an E2EE library: it cannot decode them, and it does not need to. What it can
// do — and must — is refuse bytes that are not what their id says, because an
// id that names something else is the one corruption a content-addressed store
// can catch on its own.
func (s *Store) PutObject(id store.ID, encoded []byte) error {
	if err := s.objects.WriteVerified(s.storeID, id.String(), bytes.NewReader(encoded), true); err != nil {
		if errors.Is(err, objstore.ErrContentMismatch) {
			return fmt.Errorf("%w: object %s", ErrIDMismatch, id)
		}
		return err
	}
	return nil
}

// GetObject returns an object's encoded bytes.
func (s *Store) GetObject(id store.ID) ([]byte, error) {
	var buf bytes.Buffer
	if err := s.objects.Read(s.storeID, id.String(), &buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// HasObject reports whether an object is present and usable.
func (s *Store) HasObject(id store.ID) (bool, error) {
	return s.objects.Exists(s.storeID, id.String())
}

// putEncoded stores encoded bytes under their own hash and returns the id.
func (s *Store) putEncoded(encoded []byte) (store.ID, error) {
	id := store.ObjectID(encoded)
	if err := s.PutObject(id, encoded); err != nil {
		return store.ID{}, err
	}
	return id, nil
}

// PutManifest encodes a manifest for this library's type and stores it.
func (s *Store) PutManifest(m *store.Manifest) (store.ID, error) {
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
		return &store.Manifest{FileSize: int64(n), Inline: head}, nil
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
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return m, nil
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
		id := store.ChunkID(data)
		if err := s.PutChunk(id, data); err != nil {
			return store.ChunkRef{}, err
		}
		return store.ChunkRef{ID: id, Size: size}, nil
	}

	sealed, err := store.SealChunk(s.ck, data)
	if err != nil {
		return store.ChunkRef{}, err
	}
	if err := s.PutChunk(sealed.ID, sealed.Frame); err != nil {
		return store.ChunkRef{}, err
	}
	return store.ChunkRef{ID: sealed.ID, Size: size, PlaintextHash: sealed.PlaintextHash}, nil
}

// ReadFile writes a manifest's file content to w.
func (s *Store) ReadFile(m *store.Manifest, w io.Writer) error {
	if err := m.Validate(); err != nil {
		return err
	}
	if store.Inlined(m.FileSize) {
		_, err := w.Write(m.Inline)
		return err
	}
	if s.e2ee && !s.HasKey() {
		return ErrNoContentKey
	}

	for i, ref := range m.Chunks {
		stored, err := s.GetChunk(ref.ID)
		if err != nil {
			return fmt.Errorf("chunk %d of %d (%s): %w", i+1, len(m.Chunks), ref.ID, err)
		}
		data := stored
		if s.e2ee {
			data, err = store.OpenChunk(s.ck, ref.PlaintextHash, stored)
			if err != nil {
				return fmt.Errorf("chunk %d of %d (%s): %w", i+1, len(m.Chunks), ref.ID, err)
			}
		} else if store.ChunkID(data) != ref.ID {
			// A plain library has no tag to catch a substituted chunk, so the
			// id is the check, and it is worth making rather than assuming:
			// the bytes came off a disk the threat model does not trust.
			return fmt.Errorf("chunk %d of %d: %w: %s", i+1, len(m.Chunks), ErrIDMismatch, ref.ID)
		}
		if int64(len(data)) != ref.Size {
			return fmt.Errorf("chunk %d of %d (%s): %d bytes, manifest says %d",
				i+1, len(m.Chunks), ref.ID, len(data), ref.Size)
		}
		if _, err := w.Write(data); err != nil {
			return err
		}
	}
	return nil
}
