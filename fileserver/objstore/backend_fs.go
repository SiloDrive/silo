// The filesystem storage backend: server-local disk, the hot tier.
//
// It implements the pack interface degenerately — one object per pack — so
// that the seam is already the shape the pack store and the object-storage
// tiers need. What it does not do is pretend: there is no packing here, and
// the places where a real pack store would do more are named as they come up.
package objstore

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
)

// fsBackend holds one thing: the directory this object type's libraries sit
// under. Every path it builds starts there, so it is derived once at open
// rather than recomputed per call.
//
// It used to carry the object type as well, written at open and read by
// nothing. TypeDir has already folded the type into root by the time the struct
// exists, so the second copy could only ever disagree with the first.
type fsBackend struct {
	root string
}

func newFSBackend(dataDir string, objType string) (*fsBackend, error) {
	root := TypeDir(dataDir, objType)
	if err := os.MkdirAll(root, os.ModePerm); err != nil {
		return nil, err
	}
	return &fsBackend{root: root}, nil
}

// packPath builds the on-disk path for a pack. Ids are fanned out as
// id[:2]/id[2:], which panics on an id shorter than two characters, so the
// store validates rather than trusting its callers.
func (b *fsBackend) packPath(libraryID string, packID string) (string, error) {
	if !validPackID(packID) {
		return "", fmt.Errorf("invalid object id %q", packID)
	}
	return path.Join(b.root, libraryID, packID[:2], packID[2:]), nil
}

func (b *fsBackend) libraryPath(libraryID string) string {
	return path.Join(b.root, libraryID)
}

// notFound maps the filesystem's absence to the seam's, leaving every other
// error alone. A caller deciding whether to fall through to a durable tier
// asks one question regardless of which backend answered it.
func notFound(err error, libraryID, packID string) error {
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%s/%s: %w", libraryID, packID, ErrNotFound)
	}
	return err
}

func (b *fsBackend) read(libraryID string, packID string, w io.Writer) error {
	p, err := b.packPath(libraryID, packID)
	if err != nil {
		return err
	}
	fd, err := os.Open(p)
	if err != nil {
		return notFound(err, libraryID, packID)
	}
	defer func() { _ = fd.Close() }()

	if _, err := io.Copy(w, fd); err != nil {
		return err
	}
	return nil
}

