package client

import (
	"crypto/sha1"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

func sha1Of(s string) string {
	sum := sha1.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "file.bin")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatalf("failed to write the test file: %v", err)
	}
	return p
}

// TestChunkFileNamesBlocksTheWayTheServerWould pins the premise the whole
// block surface rests on: cut at fixed offsets, hash each piece, and the names
// are the ones the store already uses. Nothing negotiates this — if it drifts,
// uploads still succeed and simply stop deduping, which is the kind of failure
// that goes unnoticed for a year.
func TestChunkFileNamesBlocksTheWayTheServerWould(t *testing.T) {
	// Two full blocks and a short one, which is every case a file has.
	path := writeTemp(t, "AAAABBBBCC")

	ids, offsets, err := chunkFile(path, 4)
	if err != nil {
		t.Fatalf("chunkFile failed: %v", err)
	}

	want := []string{sha1Of("AAAA"), sha1Of("BBBB"), sha1Of("CC")}
	if len(ids) != len(want) {
		t.Fatalf("got %d blocks, want %d", len(ids), len(want))
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Errorf("block %d = %s, want %s", i, ids[i], want[i])
		}
	}
	// The short final block is hashed over what is there, not over a padded
	// buffer — a buffer reused between reads makes that an easy mistake, and
	// its symptom is a trailing block nothing else ever matches.
	for i, off := range []int64{0, 4, 8} {
		if offsets[want[i]] != off {
			t.Errorf("block %d starts at %d, want %d", i, offsets[want[i]], off)
		}
	}
}

// TestChunkFileUploadsARepeatedBlockOnce covers the case dedup exists for. A
// file of identical blocks names the same id repeatedly, and only one copy is
// worth sending.
func TestChunkFileUploadsARepeatedBlockOnce(t *testing.T) {
	path := writeTemp(t, "XXXXXXXXXXXX")

	ids, offsets, err := chunkFile(path, 4)
	if err != nil {
		t.Fatalf("chunkFile failed: %v", err)
	}

	// Three times in the file, because the file really is three blocks long.
	if len(ids) != 3 || ids[0] != ids[1] || ids[1] != ids[2] {
		t.Fatalf("ids = %v, want the same id three times", ids)
	}
	// Once to upload: the offsets map is what the upload loop reads, and a
	// second entry would mean sending the same bytes again.
	if len(offsets) != 1 {
		t.Fatalf("offsets = %v, want one entry", offsets)
	}
}

func TestChunkFileOfAnEmptyFileNamesNothing(t *testing.T) {
	ids, offsets, err := chunkFile(writeTemp(t, ""), 4)
	if err != nil {
		t.Fatalf("chunkFile failed: %v", err)
	}
	// An empty file is not a block of zero bytes: it has no content to
	// address, and the commit call turns an empty list into the empty-file id
	// server-side.
	if len(ids) != 0 || len(offsets) != 0 {
		t.Fatalf("an empty file produced %v / %v", ids, offsets)
	}
}

// TestBlockSizeOfFallsBackWhenUnreported keeps an older server — one with no
// block_size field — from being read as "chunk at zero", which would loop
// forever rather than fail.
func TestBlockSizeOfFallsBackWhenUnreported(t *testing.T) {
	if got := blockSizeOf(ServerInfo{}); got != DefaultBlockSize {
		t.Errorf("blockSizeOf(zero) = %d, want %d", got, DefaultBlockSize)
	}
	if got := blockSizeOf(ServerInfo{BlockSize: 1024}); got != 1024 {
		t.Errorf("blockSizeOf(1024) = %d, want 1024", got)
	}
}
