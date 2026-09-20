package silod

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/SiloDrive/silo/fileserver/account"
	"github.com/SiloDrive/silo/fileserver/api"
	"github.com/SiloDrive/silo/fileserver/libmgr"
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

// versionList is the body of GET entries/{path}?type=history, named here so a
// field cannot be renamed on the wire without a test noticing. ID is a pointer
// because null is a value on this surface: it is how a deletion is spelled.
type versionList struct {
	Versions []struct {
		Commit    string  `json:"commit"`
		CreatedAt int64   `json:"created_at"`
		ID        *string `json:"id"`
		Type      string  `json:"type"`
		Size      *int64  `json:"size"`
		Author    string  `json:"author"`
		Message   string  `json:"message"`
	} `json:"versions"`
}

func listVersions(t *testing.T, libraryID string, acct *account.Account, path, query string) (*http.Response, versionList) {
	t.Helper()
	vars := map[string]string{"libraryid": libraryID, "path": path}
	if query == "" {
		query = "?type=history"
	}
	w := do(t, entriesHandler, acct, "GET", "/x"+query, vars, nil)
	var got versionList
	if w.Code == http.StatusOK {
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatalf("decoding %s: %v", w.Body.String(), err)
		}
	}
	return w.Result(), got
}

// The whole point of the endpoint: a file's versions are the commits where its
// id changed, and nothing else. A client could get here by reading the file at
// every commit and discarding repeats; the server holds the trees and does it
// in one pass.
func TestEntryHistoryListsOnlyTheCommitsWhereTheFileChanged(t *testing.T) {
	libraryID, acct := testLibrary(t)

	first := commitFile(t, libraryID, acct, "a.txt", "one")
	commitFile(t, libraryID, acct, "b.txt", "unrelated")
	second := commitFile(t, libraryID, acct, "a.txt", "two")
	// Two more commits that carry a.txt unchanged. The id is the content
	// hash, so at both of them the file is what it was, and neither is a
	// version of it.
	commitFile(t, libraryID, acct, "b.txt", "still unrelated")
	commitFile(t, libraryID, acct, "c.txt", "and again")

	resp, got := listVersions(t, libraryID, acct, "a.txt", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("history = %d, want 200", resp.StatusCode)
	}
	if len(got.Versions) != 2 {
		t.Fatalf("got %d versions %+v, want 2: the two distinct contents", len(got.Versions), got.Versions)
	}
	for i, want := range []string{second, first} {
		v := got.Versions[i]
		if v.Commit != want {
			t.Errorf("version %d is at commit %s, want %s", i, v.Commit, want)
		}
		if v.ID == nil || *v.ID == "" {
			t.Errorf("version %d has no id, so a client cannot fetch it", i)
		}
		if v.Type != "file" {
			t.Errorf("version %d type = %q, want file", i, v.Type)
		}
		if v.Size == nil || *v.Size != 3 {
			t.Errorf("version %d size = %v, want 3", i, v.Size)
		}
		if v.CreatedAt == 0 {
			t.Errorf("version %d has no created_at", i)
		}
		if v.Author == "" {
			t.Errorf("version %d has no author on a plain library", i)
		}
	}
	if *got.Versions[0].ID == *got.Versions[1].ID {
		t.Error("two different contents share an id")
	}
}

// A path deleted and recreated is two lives of one name. A row with a null id
// says so; a version list that omitted the gap would read as one continuous
// file, and a client restoring "the version before this one" would reach
// across a deletion it was never shown.
func TestEntryHistorySpellsADeletionAsANullID(t *testing.T) {
	libraryID, acct := testLibrary(t)

	first := commitFile(t, libraryID, acct, "a.txt", "one")
	vars := map[string]string{"libraryid": libraryID, "path": "a.txt"}
	if w := do(t, entriesHandler, acct, "DELETE", "/x", vars, nil); w.Code != http.StatusOK {
		t.Fatalf("DELETE = %d (%s), want 200", w.Code, w.Body.String())
	}
	deleted := libmgr.Get(libraryID).HeadCommitID
	again := commitFile(t, libraryID, acct, "a.txt", "one")

	_, got := listVersions(t, libraryID, acct, "a.txt", "")
	if len(got.Versions) != 3 {
		t.Fatalf("got %d versions %+v, want 3: created, deleted, recreated", len(got.Versions), got.Versions)
	}
	if got.Versions[0].Commit != again || got.Versions[0].ID == nil {
		t.Errorf("newest = %+v, want the recreation at %s with an id", got.Versions[0], again)
	}
	if got.Versions[1].Commit != deleted || got.Versions[1].ID != nil {
		t.Errorf("middle = %+v, want the deletion at %s with a null id", got.Versions[1], deleted)
	}
	if got.Versions[2].Commit != first || got.Versions[2].ID == nil {
		t.Errorf("oldest = %+v, want the creation at %s with an id", got.Versions[2], first)
	}
	// The recreation carries the same bytes, so the same id: the id is
	// absolute, and a client holding those chunks already has this version.
	if *got.Versions[0].ID != *got.Versions[2].ID {
		t.Error("the same content before and after a deletion has two ids")
	}
	// Before the first commit that had the file there was nothing, and nothing
	// is not a version: the list does not end in a null row for the time
	// before the file existed.
	_, b := listVersions(t, libraryID, acct, "never.txt", "")
	if len(b.Versions) != 0 {
		t.Errorf("a path that never existed has versions: %+v", b.Versions)
	}
}

