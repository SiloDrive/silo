package silod

import (
	"bytes"
	"crypto/rand"
	"errors"
	"io"
	"net/http"
	"testing"

	"github.com/dkam/silo/client"
	"github.com/dkam/silo/store"
)

// Reading an encrypted library, by the client, over HTTP.
//
// The tree is built here by hand and pushed through the id-addressed surface,
// because the E2EE write path is silo#9 and does not exist yet. That makes
// this the same prototype-then-move shape mintSeed had: what is written here
// is what a writer will write, and when the writer lands this helper goes.
//
// What it is proving is that the walk works from nothing but a password: head
// commit, root directory, a name encrypted under its parent's salt, a manifest,
// chunks, and the per-chunk key that no id can be turned into.

// tree is a small library: an inline file at the root, and a chunked one a
// directory down.
type tree struct {
	lib     *client.Library
	kr      *store.Keyring
	notes   []byte
	big     []byte
	bigRefs int
}

func putObject(t *testing.T, base, token, libraryID string, id store.ID, body []byte) {
	t.Helper()
	code, out := call(t, "PUT",
		base+"/api/silo/v1/libraries/"+libraryID+"/objects/"+id.String(), token, string(body))
	if code != http.StatusCreated && code != http.StatusOK && code != http.StatusNoContent {
		t.Fatalf("PUT object %s: status %d, body %s", id, code, out)
	}
}

