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

	// No key, and a store that holds something. Generating a fresh one here
	// looks exactly like a clean first start: the server comes up, prints the
	// warning below, serves requests, and the damage is only found at the
	// first read of an object sealed under the old key, by which time more has
	// been written under the new one. Cannot-lose has to be enforced at the
	// moment of loss.
	//
	// The message offers both answers because both are real. There is one
	// install of this server and it is the author's, so discarding the store
	// is a sanctioned way out — docs/storage.md says every object in every
	// store can still be discarded and rewritten, and that freedom expires the
	// first time someone else runs this. Until then, "delete it" is an answer
	// and not a euphemism for data loss.
	empty, err := storeIsEmpty(dataDir)
	if err != nil {
		return nil, false, err
	}
	if !empty {
		return nil, false, fmt.Errorf(
			"%s is missing but %s holds objects: restore the key from backup, or delete %s and let the store be rebuilt — a new key cannot read what is there",
			path, Root(dataDir), Root(dataDir))
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
	// CreateTemp opens 0600, and umask can only clear bits rather than add
	// them, so the key is never briefly world-readable and needs no chmod.
	tmp, err := os.CreateTemp(dir, KeyName+".*")
	if err != nil {
		return nil, false, fmt.Errorf("cannot create %s: %w", path, err)
	}
	defer func() {
		// Close before Remove, and both unconditionally: a second Close on
		// the success path returns ErrClosed, which is exactly the error
		// worth discarding.
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
	}()

	if _, err := tmp.Write(key); err != nil {
		return nil, false, fmt.Errorf("cannot write %s: %w", path, err)
	}
	// Synced before it is published, and the directory after. A key that is
	// lost to a power cut after objects have been written under it is the
	// unrecoverable case this whole file is about.
	if err := tmp.Sync(); err != nil {
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

// storeIsEmpty reports whether the object store holds no objects.
//
// It stops at the first file it finds, so the case that matters — a key gone
// from a store full of objects — is answered immediately.
//
// Directories do not count: a library that has had everything reclaimed, one
// that has never been written to, and the type directories New creates at
// startup all leave the same tree behind.
func storeIsEmpty(dataDir string) (bool, error) {
	empty := true
	for _, objType := range Types {
		root := TypeDir(dataDir, objType)
		err := filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					return nil
				}
				return err
			}
			if d.IsDir() {
				return nil
			}
			empty = false
			return fs.SkipAll
		})
		if err != nil {
			return false, fmt.Errorf("cannot inspect %s: %w", root, err)
		}
		if !empty {
			return false, nil
		}
	}
	return true, nil
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
