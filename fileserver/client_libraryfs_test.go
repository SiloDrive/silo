package silod

import (
	"bytes"
	"crypto/rand"
	"errors"
	"testing"

	"github.com/dkam/silo/client"
	"github.com/dkam/silo/store"
)

// One interface over both library types.
//
// The body below is written once and run twice, which is the whole assertion:
// a caller that says "list this directory, read this file, write these bytes"
// does not need to know whether the server can read what it is storing. The
// two implementations agree on everything a caller can observe here, and
// disagree only in what they had to do to answer -- one resolves a path on the
// server, the other walks an object graph it decrypts itself.
func TestOneInterfaceOverBothLibraryTypes(t *testing.T) {
	base, token, _, acct := enrolledAccount(t)
	plainID := makeLibrary(t, base, token)
	sealed, _, err := acct.CreateEncryptedLibrary("Sealed")
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name      string
		id        string
		encrypted bool
	}{
		{"plain", plainID, false},
		{"e2ee", sealed.ID, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs, err := acct.Open(tc.id)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			if _, isE2EE := fs.(*client.EncryptedLibrary); isE2EE != tc.encrypted {
				t.Fatalf("Open chose the %s implementation for a library that is encrypted=%v",
					map[bool]string{true: "E2EE", false: "plain"}[isE2EE], tc.encrypted)
			}

			notes := []byte("the quick brown fox jumps over the lazy dog")
			big := make([]byte, 512<<10)
			if _, err := rand.Read(big); err != nil {
				t.Fatal(err)
			}

			if err := fs.MkdirAll("a/b"); err != nil {
				t.Fatalf("MkdirAll(a/b): %v", err)
			}
			if err := fs.WriteFile("a/b/notes.txt", notes, 1000); err != nil {
				t.Fatalf("WriteFile(a/b/notes.txt): %v", err)
			}
			if err := fs.WriteFile("big.bin", big, 1001); err != nil {
				t.Fatalf("WriteFile(big.bin): %v", err)
			}

			fs.Refresh()
			root, err := fs.List("/")
			if err != nil {
				t.Fatalf("List(/): %v", err)
			}
			kinds := map[string]store.NodeType{}
			for _, e := range root {
				kinds[e.Name] = e.Type
			}
			if len(kinds) != 2 || kinds["a"] != store.NodeDir || kinds["big.bin"] != store.NodeFile {
				t.Errorf("List(/) = %v, want a as a directory and big.bin as a file", kinds)
			}

			if got, err := fs.ReadFile("a/b/notes.txt"); err != nil {
				t.Errorf("ReadFile: %v", err)
			} else if !bytes.Equal(got, notes) {
				t.Error("notes.txt did not come back")
			}
			if got, err := fs.ReadFile("big.bin"); err != nil {
				t.Errorf("ReadFile(big.bin): %v", err)
			} else if !bytes.Equal(got, big) {
				t.Errorf("big.bin came back as %d bytes, want %d", len(got), len(big))
			}

			for _, r := range []struct{ off, n int64 }{{0, 16}, {1000, 4096}, {int64(len(big)) - 3, 3}} {
				got, err := fs.ReadAt("big.bin", r.off, r.n)
				if err != nil {
					t.Errorf("ReadAt(%d, %d): %v", r.off, r.n, err)
					continue
				}
				if !bytes.Equal(got, big[r.off:r.off+r.n]) {
					t.Errorf("ReadAt(%d, %d) returned bytes that do not match", r.off, r.n)
				}
			}

			st, err := fs.Stat("a/b/notes.txt")
			if err != nil {
				t.Fatalf("Stat: %v", err)
			}
			if st.Name != "notes.txt" || st.Type != store.NodeFile {
				t.Errorf("Stat = %+v, want notes.txt as a file", st)
			}
			if dir, err := fs.Stat("a/b"); err != nil || dir.Type != store.NodeDir {
				t.Errorf("Stat(a/b) = %+v, %v; want a directory", dir, err)
			}

			if err := fs.Remove("a/b/notes.txt"); err != nil {
				t.Fatalf("Remove: %v", err)
			}
			fs.Refresh()
			if left, err := fs.List("a/b"); err != nil {
				t.Errorf("List(a/b) after a remove: %v", err)
			} else if len(left) != 0 {
				t.Errorf("a/b still holds %+v", left)
			}
			if _, err := fs.ReadFile("a/b/notes.txt"); !errors.Is(err, client.ErrNotFound) {
				t.Errorf("reading a removed file = %v, want ErrNotFound", err)
			}
			if _, err := fs.Stat("nothing/here"); !errors.Is(err, client.ErrNotFound) {
				t.Errorf("Stat of a path that is not there = %v, want ErrNotFound", err)
			}
		})
	}
}

// The mtime a caller asks for is honoured on an encrypted library and lost on a
// plain one: PUT entries/{path} stamps the server's clock and takes no mtime
// from the client. Pinned so the difference is a known one — silo#31.
func TestOnlyTheEncryptedWriteKeepsTheFilesOwnMtime(t *testing.T) {
	base, token, _, acct := enrolledAccount(t)
	sealed, _, err := acct.CreateEncryptedLibrary("Sealed")
	if err != nil {
		t.Fatal(err)
	}

	enc, err := acct.Open(sealed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := enc.WriteFile("old.txt", []byte("x"), 1000); err != nil {
		t.Fatal(err)
	}
	if st, err := enc.Stat("old.txt"); err != nil {
		t.Fatal(err)
	} else if st.Mtime != 1000 {
		t.Errorf("the encrypted write recorded mtime %d, want the file's own 1000", st.Mtime)
	}

	plain, err := acct.Open(makeLibrary(t, base, token))
	if err != nil {
		t.Fatal(err)
	}
	if err := plain.WriteFile("old.txt", []byte("x"), 1000); err != nil {
		t.Fatal(err)
	}
	st, err := plain.Stat("old.txt")
	if err != nil {
		t.Fatal(err)
	}
	if st.Mtime == 1000 {
		t.Error("the path surface preserved the mtime; silo#31 can be closed and this test rewritten")
	}
}

// A library the account cannot see is not a library it gets a broken handle on.
func TestOpeningALibraryThatIsNotThere(t *testing.T) {
	_, _, _, acct := enrolledAccount(t)
	if _, err := acct.Open("11111111-2222-3333-4444-555555555555"); !errors.Is(err, client.ErrNotFound) {
		t.Errorf("Open on a library that is not there = %v, want ErrNotFound", err)
	}
}
