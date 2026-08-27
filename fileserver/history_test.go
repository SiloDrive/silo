package silod

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/dkam/silo/fileserver/account"
	"github.com/dkam/silo/fileserver/api"
	"github.com/dkam/silo/fileserver/libmgr"
)

// commitList is the commits endpoint's body, named here so a field cannot be
// renamed on the wire without a test noticing.
type commitList struct {
	Commits []struct {
		ID        string `json:"id"`
		CreatedAt int64  `json:"created_at"`
		Author    string `json:"author"`
		Message   string `json:"message"`
	} `json:"commits"`
}

// commitFile puts one file and returns the commit it produced, so a test can
// build a history it knows the shape of.
func commitFile(t *testing.T, libraryID string, acct *account.Account, path, body string) string {
	t.Helper()
	vars := map[string]string{"libraryid": libraryID, "path": path}
	if w := do(t, entriesHandler, acct, "PUT", "/x", vars, []byte(body)); w.Code != http.StatusCreated {
		t.Fatalf("PUT %s = %d (%s), want 201", path, w.Code, w.Body.String())
	}
	library := libmgr.Get(libraryID)
	if library == nil {
		t.Fatalf("library %s vanished after a write", libraryID)
	}
	return library.HeadCommitID
}

func listCommits(t *testing.T, libraryID string, acct *account.Account, query string) (*http.Response, commitList) {
	t.Helper()
	vars := map[string]string{"libraryid": libraryID}
	w := do(t, api.CommitsHandler, acct, "GET", "/x"+query, vars, nil)
	var got commitList
	if w.Code == http.StatusOK {
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatalf("decoding %s: %v", w.Body.String(), err)
		}
	}
	return w.Result(), got
}

// The whole point of the endpoint: history is walkable without the client
// fetching commit objects one at a time to find out what the previous one was.
func TestCommitsListsHistoryNewestFirst(t *testing.T) {
	libraryID, acct := testLibrary(t)

	first := commitFile(t, libraryID, acct, "a.txt", "one")
	second := commitFile(t, libraryID, acct, "a.txt", "two")
	third := commitFile(t, libraryID, acct, "b.txt", "three")

	_, got := listCommits(t, libraryID, acct, "")
	if len(got.Commits) < 3 {
		t.Fatalf("got %d commits, want at least the 3 writes", len(got.Commits))
	}
	for i, want := range []string{third, second, first} {
		if got.Commits[i].ID != want {
			t.Errorf("commit %d = %s, want %s", i, got.Commits[i].ID, want)
		}
	}
	if got.Commits[0].CreatedAt == 0 {
		t.Error("the newest commit has no created_at, so a client cannot name it by time")
	}
}

// A plain library's author and message are public -- they are only sealed
// under E2EE -- so a history listing that dropped them would make plain
// history anonymous for no reason.
func TestCommitsCarriesAuthorAndMessageForAPlainLibrary(t *testing.T) {
	libraryID, acct := testLibrary(t)
	commitFile(t, libraryID, acct, "a.txt", "one")

	_, got := listCommits(t, libraryID, acct, "")
	if got.Commits[0].Author == "" {
		t.Error("the newest commit has no author in a plain library")
	}
}

// Paging is opt-in here for the reason pagination.go gives, and the cursor
// resumes the walk rather than indexing into it: history is a linked list, and
// an offset cursor would re-walk from the head on every page.
func TestCommitsPagesWithoutRepeatingOrSkipping(t *testing.T) {
	libraryID, acct := testLibrary(t)
	commitFile(t, libraryID, acct, "a.txt", "one")
	commitFile(t, libraryID, acct, "a.txt", "two")
	commitFile(t, libraryID, acct, "b.txt", "three")

	_, whole := listCommits(t, libraryID, acct, "")

	var paged []string
	query := "?limit=2"
	for range 10 {
		resp, page := listCommits(t, libraryID, acct, query)
		for _, c := range page.Commits {
			paged = append(paged, c.ID)
		}
		next := resp.Header.Get("Link")
		if next == "" {
			break
		}
		if len(page.Commits) != 2 {
			t.Fatalf("a page with more after it held %d commits, want the limit of 2", len(page.Commits))
		}
		query = linkTarget(t, next)
	}

	if len(paged) != len(whole.Commits) {
		t.Fatalf("paging gave %d commits, the whole listing gave %d", len(paged), len(whole.Commits))
	}
	for i := range paged {
		if paged[i] != whole.Commits[i].ID {
			t.Errorf("paged commit %d = %s, want %s", i, paged[i], whole.Commits[i].ID)
		}
	}
}

