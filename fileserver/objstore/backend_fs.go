// The filesystem storage backend: server-local disk, the hot tier.
//
// It implements the pack interface degenerately — one object per pack — so
// that the seam is already the shape the pack store and the object-storage
// tiers need. What it does not do is pretend: there is no packing here, and
// the places where a real pack store would do more are named as they come up.
package objstore

import (
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
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

// sha1Hash is the digest a verified write of a legacy object is checked
// against. It lives here rather than at the call site so that the one place
// that knows this store's ids are SHA-1 is the store.
func sha1Hash() hash.Hash { return sha1.New() }

// validPackID reports whether an id is one this store will build a path from.
//
// Lowercase hex, and either 40 characters or 64: SHA-1 for the objects that
// exist today and SHA-256 for store-v2's chunks and packs, which are already
// arriving while the old ones are still here. Both widths are more than the
// two characters the fan-out slices off, which is the crash this guards.
//
// One case only. Two spellings of an id are two files holding one object, and
// the second is invisible to every reader looking for the first.
func validPackID(id string) bool {
	if len(id) != 40 && len(id) != 64 {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') {
			continue
		}
		return false
	}
	return true
}

// packPath builds the on-disk path for a pack. Ids are fanned out as
// id[:2]/id[2:], which panics on an id shorter than two characters, so the
// store validates rather than trusting its callers.
func (b *fsBackend) packPath(repoID string, packID string) (string, error) {
	if !validPackID(packID) {
		return "", fmt.Errorf("invalid object id %q", packID)
	}
	return path.Join(b.objDir, repoID, packID[:2], packID[2:]), nil
}

func (b *fsBackend) repoPath(repoID string) string {
	return path.Join(b.objDir, repoID)
}

// notFound maps the filesystem's absence to the seam's, leaving every other
// error alone. A caller deciding whether to fall through to a durable tier
// asks one question regardless of which backend answered it.
func notFound(err error, repoID, packID string) error {
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%s/%s: %w", repoID, packID, ErrNotFound)
	}
	return err
}

func (b *fsBackend) read(repoID string, packID string, w io.Writer) error {
	p, err := b.packPath(repoID, packID)
	if err != nil {
		return err
	}
	fd, err := os.Open(p)
	if err != nil {
		return notFound(err, repoID, packID)
	}
	defer func() { _ = fd.Close() }()

	_, err = io.Copy(w, fd)
	return err
}

// readAt reads one byte range out of a pack.
//
// io.ReaderAt semantics, which are stricter than Read's and are what the
// callers above this need: it fills p or reports why it could not, and a read
// running past the end returns io.EOF with the bytes it did get. A pack index
// asking for a frame at a known offset and length must be able to tell "the
// pack is shorter than the index says" from "the read was short this time".
func (b *fsBackend) readAt(repoID string, packID string, p []byte, off int64) (int, error) {
	fp, err := b.packPath(repoID, packID)
	if err != nil {
		return 0, err
	}
	fd, err := os.Open(fp)
	if err != nil {
		return 0, notFound(err, repoID, packID)
	}
	defer func() { _ = fd.Close() }()

	return fd.ReadAt(p, off)
}

// write stores a pack and publishes it atomically. When sync is set the pack
// is durable by the time write returns: the data is fsynced before the rename
// that publishes it, and the directory entry is fsynced after.
//
// That ordering is not optional for correctness. The branch head lives in
// SQLite, which fsyncs its WAL, so a head-commit UPDATE can survive a power cut
// that the objects it references do not — leaving a head pointing at a
// zero-length or absent block. Nothing repairs that afterwards: the client
// believes it has already uploaded those blocks, so a resync does not send them
// again.
//
// The temp-file-then-rename is also what "seal" means for this backend. A pack
// becomes visible in one step, complete, or not at all — the property a real
// pack store gets from writing a pack once and never appending to it again,
// and the property object storage gets from a single PUT.
func (b *fsBackend) write(repoID string, packID string, r io.Reader, opts writeOpts) error {
	p, err := b.packPath(repoID, packID)
	if err != nil {
		return err
	}
	parentDir := path.Dir(p)
	if err := b.mkObjDirs(parentDir, opts.sync); err != nil {
		return err
	}

	tmpDir := b.tmpDir
	if b.objType != TypeBlocks {
		tmpDir = parentDir
	}
	tFile, err := os.CreateTemp(tmpDir, packID+".*")
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
	if opts.verify != nil {
		r = io.TeeReader(r, opts.verify)
	}

	_, err = io.Copy(tFile, r)
	if err != nil {
		_ = tFile.Close()
		return err
	}

	if opts.verify != nil {
		if got := hex.EncodeToString(opts.verify.Sum(nil)); got != packID {
			_ = tFile.Close()
			return fmt.Errorf("object %s/%s hashes to %s: %w", repoID, packID, got, ErrContentMismatch)
		}
	}

	if opts.sync {
		if err := tFile.Sync(); err != nil {
			_ = tFile.Close()
			return fmt.Errorf("failed to sync object %s/%s: %v", repoID, packID, err)
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

	if opts.sync {
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

func (b *fsBackend) stat(repoID string, packID string) (int64, error) {
	p, err := b.packPath(repoID, packID)
	if err != nil {
		return -1, err
	}
	fileInfo, err := os.Stat(p)
	if err != nil {
		return -1, notFound(err, repoID, packID)
	}
	return fileInfo.Size(), nil
}

// list walks a repo's packs.
//
// A repo directory that does not exist holds no packs, which is not an error:
// a repo that has never been written to and one that has had everything
// reclaimed are the same answer to this question, and both are ordinary.
//
// Only well-formed ids are yielded. What that skips is the debris of an
// interrupted write — a temp file under its `<id>.<random>` name, which the
// fan-out directory can hold for fs and commit objects. Those are not packs:
// nothing references them, and reporting them as packs would have the caller
// asking a pack index about an id that was never in it.
func (b *fsBackend) list(repoID string, fn func(packID string, size int64) error) error {
	repoDir := b.repoPath(repoID)
	entries, err := os.ReadDir(repoDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("failed to read %s: %v", repoDir, err)
	}

	for _, fanout := range entries {
		if !fanout.IsDir() || len(fanout.Name()) != 2 {
			continue
		}
		inner, err := os.ReadDir(filepath.Join(repoDir, fanout.Name()))
		if err != nil {
			return fmt.Errorf("failed to read %s: %v", filepath.Join(repoDir, fanout.Name()), err)
		}
		for _, e := range inner {
			if e.IsDir() {
				continue
			}
			packID := fanout.Name() + e.Name()
			if !validPackID(packID) {
				continue
			}
			info, err := e.Info()
			if errors.Is(err, fs.ErrNotExist) {
				// Removed between the readdir and the stat. A pack that is
				// gone is not one to report, and a concurrent GC is allowed.
				continue
			}
			if err != nil {
				return err
			}
			if err := fn(packID, info.Size()); err != nil {
				return err
			}
		}
	}
	return nil
}

// remove deletes one pack, idempotently.
//
// Removing something that is already gone is success. Compaction deletes a
// pack after its live frames have been rewritten and its index entries
// swapped, and it must be interruptible at every step — so a retry that finds
// the pack already deleted has to be able to carry on rather than stop.
func (b *fsBackend) remove(repoID string, packID string) error {
	p, err := b.packPath(repoID, packID)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// removeRepo deletes every pack a repo holds, idempotently.
func (b *fsBackend) removeRepo(repoID string) error {
	return os.RemoveAll(b.repoPath(repoID))
}
