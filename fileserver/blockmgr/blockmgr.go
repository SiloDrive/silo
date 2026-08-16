// Package blockmgr provides operations on blocks
package blockmgr

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"io"

	"github.com/dkam/silo/fileserver/objstore"
	"github.com/dkam/silo/fileserver/option"
)

var store *objstore.ObjectStore

// Init initializes block manager and creates underlying object store.
func Init(seafileConfPath string, seafileDataDir string) {
	store = objstore.New(seafileConfPath, seafileDataDir, "blocks")
}

// Read reads block from storage backend.
func Read(repoID string, blockID string, w io.Writer) error {
	err := store.Read(repoID, blockID, w)
	if err != nil {
		return err
	}

	return nil
}

// Write writes block to storage backend, refusing to publish content that
// does not hash to blockID.
//
// A block id is the SHA-1 of exactly the bytes stored, so the check is
// unconditional and costs one pass of hashing over data already being copied.
// It is not configurable: a wrong-bytes-under-a-right-id write is permanent —
// every later writer of that id skips it as already present, and reads serve
// the wrong content to everyone sharing the store, virtual repos included.
// The sync ingest path (putSendBlockCB) wrote whatever a client sent under
// whatever id it named; the local upload path already hashed, and now cannot
// drift from this.
func Write(repoID string, blockID string, r io.Reader) error {
	err := store.WriteVerified(repoID, blockID, r, option.SyncObjectWrites)
	if err != nil {
		return err
	}

	return nil
}

// WriteBytes stores a block already held in memory and returns the id it was
// stored under, which is derived from the content rather than supplied.
//
// This is for the upload paths, which have the whole block in hand and would
// otherwise hash it once to name it and again to verify it. Naming the block
// from its own digest makes the two agree by construction, so a second pass
// buys nothing — it is a stronger guarantee than Write's check, not a way
// around it.
//
// wantID, when non-empty, is the id the caller was promised: the block is
// rejected before anything is written if the content does not match it.
//
// A block that is already present is left alone. Blocks are immutable, so a
// second copy of the same id is the same bytes.
func WriteBytes(repoID string, data []byte, wantID string) (string, error) {
	checkSum := sha1.Sum(data)
	blockID := hex.EncodeToString(checkSum[:])
	if wantID != "" && blockID != wantID {
		return "", fmt.Errorf("block id %s:%s doesn't match content", blockID, wantID)
	}
	if exists, _ := store.Exists(repoID, blockID); exists {
		return blockID, nil
	}
	if err := store.Write(repoID, blockID, bytes.NewReader(data), option.SyncObjectWrites); err != nil {
		return "", err
	}
	return blockID, nil
}

// Exists checks block if exists.
func Exists(repoID string, blockID string) bool {
	ret, _ := store.Exists(repoID, blockID)
	return ret
}

// Stat calculates block size.
func Stat(repoID string, blockID string) (int64, error) {
	ret, err := store.Stat(repoID, blockID)
	return ret, err
}
