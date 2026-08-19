// Implementation of file system storage backend.
package objstore

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"os"
	"path"

	"github.com/dkam/silo/fileserver/utils"
)

type fsBackend struct {
	// Path of the object directory
	objDir  string
	objType string
	tmpDir  string
}

func newFSBackend(seafileDataDir string, objType string) (*fsBackend, error) {
	objDir := TypeDir(seafileDataDir, objType)
	err := os.MkdirAll(objDir, os.ModePerm)
	if err != nil {
		return nil, err
	}
	tmpDir := path.Join(seafileDataDir, "tmpfiles")
	err = os.MkdirAll(tmpDir, os.ModePerm)
	if err != nil {
		return nil, err
	}
	backend := new(fsBackend)
	backend.objDir = objDir
	backend.objType = objType
	backend.tmpDir = tmpDir
	return backend, nil
}

// objPath builds the on-disk path for an object. Object IDs are fanned out
// as objID[:2]/objID[2:], which panics on an ID shorter than two characters,
// so the store validates rather than trusting its callers.
func (b *fsBackend) objPath(repoID string, objID string) (string, error) {
	if !utils.IsObjectIDValid(objID) {
		return "", fmt.Errorf("invalid object id %q", objID)
	}
	return path.Join(b.objDir, repoID, objID[:2], objID[2:]), nil
}

func (b *fsBackend) read(repoID string, objID string, w io.Writer) error {
	p, err := b.objPath(repoID, objID)
	if err != nil {
		return err
	}
	fd, err := os.Open(p)
	if err != nil {
		return err
	}
	defer func() { _ = fd.Close() }()

	_, err = io.Copy(w, fd)
	if err != nil {
		return err
	}

	return nil
}

// write stores an object. When sync is set the object is durable by the time
// write returns: the data is fsynced before the rename that publishes it, and
// the directory entry is fsynced after.
//
// That ordering is not optional for correctness. The branch head lives in
// SQLite, which fsyncs its WAL, so a head-commit UPDATE can survive a power cut
// that the objects it references do not — leaving a head pointing at a
// zero-length or absent block. Nothing repairs that afterwards: the client
// believes it has already uploaded those blocks, so a resync does not send them
// again.
func (b *fsBackend) write(repoID string, objID string, r io.Reader, sync, verify bool) error {
	p, err := b.objPath(repoID, objID)
	if err != nil {
		return err
	}
	parentDir := path.Dir(p)
	if err := b.mkObjDirs(parentDir, sync); err != nil {
		return err
	}

	tmpDir := b.tmpDir
	if b.objType != "blocks" {
		tmpDir = parentDir
	}
	tFile, err := os.CreateTemp(tmpDir, objID+".*")
	if err != nil {
		return err
	}
	success := false
	defer func() {
		if !success {
			_ = os.Remove(tFile.Name())
		}
	}()

	// Hashed on the way past rather than in a second pass, so verification
	// costs no extra read of an object that can be several megabytes.
	var hash hash.Hash
	if verify {
		hash = sha1.New()
		r = io.TeeReader(r, hash)
	}

	_, err = io.Copy(tFile, r)
	if err != nil {
		_ = tFile.Close()
		return err
	}

	if verify {
		if got := hex.EncodeToString(hash.Sum(nil)); got != objID {
			_ = tFile.Close()
			return fmt.Errorf("object %s/%s hashes to %s: %w", repoID, objID, got, ErrContentMismatch)
		}
	}

	if sync {
		if err := tFile.Sync(); err != nil {
			_ = tFile.Close()
			return fmt.Errorf("failed to sync object %s/%s: %v", repoID, objID, err)
		}
	}

	err = tFile.Close()
	if err != nil {
		return err
	}

	err = os.Rename(tFile.Name(), p)
	if err != nil {
		return err
	}

	if sync {
		// Until the directory itself is synced the rename can be lost, which
		// would leave the object under its temp name — invisible to reads and
		// invisible to the GC, which only walks well-formed object paths.
		if err := syncDir(parentDir); err != nil {
			return err
		}
	}

	success = true
	return nil
}

// mkObjDirs creates the repo and fan-out directories an object is written
// into. When sync is set, any directory it has to create is made durable
// before the object lands in it — fsyncing a file does not make the chain of
// directory entries leading to it durable, so a new fan-out directory can
// otherwise take a freshly synced object down with it.
//
// The object type directory is created once at startup, so at most two levels
// can be missing here and the syncs cost nothing in the common case where the
// repo has been written to before.
func (b *fsBackend) mkObjDirs(parentDir string, sync bool) error {
	if !sync {
		return os.MkdirAll(parentDir, os.ModePerm)
	}

	repoDir := path.Dir(parentDir)
	newRepoDir := !dirExists(repoDir)
	newParentDir := newRepoDir || !dirExists(parentDir)
	if err := os.MkdirAll(parentDir, os.ModePerm); err != nil {
		return err
	}
	if newRepoDir {
		if err := syncDir(b.objDir); err != nil {
			return err
		}
	}
	if newParentDir {
		if err := syncDir(repoDir); err != nil {
			return err
		}
	}
	return nil
}

func dirExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

// syncDir fsyncs a directory so that renames and creations within it survive
// a crash.
func syncDir(p string) error {
	d, err := os.Open(p)
	if err != nil {
		return fmt.Errorf("failed to open dir %s for sync: %v", p, err)
	}
	if err := d.Sync(); err != nil {
		_ = d.Close()
		return fmt.Errorf("failed to sync dir %s: %v", p, err)
	}
	if err := d.Close(); err != nil {
		return fmt.Errorf("failed to close dir %s after sync: %v", p, err)
	}
	return nil
}

// exists reports whether an object is present and usable.
//
// A zero-length file counts as absent. No object type has a valid empty
// encoding, so a zero-length file is the signature of a write that was
// published but never made durable — the pre-fsync failure mode. Calling it
// present is what made that damage permanent: /check-blocks would answer that
// the client already uploaded the block, so it would never be sent again.
//
// A stat error other than "not exist" is returned rather than swallowed, and
// never reported as present.
func (b *fsBackend) exists(repoID string, objID string) (bool, error) {
	p, err := b.objPath(repoID, objID)
	if err != nil {
		return false, err
	}
	fileInfo, err := os.Stat(p)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	return fileInfo.Size() > 0, nil
}

func (b *fsBackend) stat(repoID string, objID string) (int64, error) {
	path, err := b.objPath(repoID, objID)
	if err != nil {
		return -1, err
	}
	fileInfo, err := os.Stat(path)
	if err != nil {
		return -1, err
	}
	return fileInfo.Size(), nil
}
