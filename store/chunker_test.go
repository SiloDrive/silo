package store

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"io"
	"testing"
)

// pseudoRandom generates the deterministic byte streams the chunker tests and
// the shipped vectors run against: block i is SHA-256(label ‖ uint64le(i)),
// concatenated and truncated.
//
// The rule is in the spec rather than the data, so porter-mac can regenerate
// gigabytes of test input from one line of prose instead of us committing it.
func pseudoRandom(label string, n int) []byte {
	out := make([]byte, 0, n+sha256.Size)
	var counter [8]byte
	h := sha256.New()
	for i := uint64(0); len(out) < n; i++ {
		binary.LittleEndian.PutUint64(counter[:], i)
		h.Reset()
		h.Write([]byte(label))
		h.Write(counter[:])
		out = h.Sum(out)
	}
	return out[:n]
}

func chunkAll(t *testing.T, p Params, data []byte) []Chunk {
	t.Helper()
	return chunkReader(t, p, bytes.NewReader(data))
}

func chunkReader(t *testing.T, p Params, r io.Reader) []Chunk {
	t.Helper()
	c, err := NewChunker(p, r)
	if err != nil {
		t.Fatalf("NewChunker: %v", err)
	}
	var out []Chunk
	for {
		ch, err := c.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		out = append(out, Chunk{Offset: ch.Offset, Data: bytes.Clone(ch.Data)})
	}
}

func testParams() Params { return DefaultParams(PlainSeed()) }

func TestChunksReassembleToTheInput(t *testing.T) {
	data := pseudoRandom("reassemble", 20<<20)
	var got []byte
	var want int64
	for _, ch := range chunkAll(t, testParams(), data) {
		if ch.Offset != want {
			t.Fatalf("chunk at offset %d, want %d", ch.Offset, want)
		}
		got = append(got, ch.Data...)
		want += int64(len(ch.Data))
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("reassembled %d bytes, want the original %d", len(got), len(data))
	}
}

func TestEveryChunkButTheLastRespectsTheBounds(t *testing.T) {
	p := testParams()
	chunks := chunkAll(t, p, pseudoRandom("bounds", 32<<20))
	if len(chunks) < 8 {
		t.Fatalf("got %d chunks, too few to say anything", len(chunks))
	}
	for i, ch := range chunks[:len(chunks)-1] {
		if len(ch.Data) < p.MinSize || len(ch.Data) > p.MaxSize {
			t.Errorf("chunk %d is %d bytes, outside [%d, %d]", i, len(ch.Data), p.MinSize, p.MaxSize)
		}
	}
	if last := chunks[len(chunks)-1]; len(last.Data) > p.MaxSize {
		t.Errorf("last chunk is %d bytes, above the max %d", len(last.Data), p.MaxSize)
	}
}

// The normalisation level exists to pull sizes in around the target. If the
// mean drifted far from 1 MiB the manifest-size and read-amplification
// arguments that picked the target would both stop holding, and nothing else
// in the suite would notice.
func TestSizesConcentrateAroundTheTarget(t *testing.T) {
	p := testParams()
	chunks := chunkAll(t, p, pseudoRandom("distribution", 128<<20))
	var total int
	for _, ch := range chunks[:len(chunks)-1] {
		total += len(ch.Data)
	}
	mean := total / (len(chunks) - 1)
	if mean < p.TargetSize*3/4 || mean > p.TargetSize*5/4 {
		t.Errorf("mean chunk size %d, want within 25%% of the %d target (%d chunks)",
			mean, p.TargetSize, len(chunks))
	}
}

// The point of content-defined chunking, and the one property a fixed-block
// store cannot have: inserting bytes at the front must not renumber the whole
// file. Everything past the insertion point has to re-align onto the same
// boundaries, so a client re-uploads the bytes that changed and nothing else.
func TestInsertingAtTheFrontRealignsRatherThanRewrites(t *testing.T) {
	p := testParams()
	data := pseudoRandom("realign", 64<<20)
	shifted := append(pseudoRandom("prefix", 5000), data...)

	original := chunkAll(t, p, data)
	after := chunkAll(t, p, shifted)

	ids := map[ID]bool{}
	for _, ch := range original {
		ids[ChunkID(ch.Data)] = true
	}
	var shared int
	for _, ch := range after {
		if ids[ChunkID(ch.Data)] {
			shared++
		}
	}
	if want := len(after) * 9 / 10; shared < want {
		t.Errorf("only %d of %d chunks survived a 5000-byte insertion, want at least %d",
			shared, len(after), want)
	}
}