// A file deleted at the head is a null row first, then its versions: the
// answer to "what has this file been" includes "gone, currently".
func TestEntryHistoryOfADeletedFileStartsWithTheDeletion(t *testing.T) {
	libraryID, acct := testLibrary(t)

	first := commitFile(t, libraryID, acct, "a.txt", "one")
	vars := map[string]string{"libraryid": libraryID, "path": "a.txt"}
	if w := do(t, entriesHandler, acct, "DELETE", "/x", vars, nil); w.Code != http.StatusOK {
		t.Fatalf("DELETE = %d (%s), want 200", w.Code, w.Body.String())
	}
	deleted := libmgr.Get(libraryID).HeadCommitID

	_, got := listVersions(t, libraryID, acct, "a.txt", "")
	if len(got.Versions) != 2 {
		t.Fatalf("got %d versions %+v, want 2: deleted, created", len(got.Versions), got.Versions)
	}
	if got.Versions[0].Commit != deleted || got.Versions[0].ID != nil {
		t.Errorf("newest = %+v, want the deletion at %s with a null id", got.Versions[0], deleted)
	}
	if got.Versions[1].Commit != first || got.Versions[1].ID == nil {
		t.Errorf("oldest = %+v, want the creation at %s", got.Versions[1], first)
	}
}

// limit bounds versions, not commits, and paging through them neither repeats
// nor skips one. That is the number a client needs bounded: three versions in
// forty thousand commits is three rows, however many pages the scan takes.
func TestEntryHistoryPagesByVersion(t *testing.T) {
	libraryID, acct := testLibrary(t)
	for _, body := range []string{"one", "two", "three", "four", "five"} {
		commitFile(t, libraryID, acct, "a.txt", body)
		commitFile(t, libraryID, acct, "b.txt", body+" noise")
	}

	_, whole := listVersions(t, libraryID, acct, "a.txt", "")
	if len(whole.Versions) != 5 {
		t.Fatalf("got %d versions, want 5", len(whole.Versions))
	}

	var paged []string
	query := "?type=history&limit=2"
	for range 10 {
		resp, page := listVersions(t, libraryID, acct, "a.txt", query)
		for _, v := range page.Versions {
			paged = append(paged, v.Commit)
		}
		next := resp.Header.Get("Link")
		if next == "" {
			break
		}
		if len(page.Versions) > 2 {
			t.Fatalf("a page held %d versions, above the limit of 2", len(page.Versions))
		}
		query = linkTarget(t, next)
		if !strings.Contains(query, "type=history") {
			t.Fatalf("the next link %q dropped type=history, so following it reads the file", query)
		}
	}
	if len(paged) != 5 {
		t.Fatalf("paging gave %d versions, the whole listing gave 5: %v", len(paged), paged)
	}
	for i := range paged {
		if paged[i] != whole.Versions[i].Commit {
			t.Errorf("paged version %d = %s, want %s", i, paged[i], whole.Versions[i].Commit)
		}
	}
}

// at names a point in time and history is a walk through all of them; the two
// do not compose, and dropping one silently is how a client reads the wrong
// answer with a 200 on it.
func TestEntryHistoryRefusesAt(t *testing.T) {
	libraryID, acct := testLibrary(t)
	head := commitFile(t, libraryID, acct, "a.txt", "one")
	resp, _ := listVersions(t, libraryID, acct, "a.txt", "?type=history&at="+head)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("history with at = %d, want 400", resp.StatusCode)
	}
}

// A credential scoped to a folder may see the history of what is inside it and
// nothing else: the same rule as the read it is a history of.
func TestEntryHistoryRefusesAnAccountWithNoPermission(t *testing.T) {
	libraryID, acct := testLibrary(t)
	commitFile(t, libraryID, acct, "a.txt", "one")
	_, other := testLibrary(t)
	resp, _ := listVersions(t, libraryID, other, "a.txt", "")
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("another account's history = %d, want 403", resp.StatusCode)
	}
}
