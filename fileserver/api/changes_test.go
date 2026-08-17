package api

import (
	"testing"

	"github.com/dkam/silo/fileserver/diff"
)

// TestChangesFromDiff pins the translation from the diff package's vocabulary
// to the one a sync client thinks in: the diff uses a different status letter
// for files and directories, and a client wants that as a property of the item
// (is_dir) rather than of the operation.
func TestChangesFromDiff(t *testing.T) {
	cases := []struct {
		name  string
		entry *diff.DiffEntry
		want  change
	}{
		{
			name:  "added file",
			entry: &diff.DiffEntry{Status: diff.DiffStatusAdded, Name: "a.txt", Sha1: "aaa", Size: 3},
			want:  change{Op: "create", Path: "/a.txt", ID: "aaa", Size: 3},
		},
		{
			// A delete has no content to point at, so the id is cleared rather
			// than left holding the hash the item used to have.
			name:  "deleted file drops the id",
			entry: &diff.DiffEntry{Status: diff.DiffStatusDeleted, Name: "a.txt", Sha1: "aaa"},
			want:  change{Op: "delete", Path: "/a.txt"},
		},
		{
			name:  "modified file",
			entry: &diff.DiffEntry{Status: diff.DiffStatusModified, Name: "a.txt", Sha1: "bbb", Size: 9},
			want:  change{Op: "modify", Path: "/a.txt", ID: "bbb", Size: 9},
		},
		{
			// Renames carry both ends: Name is where it was, NewName where it
			// is now. A rename and a move are the same operation here.
			name:  "renamed file carries both paths",
			entry: &diff.DiffEntry{Status: diff.DiffStatusRenamed, Name: "old.txt", NewName: "sub/new.txt", Sha1: "aaa"},
			want:  change{Op: "move", Path: "/sub/new.txt", OldPath: "/old.txt", ID: "aaa"},
		},
		{
			name:  "added directory",
			entry: &diff.DiffEntry{Status: diff.DiffStatusDirAdded, Name: "sub", Sha1: "ddd"},
			want:  change{Op: "create", Path: "/sub", ID: "ddd", IsDir: true},
		},
		{
			name:  "deleted directory",
			entry: &diff.DiffEntry{Status: diff.DiffStatusDirDeleted, Name: "sub", Sha1: "ddd"},
			want:  change{Op: "delete", Path: "/sub", IsDir: true},
		},
		{
			name:  "renamed directory",
			entry: &diff.DiffEntry{Status: diff.DiffStatusDirRenamed, Name: "old", NewName: "new", Sha1: "ddd"},
			want:  change{Op: "move", Path: "/new", OldPath: "/old", ID: "ddd", IsDir: true},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := changesFromDiff([]*diff.DiffEntry{c.entry})
			if len(got) != 1 {
				t.Fatalf("got %d changes, want 1", len(got))
			}
			if got[0] != c.want {
				t.Errorf("got %+v, want %+v", got[0], c.want)
			}
		})
	}
}

// TestUnhandledStatusIsSkipped covers the unmerged status and anything a later
// diff version adds. Applying an operation the server did not mean would
// corrupt a client's view, and the fallback — enumerate from scratch — is
// always available, so silence beats a guess.
func TestUnhandledStatusIsSkipped(t *testing.T) {
	entries := []*diff.DiffEntry{
		{Status: diff.DiffStatusAdded, Name: "a.txt", Sha1: "aaa"},
		{Status: diff.DiffStatusUnmerged, Name: "conflict.txt", Sha1: "ccc"},
		{Status: 'Z', Name: "future.txt", Sha1: "zzz"},
	}
	got := changesFromDiff(entries)
	if len(got) != 1 {
		t.Fatalf("got %d changes, want 1 (only the added file)", len(got))
	}
	if got[0].Path != "/a.txt" {
		t.Errorf("kept %q, want /a.txt", got[0].Path)
	}
}

// TestChangesFromDiffIsNeverNil matters at the JSON boundary: a nil slice
// encodes as null, and a client that iterates the result would have to
// special-case it. An empty library returns [].
func TestChangesFromDiffIsNeverNil(t *testing.T) {
	if got := changesFromDiff(nil); got == nil {
		t.Error("changesFromDiff(nil) returned nil, want an empty slice")
	}
}

func TestAbsPath(t *testing.T) {
	cases := map[string]string{
		"":          "/",
		"a.txt":     "/a.txt",
		"/a.txt":    "/a.txt",
		"sub/a.txt": "/sub/a.txt",
	}
	for in, want := range cases {
		if got := absPath(in); got != want {
			t.Errorf("absPath(%q) = %q, want %q", in, got, want)
		}
	}
}
