package objstore

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// frameSite is the pack holding an object's frame and where in it.
//
// Since the cutover the write path puts every frame in a pack, so the tests
// below ask for the frame rather than for a file -- what they are about is the
// frame, and which container holds it is not their subject. An id that is in no
// pack was never written through the store, which is a broken test rather than
// a loose object: the tests that plant bytes by hand know where they put them.
func frameSite(t *testing.T, s *ObjectStore, id string) (path string, at indexEntry) {
	t.Helper()
	set, err := s.packs.set(libraryID)
	if err != nil {
		t.Fatal(err)
	}
	// The store's own lookup, so the test measures the pack a read would use.
	r, e, ok := set.find(id)
	if !ok {
		t.Fatalf("%s is in no pack of the %s store", id, s.ObjType)
	}
	switch p := r.(type) {
	case *openPack:
		return p.path, e
	case *sealedPack:
		return p.path, e
	}
	t.Fatalf("%s is in a %T, which has no path", id, r)
	return "", indexEntry{}
}

// storedFrame is an object's frame as it sits on the disk.
func storedFrame(t *testing.T, s *ObjectStore, id string) []byte {
	t.Helper()
	path, at := frameSite(t, s, id)
	return readAt(t, path, at.Offset, int(at.Length))
}

// flipAByteOfTheFrame corrupts an object's frame where it lies, without
// disturbing anything around it. In a pack that is one byte in the middle of a
// file, which is what makes it a truer test of the AEAD than rewriting a file.
func flipAByteOfTheFrame(t *testing.T, s *ObjectStore, id string) {
	t.Helper()
	path, at := frameSite(t, s, id)
	last := at.Offset + at.Length - 1
	b := readAt(t, path, last, 1)
	b[0] ^= 0x01
	writeAt(t, path, last, b)
}

// The assertion the whole issue exists for. Everything else here checks that
// framing is invisible; this one checks that it happened.
func TestStoredObjectIsCiphertextOnDisk(t *testing.T) {
	s := New(confPath, dataDir, "sealed-disk")
	plain := []byte("the needle in this haystack is CONFIDENTIAL-MARKER and it must not be on disk")
	id := idOf(plain)

	if err := s.WriteVerified(libraryID, id, bytes.NewReader(plain), false); err != nil {
		t.Fatalf("WriteVerified: %v", err)
	}

	raw := storedFrame(t, s, id)
	if bytes.Contains(raw, []byte("CONFIDENTIAL-MARKER")) {
		t.Error("the object's plaintext is on disk")
	}
	// And not anywhere else in the file that holds it, which is the assertion
	// that survived the move into packs: a pack is one file with many frames,
	// and a leak in the header or the index would not show up in the frame.
	path, _ := frameSite(t, s, id)
	whole, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(whole, []byte("CONFIDENTIAL-MARKER")) {
		t.Errorf("the object's plaintext is somewhere in %s", filepath.Base(path))
	}
	if !isFrame(raw) {
		t.Error("the object on disk is not a sealed frame")
	}
	if len(raw) != len(plain)+frameOverhead {
		t.Errorf("the frame is %d bytes for %d of plaintext, want %d more", len(raw), len(plain), frameOverhead)
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
	id := idOf([]byte(plain))
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

	if _, at := frameSite(t, s, id); at.Length != int64(len(plain)+frameOverhead) {
		t.Errorf("the frame is %d bytes, want %d", at.Length, len(plain)+frameOverhead)
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
	putObjectFile(t, dataDir, "sealed-short", libraryID, id, []byte(frameMagic+"trunc"))
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
	id := idOf(plain)

	if err := s.WriteVerified(libraryID, id, bytes.NewReader(plain), false); err != nil {
		t.Fatalf("WriteVerified under the plaintext's own hash: %v", err)
	}

	wrong := idOf([]byte("something else"))
	err := s.WriteVerified(libraryID, wrong, bytes.NewReader(plain), false)
	if err == nil {
		t.Fatal("WriteVerified accepted content that does not hash to its id")
	}
	if !strings.Contains(err.Error(), "does not match") && !strings.Contains(err.Error(), "hashes to") {
		t.Errorf("unexpected error: %v", err)
	}
	if exists, _ := s.Exists(libraryID, wrong); exists {
		t.Error("the rejected object was published anyway")
	}
}

// A frame whose bytes have been altered is not readable, and says so rather
// than handing back plausible rubbish. This is what the AEAD buys over
// storing plaintext.
func TestCorruptedFrameIsAnError(t *testing.T) {
	s := New(confPath, dataDir, "sealed-corrupt")
	plain := []byte("bit rot happens")
	id := idOf(plain)
	if err := s.WriteVerified(libraryID, id, bytes.NewReader(plain), false); err != nil {
		t.Fatal(err)
	}

	flipAByteOfTheFrame(t, s, id)

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
	id := idOf(plain)
	if err := s.WriteVerified(libraryID, id, bytes.NewReader(plain), false); err != nil {
		t.Fatal(err)
	}
	raw := storedFrame(t, s, id)

	// A different id, whatever this one happens to start with.
	other := "d" + id[1:]
	if other == id {
		other = "e" + id[1:]
	}
	putObjectFile(t, dataDir, "sealed-move", libraryID, other, raw)
	if err := s.Read(libraryID, other, io.Discard); err == nil {
		t.Error("a frame opened at another object's path")
	}
}

// Nothing in the store is unsealed, so bytes that are not a frame are
// corruption and are reported as such. There is no reading of plaintext from
// the store: a store holding any is one the server refuses to start on.
func TestUnsealedBytesAreCorruption(t *testing.T) {
	s := New(confPath, dataDir, "sealed-unsealed")
	plain := "written by something that is not this store"
	id := idOf([]byte(plain))
	putObjectFile(t, dataDir, "sealed-unsealed", libraryID, id, []byte(plain))

	if err := s.Read(libraryID, id, io.Discard); !errors.Is(err, ErrFrameCorrupt) {
		t.Errorf("Read of unsealed bytes = %v, want ErrFrameCorrupt", err)
	}
	if _, err := s.ReadAt(libraryID, id, make([]byte, 4), 0); !errors.Is(err, ErrFrameCorrupt) {
		t.Errorf("ReadAt of unsealed bytes = %v, want ErrFrameCorrupt", err)
	}
}

// Rewriting an object that is already there is ordinary — a client re-uploads
// a chunk it is not sure landed — and the fresh nonce means the bytes differ
// each time. The object must still read back, and the id must not change.
func TestRewritingAnObjectIsFine(t *testing.T) {
	s := New(confPath, dataDir, "sealed-rewrite")
	plain := []byte("written twice, one id")
	id := idOf(plain)

	if err := s.WriteVerified(libraryID, id, bytes.NewReader(plain), false); err != nil {
		t.Fatal(err)
	}
	first := storedFrame(t, s, id)
	if err := s.WriteVerified(libraryID, id, bytes.NewReader(plain), false); err != nil {
		t.Fatal(err)
	}
	second := storedFrame(t, s, id)
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
