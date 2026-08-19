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

func TestDestructiveCollision(t *testing.T) {
	modeDir := uint32(syscall.S_IFDIR | 0644)
	modeFile := uint32(syscall.S_IFREG | 0644)
	dirent := func(mode uint32) *fsmgr.SeafDirent {
		return fsmgr.NewDirent(fsmgr.EmptySha1, "dst", mode, 0, "", 0)
	}

	cases := []struct {
		what    string
		srcMode uint32
		dst     *fsmgr.SeafDirent
		want    bool
	}{
		{"nothing at the destination", modeFile, nil, false},
		{"file onto file replaces, as PUT does", modeFile, dirent(modeFile), false},
		{"file onto directory unlinks the subtree", modeFile, dirent(modeDir), true},
		{"directory onto directory unlinks the subtree", modeDir, dirent(modeDir), true},
		{"directory onto file discards the file", modeDir, dirent(modeFile), true},
	}
	for _, c := range cases {
		if got := destructiveCollision(c.srcMode, c.dst) != ""; got != c.want {
			t.Errorf("destructiveCollision(%s) = %v, want %v", c.what, got, c.want)
		}
	}
}

// The data loss destructiveCollision exists to prevent, run through the same two
// phases moveHandler uses. Moving a file onto a directory swaps the directory's
// dirent for the file's, and every descendant goes with it — on a 200, with no
// error anywhere for a caller to notice.
func TestMoveOntoDirectoryWouldDestroyIt(t *testing.T) {
	confPath := t.TempDir()
	dataDir := filepath.Join(confPath, "seafile-data")
	fsmgr.Init(confPath, dataDir, option.FsCacheLimit)

	const storeID = "9c2e4b81-3a5d-4f7e-b6c1-08e7a2d95f34"
	repo := &repomgr.Repo{ID: storeID, StoreID: storeID, Version: 1}

	modeDir := uint32(syscall.S_IFDIR | 0644)
	modeFile := uint32(syscall.S_IFREG | 0644)

	// /Precious/keep.txt, plus /junk.txt beside it at the root.
	keep, err := fsmgr.NewSeafile(1, 4, []string{"4f616f98d6a264f75abffe1bc150019c880be239"})
	if err != nil {
		t.Fatalf("failed to create keep.txt: %v", err)
	}
	if err := fsmgr.SaveSeafile(storeID, keep); err != nil {
		t.Fatalf("failed to save keep.txt: %v", err)
	}
	precious, err := fsmgr.NewSeafdir(1, []*fsmgr.SeafDirent{
		fsmgr.NewDirent(keep.FileID, "keep.txt", modeFile, 0, "", 4),
	})
	if err != nil {
		t.Fatalf("failed to create /Precious: %v", err)
	}
	if err := fsmgr.SaveSeafdir(storeID, precious); err != nil {
		t.Fatalf("failed to save /Precious: %v", err)
	}
	junk, err := fsmgr.NewSeafile(1, 5, []string{"da39a3ee5e6b4b0d3255bfef95601890afd80709"})
	if err != nil {
		t.Fatalf("failed to create junk.txt: %v", err)
	}
	if err := fsmgr.SaveSeafile(storeID, junk); err != nil {
		t.Fatalf("failed to save junk.txt: %v", err)
	}
	root, err := fsmgr.NewSeafdir(1, []*fsmgr.SeafDirent{
		fsmgr.NewDirent(precious.DirID, "Precious", modeDir, 0, "", 0),
		fsmgr.NewDirent(junk.FileID, "junk.txt", modeFile, 0, "", 5),
	})
	if err != nil {
		t.Fatalf("failed to create root: %v", err)
	}
	if err := fsmgr.SaveSeafdir(storeID, root); err != nil {
		t.Fatalf("failed to save root: %v", err)
	}

	if _, err := fsmgr.GetDirentByPath(storeID, root.DirID, "/Precious/keep.txt"); err != nil {
		t.Fatalf("test tree is wrong, /Precious/keep.txt missing up front: %v", err)
	}

	// mv /junk.txt -> /Precious, exactly as moveHandler would run it.
	srcPath, dstDir, dstName := "/junk.txt", "/", "Precious"

	newDent := fsmgr.NewDirent(junk.FileID, dstName, modeFile, 0, "", 5)
	var names []string
	rootAfterAdd, err := DoPostMultiFiles(repo, root.DirID, dstDir, []*fsmgr.SeafDirent{newDent}, "user@example.com", true, &names)
	if err != nil {
		t.Fatalf("phase 1 failed: %v", err)
	}
	rootAfterDel, err := DelFileFromTree(storeID, rootAfterAdd, upath.Dir(srcPath), upath.Base(srcPath))
	if err != nil {
		t.Fatalf("phase 2 failed: %v", err)
	}

	if _, err := fsmgr.GetDirentByPath(storeID, rootAfterDel, "/Precious/keep.txt"); err == nil {
		t.Error("expected /Precious/keep.txt to be destroyed by the move, but it survived — has moveHandler's algorithm changed?")
	}

	// Which is why the guard must reject this move before phase 1 runs.
	dst, err := fsmgr.GetDirentByPath(storeID, root.DirID, "/Precious")
	if err != nil {
		t.Fatalf("failed to look up the destination: %v", err)
	}
	if destructiveCollision(modeFile, dst) == "" {
		t.Error("destructiveCollision permitted the move that destroys /Precious/keep.txt")
	}
}

