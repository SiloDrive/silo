package objmgr

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"testing"

	"github.com/dkam/silo/fileserver/objstore"
	"github.com/dkam/silo/store"
)

const testStoreID = "b1f2ad61-9164-418a-a47f-ab805dbd5694"

var testCK = []byte("a thirty-two byte library key!!!")

// deterministic bytes, so a failure is the same failure twice.
func testData(label string, n int) []byte {
	out := make([]byte, 0, n+sha256.Size)
	var counter [8]byte
	for i := 0; len(out) < n; i++ {
		binary.LittleEndian.PutUint64(counter[:], uint64(i))
		sum := sha256.Sum256(append([]byte(label), counter[:]...))
		out = append(out, sum[:]...)
	}
	return out[:n]
}

// Every store here seals its packs on the way out, before the temp directory
// under it goes. objstore.Close is process-wide -- the pack registry is -- so a
// test that left an open pack behind would have the next test's Close trip over
// a directory that is no longer there.
func plainStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	t.Cleanup(func() { _ = objstore.Close() })
	s, err := New(Config{
		DataDir: dir,
		StoreID: testStoreID,
		Params:  store.DefaultParams(store.PlainSeed()),
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func sealedStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	t.Cleanup(func() { _ = objstore.Close() })
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
	return s
}

// serverView is what a server holds for an E2EE library: the same directory,
// the same catalog parameters, and no key.
func serverView(t *testing.T, dataDir string) *Store {
	t.Helper()
	s, err := New(Config{
		DataDir: dataDir,
		StoreID: testStoreID,
		E2EE:    true,
		Params:  store.DefaultParams(store.ChunkerSeed(testCK)),
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestAFileRoundTripsInBothLibraryTypes(t *testing.T) {
	for _, tc := range []struct {
		name  string
		build func(*testing.T) *Store
	}{
		{"plain", plainStore},
		{"sealed", sealedStore},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := tc.build(t)
			data := testData("roundtrip", 3<<20)

			m, err := s.WriteFile(bytes.NewReader(data))
			if err != nil {
				t.Fatal(err)
			}
			if m.FileSize != int64(len(data)) {
				t.Fatalf("manifest says %d bytes, wrote %d", m.FileSize, len(data))
			}
			if len(m.Chunks) < 2 {
				t.Fatalf("a 3 MiB file produced %d chunks", len(m.Chunks))
			}

			id, err := s.PutManifest(m)
			if err != nil {
				t.Fatal(err)
			}
			back, err := s.GetManifest(id)
			if err != nil {
				t.Fatal(err)
			}

			var got bytes.Buffer
			if err := s.ReadFile(back, &got); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got.Bytes(), data) {
				t.Fatalf("read back %d bytes, wrote %d", got.Len(), len(data))
			}
		})
	}
}

// Inlining is decided by size, never by the caller. Two writers disagreeing
// about a 30 KB file would mint two manifest ids for identical content.
func TestASmallFileInlinesAndStoresNoChunks(t *testing.T) {
	for _, tc := range []struct {
		name  string
		build func(*testing.T) *Store
	}{
		{"plain", plainStore},
		{"sealed", sealedStore},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := tc.build(t)
			data := testData("inline", store.InlineThreshold-1)

			m, err := s.WriteFile(bytes.NewReader(data))
			if err != nil {
				t.Fatal(err)
			}
			if len(m.Chunks) != 0 {
				t.Fatalf("an inline file listed %d chunks", len(m.Chunks))
			}
			if !bytes.Equal(m.Inline, data) {
				t.Fatal("the inline bytes are not the file")
			}

			id, err := s.PutManifest(m)
			if err != nil {
				t.Fatal(err)
			}
			back, err := s.GetManifest(id)
			if err != nil {
				t.Fatal(err)
			}
			var got bytes.Buffer
			if err := s.ReadFile(back, &got); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got.Bytes(), data) {
				t.Fatal("an inline file did not round trip")
			}
		})
	}
}

