package client

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/dkam/silo/store"
)

// testParams are the default chunker with every size divided down, so a test
// file can be a few kilobytes instead of a few megabytes and still be cut into
// enough pieces to have a shape. The algorithm, the seed and the normalisation
// are the real ones — those are what the ids depend on.
func testParams() store.Params {
	p := store.DefaultParams(store.PlainSeed())
	p.MinSize, p.TargetSize, p.MaxSize = 64, 256, 1024
	return p
}

func writeTemp(t *testing.T, content []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "file.bin")
	if err := os.WriteFile(p, content, 0o600); err != nil {
		t.Fatalf("failed to write the test file: %v", err)
	}
	return p
}

// compressible bytes with enough variety to give the chunker boundaries to
// find. Seeded, so a failure is reproducible.
func testBytes(n int) []byte {
	b := make([]byte, n)
	r := rand.New(rand.NewSource(1))
	for i := range b {
		b[i] = byte(r.Intn(251))
	}
	return b
}

// TestEveryChunkIDNamesTheBytesAtItsSource pins the premise the whole chunk
// surface rests on, and it is the one that was wrong: an id is the SHA-256 of
// the chunk's bytes, and the source recorded for it points at exactly those
// bytes in the file.
//
// This is checked by re-reading the file at the recorded offset and length
// rather than by trusting the chunker's own output, because the failure being
// guarded against is precisely a source that does not describe the bytes that
// were hashed. Nothing negotiates any of it — get it wrong and the server
// answers 400, or worse, accepts an id naming the wrong content.
func TestEveryChunkIDNamesTheBytesAtItsSource(t *testing.T) {
	content := testBytes(20000)
	path := writeTemp(t, content)

	ids, sources, err := chunkFile(path, testParams())
	if err != nil {
		t.Fatalf("chunkFile failed: %v", err)
	}
	if len(ids) < 3 {
		t.Fatalf("got %d chunks from %d bytes; the test needs a file with a shape", len(ids), len(content))
	}

	for id, src := range sources {
		if src.off < 0 || src.off+src.size > int64(len(content)) {
			t.Fatalf("chunk %.8s claims bytes [%d,%d) of a %d-byte file", id, src.off, src.off+src.size, len(content))
		}
		sum := sha256.Sum256(content[src.off : src.off+src.size])
		if got := hex.EncodeToString(sum[:]); got != id {
			t.Errorf("chunk at [%d,%d) hashes to %.8s but is named %.8s", src.off, src.off+src.size, got, id)
		}
	}
}

// TestTheChunksReassembleIntoTheFile is the other half: every byte is in some
// chunk, once, in order. A chunker that dropped or repeated a byte would still
// produce ids that name their own bytes and still upload cleanly — the file
// would simply come back wrong.
func TestTheChunksReassembleIntoTheFile(t *testing.T) {
	content := testBytes(20000)
	path := writeTemp(t, content)

	ids, sources, err := chunkFile(path, testParams())
	if err != nil {
		t.Fatalf("chunkFile failed: %v", err)
	}

	var rebuilt []byte
	for _, id := range ids {
		src := sources[id]
		rebuilt = append(rebuilt, content[src.off:src.off+src.size]...)
	}
	if len(rebuilt) != len(content) {
		t.Fatalf("the chunks total %d bytes, the file is %d", len(rebuilt), len(content))
	}
	for i := range rebuilt {
		if rebuilt[i] != content[i] {
			t.Fatalf("the chunks differ from the file at byte %d", i)
		}
	}
}

