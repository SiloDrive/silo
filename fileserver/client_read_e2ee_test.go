package silod

import (
	"bytes"
	"crypto/rand"
	"errors"
	"net/http"
	"testing"

	"github.com/dkam/silo/client"
	"github.com/dkam/silo/store"
)

// Reading an encrypted library, by the client, over HTTP.
//
// What it proves is that the walk works from nothing but a password: head
// commit, root directory, a name encrypted under its parent's salt, a
// manifest, chunks, and the per-chunk key that no id can be turned into.
//
// The tree is built by the write path. It was built by hand here until that
// landed, which was the right prototype and the wrong thing to keep: a reader
// tested against bytes a test assembled is a reader tested against one guess
// at the format.

// tree is a small library: an inline file at the root, and a chunked one a
// directory down.
type tree struct {
	lib   *client.Library
	notes []byte
	big   []byte
}

func sealTree(t *testing.T, c *client.APIClient, acct *client.Account) tree {
	t.Helper()

	lib, kr, err := acct.CreateEncryptedLibrary("Read me")
	if err != nil {
		t.Fatalf("CreateEncryptedLibrary: %v", err)
	}
	w := client.NewEncryptedLibrary(c, lib.ID, kr)

	// notes.txt inlines: under the threshold there are no chunks at all.
	notes := []byte("the quick brown fox jumps over the lazy dog")
	if err := w.WriteFile("notes.txt", notes, 1756339200); err != nil {
		t.Fatalf("writing notes.txt: %v", err)
	}

	// big.bin does not: three megabytes cut under this library's own seed is
	// several chunks, which is what makes ReadAt's offset arithmetic worth
	// testing.
	big := make([]byte, 3<<20)
	if _, err := rand.Read(big); err != nil {
		t.Fatal(err)
	}
	if err := w.MkdirAll("photos"); err != nil {
		t.Fatalf("MkdirAll(photos): %v", err)
	}
	if err := w.WriteFile("photos/big.bin", big, 1756339200); err != nil {
		t.Fatalf("writing photos/big.bin: %v", err)
	}

	return tree{lib: lib, notes: notes, big: big}
}

func TestAClientReadsAnEncryptedLibrary(t *testing.T) {
	base, _, c, acct := enrolledAccount(t)
	tr := sealTree(t, c, acct)

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
	m, err := lib.Manifest("photos/big.bin")
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Chunks) < 2 {
		t.Fatalf("big.bin is %d chunks; the ranged read below needs more than one", len(m.Chunks))
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
	base, token, c, acct := enrolledAccount(t)
	tr := sealTree(t, c, acct)

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
