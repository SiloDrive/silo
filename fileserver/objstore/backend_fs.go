// Implementation of file system storage backend.
package objstore

import (
	"fmt"
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
	objDir := path.Join(seafileDataDir, "storage", objType)
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

func (b *fsBackend) write(repoID string, objID string, r io.Reader, sync bool) error {
	if !utils.IsObjectIDValid(objID) {
		return fmt.Errorf("invalid object id %q", objID)
	}
	parentDir := path.Join(b.objDir, repoID, objID[:2])
	p := path.Join(parentDir, objID[2:])
	err := os.MkdirAll(parentDir, os.ModePerm)
	if err != nil {
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

	_, err = io.Copy(tFile, r)
	if err != nil {
		_ = tFile.Close()
		return err
	}

	err = tFile.Close()
	if err != nil {
		return err
	}

	err = os.Rename(tFile.Name(), p)
	if err != nil {
		return err
	}

	success = true
	return nil
}

func (b *fsBackend) exists(repoID string, objID string) (bool, error) {
	path, err := b.objPath(repoID, objID)
	if err != nil {
		return false, err
	}
	_, err = os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, err
		}
		return true, err
	}
	return true, nil
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
