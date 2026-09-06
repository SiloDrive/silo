package objmgr

import (
	"bytes"
	"errors"
	"sort"
	"testing"

	"github.com/dkam/silo/store"
)

// buildTree writes this shape and returns the root directory id:
//
//	/notes.txt          "the notes"
//	/photos/            dir
//	/photos/beach.jpg   3 MiB, chunked
//	/photos/raw/        dir
//	/photos/raw/a.dng   "raw one"
func buildTree(t *testing.T, s *Store) store.ID {
	t.Helper()

	put := func(name, content string) Node {
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

	mkdir := func(name string, children []Node) Node {
		t.Helper()
		salt, err := s.NewDirSalt()
		if err != nil {
			t.Fatal(err)
		}
		id, err := s.PutDir(salt, children)
		if err != nil {
			t.Fatal(err)
		}
		return Node{ID: id, Type: store.NodeDir, Name: name, Mtime: 1755950500, Mode: 0o755}
	}

	raw := mkdir("raw", []Node{put("a.dng", "raw one")})

	big, err := s.WriteFile(bytes.NewReader(testData("beach", 3<<20)))
	if err != nil {
		t.Fatal(err)
	}
	bigID, err := s.PutManifest(big)
	if err != nil {
		t.Fatal(err)
	}
	beach := Node{ID: bigID, Type: store.NodeFile, Name: "beach.jpg", Mtime: 1755950600, Mode: 0o644}

	photos := mkdir("photos", []Node{beach, raw})
	root := mkdir("", []Node{put("notes.txt", "the notes"), photos})
	return root.ID
}

func bothTypes(t *testing.T, fn func(*testing.T, *Store)) {
	t.Helper()
	for _, tc := range []struct {
		name  string
		build func(*testing.T) *Store
	}{
		{"plain", plainStore},
		{"sealed", sealedStore},
	} {
		t.Run(tc.name, func(t *testing.T) { fn(t, tc.build(t)) })
	}
}

func TestAPathResolvesToTheNodeItNames(t *testing.T) {
	bothTypes(t, func(t *testing.T, s *Store) {
		root := buildTree(t, s)

		for _, tc := range []struct {
			path string
			typ  store.NodeType
			name string
		}{
			{"", store.NodeDir, ""},
			{"/", store.NodeDir, ""},
			{"notes.txt", store.NodeFile, "notes.txt"},
			{"/notes.txt", store.NodeFile, "notes.txt"},
			{"photos", store.NodeDir, "photos"},
			{"photos/", store.NodeDir, "photos"},
			{"/photos/beach.jpg", store.NodeFile, "beach.jpg"},
			{"photos/raw", store.NodeDir, "raw"},
			{"photos/raw/a.dng", store.NodeFile, "a.dng"},
		} {
			n, err := s.Resolve(root, tc.path)
			if err != nil {
				t.Errorf("%q: %v", tc.path, err)
				continue
			}
			if n.Type != tc.typ || n.Name != tc.name {
				t.Errorf("%q resolved to %+v", tc.path, n)
			}
		}
	})
}

func TestResolveSaysWhichWayItFailed(t *testing.T) {
	bothTypes(t, func(t *testing.T, s *Store) {
		root := buildTree(t, s)

		for _, tc := range []struct {
			path string
			want error
		}{
			{"nope.txt", ErrNotFound},
			{"photos/nope.jpg", ErrNotFound},
			{"photos/raw/nope", ErrNotFound},
			{"notes.txt/deeper", ErrNotDir},
			{"photos/beach.jpg/deeper", ErrNotDir},
			{"../escape", store.ErrName},
			{"photos/../../etc", store.ErrName},
			{"./here", store.ErrName},
		} {
			if _, err := s.Resolve(root, tc.path); !errors.Is(err, tc.want) {
				t.Errorf("%q: %v, want %v", tc.path, err, tc.want)
			}
		}
	})
}

func TestListingADirectoryGivesNamesInTheClear(t *testing.T) {
	bothTypes(t, func(t *testing.T, s *Store) {
		root := buildTree(t, s)

		entries, err := s.ListPath(root, "/")
		if err != nil {
			t.Fatal(err)
		}
		names := make([]string, 0, len(entries))
		for _, n := range entries {
			names = append(names, n.Name)
		}
		sort.Strings(names)
		if len(names) != 2 || names[0] != "notes.txt" || names[1] != "photos" {
			t.Fatalf("root lists %v", names)
		}

		sub, err := s.ListPath(root, "photos")
		if err != nil {
			t.Fatal(err)
		}
		if len(sub) != 2 {
			t.Fatalf("photos lists %d entries", len(sub))
		}
		for _, n := range sub {
			switch n.Name {
			case "beach.jpg":
				if n.IsDir() {
					t.Error("beach.jpg is a directory")
				}
			case "raw":
				if !n.IsDir() {
					t.Error("raw is not a directory")
				}
			default:
				t.Errorf("unexpected entry %q", n.Name)
			}
		}
	})
}

func TestListingAFileIsRefused(t *testing.T) {
	bothTypes(t, func(t *testing.T, s *Store) {
		root := buildTree(t, s)
		if _, err := s.ListPath(root, "notes.txt"); !errors.Is(err, ErrNotDir) {
			t.Fatalf("%v, want ErrNotDir", err)
		}
		if _, err := s.OpenPath(root, "photos"); !errors.Is(err, ErrNotDir) {
			t.Fatalf("opening a directory as a file: %v", err)
		}
	})
}

func TestReadingAFileByPath(t *testing.T) {
	bothTypes(t, func(t *testing.T, s *Store) {
		root := buildTree(t, s)

		var got bytes.Buffer
		if err := s.ReadPath(root, "photos/raw/a.dng", &got); err != nil {
			t.Fatal(err)
		}
		if got.String() != "raw one" {
			t.Fatalf("read %q", got.String())
		}

		got.Reset()
		if err := s.ReadPath(root, "/photos/beach.jpg", &got); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got.Bytes(), testData("beach", 3<<20)) {
			t.Fatalf("read %d bytes of beach.jpg", got.Len())
		}
	})
}