func sealTree(t *testing.T, base, token string, acct *client.Account) tree {
	t.Helper()

	lib, kr, err := acct.CreateEncryptedLibrary("Read me")
	if err != nil {
		t.Fatalf("CreateEncryptedLibrary: %v", err)
	}
	objects := base + "/api/silo/v1/libraries/" + lib.ID + "/objects/"

	// The root's salt is the seed's, and a rewritten directory carries it
	// forward: minting a fresh one changes the root's id on every write.
	code, body := call(t, "GET", objects+lib.HeadCommitID, token, "")
	if code != http.StatusOK {
		t.Fatalf("reading the initial commit: status %d, body %s", code, body)
	}
	firstCommit, err := kr.OpenCommit([]byte(body))
	if err != nil {
		t.Fatalf("opening the initial commit: %v", err)
	}
	code, body = call(t, "GET", objects+firstCommit.Root.String(), token, "")
	if code != http.StatusOK {
		t.Fatalf("reading the initial root: status %d, body %s", code, body)
	}
	firstRoot, err := kr.OpenDirectory([]byte(body))
	if err != nil {
		t.Fatalf("opening the initial root: %v", err)
	}

	// notes.txt inlines: under the threshold there are no chunks at all.
	notes := []byte("the quick brown fox jumps over the lazy dog")
	notesManifest := &store.Manifest{FileSize: int64(len(notes)), Inline: notes}
	notesBytes, err := kr.SealManifest(notesManifest)
	if err != nil {
		t.Fatalf("sealing the inline manifest: %v", err)
	}
	putObject(t, base, token, lib.ID, store.ObjectID(notesBytes), notesBytes)

	// big.bin does not: three megabytes cut under this library's own seed,
	// which is several chunks, which is what makes the offset arithmetic in
	// ReadAt worth testing.
	big := make([]byte, 3<<20)
	if _, err := rand.Read(big); err != nil {
		t.Fatal(err)
	}
	chunker, err := store.NewChunker(kr.Params(), bytes.NewReader(big))
	if err != nil {
		t.Fatal(err)
	}
	var refs []store.ChunkRef
	for {
		c, err := chunker.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("chunking: %v", err)
		}
		sealed, err := kr.SealChunk(c.Data)
		if err != nil {
			t.Fatalf("sealing a chunk: %v", err)
		}
		code, out := call(t, "PUT",
			base+"/api/silo/v1/libraries/"+lib.ID+"/chunks/"+sealed.ID.String(), token, string(sealed.Frame))
		if code != http.StatusCreated && code != http.StatusOK && code != http.StatusNoContent {
			t.Fatalf("PUT chunk %s: status %d, body %s", sealed.ID, code, out)
		}
		refs = append(refs, store.ChunkRef{
			ID: sealed.ID, Size: int64(len(c.Data)), PlaintextHash: sealed.PlaintextHash,
		})
	}
	if len(refs) < 2 {
		t.Fatalf("three megabytes cut into %d chunks; the offset arithmetic needs more than one", len(refs))
	}
	bigManifest := &store.Manifest{FileSize: int64(len(big)), Chunks: refs}
	bigBytes, err := kr.SealManifest(bigManifest)
	if err != nil {
		t.Fatalf("sealing the chunked manifest: %v", err)
	}
	putObject(t, base, token, lib.ID, store.ObjectID(bigBytes), bigBytes)

	// photos/ is a new directory, so it gets a salt of its own.
	var photoSalt [store.DirSaltSize]byte
	if _, err := rand.Read(photoSalt[:]); err != nil {
		t.Fatal(err)
	}
	photoNames, err := kr.NameCipher(photoSalt)
	if err != nil {
		t.Fatal(err)
	}
	bigName, err := photoNames.Encrypt("big.bin")
	if err != nil {
		t.Fatal(err)
	}
	photos := &store.Directory{Salt: photoSalt, Entries: []store.DirEntry{{
		ChildID: store.ObjectID(bigBytes), Type: store.NodeFile,
		Name: bigName, Mtime: 1756339200, Mode: 0o644,
	}}}
	photosBytes, err := kr.SealDirectory(photos)
	if err != nil {
		t.Fatalf("sealing photos/: %v", err)
	}
	putObject(t, base, token, lib.ID, store.ObjectID(photosBytes), photosBytes)

	rootNames, err := kr.NameCipher(firstRoot.Salt)
	if err != nil {
		t.Fatal(err)
	}
	notesName, err := rootNames.Encrypt("notes.txt")
	if err != nil {
		t.Fatal(err)
	}
	photosName, err := rootNames.Encrypt("photos")
	if err != nil {
		t.Fatal(err)
	}
	root := &store.Directory{Salt: firstRoot.Salt, Entries: []store.DirEntry{
		{ChildID: store.ObjectID(notesBytes), Type: store.NodeFile, Name: notesName, Mtime: 1756339200, Mode: 0o644},
		{ChildID: store.ObjectID(photosBytes), Type: store.NodeDir, Name: photosName, Mtime: 1756339201, Mode: 0o755},
	}}
	rootBytes, err := kr.SealDirectory(root)
	if err != nil {
		t.Fatalf("sealing the root: %v", err)
	}
	putObject(t, base, token, lib.ID, store.ObjectID(rootBytes), rootBytes)

	head, err := store.ParseID(lib.HeadCommitID)
	if err != nil {
		t.Fatal(err)
	}
	commit := &store.Commit{Root: store.ObjectID(rootBytes), Parents: []store.ID{head}, CreatedAt: 1756339202}
	commitBytes, err := kr.SealCommit(commit)
	if err != nil {
		t.Fatalf("sealing the commit: %v", err)
	}
	putObject(t, base, token, lib.ID, store.ObjectID(commitBytes), commitBytes)

	newHead := store.ObjectID(commitBytes)
	req, err := http.NewRequest("PUT",
		base+"/api/silo/v1/libraries/"+lib.ID+"/head", bytes.NewReader([]byte(newHead.String())))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("If-Match", `"`+lib.HeadCommitID+`"`)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT head: %v", err)
	}
	out, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		t.Fatalf("PUT head: status %d, body %s", resp.StatusCode, out)
	}

	return tree{lib: lib, notes: notes, big: big}
}