// readAt reads one byte range out of a pack.
//
// io.ReaderAt semantics, which are stricter than Read's and are what the
// callers above this need: it fills p or reports why it could not, and a read
// running past the end returns io.EOF with the bytes it did get. A pack index
// asking for a frame at a known offset and length must be able to tell "the
// pack is shorter than the index says" from "the read was short this time".
func (b *fsBackend) readAt(libraryID string, packID string, p []byte, off int64) (int, error) {
	fp, err := b.packPath(libraryID, packID)
	if err != nil {
		return 0, err
	}
	fd, err := os.Open(fp)
	if err != nil {
		return 0, notFound(err, libraryID, packID)
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
// zero-length or absent chunk. Nothing repairs that afterwards: the client
// believes it has already uploaded those chunks, so a resync does not send them
// again.
//
// The temp-file-then-rename is what publishing means here, and it is worth
// being exact about how that differs from a real pack store rather than
// implying they are the same mechanism.
//
// A real pack is *appended to* while it is open: chunk frames go on the end,
// and each one is append → fsync → atomic index update, so a torn tail is
// recoverable by truncating the pack to its last indexed offset. Its crash
// safety comes from the index, not from atomic publication.
//
// This interface never sees a pack in that state. An open pack is local
// staging — it cannot live on a durable tier at all, since the S3 feature
// floor is PUT, ranged GET, DELETE and LIST with no append and no multipart —
// so a pack enters this interface only once it is sealed, and arrives whole.
// Here, where a pack holds exactly one object, it is sealed the moment it is
// written, and the rename is what makes it appear complete or not at all.
func (b *fsBackend) write(libraryID string, packID string, r io.Reader, sync bool) error {
	p, err := b.packPath(libraryID, packID)
	if err != nil {
		return err
	}
	parentDir := path.Dir(p)
	if err := b.mkObjDirs(parentDir, sync); err != nil {
		return err
	}

	// The temp file is written into the directory it will be renamed within,
	// so the publish is a rename inside one directory rather than a move that
	// might cross a filesystem. list skips what it leaves behind on a failure
	// by the suffix CreateTemp adds.
	tFile, err := os.CreateTemp(parentDir, packID+".*")
	if err != nil {
		return err
	}
	// Cleaned up unless the rename below has published it, which is tracked by
	// clearing the name rather than by a "did it work" flag.
	//
	// The difference matters, and is not only about spelling. The flag was set
	// after the directory sync, so a sync failure ran the cleanup — but by then
	// the rename had already happened and the temp name was free, and CreateTemp
	// is entitled to hand that same name to the next writer. The cleanup would
	// then have deleted a file belonging to somebody else's in-flight write. It
	// is a narrow race and not one that can be provoked on demand, which is why
	// it is described here rather than pinned by a test.
	tmpName := tFile.Name()
	defer func() {
		if tmpName != "" {
			_ = os.Remove(tmpName)
		}
	}()

	if _, err := io.Copy(tFile, r); err != nil {
		_ = tFile.Close()
		return err
	}

	if sync {
		if err := tFile.Sync(); err != nil {
			_ = tFile.Close()
			return fmt.Errorf("failed to sync object %s/%s: %v", libraryID, packID, err)
		}
	}

	if err := tFile.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, p); err != nil {
		return err
	}
	tmpName = ""

	if sync {
		// Until the directory itself is synced the rename can be lost, which
		// would leave the object under its temp name — invisible to reads and
		// invisible to the GC, which only walks well-formed object paths.
		if err := syncDir(parentDir); err != nil {
			return err
		}
	}
	return nil
}

// mkObjDirs creates the library and fan-out directories an object is written
// into. When sync is set, any directory it has to create is made durable
// before the object lands in it — fsyncing a file does not make the chain of
// directory entries leading to it durable, so a new fan-out directory can
// otherwise take a freshly synced object down with it.
//
// The object type directory is created once at startup, so at most two levels
// can be missing here and the syncs cost nothing in the common case where the
// library has been written to before.
func (b *fsBackend) mkObjDirs(parentDir string, sync bool) error {
	if !sync {
		return os.MkdirAll(parentDir, os.ModePerm)
	}

	libraryDir := path.Dir(parentDir)
	newLibraryDir := !dirExists(libraryDir)
	newParentDir := newLibraryDir || !dirExists(parentDir)
	if err := os.MkdirAll(parentDir, os.ModePerm); err != nil {
		return err
	}
	if newLibraryDir {
		if err := syncDir(b.root); err != nil {
			return err
		}
	}
	if newParentDir {
		if err := syncDir(libraryDir); err != nil {
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

func (b *fsBackend) stat(libraryID string, packID string) (int64, error) {
	p, err := b.packPath(libraryID, packID)
	if err != nil {
		return -1, err
	}
	fileInfo, err := os.Stat(p)
	if err != nil {
		return -1, notFound(err, libraryID, packID)
	}
	return fileInfo.Size(), nil
}

// modTime is the mtime of an object's file.
// list walks a library's packs.
//
// A library directory that does not exist holds no packs, which is not an error:
// a library that has never been written to and one that has had everything
// reclaimed are the same answer to this question, and both are ordinary.
//
// Only well-formed ids are yielded. What that skips is the debris of an
// interrupted write — a temp file under its `<id>.<random>` name, which the
// fan-out directory can hold for fs and commit objects. Those are not packs:
// nothing references them, and reporting them as packs would have the caller
// asking a pack index about an id that was never in it.
func (b *fsBackend) list(libraryID string, fn func(packInfo) error) error {
	libraryDir := b.libraryPath(libraryID)
	entries, err := os.ReadDir(libraryDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("failed to read %s: %v", libraryDir, err)
	}

	for _, fanout := range entries {
		if !fanout.IsDir() || len(fanout.Name()) != 2 {
			continue
		}
		inner, err := os.ReadDir(filepath.Join(libraryDir, fanout.Name()))
		if err != nil {
			return fmt.Errorf("failed to read %s: %v", filepath.Join(libraryDir, fanout.Name()), err)
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
			if err := fn(packInfo{id: packID, size: info.Size(), modTime: info.ModTime()}); err != nil {
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
func (b *fsBackend) remove(libraryID string, packID string) error {
	p, err := b.packPath(libraryID, packID)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// removeLibrary deletes every pack a library holds, idempotently.
func (b *fsBackend) removeLibrary(libraryID string) error {
	return os.RemoveAll(b.libraryPath(libraryID))
}

// libraryUsage adds up everything under a library's directory.
//
// Every file, not every object: this is the pair of removeLibrary above, and
// that one is a RemoveAll. So it counts the fan-out, the packs directory, a
// pack's footer and filter, and the temp file an interrupted write left — all
// of which removeLibrary deletes and none of which list reports.
//
// A directory that does not exist holds nothing, which is not an error for the
// same reason it is not one in list: a library never written to and one
// already reclaimed are the same answer.
func (b *fsBackend) libraryUsage(libraryID string) (files int, bytes int64, err error) {
	dir := b.libraryPath(libraryID)
	err = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if errors.Is(err, fs.ErrNotExist) {
			// Removed between the readdir and the stat, so it is not there to
			// be reclaimed and not there to be counted.
			return nil
		}
		if err != nil {
			return err
		}
		files++
		bytes += info.Size()
		return nil
	})
	if errors.Is(err, fs.ErrNotExist) {
		return 0, 0, nil
	}
	if err != nil {
		return 0, 0, fmt.Errorf("failed to walk %s: %v", dir, err)
	}
	return files, bytes, nil
}
