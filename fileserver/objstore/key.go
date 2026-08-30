// storage.key: the one secret the object store cannot be operated without.
//
// Generated rather than derived — there is no secret worth deriving from —
// 32 random bytes at first start, 0600 in the data directory. It is the first
// item in the must-not-lose set, and it is effectively unrotatable: every
// tier holds byte-identical ciphertext under it, so rotation is a rewrite of
// everything everywhere. Cannot-lose and cannot-rotate are two halves of one
// fact; docs/storage.md § Storage encryption, universal and docs/backup.md
// state them together.
//
// It lives in this package rather than one of its own so that the store root
// it has to inspect is Root(), rather than a second spelling of "storage"
// somewhere else. gc already demonstrated what a second spelling costs: a
// renamed store turned "silo gc -delete" into a silent no-op that reported
// success.
package objstore

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sync"

	log "github.com/sirupsen/logrus"
)

// KeyName is the key file's name within the data directory.
const KeyName = "storage.key"

// KeyPath returns where storage.key lives for a data directory.
func KeyPath(dataDir string) string { return filepath.Join(dataDir, KeyName) }

// The key is read once per data directory. Every library opens two object
// stores and a server opens a Store per library, so without this the key file
// would be read on a path that runs thousands of times for a value that
// cannot change while the process is up.
var (
	keyCacheMu sync.Mutex
	keyCache   = map[string][]byte{}
)

// storageKeyFor returns the data directory's key, loading it once.
func storageKeyFor(dataDir string) ([]byte, error) {
	abs, err := filepath.Abs(dataDir)
	if err != nil {
		return nil, err
	}
	keyCacheMu.Lock()
	defer keyCacheMu.Unlock()
	if key, ok := keyCache[abs]; ok {
		return key, nil
	}
	key, _, err := loadStorageKey(abs)
	if err != nil {
		return nil, err
	}
	keyCache[abs] = key
	return key, nil
}

// loadStorageKey reads the data directory's storage key, generating it if this
// is a first start. It reports whether it generated one.
func loadStorageKey(dataDir string) ([]byte, bool, error) {
	path := KeyPath(dataDir)
	key, err := os.ReadFile(path)
	switch {
	case err == nil:
		if len(key) != StorageKeySize {
			return nil, false, fmt.Errorf(
				"%s is %d bytes, want %d: this is not a storage key, and starting would write objects under a key that cannot read the ones already there",
				path, len(key), StorageKeySize)
		}
		if info, statErr := os.Stat(path); statErr == nil && info.Mode().Perm()&0o077 != 0 {
			log.Warnf("%s is mode %04o and should be 0600: it is readable by other users on this machine", path, info.Mode().Perm())
		}
		return key, false, nil
	case !errors.Is(err, fs.ErrNotExist):
		return nil, false, fmt.Errorf("cannot read %s: %w", path, err)
	}

	// No key. That is one of three things, and they are not the same thing:
	//
	//   - a genuine first start, which is the ordinary case;
	//   - a store written before this server sealed anything, being restarted
	//     on one that does — an upgrade, and the plaintext fallback in
	//     object() is exactly what exists to carry it;
	//   - a key that has gone missing from a store that holds sealed frames,
	//     which is unrecoverable.
	//
	// Only the third is a refusal, and the question that separates it is
	// whether anything on disk is a frame — not whether anything is there at
	// all. Generating a fresh key over sealed frames looks exactly like a
	// clean first start: the server comes up, prints the warning below, serves
	// requests, and the damage is only found at the first read of an object
	// sealed under the old key, by which time more has been written under the
	// new one. Cannot-lose has to be enforced at the moment of loss.
	sealed, err := aSealedObject(dataDir)
	if err != nil {
		return nil, false, err
	}
	if sealed != "" {
		return nil, false, fmt.Errorf(
			"%s is missing but %s holds objects sealed under it (%s): restore the key from backup, because a new one cannot read what is there",
			path, Root(dataDir), sealed)
	}

	return generateStorageKey(path)
}

