package objstore

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"testing"
)

// What the verification in WriteVerified actually costs, on the 1 MiB chunk
// the write path is sized around.
//
// The question is only worth asking for internal callers, which compute the id
// from the same buffer they are about to hand over -- so the hash inside
// WriteVerified re-derives a number the caller already has. For the exported
// ingest of client-supplied bytes it is not a cost, it is the point.
func benchWrite(b *testing.B, verify bool) {
	b.Helper()
	data := bytes.Repeat([]byte("silo benchmark payload, 32 bytes"), (1<<20)/32)
	sum := sha256.Sum256(data)
	id := hex.EncodeToString(sum[:])

	dataDir := filepath.Join(b.TempDir(), "storage-data")
	s := New(dataDir, "chunks")
	b.Cleanup(func() { _ = Close() })

	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var err error
		if verify {
			err = s.WriteVerified(libraryID, id, bytes.NewReader(data), false)
		} else {
			err = s.Write(libraryID, id, bytes.NewReader(data), false)
		}
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkWrite1MiB(b *testing.B)         { benchWrite(b, false) }
func BenchmarkWriteVerified1MiB(b *testing.B) { benchWrite(b, true) }
