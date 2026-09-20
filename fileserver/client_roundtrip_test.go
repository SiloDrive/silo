package silod

import (
	"bytes"
	"crypto/rand"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/SiloDrive/silo/client"
	"github.com/SiloDrive/silo/store"
)

// The whole of end-to-end encryption, over HTTP, against a real server.
//
// Every other E2EE test in this package builds its objects with store/
// directly, which proves the format and proves nothing about whether a client
// can use it. This one goes the way a device goes and never touches the
// database: a password becomes an identity, the identity mints a library the
// server cannot read, a tree is written through the spine, and it comes back
// out of a second client that started with nothing but the same password.
//
// The last part is the one worth having: what the server can still see. It has
// to be able to trace the library -- garbage collection and changes?since=
// both walk it -- so the shape is public and the names are not. A test that
// only checked the round trip would pass just as well if the names were in the
// clear.
func TestTheFullE2EERoundTrip(t *testing.T) {
	base, token, writer, acct := enrolledAccount(t)

	// Create. Nothing here exists before the client makes it: the id, the
	// content key, the sealed root, the initial commit, and the one wrap that
	// is the only recoverable copy of the key.
	lib, kr, err := acct.CreateEncryptedLibrary("Round trip")
	if err != nil {
		t.Fatalf("CreateEncryptedLibrary: %v", err)
	}
	empty := lib.HeadCommitID
	fs := client.NewEncryptedLibrary(writer, lib.ID, kr)

	// Write a tree. Two levels, an inline file and a chunked one, so that both
	// manifest shapes and a directory that is not the root are exercised.
	notes := []byte("the quick brown fox jumps over the lazy dog")
	big := make([]byte, 2<<20)
	if _, err := rand.Read(big); err != nil {
		t.Fatal(err)
	}
	if err := fs.MkdirAll("photos/2026"); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := fs.WriteFile("notes.txt", notes, 1000); err != nil {
		t.Fatalf("WriteFile(notes.txt): %v", err)
	}
	if err := fs.WriteFile("photos/2026/big.bin", big, 1001); err != nil {
		t.Fatalf("WriteFile(photos/2026/big.bin): %v", err)
	}

	// Read it back through the client that wrote it.
	if got, err := fs.ReadFile("notes.txt"); err != nil || !bytes.Equal(got, notes) {
		t.Fatalf("ReadFile(notes.txt) = %d bytes, %v", len(got), err)
	}
	if got, err := fs.ReadFile("photos/2026/big.bin"); err != nil || !bytes.Equal(got, big) {
		t.Fatalf("ReadFile(big.bin) = %d bytes, %v", len(got), err)
	}

	// changes?since= reports it, from the commit the library was created at.
	// The server built this answer out of public directory sections alone.
	changed, err := writer.Changes(lib.ID, empty)
	if err != nil {
		t.Fatalf("Changes: %v", err)
	}
	byPath := map[string]client.Change{}
	for _, c := range changed.Changes {
		plain, err := fs.DecryptPath(c.Path)
		if err != nil {
			t.Fatalf("a change names %q, which does not decrypt: %v", c.Path, err)
		}
		if plain == c.Path {
			t.Errorf("the change for %s is reported in the clear", plain)
		}
		byPath[plain] = c
	}
	for _, want := range []struct {
		path  string
		isDir bool
	}{
		{"/notes.txt", false},
		{"/photos", true},
		{"/photos/2026", true},
		{"/photos/2026/big.bin", false},
	} {
		c, ok := byPath[want.path]
		if !ok {
			t.Errorf("changes does not report %s: %v", want.path, byPath)
			continue
		}
		if c.IsDir != want.isDir {
			t.Errorf("changes reports %s with is_dir=%v", want.path, c.IsDir)
		}
	}
	if changed.Anchor == empty {
		t.Error("changes did not move the anchor")
	}

	// A second client, starting from the password and nothing else.
	second := client.NewClient(base)
	sacct, err := second.OpenAccount("wire@example.com", wirePassword)
	if err != nil {
		t.Fatalf("OpenAccount on the second client: %v", err)
	}
	reader, err := sacct.Open(lib.ID)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, isE2EE := reader.(*client.EncryptedLibrary); !isE2EE {
		t.Fatal("Open did not choose the encrypted implementation")
	}
	if got, err := reader.ReadFile("photos/2026/big.bin"); err != nil {
		t.Fatalf("the second client cannot read the file: %v", err)
	} else if !bytes.Equal(got, big) {
		t.Error("the second client read different bytes")
	}
	entries, err := reader.List("/photos/2026")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 1 || entries[0].Name != "big.bin" {
		t.Errorf("List(/photos/2026) = %+v, want big.bin", entries)
	}
	// And the writer's mtime survived the trip, which is the rule that says a
	// mutation's timestamp is not the file's.
	if st, err := reader.Stat("photos/2026/big.bin"); err != nil {
		t.Fatal(err)
	} else if st.Mtime != 1001 {
		t.Errorf("mtime = %d, want the file's own 1001", st.Mtime)
	}

	// What a client with no content key gets. The account holds the wrap here,
	// so this is the raw API used without a keyring -- the same view a server
	// operator has, and the same one a member of the library would have before
	// unlocking.
	t.Run("without the key", func(t *testing.T) {
		// The path surface is refused rather than answered wrongly: the server
		// cannot resolve a name it cannot read, and a 404 would be a claim
		// about whether the file exists.
		code, body := call(t, "GET", base+"/api/silo/v1/libraries/"+lib.ID+"/entries/notes.txt", token, "")
		if code != http.StatusForbidden {
			t.Errorf("entries/{path} = %d, want 403; body %s", code, body)
		}
		if !strings.Contains(strings.ToLower(body), "end-to-end encrypted") {
			t.Errorf("the refusal does not say why: %q", strings.TrimSpace(body))
		}

		// The id surface answers, and what it answers is ciphertext with a
		// public skeleton: the server needs the shape to walk the library at
		// all, and it needs nothing else.
		st, err := reader.Stat("photos/2026/big.bin")
		if err != nil {
			t.Fatal(err)
		}
		raw, err := second.Object(lib.ID, st.ID)
		if err != nil {
			t.Fatalf("reading the manifest by id: %v", err)
		}
		pub, err := store.DecodeManifestPublic(raw)
		if err != nil {
			t.Fatalf("the manifest does not decode publicly: %v", err)
		}
		if pub.FileSize != int64(len(big)) {
			t.Errorf("public file size = %d, want %d", pub.FileSize, len(big))
		}
		if len(pub.Chunks) == 0 {
			t.Error("the public manifest names no chunks, so nothing can be collected or measured")
		}
		if _, err := store.DecodeSealedManifest(raw, make([]byte, store.CKSize)); err == nil {
			t.Error("the manifest opened under a key of zeroes")
		}

		// A directory's entries are there, and their names are not.
		dirNode, err := reader.Stat("photos/2026")
		if err != nil {
			t.Fatal(err)
		}
		rawDir, err := second.Object(lib.ID, dirNode.ID)
		if err != nil {
			t.Fatal(err)
		}
		pubDir, err := store.DecodeDirectoryPublic(rawDir)
		if err != nil {
			t.Fatalf("the directory does not decode publicly: %v", err)
		}
		if !pubDir.E2EE {
			t.Error("the directory does not declare itself sealed")
		}
		if len(pubDir.Entries) != 1 {
			t.Fatalf("the public directory holds %d entries, want 1", len(pubDir.Entries))
		}
		if string(pubDir.Entries[0].Name) == "big.bin" {
			t.Error("the entry name is on the wire in the clear")
		}
		if bytes.Contains(rawDir, []byte("big.bin")) {
			t.Error("the directory object contains the plaintext name")
		}

		// And a chunk is ciphertext all the way down.
		frames, err := second.FetchChunks(lib.ID, []store.ID{pub.Chunks[0].ID})
		if err != nil {
			t.Fatalf("chunks/fetch: %v", err)
		}
		frame, ok := frames[pub.Chunks[0].ID]
		if !ok {
			t.Fatal("chunks/fetch did not return the chunk")
		}
		if bytes.Contains(big, frame) {
			t.Error("a stored chunk is a verbatim run of the plaintext")
		}
	})
}

// A member of a library holds a wrap; anybody else holds nothing, and asking
// is not an error but an answer.
func TestALibraryWithNoWrapIsNotAKeyring(t *testing.T) {
	base, token, _, acct := enrolledAccount(t)
	if _, err := acct.OpenEncryptedLibrary(makeLibrary(t, base, token)); !errors.Is(err, store.ErrNoKeyring) {
		t.Errorf("OpenEncryptedLibrary on a plain library = %v, want ErrNoKeyring", err)
	}
}
