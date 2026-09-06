package objmgr

import (
	"bytes"
	"testing"

	"github.com/dkam/silo/store"
)

// put writes a file at p and returns the new root.
func put(t *testing.T, s *Store, root store.ID, p string, content []byte) store.ID {
	t.Helper()
	m, err := s.WriteFile(bytes.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	id, err := s.PutManifest(m)
	if err != nil {
		t.Fatal(err)
	}
	name := p[len(p)-1:]
	_ = name
	root, err = s.PutNode(root, p, Node{ID: id, Type: store.NodeFile, Mode: 0o644}, 1)
	if err != nil {
		t.Fatalf("PutNode %s: %v", p, err)
	}
	return root
}

func ops(changes []Change) map[string]string {
	out := make(map[string]string, len(changes))
	for _, c := range changes {
		out[c.Path] = c.Op
	}
	return out
}

func TestDiffReportsWhatChanged(t *testing.T) {
	s := plainStore(t)
	root, err := s.EmptyDir()
	if err != nil {
		t.Fatal(err)
	}
	root = put(t, s, root, "/keep.txt", []byte("unchanged"))
	root = put(t, s, root, "/edit.txt", []byte("before"))
	root, err = s.Mkdir(root, "/sub", 0o755, 1)
	if err != nil {
		t.Fatal(err)
	}
	root = put(t, s, root, "/sub/gone.txt", []byte("doomed"))
	base := root

	root = put(t, s, root, "/edit.txt", []byte("after"))
	root = put(t, s, root, "/new.txt", []byte("fresh"))
	root, err = s.Remove(root, "/sub/gone.txt", 2)
	if err != nil {
		t.Fatal(err)
	}

	changes, err := s.Diff(base, root)
	if err != nil {
		t.Fatal(err)
	}
	got := ops(changes)
	for path, want := range map[string]string{
		"/edit.txt":     "modify",
		"/new.txt":      "create",
		"/sub/gone.txt": "delete",
	} {
		if got[path] != want {
			t.Errorf("%s = %q, want %q (all: %v)", path, got[path], want, got)
		}
	}
	// The unchanged file must not appear at all — that is the Merkle skip
	// doing its job rather than a filter after the fact.
	if _, ok := got["/keep.txt"]; ok {
		t.Errorf("an unchanged file was reported: %v", got)
	}
}

// A rename is one move, not a delete and a download. Content addressing makes
// that exact rather than a heuristic.
func TestDiffFoldsARenameIntoAMove(t *testing.T) {
	s := plainStore(t)
	root, err := s.EmptyDir()
	if err != nil {
		t.Fatal(err)
	}
	root = put(t, s, root, "/before.txt", []byte("some content worth not moving twice"))
	base := root

	root, err = s.Rename(root, "/before.txt", "/after.txt", 2)
	if err != nil {
		t.Fatal(err)
	}

	changes, err := s.Diff(base, root)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 1 {
		t.Fatalf("a rename produced %d changes, want 1: %v", len(changes), changes)
	}
	c := changes[0]
	if c.Op != "move" || c.Path != "/after.txt" || c.OldPath != "/before.txt" {
		t.Errorf("got %+v, want a move from /before.txt to /after.txt", c)
	}
}

// A directory that arrives with content is reported as itself and as every
// path inside it: a client needs each one to apply the change.
func TestDiffReportsASubtreeInFull(t *testing.T) {
	s := plainStore(t)
	base, err := s.EmptyDir()
	if err != nil {
		t.Fatal(err)
	}
	root, err := s.MkdirAll(base, "/a/b", 0o755, 1)
	if err != nil {
		t.Fatal(err)
	}
	root = put(t, s, root, "/a/b/deep.txt", []byte("inside"))

	changes, err := s.Diff(base, root)
	if err != nil {
		t.Fatal(err)
	}
	got := ops(changes)
	for _, p := range []string{"/a", "/a/b", "/a/b/deep.txt"} {
		if got[p] != "create" {
			t.Errorf("%s = %q, want create (all: %v)", p, got[p], got)
		}
	}
}

// The server's view of an E2EE library can still diff it: names come back as
// base64url of the ciphertext, which is exactly what entries/{path} routes on.
func TestTheServerViewCanDiffAnEncryptedLibrary(t *testing.T) {
	dir := storeDir(t)
	client, err := New(Config{
		DataDir: dir, StoreID: testStoreID, E2EE: true, CK: testCK,
		Params: store.DefaultParams(store.ChunkerSeed(testCK)),
	})
	if err != nil {
		t.Fatal(err)
	}
	base, err := client.EmptyDir()
	if err != nil {
		t.Fatal(err)
	}
	root := put(t, client, base, "/secret.txt", []byte("sealed content"))

	server := serverView(t, dir)
	changes, err := server.Diff(base, root)
	if err != nil {
		t.Fatalf("the server view could not diff an encrypted library: %v", err)
	}
	if len(changes) != 1 {
		t.Fatalf("got %d changes, want 1: %v", len(changes), changes)
	}
	if changes[0].Op != "create" {
		t.Errorf("op = %q, want create", changes[0].Op)
	}
	// The path must not leak the name, and must not be the plaintext one.
	if changes[0].Path == "/secret.txt" {
		t.Error("the server rendered a plaintext name for an encrypted library")
	}
	if changes[0].Size != int64(len("sealed content")) {
		t.Errorf("size = %d, want %d — file_size is public in both library types",
			changes[0].Size, len("sealed content"))
	}
}