// TestCopyLeavesTheSourceInPlace pins the one thing that separates copy from
// move: phase 1 runs, phase 2 does not, and both paths end up naming the same
// object. If a refactor ever lets the delete run for a copy, this fails.
func TestCopyLeavesTheSourceInPlace(t *testing.T) {
	confPath := t.TempDir()
	fsmgr.Init(confPath, filepath.Join(confPath, "seafile-data"), option.FsCacheLimit)

	const storeID = "1d0f5c92-77b4-4a1e-9c33-5b8e6a2f41d7"
	repo := &repomgr.Repo{ID: storeID, StoreID: storeID, Version: 1}
	modeFile := uint32(syscall.S_IFREG | 0644)

	junk, err := fsmgr.NewSeafile(1, 5, []string{"da39a3ee5e6b4b0d3255bfef95601890afd80709"})
	if err != nil {
		t.Fatalf("failed to create junk.txt: %v", err)
	}
	if err := fsmgr.SaveSeafile(storeID, junk); err != nil {
		t.Fatalf("failed to save junk.txt: %v", err)
	}
	root, err := fsmgr.NewSeafdir(1, []*fsmgr.SeafDirent{
		fsmgr.NewDirent(junk.FileID, "junk.txt", modeFile, 0, "", 5),
	})
	if err != nil {
		t.Fatalf("failed to create root: %v", err)
	}
	if err := fsmgr.SaveSeafdir(storeID, root); err != nil {
		t.Fatalf("failed to save root: %v", err)
	}

	// cp /junk.txt -> /copy.txt, exactly as copyHandler would run it: the new
	// dirent carries the source's object id, so no bytes move.
	newDent := fsmgr.NewDirent(junk.FileID, "copy.txt", modeFile, 0, "", 5)
	var names []string
	newRoot, err := DoPostMultiFiles(repo, root.DirID, "/", []*fsmgr.SeafDirent{newDent}, "user@example.com", true, &names)
	if err != nil {
		t.Fatalf("copy failed: %v", err)
	}

	src, err := fsmgr.GetDirentByPath(storeID, newRoot, "/junk.txt")
	if err != nil || src == nil {
		t.Fatalf("the source was removed by a copy: %v", err)
	}
	dst, err := fsmgr.GetDirentByPath(storeID, newRoot, "/copy.txt")
	if err != nil || dst == nil {
		t.Fatalf("the copy is missing: %v", err)
	}
	if src.ID != dst.ID {
		t.Errorf("copy stored new content: src id %s, dst id %s — a copy shares the source's id", src.ID, dst.ID)
	}
}

// TestCopyIntoOwnSubtreeTerminates is the evidence for the exemption in
// moveOrCopy: a move into its own subtree destroys the thing it moved, but a
// copy names the subtree as it stands at this commit, so the result is a finite
// snapshot and both the original and the copy are readable afterwards.
func TestCopyIntoOwnSubtreeTerminates(t *testing.T) {
	confPath := t.TempDir()
	fsmgr.Init(confPath, filepath.Join(confPath, "seafile-data"), option.FsCacheLimit)

	const storeID = "4b7a1e60-2c9d-48f3-a015-7e3d6c8b9042"
	repo := &repomgr.Repo{ID: storeID, StoreID: storeID, Version: 1}
	modeDir := uint32(syscall.S_IFDIR | 0644)
	modeFile := uint32(syscall.S_IFREG | 0644)

	keep, err := fsmgr.NewSeafile(1, 4, []string{"4f616f98d6a264f75abffe1bc150019c880be239"})
	if err != nil {
		t.Fatalf("failed to create keep.txt: %v", err)
	}
	if err := fsmgr.SaveSeafile(storeID, keep); err != nil {
		t.Fatalf("failed to save keep.txt: %v", err)
	}
	precious, err := fsmgr.NewSeafdir(1, []*fsmgr.SeafDirent{
		fsmgr.NewDirent(keep.FileID, "keep.txt", modeFile, 0, "", 4),
	})
	if err != nil {
		t.Fatalf("failed to create /Precious: %v", err)
	}
	if err := fsmgr.SaveSeafdir(storeID, precious); err != nil {
		t.Fatalf("failed to save /Precious: %v", err)
	}
	root, err := fsmgr.NewSeafdir(1, []*fsmgr.SeafDirent{
		fsmgr.NewDirent(precious.DirID, "Precious", modeDir, 0, "", 0),
	})
	if err != nil {
		t.Fatalf("failed to create root: %v", err)
	}
	if err := fsmgr.SaveSeafdir(storeID, root); err != nil {
		t.Fatalf("failed to save root: %v", err)
	}

	// cp /Precious -> /Precious/Precious
	newDent := fsmgr.NewDirent(precious.DirID, "Precious", modeDir, 0, "", 0)
	var names []string
	newRoot, err := DoPostMultiFiles(repo, root.DirID, "/Precious", []*fsmgr.SeafDirent{newDent}, "user@example.com", true, &names)
	if err != nil {
		t.Fatalf("copy failed: %v", err)
	}

	for _, path := range []string{"/Precious/keep.txt", "/Precious/Precious/keep.txt"} {
		if _, err := fsmgr.GetDirentByPath(storeID, newRoot, path); err != nil {
			t.Errorf("%s is unreadable after copying a directory into itself: %v", path, err)
		}
	}
	if _, err := fsmgr.GetDirentByPath(storeID, newRoot, "/Precious/Precious/Precious"); err == nil {
		t.Error("the copy recursed: the snapshot should be one level deep, not infinite")
	}
}