// A file exactly at the threshold is chunked, one byte under it is inlined.
func TestTheInlineBoundaryIsWhereTheFormatSaysItIs(t *testing.T) {
	s := plainStore(t)
	for _, tc := range []struct {
		size   int
		inline bool
	}{
		{store.InlineThreshold - 1, true},
		{store.InlineThreshold, false},
	} {
		m, err := s.WriteFile(bytes.NewReader(testData("boundary", tc.size)))
		if err != nil {
			t.Fatalf("%d bytes: %v", tc.size, err)
		}
		if got := len(m.Chunks) == 0; got != tc.inline {
			t.Errorf("%d bytes: inline=%v, want %v", tc.size, got, tc.inline)
		}
		if m.FileSize != int64(tc.size) {
			t.Errorf("%d bytes: manifest says %d", tc.size, m.FileSize)
		}
	}
}

// In an E2EE library a chunk id names ciphertext. If it named plaintext the
// server would hold a content-confirmation oracle for every chunk it stores.
func TestSealedChunkIDsNameCiphertext(t *testing.T) {
	s := sealedStore(t)
	data := testData("ciphertext", 3<<20)

	m, err := s.WriteFile(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}

	plain, err := New(Config{
		DataDir: t.TempDir(),
		StoreID: testStoreID,
		Params:  store.DefaultParams(store.PlainSeed()),
	})
	if err != nil {
		t.Fatal(err)
	}
	plainManifest, err := plain.WriteFile(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}

	// Different seeds cut in different places, so even the chunk count is
	// not expected to match — what must hold is that no id appears in both.
	sealedIDs := map[store.ID]bool{}
	for _, c := range m.Chunks {
		sealedIDs[c.ID] = true
	}
	for _, c := range plainManifest.Chunks {
		if sealedIDs[c.ID] {
			t.Fatal("a chunk id is shared between a sealed and a plain library")
		}
	}

	// And the stored bytes are not the plaintext.
	stored, err := s.GetChunk(m.Chunks[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, stored) {
		t.Fatal("a stored chunk is a verbatim slice of the plaintext")
	}
	if int64(len(stored)) != m.Chunks[0].Size+16 {
		t.Fatalf("stored chunk is %d bytes, plaintext %d — expected a 16-byte tag",
			len(stored), m.Chunks[0].Size)
	}
}

// The server's permanent state for an E2EE library: it stores and serves bytes,
// verifies ids, and reads the public chunk list. Everything else says so.
func TestTheServerViewCanDoItsJobAndNoMore(t *testing.T) {
	dir := t.TempDir()
	client, err := New(Config{
		DataDir: dir,
		StoreID: testStoreID,
		E2EE:    true,
		CK:      testCK,
		Params:  store.DefaultParams(store.ChunkerSeed(testCK)),
	})
	if err != nil {
		t.Fatal(err)
	}

	data := testData("serverview", 3<<20)
	m, err := client.WriteFile(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	id, err := client.PutManifest(m)
	if err != nil {
		t.Fatal(err)
	}

	srv := serverView(t, dir)
	if srv.HasKey() {
		t.Fatal("the server view reports holding a key")
	}

	// What it can do: enumerate the chunks, which is what GC needs.
	pub, err := srv.GetManifestPublic(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(pub.Chunks) != len(m.Chunks) {
		t.Fatalf("public reader saw %d chunks, manifest has %d", len(pub.Chunks), len(m.Chunks))
	}
	for i, c := range pub.Chunks {
		if c.ID != m.Chunks[i].ID {
			t.Fatalf("chunk %d differs", i)
		}
		if ok, err := srv.HasChunk(c.ID); err != nil || !ok {
			t.Fatalf("chunk %d is not present to the server: %v", i, err)
		}
	}

	// What it cannot: anything needing the key, and it says which.
	if _, err := srv.GetManifest(id); !errors.Is(err, ErrNoContentKey) {
		t.Errorf("GetManifest: %v, want ErrNoContentKey", err)
	}
	if _, err := srv.WriteFile(bytes.NewReader(data)); !errors.Is(err, ErrNoContentKey) {
		t.Errorf("WriteFile: %v, want ErrNoContentKey", err)
	}
	if err := srv.ReadFile(m, &bytes.Buffer{}); !errors.Is(err, ErrNoContentKey) {
		t.Errorf("ReadFile: %v, want ErrNoContentKey", err)
	}
	if _, err := srv.PutManifest(m); !errors.Is(err, ErrNoContentKey) {
		t.Errorf("PutManifest: %v, want ErrNoContentKey", err)
	}
	if _, err := srv.GetDirectory(id); !errors.Is(err, ErrNoContentKey) {
		t.Errorf("GetDirectory: %v, want ErrNoContentKey", err)
	}
	if _, err := srv.GetCommit(id); !errors.Is(err, ErrNoContentKey) {
		t.Errorf("GetCommit: %v, want ErrNoContentKey", err)
	}

	// And it can still take delivery of opaque bytes under a verified id,
	// which is how a client uploads into a library the server cannot read.
	encoded, err := srv.GetObject(id)
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.PutObject(id, encoded); err != nil {
		t.Fatalf("re-storing an object under its own id: %v", err)
	}
}

// An id that names something else is the one corruption a content-addressed
// store can catch by itself, so it does.
func TestBytesThatDoNotMatchTheirIDAreRefused(t *testing.T) {
	s := plainStore(t)
	data := testData("mismatch", 4096)
	wrong := store.ChunkID(testData("something else", 4096))

	if err := s.PutChunk(wrong, data); !errors.Is(err, ErrIDMismatch) {
		t.Fatalf("PutChunk: %v, want ErrIDMismatch", err)
	}
	if err := s.PutObject(wrong, data); !errors.Is(err, ErrIDMismatch) {
		t.Fatalf("PutObject: %v, want ErrIDMismatch", err)
	}
	if ok, err := s.HasChunk(wrong); err != nil || ok {
		t.Fatalf("the refused chunk is present: %v %v", ok, err)
	}
}

// A plain library has no tag to catch a substituted chunk, so the id is the
// check — and the bytes came off a disk the threat model does not trust.
func TestASubstitutedChunkIsCaughtInAPlainLibrary(t *testing.T) {
	s := plainStore(t)
	data := testData("substitute", 3<<20)
	m, err := s.WriteFile(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}

	// Overwrite one chunk's bytes on disk, the way a hostile server would.
	victim := m.Chunks[0].ID
	other := testData("replacement", int(m.Chunks[0].Size))
	if err := s.chunks.Write(s.storeID, victim.String(), bytes.NewReader(other), true); err != nil {
		t.Fatal(err)
	}

	if err := s.ReadFile(m, &bytes.Buffer{}); !errors.Is(err, ErrIDMismatch) {
		t.Fatalf("a substituted chunk was served: %v", err)
	}
}

// The same, under E2EE, where it is the AEAD tag that refuses.
func TestASubstitutedChunkIsCaughtInASealedLibrary(t *testing.T) {
	s := sealedStore(t)
	data := testData("substitute-sealed", 3<<20)
	m, err := s.WriteFile(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}

	victim := m.Chunks[0].ID
	stored, err := s.GetChunk(victim)
	if err != nil {
		t.Fatal(err)
	}
	tampered := bytes.Clone(stored)
	tampered[0] ^= 0x01
	if err := s.chunks.Write(s.storeID, victim.String(), bytes.NewReader(tampered), true); err != nil {
		t.Fatal(err)
	}

	if err := s.ReadFile(m, &bytes.Buffer{}); !errors.Is(err, store.ErrDecrypt) {
		t.Fatalf("a tampered chunk was served: %v", err)
	}
}

func TestDirectoriesAndCommitsRoundTripInBothLibraryTypes(t *testing.T) {
	for _, tc := range []struct {
		name  string
		build func(*testing.T) *Store
	}{
		{"plain", plainStore},
		{"sealed", sealedStore},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := tc.build(t)

			d := &store.Directory{Entries: []store.DirEntry{
				{ChildID: store.ObjectID([]byte("a")), Type: store.NodeFile,
					Name: []byte("notes.txt"), Mtime: 1755950400, Mode: 0o644},
				{ChildID: store.ObjectID([]byte("b")), Type: store.NodeDir,
					Name: []byte("photos"), Mtime: 1755950500, Mode: 0o755},
			}}
			dirID, err := s.PutDirectory(d)
			if err != nil {
				t.Fatal(err)
			}
			backDir, err := s.GetDirectory(dirID)
			if err != nil {
				t.Fatal(err)
			}
			if len(backDir.Entries) != 2 {
				t.Fatalf("directory came back with %d entries", len(backDir.Entries))
			}

			c := &store.Commit{
				Root:      dirID,
				CreatedAt: 1755950600,
				Author:    "d@nmilne.com",
				Message:   "first",
			}
			commitID, err := s.PutCommit(c)
			if err != nil {
				t.Fatal(err)
			}
			backCommit, err := s.GetCommit(commitID)
			if err != nil {
				t.Fatal(err)
			}
			if backCommit.Root != dirID || backCommit.Message != "first" {
				t.Fatalf("commit came back as %+v", backCommit)
			}
		})
	}
}

// Two library types, two encodings, and neither reads as the other. The
// library type is an input from the catalog, never something read out of the
// object — a server flipping the flag must not select a different parse.
func TestAnObjectFromOneLibraryTypeDoesNotReadAsTheOther(t *testing.T) {
	dir := t.TempDir()
	plain, err := New(Config{
		DataDir: dir, StoreID: testStoreID,
		Params: store.DefaultParams(store.PlainSeed()),
	})
	if err != nil {
		t.Fatal(err)
	}
	m, err := plain.WriteFile(bytes.NewReader(testData("crosstype", 3<<20)))
	if err != nil {
		t.Fatal(err)
	}
	id, err := plain.PutManifest(m)
	if err != nil {
		t.Fatal(err)
	}

	sealed, err := New(Config{
		DataDir: dir, StoreID: testStoreID, E2EE: true, CK: testCK,
		Params: store.DefaultParams(store.ChunkerSeed(testCK)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sealed.GetManifest(id); err == nil {
		t.Fatal("a plain manifest opened as a sealed one")
	}
}

// The seed check in New. An E2EE library chunking under the plain seed would
// make seal_hash — computed over plaintext hashes — reproducible without the
// key, which is a content-confirmation oracle rather than a performance bug.
func TestAMisconfiguredStoreIsRefused(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct {
		what string
		cfg  Config
	}{
		{"a plain library holding a content key", Config{
			DataDir: dir, StoreID: testStoreID, CK: testCK,
			Params: store.DefaultParams(store.PlainSeed())}},
		{"an E2EE library chunking under the plain seed", Config{
			DataDir: dir, StoreID: testStoreID, E2EE: true, CK: testCK,
			Params: store.DefaultParams(store.PlainSeed())}},
		{"a plain library chunking under a keyed seed", Config{
			DataDir: dir, StoreID: testStoreID,
			Params: store.DefaultParams(store.ChunkerSeed(testCK))}},
		{"a seed from the wrong content key", Config{
			DataDir: dir, StoreID: testStoreID, E2EE: true, CK: testCK,
			Params: store.DefaultParams(store.ChunkerSeed([]byte("a different library key!!!!!!!!!")))}},
		{"no store id", Config{
			DataDir: dir, Params: store.DefaultParams(store.PlainSeed())}},
		{"parameters no chunker can be built from", Config{
			DataDir: dir, StoreID: testStoreID, Params: store.Params{}}},
	} {
		if _, err := New(tc.cfg); err == nil {
			t.Errorf("%s: accepted", tc.what)
		}
	}
}

// An empty file is inline, has no chunks, and round trips.
func TestAnEmptyFileIsAManifestWithNothingInIt(t *testing.T) {
	for _, tc := range []struct {
		name  string
		build func(*testing.T) *Store
	}{
		{"plain", plainStore},
		{"sealed", sealedStore},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := tc.build(t)
			m, err := s.WriteFile(bytes.NewReader(nil))
			if err != nil {
				t.Fatal(err)
			}
			if m.FileSize != 0 || len(m.Chunks) != 0 {
				t.Fatalf("empty file gave %+v", m)
			}
			id, err := s.PutManifest(m)
			if err != nil {
				t.Fatal(err)
			}
			back, err := s.GetManifest(id)
			if err != nil {
				t.Fatal(err)
			}
			var got bytes.Buffer
			if err := s.ReadFile(back, &got); err != nil {
				t.Fatal(err)
			}
			if got.Len() != 0 {
				t.Fatalf("read %d bytes from an empty file", got.Len())
			}
		})
	}
}

// Identical content written twice is one set of chunks and one manifest id.
// Dedup and changes?since= both ride on this.
func TestIdenticalContentConvergesOnOneManifest(t *testing.T) {
	for _, tc := range []struct {
		name  string
		build func(*testing.T) *Store
	}{
		{"plain", plainStore},
		{"sealed", sealedStore},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := tc.build(t)
			data := testData("converge", 3<<20)

			first, err := s.WriteFile(bytes.NewReader(data))
			if err != nil {
				t.Fatal(err)
			}
			second, err := s.WriteFile(bytes.NewReader(data))
			if err != nil {
				t.Fatal(err)
			}

			idA, err := s.PutManifest(first)
			if err != nil {
				t.Fatal(err)
			}
			idB, err := s.PutManifest(second)
			if err != nil {
				t.Fatal(err)
			}
			if idA != idB {
				t.Fatal("the same file written twice produced two manifest ids")
			}
			if len(first.Chunks) != len(second.Chunks) {
				t.Fatal("the same file chunked two different ways")
			}
			for i := range first.Chunks {
				if first.Chunks[i].ID != second.Chunks[i].ID {
					t.Fatalf("chunk %d differs between two writes of one file", i)
				}
			}
		})
	}
}

