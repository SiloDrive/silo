package silod

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"path/filepath"
	"testing"

	"github.com/dkam/silo/fileserver/blockmgr"
	"github.com/dkam/silo/fileserver/fsmgr"
	"github.com/dkam/silo/fileserver/objstore"
	"github.com/dkam/silo/fileserver/option"
)

const blocksTestStore = "7c1f9b04-3d5e-4a62-8f10-2e6b9a7c4d38"

// chunkLikeAClient does what a client is expected to do before it uploads:
// cut at fixed offsets and name each piece by the SHA-1 of its bytes. It uses
// none of the server's chunking code on purpose — if these ids stop agreeing
// with the ones the store writes, the premise of the whole block surface is
// gone, and a test that borrowed indexBlocks could not tell.
func chunkLikeAClient(data []byte, blockSize int) ([]string, [][]byte) {
	var ids []string
	var blocks [][]byte
	for off := 0; off < len(data); off += blockSize {
		end := min(off+blockSize, len(data))
		blk := data[off:end]
		sum := sha1.Sum(blk)
		ids = append(ids, hex.EncodeToString(sum[:]))
		blocks = append(blocks, blk)
	}
	return ids, blocks
}

func initBlockStore(t *testing.T) {
	t.Helper()
	confPath := t.TempDir()
	blockmgr.Init(confPath, filepath.Join(confPath, "seafile-data"))
	fsmgr.Init(confPath, filepath.Join(confPath, "seafile-data"), option.FsCacheLimit)
}

func TestBlockWriteRefusesContentThatDoesNotMatchItsID(t *testing.T) {
	initBlockStore(t)

	// The id of "right", offered for "wrong" — which is what a corrupted
	// transfer or a lying client looks like from here.
	sum := sha1.Sum([]byte("right"))
	id := hex.EncodeToString(sum[:])

	err := blockmgr.Write(blocksTestStore, id, bytes.NewReader([]byte("wrong")))
	if !errors.Is(err, objstore.ErrContentMismatch) {
		t.Fatalf("a mismatched block was accepted, or failed for the wrong reason: %v", err)
	}

	// Nothing may be left behind under that name. A block published with the
	// wrong bytes is permanent damage: every later writer of the id skips it
	// as already present, and every reader sharing the store gets the wrong
	// content — so "rejected" has to mean "absent", not "rejected and stored".
	if blockmgr.Exists(blocksTestStore, id) {
		t.Fatal("a rejected block was published anyway")
	}

	// And the id is still claimable by the bytes that really do hash to it.
	if err := blockmgr.Write(blocksTestStore, id, bytes.NewReader([]byte("right"))); err != nil {
		t.Fatalf("the correct content was refused after a bad attempt: %v", err)
	}
}

func TestBlockInventoryReportsEachMissingOnceAndSizesRepeats(t *testing.T) {
	initBlockStore(t)

	held, _ := blockmgr.WriteBytes(blocksTestStore, []byte("0123456789"), "")
	absentSum := sha1.Sum([]byte("never uploaded"))
	absent := hex.EncodeToString(absentSum[:])

	// A file whose content repeats: two copies of a stored block and two of
	// one that was never sent.
	missing, size, err := blockInventory(blocksTestStore, []string{held, absent, held, absent})
	if err != nil {
		t.Fatalf("inventory failed: %v", err)
	}

	// Once, not twice: reporting a repeat twice would have the client upload
	// the same bytes twice.
	if len(missing) != 1 || missing[0] != absent {
		t.Fatalf("missing = %v, want exactly [%s]", missing, absent)
	}

	// The size counts every occurrence even though the store holds one copy.
	// The file really is 20 bytes long; dedup is a storage property, not a
	// length one.
	if size != 20 {
		t.Fatalf("size = %d, want 20", size)
	}
}

func TestBlockInventoryOfAnEmptyListIsNotNil(t *testing.T) {
	initBlockStore(t)

	// Encodes as [] rather than null, which Go hides and every other language
	// does not. Same reason GET /repos returns an empty array.
	missing, size, err := blockInventory(blocksTestStore, nil)
	if err != nil || missing == nil || len(missing) != 0 || size != 0 {
		t.Fatalf("blockInventory(nil) = %v, %d, %v", missing, size, err)
	}
}

func TestAFileBuiltFromBlocksReadsBackWhole(t *testing.T) {
	initBlockStore(t)

	// Two full blocks and a short one, so the last-block case is covered.
	const blockSize = 64
	data := bytes.Repeat([]byte("silo"), 40) // 160 bytes
	ids, blocks := chunkLikeAClient(data, blockSize)
	if len(ids) != 3 {
		t.Fatalf("expected 3 blocks, got %d", len(ids))
	}

	// The upload: every block goes up under the id the client computed, and
	// the store verifies each one against its own hash of the bytes. That the
	// writes succeed *is* the assertion that client and server agree on names.
	for i, id := range ids {
		if err := blockmgr.Write(blocksTestStore, id, bytes.NewReader(blocks[i])); err != nil {
			t.Fatalf("block %d was rejected under the id the client computed: %v", i, err)
		}
	}

	missing, size, err := blockInventory(blocksTestStore, ids)
	if err != nil || len(missing) != 0 {
		t.Fatalf("blocks the client just uploaded came back missing: %v, %v", missing, err)
	}
	if size != int64(len(data)) {
		t.Fatalf("size = %d, want %d", size, len(data))
	}

	// The commit: a Seafile object naming those blocks, which is all
	// putEntryBlocks does once the inventory is clean.
	fileID, err := writeSeafile(blocksTestStore, 1, size, ids)
	if err != nil {
		t.Fatalf("failed to write the file object: %v", err)
	}

	// The read back walks the same path a GET does.
	file, err := fsmgr.GetSeafile(blocksTestStore, fileID)
	if err != nil {
		t.Fatalf("failed to read the file object back: %v", err)
	}
	if file.FileSize != uint64(len(data)) {
		t.Fatalf("stored size = %d, want %d", file.FileSize, len(data))
	}
	var got bytes.Buffer
	for _, id := range file.BlkIDs {
		if err := blockmgr.Read(blocksTestStore, id, &got); err != nil {
			t.Fatalf("failed to read block %s: %v", id, err)
		}
	}
	if !bytes.Equal(got.Bytes(), data) {
		t.Fatal("the file assembled from its blocks is not the file that was uploaded")
	}

	// And the id is the content, so an identical upload lands on the same
	// file object — which is what lets a client compare an ETag it holds
	// against one it can compute.
	again, err := writeSeafile(blocksTestStore, 1, size, ids)
	if err != nil || again != fileID {
		t.Fatalf("the same blocks produced a different file id: %s vs %s (%v)", again, fileID, err)
	}
}
