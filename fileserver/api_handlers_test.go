package silod

import (
	"net/http/httptest"
	upath "path"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/dkam/silo/fileserver/fsmgr"
	"github.com/dkam/silo/fileserver/option"
	"github.com/dkam/silo/fileserver/repomgr"
)

func TestCheckEntryName(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"report.txt", true},
		{"a directory", true},
		{"日本語.txt", true},
		{".hidden", true},
		{"..hidden", true},
		{"", false},
		{".", false},
		{"..", false},
		{"../../../.ssh/authorized_keys", false},
		{"..\x2f..", false},
		{"sub/dir", false},
		{"/", false},
		{"lead/..", false},
		{strings.Repeat("a", 256), false},
		{"bad\xff\xfeutf8", false},
	}

	for _, c := range cases {
		w := httptest.NewRecorder()
		if got := checkEntryName(w, c.name); got != c.want {
			t.Errorf("checkEntryName(%q) = %v, want %v", c.name, got, c.want)
			continue
		}
		if !c.want && w.Code != 400 {
			t.Errorf("checkEntryName(%q) rejected with status %d, want 400", c.name, w.Code)
		}
	}
}

func TestMovesIntoOwnSubtree(t *testing.T) {
	cases := []struct {
		src, dstDir string
		want        bool
	}{
		// Destination directly inside the source.
		{"/docs", "/docs", true},
		// Destination deeper inside the source.
		{"/docs", "/docs/2024/q1", true},
		// Trailing-slash forms of the same thing.
		{"/docs/", "/docs", true},
		{"/docs", "/docs/", true},

		// Same parent — an in-place rename, allowed.
		{"/docs/report", "/docs", false},
		// Unrelated destination.
		{"/docs", "/archive", false},
		// Sibling with the source as a name prefix — must not false-positive.
		{"/ab", "/abc", false},
		{"/docs", "/docsets/old", false},
		// Source into root, and root into source.
		{"/docs", "/", false},
		{"/", "/docs", true},
	}

	for _, c := range cases {
		if got := movesIntoOwnSubtree(c.src, c.dstDir); got != c.want {
			t.Errorf("movesIntoOwnSubtree(%q, %q) = %v, want %v", c.src, c.dstDir, got, c.want)
		}
	}
}

