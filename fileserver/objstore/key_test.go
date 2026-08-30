package objstore

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	log "github.com/sirupsen/logrus"
)

// captureLog collects what the package logs while fn runs.
func captureLog(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	prev := log.StandardLogger().Out
	prevLevel := log.GetLevel()
	log.SetOutput(&buf)
	log.SetLevel(log.DebugLevel)
	defer func() {
		log.SetOutput(prev)
		log.SetLevel(prevLevel)
	}()
	fn()
	return buf.String()
}

func TestStorageKeyGeneratedAtFirstStart(t *testing.T) {
	dataDir := t.TempDir()

	var key []byte
	var created bool
	var err error
	out := captureLog(t, func() { key, created, err = loadStorageKey(dataDir) })
	if err != nil {
		t.Fatalf("first start: %v", err)
	}
	if !created {
		t.Error("first start did not report creating the key")
	}
	if len(key) != StorageKeySize {
		t.Errorf("key is %d bytes, want %d", len(key), StorageKeySize)
	}
	if bytes.Equal(key, make([]byte, StorageKeySize)) {
		t.Error("key is all zeroes")
	}

	// The one-time warning is the only notice an operator gets that something
	// irreplaceable now exists. Losing it loses every object on every tier.
	if !strings.Contains(out, "storage.key") || !strings.Contains(strings.ToLower(out), "back") {
		t.Errorf("generation did not print a back-it-up warning, got: %q", out)
	}

	info, err := os.Stat(KeyPath(dataDir))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("storage.key is mode %o, want 600", info.Mode().Perm())
	}
	if info.Size() != StorageKeySize {
		t.Errorf("storage.key is %d bytes, want %d", info.Size(), StorageKeySize)
	}
}

func TestStorageKeyReusedOnSecondStart(t *testing.T) {
	dataDir := t.TempDir()
	first, _, err := loadStorageKey(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	second, created, err := loadStorageKey(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Error("second start reported creating the key")
	}
	if !bytes.Equal(first, second) {
		t.Error("second start read a different key")
	}
	out := captureLog(t, func() { _, _, _ = loadStorageKey(dataDir) })
	if strings.Contains(strings.ToLower(out), "back") {
		t.Errorf("the back-it-up warning is not one-time, got: %q", out)
	}
}

func TestStorageKeyRefusesWrongLength(t *testing.T) {
	for _, n := range []int{0, 16, 31, 33} {
		dataDir := t.TempDir()
		if err := os.WriteFile(KeyPath(dataDir), make([]byte, n), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := loadStorageKey(dataDir); err == nil {
			t.Errorf("a %d-byte storage.key was accepted", n)
		}
	}
}

// putObjectFile writes raw bytes where an object of a type lives, without
// going through the store. These are the tests about what is already on disk.
func putObjectFile(t *testing.T, dataDir, objType, id string, content []byte) {
	t.Helper()
	dir := filepath.Join(LibraryDir(dataDir, objType, "lib"), id[:2])
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, id[2:]), content, 0o644); err != nil {
		t.Fatal(err)
	}
}

// The rule that makes cannot-lose enforceable rather than merely documented:
// a key that has gone missing must not be silently replaced, because a fresh
// key over sealed frames looks exactly like a clean first start and the damage
// is only found at the next read of an old object.
func TestStorageKeyRefusesGenerationOverSealedObjects(t *testing.T) {
	for _, objType := range Types {
		t.Run(objType, func(t *testing.T) {
			dataDir := t.TempDir()
			id := strings.Repeat("ab", 32)
			frame, err := sealFrame(testKey(t), id, []byte("sealed under a key that is now gone"))
			if err != nil {
				t.Fatal(err)
			}
			putObjectFile(t, dataDir, objType, id, frame)

			_, _, err = loadStorageKey(dataDir)
			if err == nil {
				t.Fatal("generated a new storage.key over a store that holds sealed frames")
			}
			if !strings.Contains(err.Error(), "storage.key") {
				t.Errorf("error does not name the missing file: %v", err)
			}
			if _, statErr := os.Stat(KeyPath(dataDir)); statErr == nil {
				t.Error("a key was written despite the refusal")
			}
		})
	}
}

// The other half of that rule, and the one it is easy to get wrong: a store
// written before there were frames has objects and no key, legitimately. It is
// an upgrade, not a loss, and refusing it would make this change a migration
// — the one thing the plaintext fallback exists to avoid.
//
// TODO(#23): with no plaintext objects left, every object is a frame and this
// distinction goes away with the fallback.
func TestStorageKeyGeneratesOverAPreFramingStore(t *testing.T) {
	dataDir := t.TempDir()
	id := strings.Repeat("cd", 32)
	putObjectFile(t, dataDir, TypeObjects, id, []byte("a commit from before there were frames"))

	if _, created, err := loadStorageKey(dataDir); err != nil || !created {
		t.Fatalf("a pre-framing store would not start: created=%v err=%v", created, err)
	}
}

// One sealed object anywhere is enough to refuse, even among plaintext ones.
func TestStorageKeyRefusesOnAPartlyIngestedStore(t *testing.T) {
	dataDir := t.TempDir()
	plain := strings.Repeat("cd", 32)
	putObjectFile(t, dataDir, TypeObjects, plain, []byte("not yet rewritten"))

	sealedID := strings.Repeat("ef", 32)
	frame, err := sealFrame(testKey(t), sealedID, []byte("already rewritten"))
	if err != nil {
		t.Fatal(err)
	}
	putObjectFile(t, dataDir, TypeChunks, sealedID, frame)

	if _, _, err := loadStorageKey(dataDir); err == nil {
		t.Fatal("generated a key over a store holding one sealed frame among plaintext")
	}
}

// Empty store directories are the ordinary case — a library that has had
// everything reclaimed, or the type directories objstore.New creates at
// startup — and must not be mistaken for objects.
func TestStorageKeyGeneratesOverAnEmptyStoreTree(t *testing.T) {
	dataDir := t.TempDir()
	for _, objType := range Types {
		if err := os.MkdirAll(LibraryDir(dataDir, objType, "lib"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if _, created, err := loadStorageKey(dataDir); err != nil || !created {
		t.Fatalf("empty store tree: created=%v err=%v", created, err)
	}
}

func TestStorageKeyWarnsOnLoosePermissions(t *testing.T) {
	dataDir := t.TempDir()
	if err := os.WriteFile(KeyPath(dataDir), make([]byte, StorageKeySize), 0o644); err != nil {
		t.Fatal(err)
	}
	var err error
	out := captureLog(t, func() { _, _, err = loadStorageKey(dataDir) })
	if err != nil {
		t.Fatalf("a readable key should still load: %v", err)
	}
	if !strings.Contains(out, "storage.key") {
		t.Errorf("loose permissions were not warned about, got: %q", out)
	}
}

// The cache is what keeps a per-object-store constructor from reading the key
// file on every call, and it must not hand two data directories one key.
func TestStorageKeyCacheIsPerDataDir(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	ka, err := storageKeyFor(a)
	if err != nil {
		t.Fatal(err)
	}
	kb, err := storageKeyFor(b)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(ka, kb) {
		t.Error("two data directories were given the same key")
	}
	again, err := storageKeyFor(a)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(ka, again) {
		t.Error("the cache returned a different key for one data directory")
	}
}
