package objmgr

import (
	"bytes"
	"errors"
	"testing"

	"github.com/dkam/silo/store"
)

const opTime = 1756000000

// putFile writes content and returns a file node for it.
func putFile(t *testing.T, s *Store, name, content string) Node {
	t.Helper()
	m, err := s.WriteFile(bytes.NewReader([]byte(content)))
	if err != nil {
		t.Fatal(err)
	}
	id, err := s.PutManifest(m)
	if err != nil {
		t.Fatal(err)
	}
	return Node{ID: id, Type: store.NodeFile, Name: name, Mtime: 1755950400, Mode: 0o644}
}

// entryFor returns the dirent a directory holds for one child, so a test can
// assert on the mtime and mode that live in the parent rather than the child.
func entryFor(t *testing.T, s *Store, dir store.ID, name string) Node {
	t.Helper()
	nodes, err := s.List(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range nodes {
		if n.Name == name {
			return n
		}
	}
	t.Fatalf("no entry %q in %s", name, dir)
	return Node{}
}

func TestAFilePutAtAPathReadsBackFromThere(t *testing.T) {
	bothTypes(t, func(t *testing.T, s *Store) {
		root := buildTree(t, s)
		n := putFile(t, s, "b.dng", "raw two")

		newRoot, err := s.PutNode(root, "photos/raw/b.dng", n, opTime)
		if err != nil {
			t.Fatal(err)
		}

		var buf bytes.Buffer
		if err := s.ReadPath(newRoot, "photos/raw/b.dng", &buf); err != nil {
			t.Fatal(err)
		}
		if buf.String() != "raw two" {
			t.Errorf("read %q, want %q", buf.String(), "raw two")
		}
		// The old root is untouched, which is the whole point of history.
		if _, err := s.Resolve(root, "photos/raw/b.dng"); !errors.Is(err, ErrNotFound) {
			t.Errorf("old root: %v, want ErrNotFound", err)
		}
	})
}

func TestOnlyTheSpineIsRewritten(t *testing.T) {
	bothTypes(t, func(t *testing.T, s *Store) {
		root := buildTree(t, s)
		photos, err := s.Resolve(root, "photos")
		if err != nil {
			t.Fatal(err)
		}

		newRoot, err := s.PutNode(root, "notes2.txt", putFile(t, s, "notes2.txt", "more"), opTime)
		if err != nil {
			t.Fatal(err)
		}
		if newRoot == root {
			t.Fatal("the root did not change")
		}

		after, err := s.Resolve(newRoot, "photos")
		if err != nil {
			t.Fatal(err)
		}
		if after.ID != photos.ID {
			t.Errorf("photos moved from %s to %s; an untouched subtree is shared, not copied", photos.ID, after.ID)
		}
	})
}

func TestOnlyTheChangedDirectoryGetsANewMtime(t *testing.T) {
	bothTypes(t, func(t *testing.T, s *Store) {
		root := buildTree(t, s)
		photosBefore := entryFor(t, s, root, "photos")

		newRoot, err := s.PutNode(root, "photos/raw/b.dng", putFile(t, s, "b.dng", "raw two"), opTime)
		if err != nil {
			t.Fatal(err)
		}

		photosAfter := entryFor(t, s, newRoot, "photos")
		if photosAfter.Mtime != photosBefore.Mtime {
			t.Errorf("photos mtime moved to %d; only the directory whose entries changed gets a new one",
				photosAfter.Mtime)
		}
		raw, err := s.Resolve(newRoot, "photos")
		if err != nil {
			t.Fatal(err)
		}
		if got := entryFor(t, s, raw.ID, "raw").Mtime; got != opTime {
			t.Errorf("raw mtime = %d, want %d", got, opTime)
		}
	})
}

func TestAMutationThatChangesNothingReproducesTheRoot(t *testing.T) {
	bothTypes(t, func(t *testing.T, s *Store) {
		root := buildTree(t, s)
		notes, err := s.Resolve(root, "notes.txt")
		if err != nil {
			t.Fatal(err)
		}
		again, err := s.PutNode(root, "notes.txt", notes, opTime)
		if err != nil {
			t.Fatal(err)
		}
		if again != root {
			t.Errorf("root became %s; rewriting an entry to what it already was must be a no-op", again)
		}
	})
}

func TestPutRefusesToOverwriteADirectoryOrToRename(t *testing.T) {
	bothTypes(t, func(t *testing.T, s *Store) {
		root := buildTree(t, s)
		f := putFile(t, s, "photos", "not a directory")

		if _, err := s.PutNode(root, "photos", f, opTime); !errors.Is(err, ErrIsDir) {
			t.Errorf("over a directory: %v, want ErrIsDir", err)
		}
		wrong := putFile(t, s, "elsewhere.txt", "x")
		if _, err := s.PutNode(root, "here.txt", wrong, opTime); !errors.Is(err, ErrInvalidPath) {
			t.Errorf("name disagreeing with path: %v, want ErrInvalidPath", err)
		}
		if _, err := s.PutNode(root, "nope/here.txt", putFile(t, s, "here.txt", "x"), opTime); !errors.Is(err, ErrNotFound) {
			t.Errorf("missing parent: %v, want ErrNotFound", err)
		}
		if _, err := s.PutNode(root, "notes.txt/x", putFile(t, s, "x", "x"), opTime); !errors.Is(err, ErrNotDir) {
			t.Errorf("through a file: %v, want ErrNotDir", err)
		}
		if _, err := s.PutNode(root, "/", putFile(t, s, "x", "x"), opTime); !errors.Is(err, ErrInvalidPath) {
			t.Errorf("at the root: %v, want ErrInvalidPath", err)
		}
	})
}

func TestMkdirAndItsRefusals(t *testing.T) {
	bothTypes(t, func(t *testing.T, s *Store) {
		root := buildTree(t, s)

		newRoot, err := s.Mkdir(root, "photos/2026", 0o755, opTime)
		if err != nil {
			t.Fatal(err)
		}
		nodes, err := s.ListPath(newRoot, "photos/2026")
		if err != nil {
			t.Fatal(err)
		}
		if len(nodes) != 0 {
			t.Errorf("a new directory holds %d entries, want 0", len(nodes))
		}

		for _, p := range []string{"photos/2026", "photos", "notes.txt"} {
			if _, err := s.Mkdir(newRoot, p, 0o755, opTime); !errors.Is(err, ErrExists) {
				t.Errorf("mkdir %q: %v, want ErrExists", p, err)
			}
		}
	})
}

func TestMkdirAllBuildsTheMissingTailAndStopsAtAFile(t *testing.T) {
	bothTypes(t, func(t *testing.T, s *Store) {
		root := buildTree(t, s)

		newRoot, err := s.MkdirAll(root, "photos/2026/june/one", 0o755, opTime)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.ListPath(newRoot, "photos/2026/june/one"); err != nil {
			t.Fatal(err)
		}
		// The existing part of the path is not rebuilt.
		if _, err := s.Resolve(newRoot, "photos/raw/a.dng"); err != nil {
			t.Errorf("existing tree disturbed: %v", err)
		}

		again, err := s.MkdirAll(newRoot, "photos/2026/june/one", 0o755, opTime+1)
		if err != nil {
			t.Fatal(err)
		}
		if again != newRoot {
			t.Errorf("root changed on a second mkdir -p; it must be idempotent")
		}
		if _, err := s.MkdirAll(newRoot, "notes.txt/x", 0o755, opTime); !errors.Is(err, ErrNotDir) {
			t.Errorf("through a file: %v, want ErrNotDir", err)
		}
	})
}

func TestRemoveTakesTheSubtreeButOlderRootsStillHoldIt(t *testing.T) {
	bothTypes(t, func(t *testing.T, s *Store) {
		root := buildTree(t, s)

		newRoot, err := s.Remove(root, "photos", opTime)
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range []string{"photos", "photos/raw", "photos/raw/a.dng"} {
			if _, err := s.Resolve(newRoot, p); !errors.Is(err, ErrNotFound) {
				t.Errorf("resolve %q after remove: %v, want ErrNotFound", p, err)
			}
		}
		if _, err := s.Resolve(newRoot, "notes.txt"); err != nil {
			t.Errorf("sibling lost: %v", err)
		}

		// Nothing was deleted: the old root reads exactly as before.
		var buf bytes.Buffer
		if err := s.ReadPath(root, "photos/raw/a.dng", &buf); err != nil {
			t.Fatal(err)
		}
		if buf.String() != "raw one" {
			t.Errorf("old root reads %q, want %q", buf.String(), "raw one")
		}

		if _, err := s.Remove(newRoot, "photos", opTime); !errors.Is(err, ErrNotFound) {
			t.Errorf("removing what is gone: %v, want ErrNotFound", err)
		}
		if _, err := s.Remove(root, "", opTime); !errors.Is(err, ErrInvalidPath) {
			t.Errorf("removing the root: %v, want ErrInvalidPath", err)
		}
	})
}

func TestRenameKeepsIdentityAndRefusesCollisions(t *testing.T) {
	bothTypes(t, func(t *testing.T, s *Store) {
		root := buildTree(t, s)
		before, err := s.Resolve(root, "photos/raw")
		if err != nil {
			t.Fatal(err)
		}

		newRoot, err := s.Rename(root, "photos/raw", "originals", opTime)
		if err != nil {
			t.Fatal(err)
		}
		after, err := s.Resolve(newRoot, "originals")
		if err != nil {
			t.Fatal(err)
		}
		if after.ID != before.ID {
			t.Errorf("the moved directory was rebuilt: %s -> %s", before.ID, after.ID)
		}
		if after.Mtime != before.Mtime || after.Mode != before.Mode {
			t.Errorf("mtime/mode changed on a move: %d/%o -> %d/%o",
				before.Mtime, before.Mode, after.Mtime, after.Mode)
		}
		var buf bytes.Buffer
		if err := s.ReadPath(newRoot, "originals/a.dng", &buf); err != nil {
			t.Fatal(err)
		}
		if buf.String() != "raw one" {
			t.Errorf("read %q after the move", buf.String())
		}
		if _, err := s.Resolve(newRoot, "photos/raw"); !errors.Is(err, ErrNotFound) {
			t.Errorf("the source survived the move: %v", err)
		}

		if _, err := s.Rename(newRoot, "originals", "notes.txt", opTime); !errors.Is(err, ErrExists) {
			t.Errorf("onto an existing file: %v, want ErrExists", err)
		}
		if _, err := s.Rename(newRoot, "nope", "elsewhere", opTime); !errors.Is(err, ErrNotFound) {
			t.Errorf("missing source: %v, want ErrNotFound", err)
		}
		if _, err := s.Rename(newRoot, "originals", "", opTime); !errors.Is(err, ErrInvalidPath) {
			t.Errorf("onto the root: %v, want ErrInvalidPath", err)
		}
	})
}

// File onto file is the one collision Rename must allow: it destroys nothing
// the caller did not name, and moveOrCopy's own precondition check
// deliberately falls through to it — replacing the destination the same way
// PUT entries/{path} does.
func TestRenameOntoAnExistingFileReplacesIt(t *testing.T) {
	bothTypes(t, func(t *testing.T, s *Store) {
		root := buildTree(t, s)

		newRoot, err := s.Rename(root, "photos/raw/a.dng", "notes.txt", opTime)
		if err != nil {
			t.Fatalf("file onto file: %v, want it allowed", err)
		}
		var buf bytes.Buffer
		if err := s.ReadPath(newRoot, "notes.txt", &buf); err != nil {
			t.Fatal(err)
		}
		if buf.String() != "raw one" {
			t.Errorf("notes.txt reads %q after the rename, want the moved file's content", buf.String())
		}
		if _, err := s.Resolve(newRoot, "photos/raw/a.dng"); !errors.Is(err, ErrNotFound) {
			t.Errorf("the source survived a rename that succeeded: %v", err)
		}
	})
}

func TestADirectoryCannotBeMovedIntoItself(t *testing.T) {
	bothTypes(t, func(t *testing.T, s *Store) {
		root := buildTree(t, s)

		if _, err := s.Rename(root, "photos", "photos/raw/photos", opTime); !errors.Is(err, ErrInvalidPath) {
			t.Errorf("into its own subtree: %v, want ErrInvalidPath", err)
		}
		if _, err := s.Rename(root, "photos", "photos", opTime); !errors.Is(err, ErrInvalidPath) {
			t.Errorf("onto itself: %v, want ErrInvalidPath", err)
		}
		// A prefix in the string is not a prefix in the tree: photos -> photosx
		// shares five characters with the source and no segment with it.
		if _, err := s.Rename(root, "photos", "photosx", opTime); err != nil {
			t.Errorf("photos -> photosx: %v, want it allowed", err)
		}
	})
}

func TestTheServerViewCannotMutateAnEncryptedTree(t *testing.T) {
	dir := storeDir(t)
	client, err := New(Config{
		DataDir: dir, StoreID: testStoreID, E2EE: true, CK: testCK,
		Params: store.DefaultParams(store.ChunkerSeed(testCK)),
	})
	if err != nil {
		t.Fatal(err)
	}
	root := buildTree(t, client)
	n, err := client.Resolve(root, "notes.txt")
	if err != nil {
		t.Fatal(err)
	}

	srv := serverView(t, dir)
	for _, tc := range []struct {
		name string
		call func() error
	}{
		{"PutNode", func() error { _, err := srv.PutNode(root, "notes.txt", n, opTime); return err }},
		{"Mkdir", func() error { _, err := srv.Mkdir(root, "x", 0o755, opTime); return err }},
		{"MkdirAll", func() error { _, err := srv.MkdirAll(root, "x/y", 0o755, opTime); return err }},
		{"Remove", func() error { _, err := srv.Remove(root, "notes.txt", opTime); return err }},
		{"Rename", func() error { _, err := srv.Rename(root, "notes.txt", "x.txt", opTime); return err }},
		{"EmptyDir", func() error { _, err := srv.EmptyDir(); return err }},
	} {
		if err := tc.call(); !errors.Is(err, ErrNoContentKey) {
			t.Errorf("%s: %v, want ErrNoContentKey", tc.name, err)
		}
	}
}

func TestATreeBuiltByMutationMatchesOneBuiltAtOnce(t *testing.T) {
	bothTypes(t, func(t *testing.T, s *Store) {
		// The same shape, reached two ways. Ids are content, so the answer
		// cannot depend on the order the tree was assembled in — but only if
		// every writer agrees on salts, which is why a rewrite carries the
		// old one forward.
		salt, err := s.NewDirSalt()
		if err != nil {
			t.Fatal(err)
		}
		inner, err := s.PutDir(salt, []Node{putFile(t, s, "a.txt", "one")})
		if err != nil {
			t.Fatal(err)
		}
		atOnce, err := s.PutDir(salt, []Node{
			putFile(t, s, "b.txt", "two"),
			{ID: inner, Type: store.NodeDir, Name: "d", Mtime: opTime, Mode: 0o755},
		})
		if err != nil {
			t.Fatal(err)
		}

		byParts, err := s.PutDir(salt, nil)
		if err != nil {
			t.Fatal(err)
		}
		if byParts, err = s.PutNode(byParts, "b.txt", putFile(t, s, "b.txt", "two"), opTime); err != nil {
			t.Fatal(err)
		}
		if byParts, err = s.PutNode(byParts, "d", Node{
			ID: inner, Type: store.NodeDir, Name: "d", Mtime: opTime, Mode: 0o755,
		}, opTime); err != nil {
			t.Fatal(err)
		}
		if byParts != atOnce {
			t.Errorf("assembled %s, mutated %s; the same tree must have the same id", atOnce, byParts)
		}
	})
}