// TestChunkFileUploadsARepeatedChunkOnce covers the case dedup exists for. A
// file that repeats content names the same id more than once, and only one
// copy is worth sending.
func TestChunkFileUploadsARepeatedChunkOnce(t *testing.T) {
	// A long run of one byte cuts at MaxSize every time, so every chunk is
	// identical — content-defined chunking finds no boundary in it.
	content := make([]byte, 8192)
	path := writeTemp(t, content)

	ids, sources, err := chunkFile(path, testParams())
	if err != nil {
		t.Fatalf("chunkFile failed: %v", err)
	}
	if len(ids) < 2 {
		t.Fatalf("got %d chunks, want several identical ones", len(ids))
	}
	for _, id := range ids {
		if id != ids[0] {
			t.Fatalf("ids = %v, want the same id throughout", ids)
		}
	}
	// Once to upload: the sources map is what the upload loop reads, and a
	// second entry would mean sending the same bytes again.
	if len(sources) != 1 {
		t.Fatalf("sources has %d entries, want one", len(sources))
	}
}

func TestChunkFileOfAnEmptyFileNamesNothing(t *testing.T) {
	ids, sources, err := chunkFile(writeTemp(t, nil), testParams())
	if err != nil {
		t.Fatalf("chunkFile failed: %v", err)
	}
	// An empty file is not a chunk of zero bytes: it has no content to
	// address, and the commit call turns an empty list into the empty-file id
	// server-side.
	if len(ids) != 0 || len(sources) != 0 {
		t.Fatalf("an empty file produced %v / %v", ids, sources)
	}
}

// TestTheChunkLaneIsRefusedRatherThanGuessed is the test that would have
// caught the bug this file was rewritten for.
//
// Chunking under parameters the server did not give is the one failure this
// surface cannot detect on its own: every id is well-formed, every upload
// succeeds, and not one of them ever matches anything already in the store. So
// a library that cannot be chunked correctly must not be chunked at all — the
// caller sends whole files, which is slower and right.
func TestTheChunkLaneIsRefusedRatherThanGuessed(t *testing.T) {
	good := &ChunkerParams{
		Algorithm: store.ChunkerAlgorithm, MinSize: store.DefaultMinSize,
		TargetSize: store.DefaultTargetSize, MaxSize: store.DefaultMaxSize,
		Normalization: store.DefaultNormalization,
	}
	for _, tc := range []struct {
		name string
		repo Repo
		want bool
	}{
		{"a plain library with parameters", Repo{ID: "r", Chunker: good}, true},
		{"a library the server did not describe", Repo{ID: "r"}, false},
		{"an end-to-end encrypted library", Repo{ID: "r", Encrypted: true, Chunker: good}, false},
		{"parameters no chunker can be built from", Repo{ID: "r", Chunker: &ChunkerParams{
			Algorithm: store.ChunkerAlgorithm, MinSize: 1 << 20, TargetSize: 1 << 10,
			MaxSize: 1 << 20, Normalization: store.DefaultNormalization,
		}}, false},
		{"an algorithm this client does not implement", Repo{ID: "r", Chunker: &ChunkerParams{
			Algorithm: "rollsum/v9", MinSize: store.DefaultMinSize,
			TargetSize: store.DefaultTargetSize, MaxSize: store.DefaultMaxSize,
			Normalization: store.DefaultNormalization,
		}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := clientListing(t, []Repo{tc.repo})
			_, ok := c.chunkerFor("r")
			if ok != tc.want {
				t.Errorf("chunkerFor = %v, want %v", ok, tc.want)
			}
		})
	}
}

// testChunker is the parameter set a fake server reports, scaled so that test
// files of a few bytes still take the chunk lane.
func testChunker() *ChunkerParams {
	p := testParams()
	return &ChunkerParams{
		Algorithm: p.Algorithm, MinSize: p.MinSize, TargetSize: p.TargetSize,
		MaxSize: p.MaxSize, Normalization: p.Normalization,
	}
}

// clientListing returns a client talking to a server whose only surface is the
// repos listing, which is all chunkerFor reads.
func clientListing(t *testing.T, repos []Repo) *APIClient {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/silo/v1/repos" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(repos)
	}))
	t.Cleanup(srv.Close)
	return NewClient(srv.URL)
}