// A limit that is not a positive integer is the caller's bug, and serving them
// something reasonable is how that bug reaches production. Same rule as
// parseLimit already applies to the changes feed.
func TestCommitsRefusesANonsenseLimit(t *testing.T) {
	libraryID, acct := testLibrary(t)
	commitFile(t, libraryID, acct, "a.txt", "one")

	for _, q := range []string{"?limit=0", "?limit=-1", "?limit=many"} {
		resp, _ := listCommits(t, libraryID, acct, q)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("GET commits%s = %d, want 400", q, resp.StatusCode)
		}
	}
}

// A cursor this server did not issue is refused rather than guessed at.
func TestCommitsRefusesACursorItDidNotIssue(t *testing.T) {
	libraryID, acct := testLibrary(t)
	commitFile(t, libraryID, acct, "a.txt", "one")

	resp, _ := listCommits(t, libraryID, acct, "?cursor=bm90LWEtY3Vyc29y")
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("GET commits with a forged cursor = %d, want 400", resp.StatusCode)
	}
}

// Reading is allowed to anyone who can see the library at all, and refused to
// everyone else. History is library content, not metadata about it.
func TestCommitsRefusesAnAccountWithNoPermission(t *testing.T) {
	libraryID, acct := testLibrary(t)
	commitFile(t, libraryID, acct, "a.txt", "one")

	stranger := mintAccount(t, "stranger@example.com")
	resp, _ := listCommits(t, libraryID, stranger, "")
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("GET commits as a stranger = %d, want 403", resp.StatusCode)
	}
}

// The other half: a path resolved under an old commit instead of the head.
// This is what makes a .history/ view possible without the client walking the
// tree object by object.
func TestEntriesAtAnOldCommitServesTheOldBytes(t *testing.T) {
	libraryID, acct := testLibrary(t)

	first := commitFile(t, libraryID, acct, "a.txt", "one")
	commitFile(t, libraryID, acct, "a.txt", "two")

	vars := map[string]string{"libraryid": libraryID, "path": "a.txt"}
	if w := do(t, entriesHandler, acct, "GET", "/x", vars, nil); w.Body.String() != "two" {
		t.Errorf("head a.txt = %q, want %q", w.Body.String(), "two")
	}
	w := do(t, entriesHandler, acct, "GET", "/x?at="+first, vars, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("GET at=%s = %d (%s), want 200", first, w.Code, w.Body.String())
	}
	if w.Body.String() != "one" {
		t.Errorf("a.txt at the first commit = %q, want %q", w.Body.String(), "one")
	}
}

// A file that has since been deleted is still there in the commit that had it.
// Recovering a deletion is the main thing anybody wants this for.
func TestEntriesAtAnOldCommitFindsADeletedFile(t *testing.T) {
	libraryID, acct := testLibrary(t)

	before := commitFile(t, libraryID, acct, "gone.txt", "content")
	vars := map[string]string{"libraryid": libraryID, "path": "gone.txt"}
	if w := do(t, entriesHandler, acct, "DELETE", "/x", vars, nil); w.Code != http.StatusOK {
		t.Fatalf("DELETE = %d (%s), want 200", w.Code, w.Body.String())
	}

	if w := do(t, entriesHandler, acct, "GET", "/x", vars, nil); w.Code != http.StatusNotFound {
		t.Fatalf("head gone.txt = %d, want 404", w.Code)
	}
	w := do(t, entriesHandler, acct, "GET", "/x?at="+before, vars, nil)
	if w.Code != http.StatusOK || w.Body.String() != "content" {
		t.Errorf("gone.txt at the commit before the delete = %d %q, want 200 %q",
			w.Code, w.Body.String(), "content")
	}
}

