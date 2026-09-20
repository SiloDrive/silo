package silod

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/SiloDrive/silo/client"
)

func listing(t *testing.T, c *client.APIClient, libraryID, path string) []client.DirEntry {
	t.Helper()
	entries, err := c.ListDir(libraryID, path)
	if err != nil {
		t.Fatalf("list %s: %v", path, err)
	}
	return entries
}

func byName(entries []client.DirEntry, name string) *client.DirEntry {
	for i := range entries {
		if entries[i].Name == name {
			return &entries[i]
		}
	}
	return nil
}

// A listing reports the size of every file in it.
//
// The size is not in the directory object — a dirent carries a name, a type
// and a child id and nothing else — so this is the sidecar working: the server
// reads what the manifests declare, records it, and answers from the record
// afterwards.
func TestAListingSaysHowBigEachFileIs(t *testing.T) {
	c, libraryID, _ := laneClient(t)

	dir := t.TempDir()
	// One of each: a file big enough to take the chunk lane, one small enough
	// to be inlined in its manifest, and one with no bytes at all.
	written := map[string]int{"big.bin": 12 << 20, "small.bin": 5, "empty.bin": 0}
	for name, size := range written {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, make([]byte, size), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := c.UploadFile(libraryID, "/", path); err != nil {
			t.Fatalf("upload %s: %v", name, err)
		}
	}
	if err := c.Mkdir(libraryID, "/sub"); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	entries := listing(t, c, libraryID, "/")
	for name, want := range written {
		e := byName(entries, name)
		if e == nil {
			t.Errorf("%s is missing from the listing", name)
			continue
		}
		if e.Size == nil {
			t.Errorf("%s has no size; the listing did not look one up", name)
			continue
		}
		if *e.Size != int64(want) {
			t.Errorf("%s is %d bytes, the listing says %d", name, want, *e.Size)
		}
	}

	// A directory has no file_size, so it must carry no size rather than a
	// zero — what is under it is a different question with a different answer.
	if sub := byName(entries, "sub"); sub == nil {
		t.Error("the directory is missing from the listing")
	} else if sub.Size != nil {
		t.Errorf("a directory reported size %d; it has none to report", *sub.Size)
	}
}

// An empty file says zero, and something with no size says nothing.
//
// This is the whole reason the field is a pointer, and it is asserted after a
// real decode of a real response because that is where the distinction would
// be lost: an int64 collapses both onto 0, and a client showing "0 B" tells
// someone their file is empty when the truth is that nobody measured it.
func TestZeroBytesAndNoSizeAreDifferentAnswers(t *testing.T) {
	c, libraryID, _ := laneClient(t)

	path := filepath.Join(t.TempDir(), "empty.bin")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := c.UploadFile(libraryID, "/", path); err != nil {
		t.Fatalf("upload: %v", err)
	}
	if err := c.Mkdir(libraryID, "/sub"); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	entries := listing(t, c, libraryID, "/")
	empty := byName(entries, "empty.bin")
	if empty == nil || empty.Size == nil {
		t.Fatal("an empty file came back with no size; zero has to be stated, not implied")
	}
	if *empty.Size != 0 {
		t.Errorf("an empty file reports %d bytes", *empty.Size)
	}
	if sub := byName(entries, "sub"); sub == nil || sub.Size != nil {
		t.Error("a directory came back carrying a size")
	}
}

// The sidecar is a cache, and losing it costs nothing but a re-read.
//
// The manifest stays the authority for how big a file is; this table holds a
// copy of one number out of it. Emptying it must therefore change no answer —
// if it did, the cache would have become the truth, and a cache that is the
// truth is a database nobody backs up.
func TestEmptyingTheSidecarChangesNoAnswer(t *testing.T) {
	c, libraryID, _ := laneClient(t)

	path := filepath.Join(t.TempDir(), "file.bin")
	if err := os.WriteFile(path, make([]byte, 12<<20), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := c.UploadFile(libraryID, "/", path); err != nil {
		t.Fatalf("upload: %v", err)
	}

	before := byName(listing(t, c, libraryID, "/"), "file.bin")
	if before == nil || before.Size == nil {
		t.Fatal("the first listing reported no size")
	}

	if _, err := siloPair.Write.Exec("DELETE FROM ObjectSize"); err != nil {
		t.Fatalf("clearing the sidecar: %v", err)
	}

	after := byName(listing(t, c, libraryID, "/"), "file.bin")
	if after == nil || after.Size == nil {
		t.Fatal("the listing lost the size when the sidecar was cleared; it is not a cache")
	}
	if *after.Size != *before.Size {
		t.Errorf("the size changed from %d to %d across a cleared sidecar", *before.Size, *after.Size)
	}

	// And it refilled itself on the way past, so the next listing is a lookup
	// rather than another read of every manifest.
	var rows int
	if err := siloPair.Read.QueryRow("SELECT COUNT(*) FROM ObjectSize").Scan(&rows); err != nil {
		t.Fatalf("counting the sidecar: %v", err)
	}
	if rows == 0 {
		t.Error("the listing read the sizes and did not record them, so every listing pays again")
	}
}