func TestAClientReadsAnEncryptedLibrary(t *testing.T) {
	base, token, _ := enrolled(t)
	c := client.NewClient(base)
	acct, err := c.OpenAccount("wire@example.com", wirePassword)
	if err != nil {
		t.Fatalf("OpenAccount: %v", err)
	}
	tr := sealTree(t, base, token, acct)

	// A second client, holding only the password: everything below comes from
	// the wire and the key it derived, and nothing from the one that wrote.
	reader := client.NewClient(base)
	racct, err := reader.OpenAccount("wire@example.com", wirePassword)
	if err != nil {
		t.Fatalf("OpenAccount on the reading client: %v", err)
	}
	lib, err := racct.OpenEncryptedLibrary(tr.lib.ID)
	if err != nil {
		t.Fatalf("OpenEncryptedLibrary: %v", err)
	}

	entries, err := lib.List("/")
	if err != nil {
		t.Fatalf("List(/): %v", err)
	}
	got := map[string]store.NodeType{}
	for _, e := range entries {
		got[e.Name] = e.Type
	}
	if len(got) != 2 || got["notes.txt"] != store.NodeFile || got["photos"] != store.NodeDir {
		t.Errorf("List(/) = %v, want notes.txt as a file and photos as a directory", got)
	}

	// The inline file: no chunks, so the bytes come out of the manifest.
	if b, err := lib.ReadFile("notes.txt"); err != nil {
		t.Errorf("ReadFile(notes.txt): %v", err)
	} else if !bytes.Equal(b, tr.notes) {
		t.Error("notes.txt did not come back")
	}

	// The chunked one, a directory down: a name encrypted under photos/'s own
	// salt, which cannot be computed until photos/ has been read.
	b, err := lib.ReadFile("photos/big.bin")
	if err != nil {
		t.Fatalf("ReadFile(photos/big.bin): %v", err)
	}
	if !bytes.Equal(b, tr.big) {
		t.Fatalf("big.bin came back as %d bytes, want %d", len(b), len(tr.big))
	}

	// A ranged read is arithmetic over the manifest, and the range that
	// matters is the one straddling a boundary: a reader that fetched whole
	// chunks and forgot to trim would pass every other case.
	for _, r := range []struct{ off, n int64 }{
		{0, 10},
		{1 << 20, 3},
		{int64(len(tr.big)) - 5, 5},
		{(1 << 19) - 3, 1 << 20},
	} {
		got, err := lib.ReadAt("photos/big.bin", r.off, r.n)
		if err != nil {
			t.Errorf("ReadAt(%d, %d): %v", r.off, r.n, err)
			continue
		}
		if !bytes.Equal(got, tr.big[r.off:r.off+r.n]) {
			t.Errorf("ReadAt(%d, %d) returned %d bytes that do not match", r.off, r.n, len(got))
		}
	}

	// Stat says what an entry is without reading it.
	st, err := lib.Stat("photos/big.bin")
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if st.Type != store.NodeFile || st.Name != "big.bin" || st.Mode != 0o644 || st.Mtime != 1756339200 {
		t.Errorf("Stat = %+v, want the entry as it was written", st)
	}

	if _, err := lib.ReadFile("photos/nothing.bin"); !errors.Is(err, client.ErrNotFound) {
		t.Errorf("reading a path that is not there = %v, want ErrNotFound", err)
	}
	if _, err := lib.List("notes.txt"); err == nil {
		t.Error("listing a file succeeded")
	}
}

// The server still refuses the path surface on an encrypted library, so a
// client that reads one is reading the object graph. This pins that the read
// path above is not quietly going through entries/{path}.
func TestTheReadPathDoesNotUseTheEntriesSurface(t *testing.T) {
	base, token, _ := enrolled(t)
	c := client.NewClient(base)
	acct, err := c.OpenAccount("wire@example.com", wirePassword)
	if err != nil {
		t.Fatalf("OpenAccount: %v", err)
	}
	tr := sealTree(t, base, token, acct)

	code, body := call(t, "GET", base+"/api/silo/v1/libraries/"+tr.lib.ID+"/entries/notes.txt", token, "")
	if code != http.StatusForbidden {
		t.Fatalf("entries/{path} on an encrypted library: status %d, body %s", code, body)
	}

	lib, err := acct.OpenEncryptedLibrary(tr.lib.ID)
	if err != nil {
		t.Fatal(err)
	}
	if b, err := lib.ReadFile("notes.txt"); err != nil {
		t.Errorf("the object graph read failed where the path read is refused: %v", err)
	} else if !bytes.Equal(b, tr.notes) {
		t.Error("notes.txt did not come back")
	}
}