// A directory listing at an old commit, because a .history/ view is a tree and
// not a single file.
func TestEntriesAtAnOldCommitListsTheOldDirectory(t *testing.T) {
	libraryID, acct := testLibrary(t)

	one := commitFile(t, libraryID, acct, "a.txt", "one")
	commitFile(t, libraryID, acct, "b.txt", "two")

	vars := map[string]string{"libraryid": libraryID, "path": ""}
	w := do(t, entriesHandler, acct, "GET", "/x?at="+one, vars, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("listing the root at %s = %d (%s), want 200", one, w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "a.txt") {
		t.Errorf("the root at the first commit = %s, want a.txt in it", body)
	}
	if strings.Contains(body, "b.txt") {
		t.Errorf("the root at the first commit = %s, want b.txt absent -- it did not exist yet", body)
	}
}

// History is read-only. A write carrying ?at= is refused rather than applied
// to the head, because a client that thinks it is editing the past and is
// silently editing the present is the worst available outcome.
func TestWritesAtAnOldCommitAreRefused(t *testing.T) {
	libraryID, acct := testLibrary(t)
	first := commitFile(t, libraryID, acct, "a.txt", "one")

	vars := map[string]string{"libraryid": libraryID, "path": "a.txt"}
	for _, method := range []string{"PUT", "DELETE", "POST"} {
		w := do(t, entriesHandler, acct, method, "/x?at="+first, vars, []byte(`{"op":"move","to":"/b.txt"}`))
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s with ?at= = %d (%s), want 400", method, w.Code, w.Body.String())
		}
	}

	vars = map[string]string{"libraryid": libraryID, "path": "a.txt"}
	if w := do(t, entriesHandler, acct, "GET", "/x", vars, nil); w.Body.String() != "one" {
		t.Errorf("a.txt after the refused writes = %q, want it untouched", w.Body.String())
	}
}

// A commit the library cannot resolve is 410 Gone, the same answer and for the
// same reason as changes?since= gives: the commit is not wrong, it is no
// longer reachable, and the caller's recovery is to look at what history it
// does have rather than to retry.
func TestEntriesAtAnUnreachableCommitIsGone(t *testing.T) {
	libraryID, acct := testLibrary(t)
	commitFile(t, libraryID, acct, "a.txt", "one")

	missing := "0000000000000000000000000000000000000000000000000000000000000000"
	vars := map[string]string{"libraryid": libraryID, "path": "a.txt"}
	w := do(t, entriesHandler, acct, "GET", "/x?at="+missing, vars, nil)
	if w.Code != http.StatusGone {
		t.Errorf("GET at a collected commit = %d (%s), want 410", w.Code, w.Body.String())
	}
}

// An ?at= that is not an id at all is the caller's mistake, not a cut history.
func TestEntriesAtANonsenseCommitIsBadRequest(t *testing.T) {
	libraryID, acct := testLibrary(t)
	commitFile(t, libraryID, acct, "a.txt", "one")

	vars := map[string]string{"libraryid": libraryID, "path": "a.txt"}
	w := do(t, entriesHandler, acct, "GET", "/x?at=not-an-id", vars, nil)
	if w.Code != http.StatusBadRequest {
		t.Errorf("GET at a malformed commit = %d (%s), want 400", w.Code, w.Body.String())
	}
}

// linkTarget pulls the query string out of an RFC 8288 next link, which is how
// a client follows one: the header carries a relative reference, so the test
// follows it rather than rebuilding the cursor parameter by hand.
func linkTarget(t *testing.T, header string) string {
	t.Helper()
	open, close := strings.Index(header, "<"), strings.Index(header, ">")
	if open < 0 || close < open {
		t.Fatalf("Link header %q is not <uri>; rel=…", header)
	}
	ref := header[open+1 : close]
	if q := strings.Index(ref, "?"); q >= 0 {
		return ref[q:]
	}
	return ""
}
