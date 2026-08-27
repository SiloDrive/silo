package silod

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dkam/silo/fileserver/account"
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

// A move that would replace a directory destroys its whole subtree in one
// commit — silent data loss reported as 200. Refused before anything is
// written. See docs/bugs/fixed/move-onto-directory-destroys-it.md.
func TestMoveOntoDirectoryIsRefused(t *testing.T) {
	libraryID, acct := testLibrary(t)

	mkdir(t, libraryID, acct, "/dst")
	put(t, libraryID, acct, "/dst/keep.txt", []byte("precious"))
	put(t, libraryID, acct, "/src.txt", []byte("mover"))

	w := postOp(t, libraryID, acct, "/src.txt", "move", "/dst")
	if w.Code != http.StatusConflict {
		t.Fatalf("move onto a directory = %d (%s), want 409", w.Code, w.Body.String())
	}
	if !exists(t, libraryID, acct, "/dst/keep.txt") {
		t.Error("the refused move destroyed the destination subtree anyway")
	}
}

// A move of a directory into its own subtree cannot be done at all: the
// destination would be inside the thing being removed.
func TestMoveIntoOwnSubtreeIsRefused(t *testing.T) {
	libraryID, acct := testLibrary(t)

	mkdir(t, libraryID, acct, "/docs")
	put(t, libraryID, acct, "/docs/keep.txt", []byte("precious"))

	w := postOp(t, libraryID, acct, "/docs", "move", "/docs/nested")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("move into own subtree = %d (%s), want 400", w.Code, w.Body.String())
	}
	if !exists(t, libraryID, acct, "/docs/keep.txt") {
		t.Error("the refused move destroyed the subtree anyway")
	}
}

// A copy leaves the source where it was, and transfers no content: the new
// entry names the object the source already names.
func TestCopyLeavesTheSourceInPlace(t *testing.T) {
	libraryID, acct := testLibrary(t)
	put(t, libraryID, acct, "/original.txt", []byte("content"))

	w := postOp(t, libraryID, acct, "/original.txt", "copy", "/duplicate.txt")
	if w.Code != http.StatusCreated {
		t.Fatalf("copy = %d (%s), want 201", w.Code, w.Body.String())
	}
	if !exists(t, libraryID, acct, "/original.txt") {
		t.Error("the copy removed its source")
	}
	if !exists(t, libraryID, acct, "/duplicate.txt") {
		t.Error("the copy did not appear")
	}

	// Content-addressed, so both names carry the same id — which is why a
	// directory copies in constant time however large it is.
	var made struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &made); err != nil {
		t.Fatal(err)
	}
	if made.ID == "" {
		t.Error("the copy did not report the id it shares with its source")
	}
}

// A copy into its own subtree terminates: the destination entry names the
// source as it stands at this commit, so it is a snapshot of a finite thing.
// This is the one case a move must refuse and a copy need not.
func TestCopyIntoOwnSubtreeTerminates(t *testing.T) {
	libraryID, acct := testLibrary(t)
	mkdir(t, libraryID, acct, "/docs")
	put(t, libraryID, acct, "/docs/keep.txt", []byte("precious"))

	w := postOp(t, libraryID, acct, "/docs", "copy", "/docs/nested")
	if w.Code != http.StatusCreated {
		t.Fatalf("copy into own subtree = %d (%s), want 201", w.Code, w.Body.String())
	}
	if !exists(t, libraryID, acct, "/docs/nested/keep.txt") {
		t.Error("the copy did not bring the subtree with it")
	}
	// One level only, and no deeper: the snapshot was taken before the copy
	// landed, so it cannot contain itself.
	if exists(t, libraryID, acct, "/docs/nested/nested") {
		t.Error("the copy contains itself; it was not a snapshot")
	}
}

// A Resolve failure other than "not found" used to answer a flat 404 here,
// skipping the shared error table batch operations already use — so a path
// component that is not a directory read as a plain 404 through this
// endpoint and as 409 everywhere else the same error is produced.
func TestMoveSourceThroughAFileAnswersConflictNotNotFound(t *testing.T) {
	libraryID, acct := testLibrary(t)
	put(t, libraryID, acct, "/f.txt", []byte("not a directory"))

	w := postOp(t, libraryID, acct, "/f.txt/nested", "move", "/elsewhere")
	if w.Code != http.StatusConflict {
		t.Fatalf("move through a file = %d (%s), want 409", w.Code, w.Body.String())
	}
}

func mkdir(t *testing.T, libraryID string, acct *account.Account, path string) {
	t.Helper()
	vars := map[string]string{"libraryid": libraryID, "path": strings.TrimPrefix(path, "/")}
	if w := do(t, entriesHandler, acct, "PUT", "/x?type=dir", vars, nil); w.Code != http.StatusCreated {
		t.Fatalf("mkdir %s = %d (%s)", path, w.Code, w.Body.String())
	}
}

func put(t *testing.T, libraryID string, acct *account.Account, path string, content []byte) {
	t.Helper()
	vars := map[string]string{"libraryid": libraryID, "path": strings.TrimPrefix(path, "/")}
	if w := do(t, entriesHandler, acct, "PUT", "/x", vars, content); w.Code != http.StatusCreated {
		t.Fatalf("put %s = %d (%s)", path, w.Code, w.Body.String())
	}
}

func postOp(t *testing.T, libraryID string, acct *account.Account, path, op, to string) *httptest.ResponseRecorder {
	t.Helper()
	vars := map[string]string{"libraryid": libraryID, "path": strings.TrimPrefix(path, "/")}
	body, _ := json.Marshal(map[string]string{"op": op, "to": to})
	return do(t, entriesHandler, acct, "POST", "/x", vars, body)
}

func exists(t *testing.T, libraryID string, acct *account.Account, path string) bool {
	t.Helper()
	vars := map[string]string{"libraryid": libraryID, "path": strings.TrimPrefix(path, "/")}
	return do(t, entriesHandler, acct, "HEAD", "/x", vars, nil).Code == http.StatusOK
}