// Keying the gear table is what reconciles CDC with E2EE: two libraries
// holding the same file must not cut it in the same places, or the server can
// fingerprint known content from the size sequence alone.
func TestADifferentSeedCutsInDifferentPlaces(t *testing.T) {
	data := pseudoRandom("seeded", 16<<20)
	plain := chunkAll(t, testParams(), data)
	sealed := chunkAll(t, DefaultParams(ChunkerSeed([]byte("a library content key"))), data)

	ids := map[ID]bool{}
	for _, ch := range plain {
		ids[ChunkID(ch.Data)] = true
	}
	for _, ch := range sealed {
		if ids[ChunkID(ch.Data)] {
			t.Fatalf("a chunk at offset %d is identical under both seeds", ch.Offset)
		}
	}
}

// Content with no boundaries in it — a sparse region, a run of padding — must
// still terminate, at exactly the maximum. Without the forced cut a zero-filled
// disk image would be one chunk.
func TestUncuttableContentStopsAtTheMaximum(t *testing.T) {
	p := testParams()
	chunks := chunkAll(t, p, make([]byte, 20<<20))
	for i, ch := range chunks[:len(chunks)-1] {
		if len(ch.Data) != p.MaxSize {
			t.Fatalf("chunk %d of a zero-filled stream is %d bytes, want the max %d",
				i, len(ch.Data), p.MaxSize)
		}
	}
}

// A reader handing back one byte at a time is not a hypothetical: it is a
// slow network. The boundaries must not depend on how the stream arrives.
func TestBoundariesDoNotDependOnReadSizes(t *testing.T) {
	p := testParams()
	data := pseudoRandom("dribble", 12<<20)
	whole := chunkAll(t, p, data)
	dribbled := chunkReader(t, p, &oneByteReader{bytes.NewReader(data)})
	if len(whole) != len(dribbled) {
		t.Fatalf("%d chunks read whole, %d read a byte at a time", len(whole), len(dribbled))
	}
	for i := range whole {
		if !bytes.Equal(whole[i].Data, dribbled[i].Data) {
			t.Fatalf("chunk %d differs between whole and dribbled reads", i)
		}
	}
}

type oneByteReader struct{ r io.Reader }

func (o *oneByteReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	return o.r.Read(p[:1])
}

func TestAnEmptyStreamYieldsNoChunks(t *testing.T) {
	if chunks := chunkAll(t, testParams(), nil); len(chunks) != 0 {
		t.Fatalf("got %d chunks from an empty stream, want none", len(chunks))
	}
}

func TestAShortStreamIsOneChunk(t *testing.T) {
	data := pseudoRandom("short", 100)
	chunks := chunkAll(t, testParams(), data)
	if len(chunks) != 1 || !bytes.Equal(chunks[0].Data, data) {
		t.Fatalf("got %d chunks, want one holding all %d bytes", len(chunks), len(data))
	}
}

func TestBadParametersAreRefused(t *testing.T) {
	base := testParams()
	cases := map[string]func(*Params){
		"unknown algorithm":      func(p *Params) { p.Algorithm = "fastcdc-gear64/v2" },
		"min above target":       func(p *Params) { p.MinSize = p.TargetSize + 1 },
		"target above max":       func(p *Params) { p.MaxSize = p.TargetSize - 1 },
		"min too small":          func(p *Params) { p.MinSize = 8 },
		"max beyond the ceiling": func(p *Params) { p.MaxSize = 2 << 30 },
		"normalisation too high": func(p *Params) { p.Normalization = 9 },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			p := base
			mutate(&p)
			if _, err := NewChunker(p, bytes.NewReader(nil)); !errors.Is(err, ErrParams) {
				t.Fatalf("got %v, want ErrParams", err)
			}
		})
	}
}

// A read failure part-way through a file must surface, not truncate. A
// chunker that answered io.EOF to a disk error would commit a manifest for
// half a file and call the sync successful.
func TestAReadErrorIsNotAnEndOfStream(t *testing.T) {
	want := errors.New("disk went away")
	c, err := NewChunker(testParams(), io.MultiReader(
		bytes.NewReader(pseudoRandom("failing", 3<<20)),
		errReader{want},
	))
	if err != nil {
		t.Fatalf("NewChunker: %v", err)
	}
	if _, err := c.Next(); !errors.Is(err, want) {
		t.Fatalf("got %v, want the read error", err)
	}
}

type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }
