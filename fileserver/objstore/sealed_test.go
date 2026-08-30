package objstore

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// objPath is where an object's file lands, reaching past the seam on purpose:
// these are the tests that care what is actually on the disk.
func objPath(objType, id string) string {
	return filepath.Join(TypeDir(dataDir, objType), libraryID, id[:2], id[2:])
}

func sha256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// The assertion the whole issue exists for. Everything else here checks that
// framing is invisible; this one checks that it happened.
func TestStoredObjectIsCiphertextOnDisk(t *testing.T) {
	s := New(confPath, dataDir, "sealed-disk")
	plain := []byte("the needle in this haystack is CONFIDENTIAL-MARKER and it must not be on disk")
	id := sha256Hex(plain)

	if err := s.WriteVerified(libraryID, id, bytes.NewReader(plain), false); err != nil {
		t.Fatalf("WriteVerified: %v", err)
	}

	raw, err := os.ReadFile(objPath("sealed-disk", id))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("CONFIDENTIAL-MARKER")) {
		t.Error("the object's plaintext is on disk")
	}
	if !isFrame(raw) {
		t.Error("the object on disk is not a sealed frame")
	}
	if len(raw) != len(plain)+frameOverhead {
		t.Errorf("file is %d bytes for %d of plaintext, want %d more", len(raw), len(plain), frameOverhead)
	}

	var got bytes.Buffer
	if err := s.Read(libraryID, id, &got); err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !bytes.Equal(got.Bytes(), plain) {
		t.Error("what came back is not what went in")
	}
}

// Sizes are the part of framing most easily got wrong, because a wrong one is
// a Content-Length on the wire rather than a visible failure: ChunkStoredSize
// and ObjectSize are both this number.
func TestStatAndListReportPlaintextLength(t *testing.T) {
	s := New(confPath, dataDir, "sealed-size")
	plain := strings.Repeat("s", 5000)
	id := sha256Hex([]byte(plain))
	if err := s.WriteVerified(libraryID, id, strings.NewReader(plain), false); err != nil {
		t.Fatal(err)
	}

	size, err := s.Stat(libraryID, id)
	if err != nil {
		t.Fatal(err)
	}
	if size != int64(len(plain)) {
		t.Errorf("Stat = %d, want the plaintext length %d", size, len(plain))
	}

	info, err := os.Stat(objPath("sealed-size", id))
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != int64(len(plain)+frameOverhead) {
		t.Errorf("the file is %d bytes, want %d", info.Size(), len(plain)+frameOverhead)
	}

	var listed int64 = -1
	if err := s.List(libraryID, func(o ObjectInfo) error {
		if o.ID == id {
			listed = o.Size
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if listed != int64(len(plain)) {
		t.Errorf("List reported %d, want the plaintext length %d", listed, len(plain))
	}
}

// An empty object is still absent, and it is now absent for a second reason:
// nothing shorter than a frame can be one. The pre-fsync torn write is the
// case both rules exist for.
func TestShorterThanAFrameIsAbsent(t *testing.T) {
	s := New(confPath, dataDir, "sealed-short")
	id := strings.Repeat("c", 2*sha256.Size)
	dir := filepath.Dir(objPath("sealed-short", id))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(objPath("sealed-short", id), []byte(frameMagic+"trunc"), 0o644); err != nil {
		t.Fatal(err)
	}
	exists, err := s.Exists(libraryID, id)
	if err != nil {
		t.Fatalf("Exists: %v", err)
	}
	if exists {
		t.Error("a file too short to be a frame was reported present")
	}
}

// WriteVerified hashes what the caller offered, not what goes on disk. Under
// framing those differ, and getting it backwards would verify nothing anyone
// asked about — while still passing, because the frame is self-consistent.
func TestWriteVerifiedHashesThePlaintext(t *testing.T) {
	s := New(confPath, dataDir, "sealed-verify")
	plain := []byte("verified content")
	id := sha256Hex(plain)

	if err := s.WriteVerified(libraryID, id, bytes.NewReader(plain), false); err != nil {
		t.Fatalf("WriteVerified under the plaintext's own hash: %v", err)
	}

	wrong := sha256Hex([]byte("something else"))
	err := s.WriteVerified(libraryID, wrong, bytes.NewReader(plain), false)
	if err == nil {
		t.Fatal("WriteVerified accepted content that does not hash to its id")
	}
	if !strings.Contains(err.Error(), "does not match") && !strings.Contains(err.Error(), "hashes to") {
		t.Errorf("unexpected error: %v", err)
	}
	if _, statErr := os.Stat(objPath("sealed-verify", wrong)); statErr == nil {
		t.Error("the rejected object was published anyway")
	}
}

// A frame whose bytes have been altered is not readable, and says so rather
// than handing back plausible rubbish. This is what the AEAD buys over
// storing plaintext.
func TestCorruptedFrameIsAnError(t *testing.T) {
	s := New(confPath, dataDir, "sealed-corrupt")
	plain := []byte("bit rot happens")
	id := sha256Hex(plain)
	if err := s.WriteVerified(libraryID, id, bytes.NewReader(plain), false); err != nil {
		t.Fatal(err)
	}

	p := objPath("sealed-corrupt", id)
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	raw[len(raw)-1] ^= 0x01
	if err := os.WriteFile(p, raw, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := s.Read(libraryID, id, io.Discard); err == nil {
		t.Error("Read returned a corrupted object without complaint")
	}
	if _, err := s.ReadAt(libraryID, id, make([]byte, 4), 0); err == nil {
		t.Error("ReadAt returned a corrupted object without complaint")
	}
}

// A frame cannot be moved to another object's path, because its header names
// the id it was sealed under and the header is authenticated.
func TestAFrameCannotBeMovedToAnotherPath(t *testing.T) {
	s := New(confPath, dataDir, "sealed-move")
	plain := []byte("mine, at my own id")
	id := sha256Hex(plain)
	if err := s.WriteVerified(libraryID, id, bytes.NewReader(plain), false); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(objPath("sealed-move", id))
	if err != nil {
		t.Fatal(err)
	}

	// A different id, whatever this one happens to start with.
	other := "d" + id[1:]
	if other == id {
		other = "e" + id[1:]
	}
	dir := filepath.Dir(objPath("sealed-move", other))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(objPath("sealed-move", other), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.Read(libraryID, other, io.Discard); err == nil {
		t.Error("a frame opened at another object's path")
	}
}

// The transition. Until #23 rewrites what is already on disk, an object
// written before this landed is plaintext, and refusing to read it would make
// this change a migration rather than an upgrade.
//
// TODO(#23): delete this test with the fallback it covers.
func TestPlaintextObjectsStillRead(t *testing.T) {
	s := New(confPath, dataDir, "sealed-legacy")
	plain := "written before there were frames"
	id := sha256Hex([]byte(plain))
	dir := filepath.Dir(objPath("sealed-legacy", id))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(objPath("sealed-legacy", id), []byte(plain), 0o644); err != nil {
		t.Fatal(err)
	}

	var got bytes.Buffer
	if err := s.Read(libraryID, id, &got); err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got.String() != plain {
		t.Errorf("Read = %q, want %q", got.String(), plain)
	}

	size, err := s.Stat(libraryID, id)
	if err != nil {
		t.Fatal(err)
	}
	if size != int64(len(plain)) {
		t.Errorf("Stat = %d, want %d", size, len(plain))
	}

	p := make([]byte, 7)
	n, err := s.ReadAt(libraryID, id, p, 8)
	if err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if string(p[:n]) != plain[8:15] {
		t.Errorf("ReadAt = %q, want %q", p[:n], plain[8:15])
	}

	exists, err := s.Exists(libraryID, id)
	if err != nil || !exists {
		t.Errorf("Exists = (%v, %v), want (true, nil)", exists, err)
	}

	var listed int64 = -1
	if err := s.List(libraryID, func(o ObjectInfo) error {
		if o.ID == id {
			listed = o.Size
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if listed != int64(len(plain)) {
		t.Errorf("List reported %d for a plaintext object, want %d", listed, len(plain))
	}
}

// Rewriting an object that is already there is ordinary — a client re-uploads
// a chunk it is not sure landed — and the fresh nonce means the bytes differ
// each time. The object must still read back, and the id must not change.
func TestRewritingAnObjectIsFine(t *testing.T) {
	s := New(confPath, dataDir, "sealed-rewrite")
	plain := []byte("written twice, one id")
	id := sha256Hex(plain)

	if err := s.WriteVerified(libraryID, id, bytes.NewReader(plain), false); err != nil {
		t.Fatal(err)
	}
	first, err := os.ReadFile(objPath("sealed-rewrite", id))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.WriteVerified(libraryID, id, bytes.NewReader(plain), false); err != nil {
		t.Fatal(err)
	}
	second, err := os.ReadFile(objPath("sealed-rewrite", id))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(first, second) {
		t.Error("two writes of one object produced identical frames: the nonce is not fresh")
	}

	var got bytes.Buffer
	if err := s.Read(libraryID, id, &got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.Bytes(), plain) {
		t.Error("the rewritten object does not read back")
	}
}

// A store whose key cannot be loaded reports why, on every operation, rather
// than storing plaintext or dereferencing nothing.
func TestAStoreWithNoKeyReportsWhy(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(KeyPath(dir), []byte("too short"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := New(confPath, dir, "sealed-nokey")
	if err := s.ready(); err == nil {
		t.Fatal("a store with an unusable key reported itself ready")
	}
	if !strings.Contains(s.ready().Error(), KeyName) {
		t.Errorf("the error does not name the key file: %v", s.ready())
	}
	if err := s.Write(libraryID, objID, strings.NewReader("x"), false); err == nil {
		t.Error("Write on a keyless store succeeded")
	}
	if _, err := s.Stat(libraryID, objID); err == nil {
		t.Error("Stat on a keyless store succeeded")
	}
}

// The restart this change has to survive: a data directory written by a server
// from before framing — objects on disk, no storage.key — opened by one that
// seals. It is an upgrade, and it must simply work.
//
// TODO(#23): the ingest that rewrites those objects removes this case.
func TestRestartOnAPreFramingStore(t *testing.T) {
	dir := t.TempDir()
	id := strings.Repeat("ab", 32)
	const content = "a commit from before there were frames"
	fan := filepath.Join(LibraryDir(dir, TypeObjects, libraryID), id[:2])
	if err := os.MkdirAll(fan, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fan, id[2:]), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	s := New("", dir, TypeObjects)
	if err := s.ready(); err != nil {
		t.Fatalf("the store will not open after an upgrade: %v", err)
	}

	var got bytes.Buffer
	if err := s.Read(libraryID, id, &got); err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got.String() != content {
		t.Errorf("Read = %q, want %q", got.String(), content)
	}

	// And the key it just generated is real, so anything written from now on
	// is sealed even though what was already there is not.
	newID := sha256Hex([]byte("written after the upgrade"))
	if err := s.WriteVerified(libraryID, newID, bytes.NewReader([]byte("written after the upgrade")), false); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(LibraryDir(dir, TypeObjects, libraryID), newID[:2], newID[2:]))
	if err != nil {
		t.Fatal(err)
	}
	if !isFrame(raw) {
		t.Error("an object written after the upgrade is not sealed")
	}
}
