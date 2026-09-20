package objmgr

import (
	"bytes"
	"testing"

	"github.com/SiloDrive/silo/store"
)

// A ranged read must return exactly what the same range of the whole file
// returns, at every boundary that matters — inside one chunk, across two, on a
// chunk edge, and off the end. The arithmetic is over PLAINTEXT chunk sizes,
// so an E2EE library, whose stored chunks are sixteen bytes longer apiece,
// has to produce the identical answer. That is the property the format pinned
// plaintext sizes for, so it is the one worth testing in both library types.
func TestARangedReadMatchesTheWholeFile(t *testing.T) {
	for _, lib := range []struct {
		name string
		open func(*testing.T) *Store
	}{
		{"plain", plainStore},
		{"e2ee", sealedStore},
	} {
		t.Run(lib.name, func(t *testing.T) {
			s := lib.open(t)

			// Big enough to chunk rather than inline, so the walk is exercised.
			want := testData("ranged", 6<<20)
			m, err := s.WriteFile(bytes.NewReader(want))
			if err != nil {
				t.Fatalf("WriteFile: %v", err)
			}
			if len(m.Chunks) < 3 {
				t.Fatalf("test needs a multi-chunk file, got %d chunks", len(m.Chunks))
			}

			size := m.FileSize
			mid := m.Chunks[0].Size
			cases := []struct {
				name   string
				off, n int64
			}{
				{"whole file", 0, size},
				{"open-ended", 0, -1},
				{"first byte", 0, 1},
				{"last byte", size - 1, 1},
				{"inside one chunk", 100, 200},
				{"across a boundary", mid - 50, 100},
				{"exactly on a boundary", mid, 10},
				{"ending on a boundary", mid - 10, 10},
				{"spanning three chunks", mid - 1, m.Chunks[1].Size + 2},
				{"past the end", size - 10, 999},
				{"wholly past the end", size, 10},
			}
			for _, c := range cases {
				t.Run(c.name, func(t *testing.T) {
					var got bytes.Buffer
					if err := s.ReadFileRange(m, c.off, c.n, &got); err != nil {
						t.Fatalf("ReadFileRange(%d, %d): %v", c.off, c.n, err)
					}
					end := size
					if c.n >= 0 && c.off+c.n < end {
						end = c.off + c.n
					}
					var expect []byte
					if c.off < size {
						expect = want[c.off:end]
					}
					if !bytes.Equal(got.Bytes(), expect) {
						t.Errorf("range(%d, %d): got %d bytes, want %d",
							c.off, c.n, got.Len(), len(expect))
					}
				})
			}
		})
	}
}

// An inlined file has no chunks to walk, and the same ranges must still hold.
func TestARangedReadOfAnInlinedFile(t *testing.T) {
	s := plainStore(t)
	want := testData("small", 1000)
	m, err := s.WriteFile(bytes.NewReader(want))
	if err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if len(m.Chunks) != 0 {
		t.Fatalf("a 1000-byte file should inline, got %d chunks", len(m.Chunks))
	}
	var got bytes.Buffer
	if err := s.ReadFileRange(m, 100, 50, &got); err != nil {
		t.Fatalf("ReadFileRange: %v", err)
	}
	if !bytes.Equal(got.Bytes(), want[100:150]) {
		t.Errorf("inline range mismatch")
	}
}

// The server's view of an E2EE library cannot serve a ranged read of content:
// the chunks are sealed and opening them needs the key. It must say so rather
// than return ciphertext, which would be bytes the client cannot distinguish
// from the file it asked for.
func TestTheServerViewCannotRangeReadAnEncryptedFile(t *testing.T) {
	dir := storeDir(t)
	client, err := New(Config{
		DataDir: dir,
		StoreID: testStoreID,
		E2EE:    true,
		CK:      testCK,
		Params:  store.DefaultParams(store.ChunkerSeed(testCK)),
	})
	if err != nil {
		t.Fatal(err)
	}
	m, err := client.WriteFile(bytes.NewReader(testData("sealed", 6<<20)))
	if err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	server := serverView(t, dir)
	var got bytes.Buffer
	if err := server.ReadFileRange(m, 0, 10, &got); err == nil {
		t.Fatal("the server view served a range of a sealed file")
	}
}
