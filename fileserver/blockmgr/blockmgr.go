// Package blockmgr provides operations on blocks
package blockmgr

import (
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
