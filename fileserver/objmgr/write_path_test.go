package objmgr

import (
	"bytes"
	"testing"

	"github.com/SiloDrive/silo/fileserver/option"

	"github.com/SiloDrive/silo/fileserver/objstore"
	"github.com/SiloDrive/silo/store"
)

func benchStore(b *testing.B, e2ee bool) *Store {
	b.Helper()
	dir := b.TempDir()
	b.Cleanup(func() { _ = objstore.Close() })
	cfg := Config{DataDir: dir, StoreID: testStoreID, Params: store.DefaultParams(store.PlainSeed())}
	if e2ee {
		cfg.E2EE, cfg.CK = true, testCK
		cfg.Params = store.DefaultParams(store.ChunkerSeed(testCK))
	}
	s, err := New(cfg)
	if err != nil {
		b.Fatal(err)
	}
	return s
}

// What the second SHA-256 cost, measured on this machine (Apple M1 Max,
// 1 MiB chunk, medians of five):
//
//	                       WriteVerified   Write    saving
//	fsync on (default)        15.031 ms   14.683 ms   2.3%
//	fsync off                  1.821 ms    1.433 ms    21%
//
// Both numbers matter and neither alone is the answer. The hash costs about
// 0.35 ms per MiB either way — that part is constant — but option.
// SyncObjectWrites is true by default, and an fsync per object dwarfs it. A
// measurement taken with sync off is measuring the write path with its
// dominant cost removed, which is how silo#24 came to claim 38%.
func benchPutChunk(b *testing.B, e2ee bool, sync bool) {
	was := option.SyncObjectWrites
	option.SyncObjectWrites = sync
	b.Cleanup(func() { option.SyncObjectWrites = was })
	s := benchStore(b, e2ee)
	data := bytes.Repeat([]byte("silo benchmark payload, 32 bytes"), (1<<20)/32)
	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := s.putChunkData(data); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkPutChunkDataPlain1MiB(b *testing.B)       { benchPutChunk(b, false, true) }
func BenchmarkPutChunkDataPlain1MiBNoSync(b *testing.B) { benchPutChunk(b, false, false) }

// An E2EE chunk is addressed by its sealed bytes, so hashing the frame on the
// way to disk lands on the id SealChunk already computed.
//
// putChunkData used to pass sealed.ID to a verifying write, which checked that
// equality on every chunk at the cost of a second SHA-256 over the frame. It
// now writes the frame under whatever it hashes to, which is the same id for
// the same reason — asserted once here rather than recomputed forever.
func TestTheSealedChunkIsStoredUnderTheIdSealChunkGave(t *testing.T) {
	s := sealedStore(t)

	for _, size := range []int{1, 1000, 1 << 20} {
		data := bytes.Repeat([]byte("y"), size)
		sealed, err := store.SealChunk(testCK, data)
		if err != nil {
			t.Fatal(err)
		}
		ref, err := s.putChunkData(data)
		if err != nil {
			t.Fatal(err)
		}
		if ref.ID != sealed.ID {
			t.Errorf("%d-byte chunk stored under %s, SealChunk said %s", size, ref.ID, sealed.ID)
		}
		if ref.ID != store.ChunkID(sealed.Frame) {
			t.Errorf("%d-byte chunk id is not the hash of its frame", size)
		}
		if ref.PlaintextHash != sealed.PlaintextHash {
			t.Errorf("%d-byte chunk kept the wrong plaintext hash", size)
		}
	}
}
