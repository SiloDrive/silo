package objmgr

import (
	"bytes"
	"testing"

	"github.com/dkam/silo/fileserver/objstore"
	"github.com/dkam/silo/store"
)

// The two encryptions are two, and this is the test that shows it.
//
// A client seals a chunk under its library's content key and uploads the
// frame; the server stores that frame inside a second frame under
// storage.key, which it holds and the client never sees. What comes back out
// is the client's frame, byte for byte — the server has not learned anything
// and has not changed anything — while what sits on the disk is neither the
// plaintext nor the client's bytes.
//
// The fail-first assertion for at-rest encryption is in objstore
// (TestStoredObjectIsCiphertextOnDisk); this one extends it to the case where
// the object was already ciphertext when it arrived.
func TestAnE2EEChunkGetsASecondWrapOnDisk(t *testing.T) {
	dir := storeDir(t)
	s, err := New(Config{
		DataDir: dir,
		StoreID: testStoreID,
		E2EE:    true,
		CK:      testCK,
		Params:  store.DefaultParams(store.ChunkerSeed(testCK)),
	})
	if err != nil {
		t.Fatal(err)
	}

	plain := testData("second wrap", 4096)
	sealed, err := store.SealChunk(testCK, plain)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.PutChunk(sealed.ID, sealed.Frame); err != nil {
		t.Fatal(err)
	}

	// The whole pack rather than a file of the object's own, since the cutover:
	// which is the stronger reading anyway, because a leak into a pack's header
	// or index would not show up in the frame.
	seal(t)
	onDisk := packBytes(t, dir, objstore.TypeChunks, testStoreID)
	if bytes.Contains(onDisk, sealed.Frame) {
		t.Error("the client's frame appears verbatim inside what is on disk")
	}
	if bytes.Contains(onDisk, plain[:64]) {
		t.Error("plaintext is on disk")
	}

	got, err := s.GetChunk(sealed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, sealed.Frame) {
		t.Error("GetChunk did not return the client's frame unchanged")
	}

	// The size a manifest records is still the plaintext length: the storage
	// frame is below the layer that answers this, and the AEAD tag it
	// subtracts is the client's, not its own.
	size, err := s.ChunkSize(sealed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if size != int64(len(plain)) {
		t.Errorf("ChunkSize = %d, want the plaintext length %d", size, len(plain))
	}
	stored, err := s.ChunkStoredSize(sealed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored != int64(len(sealed.Frame)) {
		t.Errorf("ChunkStoredSize = %d, want the client frame's length %d", stored, len(sealed.Frame))
	}
}