// generateStorageKey writes a new key, atomically and without clobbering.
//
// Temp-file-then-link rather than a rename: rename would overwrite, and the
// one thing this must never do is replace a key that another process created
// between the read above and this write. Link fails on an existing target,
// which turns that race into a re-read of the winner's key.
func generateStorageKey(path string) ([]byte, bool, error) {
	key := make([]byte, StorageKeySize)
	if _, err := rand.Read(key); err != nil {
		return nil, false, fmt.Errorf("no randomness for %s: %w", path, err)
	}

	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, KeyName+".*")
	if err != nil {
		return nil, false, fmt.Errorf("cannot create %s: %w", path, err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()

	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return nil, false, fmt.Errorf("cannot set permissions on %s: %w", path, err)
	}
	if _, err := tmp.Write(key); err != nil {
		_ = tmp.Close()
		return nil, false, fmt.Errorf("cannot write %s: %w", path, err)
	}
	// Synced before it is published, and the directory after. A key that is
	// lost to a power cut after objects have been written under it is the
	// unrecoverable case this whole file is about.
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return nil, false, fmt.Errorf("cannot sync %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return nil, false, fmt.Errorf("cannot close %s: %w", path, err)
	}

	if err := os.Link(tmp.Name(), path); err != nil {
		if !errors.Is(err, fs.ErrExist) {
			return nil, false, fmt.Errorf("cannot publish %s: %w", path, err)
		}
		// Another process won. Its key is the store's key.
		existing, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil, false, fmt.Errorf("cannot read %s: %w", path, readErr)
		}
		if len(existing) != StorageKeySize {
			return nil, false, fmt.Errorf("%s is %d bytes, want %d", path, len(existing), StorageKeySize)
		}
		return existing, false, nil
	}
	if err := syncDir(dir); err != nil {
		return nil, false, err
	}

	log.Warnf("Generated %s — 32 bytes that every object in this store is encrypted under. "+
		"Back it up now, somewhere other than this machine. It cannot be rotated, and without it "+
		"every object on every tier is unreadable. This is printed once.", path)
	return key, true, nil
}

// aSealedObject returns the path of some object on disk that is a sealed
// frame, or "" if none is.
//
// One is enough: the question is whether anything would be orphaned by
// generating a new key, and one orphan is a refusal. That also makes the
// answer cheap in the case that matters most — a lost key over a sealed store
// stops at the first file it meets.
//
// The case it is not cheap in is the upgrade: a store with no frames in it
// has to be walked to the end to establish that. It is a walk of the whole
// store, reading four bytes per object, once in an install's life, and #23
// walks the same tree to rewrite it. Directories do not count, and neither
// does an empty store: a library that has had everything reclaimed, one that
// has never been written to, and the type directories New creates at startup
// all leave the same tree behind.
func aSealedObject(dataDir string) (string, error) {
	found := ""
	for _, objType := range Types {
		root := TypeDir(dataDir, objType)
		err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					return nil
				}
				return err
			}
			if d.IsDir() {
				return nil
			}
			sealed, err := fileIsFrame(p)
			if err != nil {
				return err
			}
			if sealed {
				found = p
				return fs.SkipAll
			}
			return nil
		})
		if err != nil {
			return "", fmt.Errorf("cannot inspect %s: %w", root, err)
		}
		if found != "" {
			return found, nil
		}
	}
	return "", nil
}

// fileIsFrame reports whether a file on disk begins a sealed frame.
func fileIsFrame(p string) (bool, error) {
	fd, err := os.Open(p)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// Removed between the walk and the open. A file that is gone
			// cannot be orphaned by a new key.
			return false, nil
		}
		return false, err
	}
	defer func() { _ = fd.Close() }()

	magic := make([]byte, len(frameMagic))
	n, err := io.ReadFull(fd, magic)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return false, err
	}
	return isFrame(magic[:n]), nil
}

// EnsureKey loads or generates the data directory's storage key, so that a
// server can fail at startup rather than on the first request.
//
// Without this the key is loaded lazily, at the first ObjectStore opened for
// the first library someone touches — which turns "this store's key is gone"
// into a 500 on an ordinary request rather than a server that says why it
// will not run.
func EnsureKey(dataDir string) error {
	_, err := storageKeyFor(dataDir)
	return err
}