// moveHandler is add-to-destination then delete-from-source. This builds the
// tree that guard protects and runs both phases directly, showing the delete
// takes the just-added copy with it. It is the data-loss case #5 describes,
// and the reason movesIntoOwnSubtree has to reject before phase 1 runs.
func TestMoveIntoOwnSubtreeWouldDestroyIt(t *testing.T) {
	confPath := t.TempDir()
	dataDir := filepath.Join(confPath, "seafile-data")
	fsmgr.Init(confPath, dataDir, option.FsCacheLimit)

	const storeID = "3f0c1e6a-77b6-4a9f-8f1e-2d9a1c4b5e70"
	repo := &repomgr.Repo{ID: storeID, StoreID: storeID, Version: 1}

	modeDir := uint32(syscall.S_IFDIR | 0644)
	modeFile := uint32(syscall.S_IFREG | 0644)

	// /docs/keep.txt
	file, err := fsmgr.NewSeafile(1, 4, []string{"4f616f98d6a264f75abffe1bc150019c880be239"})
	if err != nil {
		t.Fatalf("failed to create seafile: %v", err)
	}
	if err := fsmgr.SaveSeafile(storeID, file); err != nil {
		t.Fatalf("failed to save seafile: %v", err)
	}
	docs, err := fsmgr.NewSeafdir(1, []*fsmgr.SeafDirent{
		fsmgr.NewDirent(file.FileID, "keep.txt", modeFile, 0, "", 4),
	})
	if err != nil {
		t.Fatalf("failed to create docs dir: %v", err)
	}
	if err := fsmgr.SaveSeafdir(storeID, docs); err != nil {
		t.Fatalf("failed to save docs dir: %v", err)
	}
	root, err := fsmgr.NewSeafdir(1, []*fsmgr.SeafDirent{
		fsmgr.NewDirent(docs.DirID, "docs", modeDir, 0, "", 0),
	})
	if err != nil {
		t.Fatalf("failed to create root: %v", err)
	}
	if err := fsmgr.SaveSeafdir(storeID, root); err != nil {
		t.Fatalf("failed to save root: %v", err)
	}

	if _, err := fsmgr.GetDirentByPath(storeID, root.DirID, "/docs/keep.txt"); err != nil {
		t.Fatalf("test tree is wrong, /docs/keep.txt missing up front: %v", err)
	}

	// mv /docs -> /docs/nested, exactly as moveHandler would run it.
	srcPath, dstDir, dstName := "/docs", "/docs", "nested"

	newDent := fsmgr.NewDirent(docs.DirID, dstName, modeDir, 0, "", 0)
	var names []string
	rootAfterAdd, err := DoPostMultiFiles(repo, root.DirID, dstDir, []*fsmgr.SeafDirent{newDent}, "user@example.com", true, &names)
	if err != nil {
		t.Fatalf("phase 1 failed: %v", err)
	}
	if _, err := fsmgr.GetDirentByPath(storeID, rootAfterAdd, "/docs/nested/keep.txt"); err != nil {
		t.Fatalf("phase 1 did not place the copy: %v", err)
	}

	rootAfterDel, err := DelFileFromTree(storeID, rootAfterAdd, upath.Dir(srcPath), upath.Base(srcPath))
	if err != nil {
		t.Fatalf("phase 2 failed: %v", err)
	}

	// Both the original and the copy are gone: the file is destroyed.
	if _, err := fsmgr.GetDirentByPath(storeID, rootAfterDel, "/docs/nested/keep.txt"); err == nil {
		t.Error("expected the copy to be destroyed by phase 2, but it survived — has moveHandler's algorithm changed?")
	}
	if _, err := fsmgr.GetDirentByPath(storeID, rootAfterDel, "/docs/keep.txt"); err == nil {
		t.Error("expected the original to be gone after phase 2, but it survived")
	}

	// Which is why the guard must reject this move before phase 1 runs.
	if !movesIntoOwnSubtree(srcPath, dstDir) {
		t.Error("movesIntoOwnSubtree did not reject the move that destroys the subtree")
	}
}

// addNewEntries is the last chokepoint before a dirent is written into a tree,
// so it must reject a traversal name even if a caller forgets to validate.
func TestAddNewEntriesRejectsInvalidName(t *testing.T) {
	var oldDents []*fsmgr.SeafDirent
	var names []string
	dent := fsmgr.NewDirent(fsmgr.EmptySha1, "../../../.ssh/authorized_keys", 0644, time.Now().Unix(), "", 0)

	err := addNewEntries(nil, "user@example.com", &oldDents, []*fsmgr.SeafDirent{dent}, false, &names)
	if err == nil {
		t.Fatal("addNewEntries accepted a traversal name, want error")
	}
	if len(oldDents) != 0 || len(names) != 0 {
		t.Errorf("addNewEntries mutated the tree on rejection: dents=%v names=%v", oldDents, names)
	}
}

func TestAddNewEntriesAcceptsValidName(t *testing.T) {
	var oldDents []*fsmgr.SeafDirent
	var names []string
	dent := fsmgr.NewDirent(fsmgr.EmptySha1, "report.txt", 0644, time.Now().Unix(), "", 0)

	if err := addNewEntries(nil, "user@example.com", &oldDents, []*fsmgr.SeafDirent{dent}, false, &names); err != nil {
		t.Fatalf("addNewEntries rejected a valid name: %v", err)
	}
	if len(oldDents) != 1 || oldDents[0].Name != "report.txt" {
		t.Errorf("addNewEntries did not add the entry: %v", oldDents)
	}
}
