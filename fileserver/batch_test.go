package silod

import (
	"crypto/sha1"
	"encoding/hex"
	"net/http"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/dkam/silo/fileserver/blockmgr"
	"github.com/dkam/silo/fileserver/fsmgr"
	"github.com/dkam/silo/fileserver/option"
	"github.com/dkam/silo/fileserver/repomgr"
)

const batchTestStore = "3f2a8c14-6b9d-4e07-a512-c8d3f0b71e29"

// newBatchTree stands up a store holding one file at the root and returns the
// repo and the root id to apply operations against.
func newBatchTree(t *testing.T) (*repomgr.Repo, string) {
	t.Helper()
	confPath := t.TempDir()
	dataDir := filepath.Join(confPath, "seafile-data")
	fsmgr.Init(confPath, dataDir, option.FsCacheLimit)
	blockmgr.Init(confPath, dataDir)

	repo := &repomgr.Repo{ID: batchTestStore, StoreID: batchTestStore, Version: 1}
	file, err := fsmgr.NewSeafile(1, 5, []string{"da39a3ee5e6b4b0d3255bfef95601890afd80709"})
	if err != nil {
		t.Fatalf("failed to build a file object: %v", err)
	}
	if err := fsmgr.SaveSeafile(batchTestStore, file); err != nil {
		t.Fatalf("failed to save a file object: %v", err)
	}
	root, err := fsmgr.NewSeafdir(1, []*fsmgr.SeafDirent{
		fsmgr.NewDirent(file.FileID, "existing.txt", uint32(syscall.S_IFREG|0644), 0, "", 5),
	})
	if err != nil {
		t.Fatalf("failed to build the root: %v", err)
	}
	if err := fsmgr.SaveSeafdir(batchTestStore, root); err != nil {
		t.Fatalf("failed to save the root: %v", err)
	}
	return repo, root.DirID
}

// apply runs a list of operations the way batchHandler does — each against the
// root the last one produced — and returns the final root.
func apply(t *testing.T, repo *repomgr.Repo, root string, ops ...batchOp) (string, *batchFailure) {
	t.Helper()
	for i, op := range ops {
		next, fail := applyBatchOp(repo, root, "user@example.com", op)
		if fail != nil {
			return root, fail
		}
		if next == "" {
			t.Fatalf("operation %d (%s) returned an empty root, which would commit an empty library", i, op.Op)
		}
		root = next
	}
	return root, nil
}

// TestBatchOperationsSeeTheOnesBeforeThem is the property that makes a batch
// worth having rather than being a loop the client could run itself: a
// directory created in operation one can be written into by operation two,
// inside a single commit.
func TestBatchOperationsSeeTheOnesBeforeThem(t *testing.T) {
	repo, root := newBatchTree(t)

	data := []byte("contents")
	sum := sha1.Sum(data)
	blockID := hex.EncodeToString(sum[:])
	if _, err := blockmgr.WriteBytes(batchTestStore, data, ""); err != nil {
		t.Fatalf("failed to store a block: %v", err)
	}

	final, fail := apply(t, repo, root,
		batchOp{Op: "mkdir", Path: "/reports"},
		batchOp{Op: "create", Path: "/reports/q3.txt", Blocks: []string{blockID}},
	)
	if fail != nil {
		t.Fatalf("batch failed: %d %s", fail.code, fail.message)
	}

	dent, err := fsmgr.GetDirentByPath(batchTestStore, final, "/reports/q3.txt")
	if err != nil || dent == nil {
		t.Fatalf("the file created inside a directory from the same batch is missing: %v", err)
	}
	if dent.Size != int64(len(data)) {
		t.Errorf("size = %d, want %d — the size is summed from the blocks, not taken on trust", dent.Size, len(data))
	}
	// The file the library started with is still there: a batch adds to the
	// tree it was given rather than replacing it.
	if _, err := fsmgr.GetDirentByPath(batchTestStore, final, "/existing.txt"); err != nil {
		t.Errorf("a batch removed a file it never named: %v", err)
	}
}

// TestBatchLeavesTheTreeUntouchedWhenAnOperationFails covers all-or-nothing
// from the caller's side: the root it holds after a failure is the root it had
// before, so committing is something it simply never reaches.
func TestBatchLeavesTheTreeUntouchedWhenAnOperationFails(t *testing.T) {
	repo, root := newBatchTree(t)

	reached, fail := apply(t, repo, root,
		batchOp{Op: "mkdir", Path: "/a"},
		batchOp{Op: "mkdir", Path: "/a/b"},
		batchOp{Op: "delete", Path: "/nothing-here"},
	)
	if fail == nil {
		t.Fatal("deleting a path that does not exist was accepted")
	}
	if fail.code != http.StatusNotFound {
		t.Errorf("code = %d, want 404", fail.code)
	}

	// The first two operations really did happen — this is not a batch that
	// validates everything before doing anything, and pretending otherwise
	// would hide what the rollback is actually made of.
	if _, err := fsmgr.GetDirentByPath(batchTestStore, reached, "/a/b"); err != nil {
		t.Fatalf("the operations before the failure did not take effect: %v", err)
	}

	// And none of it is reachable from the root the library still points at.
	// That is the whole rollback: the trees the batch built are unreferenced
	// objects, exactly what an abandoned upload leaves behind, and the branch
	// head never learns they existed because the handler returns before it
	// commits.
	if _, err := fsmgr.GetDirentByPath(batchTestStore, root, "/a"); err == nil {
		t.Error("an abandoned operation reached the root the library still points at")
	}
}