func TestWalkVisitsEveryNodeOnce(t *testing.T) {
	bothTypes(t, func(t *testing.T, s *Store) {
		root := buildTree(t, s)

		seen := map[string]store.NodeType{}
		if err := s.Walk(root, func(p string, n Node) error {
			if _, dup := seen[p]; dup {
				t.Errorf("%q visited twice", p)
			}
			seen[p] = n.Type
			return nil
		}); err != nil {
			t.Fatal(err)
		}

		want := map[string]store.NodeType{
			"notes.txt":        store.NodeFile,
			"photos":           store.NodeDir,
			"photos/beach.jpg": store.NodeFile,
			"photos/raw":       store.NodeDir,
			"photos/raw/a.dng": store.NodeFile,
		}
		if len(seen) != len(want) {
			t.Fatalf("walked %v", seen)
		}
		for p, typ := range want {
			if seen[p] != typ {
				t.Errorf("%q walked as type %d, want %d", p, seen[p], typ)
			}
		}
	})
}

func TestWalkStopsOnTheCallbacksError(t *testing.T) {
	s := plainStore(t)
	root := buildTree(t, s)

	sentinel := errors.New("stop")
	n := 0
	if err := s.Walk(root, func(string, Node) error { n++; return sentinel }); !errors.Is(err, sentinel) {
		t.Fatalf("Walk returned %v", err)
	}
	if n != 1 {
		t.Fatalf("callback ran %d times after returning an error", n)
	}
}