func TestTheServerViewReadsEdgesAndRootsWithoutTheKey(t *testing.T) {
	dir := t.TempDir()
	client, err := New(Config{
		DataDir: dir, StoreID: testStoreID, E2EE: true, CK: testCK,
		Params: store.DefaultParams(store.ChunkerSeed(testCK)),
	})
	if err != nil {
		t.Fatal(err)
	}
	root := buildTree(t, client)
	head, err := client.PutCommit(&store.Commit{
		Root: root, CreatedAt: 1755950400, Author: "a@b.c", Message: "the first commit",
	})
	if err != nil {
		t.Fatal(err)
	}

	srv := serverView(t, dir)

	// Where every server-side walk starts, and the one thing the head commit
	// is still for.
	pc, err := srv.GetCommitPublic(head)
	if err != nil {
		t.Fatal(err)
	}
	if pc.Root != root {
		t.Errorf("root %s, want %s", pc.Root, root)
	}
	if !pc.E2EE {
		t.Error("the commit did not declare itself sealed")
	}

	// And one step further, which is what the collector needs and what List
	// refuses to give it.
	pd, err := srv.GetDirectoryPublic(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(pd.Entries) != 2 {
		t.Fatalf("%d edges out of the root, want 2", len(pd.Entries))
	}
	for _, e := range pd.Entries {
		if e.Mtime != 0 || e.Mode != 0 {
			t.Errorf("a keyless read gave up mtime %d mode %#o", e.Mtime, e.Mode)
		}
	}
	if _, err := srv.List(root); !errors.Is(err, ErrNoContentKey) {
		t.Errorf("List: %v, want ErrNoContentKey — the names are still shut", err)
	}
}

func TestAnEncryptedLibraryCannotBeOpenedOnThePlainSeed(t *testing.T) {
	_, err := New(Config{
		DataDir: t.TempDir(), StoreID: testStoreID, E2EE: true,
		Params: store.DefaultParams(store.PlainSeed()),
	})
	if err == nil {
		t.Fatal("a server view of an E2EE library accepted the published seed")
	}
}
