// Package objstore provides operations for commit, fs and block objects.
// It is low-level package used by commitmgr, fsmgr, blockmgr packages to access storage.
package objstore

import (
	"io"
)

// ObjectStore is a container to access storage backend
type ObjectStore struct {
	// can be "commit", "fs", or "block"
	ObjType string
	backend storageBackend
}

// storageBackend is the interface implemented by storage backends.
// An object store may have one or multiple storage backends.
type storageBackend interface {
	// Read an object from backend and write the contents into w.
	read(repoID string, objID string, w io.Writer) (err error)
	// Write the contents from r to the object. When verify is set, the
	// object is published only if its content hashes to objID.
	write(repoID string, objID string, r io.Reader, sync, verify bool) (err error)
	// exists checks whether an object exists.
	exists(repoID string, objID string) (res bool, err error)
	// stat calculates an object's size
	stat(repoID string, objID string) (res int64, err error)
}

// New returns a new object store for a given type of objects.
// objType can be "commit", "fs", or "block".
func New(seafileConfPath string, seafileDataDir string, objType string) *ObjectStore {
	obj := new(ObjectStore)
	obj.ObjType = objType
	obj.backend, _ = newFSBackend(seafileDataDir, objType)
	return obj
}

// Read data from storage backends.
func (s *ObjectStore) Read(repoID string, objID string, w io.Writer) (err error) {
	return s.backend.read(repoID, objID, w)
}

// Write data to storage backends.
func (s *ObjectStore) Write(repoID string, objID string, r io.Reader, sync bool) (err error) {
	return s.backend.write(repoID, objID, r, sync, false)
}

// WriteVerified writes an object and publishes it only if its content hashes
// to objID.
//
// For blocks — the only object type whose id is the SHA-1 of exactly the bytes
// stored — this is the invariant of the store itself, so it is enforced here
// rather than at each caller. Commit and fs ids are computed over other
// representations and cannot use this.
//
// The check runs before the rename, not after the write, which matters: the
// object may already exist with the correct content, and a verify-then-delete
// would let one bad upload destroy a good block.
func (s *ObjectStore) WriteVerified(repoID string, objID string, r io.Reader, sync bool) (err error) {
	return s.backend.write(repoID, objID, r, sync, true)
}

// Check whether object exists.
func (s *ObjectStore) Exists(repoID string, objID string) (res bool, err error) {
	return s.backend.exists(repoID, objID)
}

// Stat calculates object size.
func (s *ObjectStore) Stat(repoID string, objID string) (res int64, err error) {
	return s.backend.stat(repoID, objID)
}
