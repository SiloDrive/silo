package objmgr

import (
	"bytes"
	"errors"
	"testing"

	"github.com/SiloDrive/silo/store"
)

// putChunks stores raw chunks and returns their ids in order.
func putChunks(t *testing.T, s *Store, parts ...[]byte) []store.ID {
	t.Helper()
	ids := make([]store.ID, 0, len(parts))
	for _, p := range parts {
		id := store.ChunkID(p)
		if err := s.PutChunk(id, p); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	return ids
}

// A file assembled from chunks reads back as the chunks, in the order named.
// This is the resumable upload's whole promise.
func TestAManifestBuiltFromChunksReadsBackAsTheFile(t *testing.T) {
	s := plainStore(t)
	parts := [][]byte{testData("one", 300_000), testData("two", 200_000)}
	ids := putChunks(t, s, parts...)

	m, missing, err := s.ManifestFromChunks(ids)
	if err != nil {
		t.Fatal(err)
	}
	if len(missing) != 0 {
		t.Fatalf("missing = %v, want none", missing)
	}
	want := append(append([]byte{}, parts[0]...), parts[1]...)
	if m.FileSize != int64(len(want)) {
		t.Errorf("FileSize = %d, want %d", m.FileSize, len(want))
	}

	var got bytes.Buffer
	if err := s.ReadFile(m, &got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.Bytes(), want) {
		t.Error("the assembled file is not the chunks that went in")
	}
}

// Whether a file inlines is decided by its size, never by which writer built
// it. A small file assembled from chunks has to come out the same shape
// WriteFile would have given it — otherwise two writers mint two ids for
// identical content and every reader downstream sees a modification that did
// not happen.
func TestASmallFileFromChunksInlinesLikeOneWritten(t *testing.T) {
	s := plainStore(t)
	small := testData("small", 400)
	ids := putChunks(t, s, small)

	m, _, err := s.ManifestFromChunks(ids)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Chunks) != 0 {
		t.Errorf("a %d-byte file kept %d chunks, want an inline manifest", len(small), len(m.Chunks))
	}
	if !bytes.Equal(m.Inline, small) {
		t.Error("the inline bytes are not the chunk's bytes")
	}

	// And the id is the one WriteFile would have produced for the same bytes.
	written, err := s.WriteFile(bytes.NewReader(small))
	if err != nil {
		t.Fatal(err)
	}
	fromChunks, err := s.PutManifest(m)
	if err != nil {
		t.Fatal(err)
	}
	direct, err := s.PutManifest(written)
	if err != nil {
		t.Fatal(err)
	}
	if fromChunks != direct {
		t.Errorf("two writers minted two ids for the same %d bytes: %s and %s", len(small), fromChunks, direct)
	}
}

// A repeated chunk is one object and two entries, and the size counts it
// twice, because the repeat is a real part of the file.
func TestARepeatedChunkIsCountedEveryTimeItAppears(t *testing.T) {
	s := plainStore(t)
	part := testData("repeat", 300_000)
	ids := putChunks(t, s, part)
	twice := []store.ID{ids[0], ids[0]}

	m, _, err := s.ManifestFromChunks(twice)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Chunks) != 2 {
		t.Fatalf("chunks = %d, want 2", len(m.Chunks))
	}
	if m.FileSize != int64(2*len(part)) {
		t.Errorf("FileSize = %d, want %d", m.FileSize, 2*len(part))
	}
}

// A chunk the store does not hold is reported, not an error: the request was
// well formed and the content is simply not here yet. It is named once however
// often it appears, so a client is never told to upload the same bytes twice.
func TestAnAbsentChunkIsReportedOnceAndIsNotAnError(t *testing.T) {
	s := plainStore(t)
	here := putChunks(t, s, testData("here", 300_000))
	absent := store.ChunkID([]byte("never stored"))

	m, missing, err := s.ManifestFromChunks([]store.ID{here[0], absent, absent})
	if err != nil {
		t.Fatalf("a missing chunk is not an error: %v", err)
	}
	if m != nil {
		t.Error("a manifest was built for a file with a hole in it")
	}
	if len(missing) != 1 || missing[0] != absent {
		t.Errorf("missing = %v, want [%s] exactly once", missing, absent)
	}
}

// The server cannot assemble a manifest for an E2EE library, and the refusal
// is a stated impossibility rather than a policy: opening a sealed chunk needs
// a key derived from its plaintext hash, and that hash lives only in the
// manifest this would be building.
func TestAnE2EELibraryRefusesToAssembleAManifest(t *testing.T) {
	s := sealedStore(t)
	if _, _, err := s.ManifestFromChunks([]store.ID{store.ChunkID([]byte("x"))}); !errors.Is(err, ErrSealedChunks) {
		t.Errorf("err = %v, want ErrSealedChunks", err)
	}
}

// ChunkSize is the plaintext length and ChunkStoredSize is what is on disk.
// They differ by the tag under E2EE, and the difference is invisible in a
// plain library — which is exactly why it is asserted in a sealed one.
func TestChunkSizeIsPlaintextAndStoredSizeIsNot(t *testing.T) {
	plain := plainStore(t)
	data := testData("sized", 5000)
	id := putChunks(t, plain, data)[0]

	got, err := plain.ChunkSize(id)
	if err != nil {
		t.Fatal(err)
	}
	if got != int64(len(data)) {
		t.Errorf("plain ChunkSize = %d, want %d", got, len(data))
	}

	sealed := sealedStore(t)
	m, err := sealed.WriteFile(bytes.NewReader(testData("sealed", 3<<20)))
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Chunks) == 0 {
		t.Fatal("a 3 MiB file produced no chunks")
	}
	ref := m.Chunks[0]
	plaintext, err := sealed.ChunkSize(ref.ID)
	if err != nil {
		t.Fatal(err)
	}
	if plaintext != ref.Size {
		t.Errorf("ChunkSize = %d, want the manifest's %d", plaintext, ref.Size)
	}
	stored, err := sealed.ChunkStoredSize(ref.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored != ref.Size+store.TagSize {
		t.Errorf("ChunkStoredSize = %d, want %d", stored, ref.Size+store.TagSize)
	}
}

// A manifest that breaks the format's rules must not get an id. PutManifest is
// the seam every writer crosses, so it is where the check belongs — a bad
// manifest that is stored becomes a real object, and the failure surfaces on a
// client at read time, a long way from whoever wrote it.
func TestPutManifestRefusesAManifestTheFormatWouldReject(t *testing.T) {
	s := plainStore(t)
	// Inline bytes that do not match the declared size.
	bad := &store.Manifest{FileSize: 99, Inline: []byte("four")}
	if _, err := s.PutManifest(bad); err == nil {
		t.Error("PutManifest minted an id for a manifest Validate rejects")
	}
}