// The per-directory name key is what stops the server learning that two
// directories hold a file with the same name.
func TestTheSameNameInTwoDirectoriesEncryptsDifferently(t *testing.T) {
	s := sealedStore(t)

	child := func() Node {
		m, err := s.WriteFile(bytes.NewReader([]byte("x")))
		if err != nil {
			t.Fatal(err)
		}
		id, err := s.PutManifest(m)
		if err != nil {
			t.Fatal(err)
		}
		return Node{ID: id, Type: store.NodeFile, Name: "taxes.pdf", Mode: 0o644}
	}

	var stored [][]byte
	for range 2 {
		salt, err := s.NewDirSalt()
		if err != nil {
			t.Fatal(err)
		}
		id, err := s.PutDir(salt, []Node{child()})
		if err != nil {
			t.Fatal(err)
		}
		d, err := s.GetDirectory(id)
		if err != nil {
			t.Fatal(err)
		}
		stored = append(stored, d.Entries[0].Name)

		// And it still reads back as the name that went in.
		entries, err := s.List(id)
		if err != nil {
			t.Fatal(err)
		}
		if entries[0].Name != "taxes.pdf" {
			t.Fatalf("read back %q", entries[0].Name)
		}
	}

	if bytes.Equal(stored[0], stored[1]) {
		t.Fatal("one name in two directories produced one ciphertext")
	}
	if bytes.Contains(stored[0], []byte("taxes")) {
		t.Fatal("the plaintext name is in the stored entry")
	}
}

// A plain library stores names as themselves, which is what makes the server
// able to read them — stated as a test so a change that "helpfully" encrypted
// them has to come past it.
func TestAPlainLibraryStoresNamesAsThemselves(t *testing.T) {
	s := plainStore(t)
	salt, err := s.NewDirSalt()
	if err != nil {
		t.Fatal(err)
	}
	if salt != ([store.DirSaltSize]byte{}) {
		t.Fatal("a plain library generated a directory salt")
	}
	id, err := s.PutDir(salt, []Node{{ID: store.ObjectID([]byte("x")),
		Type: store.NodeFile, Name: "notes.txt", Mode: 0o644}})
	if err != nil {
		t.Fatal(err)
	}
	d, err := s.GetDirectory(id)
	if err != nil {
		t.Fatal(err)
	}
	if string(d.Entries[0].Name) != "notes.txt" {
		t.Fatalf("stored name is %q", d.Entries[0].Name)
	}
}

// Identical trees mint identical ids, which is what changes?since= rides on.
func TestAnIdenticalTreeHasAnIdenticalRoot(t *testing.T) {
	bothTypes(t, func(t *testing.T, s *Store) {
		salt, err := s.NewDirSalt()
		if err != nil {
			t.Fatal(err)
		}
		m, err := s.WriteFile(bytes.NewReader([]byte("content")))
		if err != nil {
			t.Fatal(err)
		}
		fileID, err := s.PutManifest(m)
		if err != nil {
			t.Fatal(err)
		}
		nodes := []Node{{ID: fileID, Type: store.NodeFile, Name: "a.txt", Mtime: 7, Mode: 0o644}}

		// The same salt is the point: a rewrite carries it forward, and only
		// then does an unchanged directory keep its id.
		first, err := s.PutDir(salt, nodes)
		if err != nil {
			t.Fatal(err)
		}
		second, err := s.PutDir(salt, nodes)
		if err != nil {
			t.Fatal(err)
		}
		if first != second {
			t.Fatal("the same directory written twice produced two ids")
		}
	})
}

// The server's view of an E2EE library cannot walk the tree, because names are
// the one thing it has no way to read.
func TestTheServerViewCannotWalkAnEncryptedTree(t *testing.T) {
	dir := storeDir(t)
	client, err := New(Config{
		DataDir: dir, StoreID: testStoreID, E2EE: true, CK: testCK,
		Params: store.DefaultParams(store.ChunkerSeed(testCK)),
	})
	if err != nil {
		t.Fatal(err)
	}
	root := buildTree(t, client)

	srv := serverView(t, dir)
	if _, err := srv.List(root); !errors.Is(err, ErrNoContentKey) {
		t.Errorf("List: %v, want ErrNoContentKey", err)
	}
	if _, err := srv.Resolve(root, "notes.txt"); !errors.Is(err, ErrNoContentKey) {
		t.Errorf("Resolve: %v, want ErrNoContentKey", err)
	}
	if err := srv.Walk(root, func(string, Node) error { return nil }); !errors.Is(err, ErrNoContentKey) {
		t.Errorf("Walk: %v, want ErrNoContentKey", err)
	}
	if _, err := srv.NewDirSalt(); !errors.Is(err, ErrNoContentKey) {
		t.Errorf("NewDirSalt: %v, want ErrNoContentKey", err)
	}
}