func TestBatchGuards(t *testing.T) {
	cases := []struct {
		name string
		ops  []batchOp
		code int
	}{
		// The single-operation handlers refuse each of these, and a batch is
		// not a way around them.
		{"delete the root", []batchOp{{Op: "delete", Path: "/"}}, http.StatusBadRequest},
		{"move the root", []batchOp{{Op: "move", Path: "/", To: "/x"}}, http.StatusBadRequest},
		{"move into a missing directory", []batchOp{{Op: "move", Path: "/existing.txt", To: "/nope/x.txt"}}, http.StatusNotFound},
		{"create in a missing directory", []batchOp{{Op: "create", Path: "/nope/x.txt"}}, http.StatusNotFound},
		{"move a directory into itself", []batchOp{
			{Op: "mkdir", Path: "/d"},
			{Op: "move", Path: "/d", To: "/d/inner"},
		}, http.StatusBadRequest},
		// A file onto a directory unreachables everything below it, which is
		// the collision docs/bugs/fixed/move-onto-directory-destroys-it.md is
		// about.
		{"move a file onto a directory", []batchOp{
			{Op: "mkdir", Path: "/d"},
			{Op: "move", Path: "/existing.txt", To: "/d"},
		}, http.StatusConflict},
		{"an unknown operation", []batchOp{{Op: "chmod", Path: "/existing.txt"}}, http.StatusBadRequest},
		// Dirent names are written straight to disk relative to the library
		// root by every syncing client, so ".." is a path traversal on someone
		// else's machine.
		{"a name that escapes the library", []batchOp{{Op: "mkdir", Path: "/a/.."}}, http.StatusBadRequest},
		// Blocks that were never uploaded: the same instruction as the
		// single-file path, and a batch is all-or-nothing so it names them.
		{"create from blocks that are absent", []batchOp{
			{Op: "create", Path: "/x.txt", Blocks: []string{"da39a3ee5e6b4b0d3255bfef95601890afd80709"}},
		}, http.StatusFailedDependency},
		{"create from a block id that is not one", []batchOp{
			{Op: "create", Path: "/x.txt", Blocks: []string{"nonsense"}},
		}, http.StatusBadRequest},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			repo, root := newBatchTree(t)
			_, fail := apply(t, repo, root, c.ops...)
			if fail == nil {
				t.Fatalf("%s was accepted", c.name)
			}
			if fail.code != c.code {
				t.Errorf("code = %d (%s), want %d", fail.code, fail.message, c.code)
			}
		})
	}
}

// TestBatchMkdirOfAnExistingDirectorySucceeds pins a deliberate difference from
// the single-operation mkdir. A batch describes where the library should end
// up, and "make sure this folder exists" has to be expressible in one request
// or a client is back to asking first and racing the answer.
func TestBatchMkdirOfAnExistingDirectorySucceeds(t *testing.T) {
	repo, root := newBatchTree(t)

	after, fail := apply(t, repo, root,
		batchOp{Op: "mkdir", Path: "/twice"},
		batchOp{Op: "mkdir", Path: "/twice"},
	)
	if fail != nil {
		t.Fatalf("the second mkdir failed: %d %s", fail.code, fail.message)
	}
	if _, err := fsmgr.GetDirentByPath(batchTestStore, after, "/twice"); err != nil {
		t.Fatalf("the directory is missing: %v", err)
	}

	// A file there is a different matter: that is a real collision, and
	// silently replacing it would destroy content the caller never named.
	_, fail = apply(t, repo, root, batchOp{Op: "mkdir", Path: "/existing.txt"})
	if fail == nil || fail.code != http.StatusConflict {
		t.Fatalf("mkdir over a file: %+v, want 409", fail)
	}
}

// TestBatchCopyLeavesTheSourceAndMoveDoesNot is the one distinction between the
// two operations, and it is worth an assertion rather than an inspection of
// which branch runs.
func TestBatchCopyLeavesTheSourceAndMoveDoesNot(t *testing.T) {
	repo, root := newBatchTree(t)

	after, fail := apply(t, repo, root,
		batchOp{Op: "copy", Path: "/existing.txt", To: "/copy.txt"},
		batchOp{Op: "move", Path: "/existing.txt", To: "/moved.txt"},
	)
	if fail != nil {
		t.Fatalf("batch failed: %d %s", fail.code, fail.message)
	}

	for _, present := range []string{"/copy.txt", "/moved.txt"} {
		if _, err := fsmgr.GetDirentByPath(batchTestStore, after, present); err != nil {
			t.Errorf("%s is missing: %v", present, err)
		}
	}
	if dent, err := fsmgr.GetDirentByPath(batchTestStore, after, "/existing.txt"); err == nil && dent != nil {
		t.Error("the move left its source in place")
	}
}
