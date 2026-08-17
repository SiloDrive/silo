package silod

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gorilla/mux"
)

func TestEntryPath(t *testing.T) {
	cases := []struct {
		raw  string
		want string
	}{
		// The empty match is /entries/ with nothing after it: the library root.
		{"", "/"},
		{"/", "/"},
		{"notes", "/notes"},
		{"/notes", "/notes"},
		// A trailing slash is how a client says "directory"; it is not part of
		// the path the rest of the API speaks.
		{"notes/", "/notes"},
		{"a/b/c.txt", "/a/b/c.txt"},
		{"a/b/", "/a/b"},
	}
	for _, c := range cases {
		if got := entryPath(c.raw); got != c.want {
			t.Errorf("entryPath(%q) = %q, want %q", c.raw, got, c.want)
		}
	}
}

func TestMatchesETag(t *testing.T) {
	const etag = `"v1-abc123"`
	cases := []struct {
		header string
		want   bool
	}{
		{"", false},
		{`"v1-abc123"`, true},
		{`"v1-different"`, false},
		// "*" matches anything that exists.
		{"*", true},
		// The list form, with and without spaces.
		{`"v1-other","v1-abc123"`, true},
		{`"v1-other", "v1-abc123"`, true},
		{`"v1-other", "v1-nope"`, false},
		// RFC 9110 requires the weak comparison for If-None-Match, so a weak
		// candidate matches the strong tag with the same opaque part.
		{`W/"v1-abc123"`, true},
		{`W/"v1-different"`, false},
		// The prefix versions the representation, so an unprefixed id — or one
		// from a different representation version — must not validate.
		{`"abc123"`, false},
		{`"v0-abc123"`, false},
	}
	for _, c := range cases {
		if got := matchesETag(c.header, etag); got != c.want {
			t.Errorf("matchesETag(%q, %q) = %v, want %v", c.header, etag, got, c.want)
		}
	}
}

// requestWithPath builds a request carrying the mux variables the handlers read,
// without needing a router.
func requestWithPath(target, pathVar string) *http.Request {
	r := httptest.NewRequest("PUT", target, nil)
	return mux.SetURLVars(r, map[string]string{"path": pathVar})
}

func TestWantsDirectory(t *testing.T) {
	cases := []struct {
		name    string
		target  string
		pathVar string
		want    bool
	}{
		// Both spellings are accepted: a trailing slash is how a directory is
		// written in a path, and ?type=dir is how a client that builds URLs
		// from components says the same thing.
		{"explicit type", "/entries/notes?type=dir", "notes", true},
		{"explicit type, mixed case", "/entries/notes?type=DIR", "notes", true},
		{"trailing slash", "/entries/notes/", "notes/", true},
		{"neither", "/entries/a.txt", "a.txt", false},
		{"some other type", "/entries/a.txt?type=file", "a.txt", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := wantsDirectory(requestWithPath(c.target, c.pathVar)); got != c.want {
				t.Errorf("wantsDirectory(%q, path=%q) = %v, want %v", c.target, c.pathVar, got, c.want)
			}
		})
	}
}

// TestUnsupportedMethodSaysWhatIsAllowed pins the 405: a client that guesses a
// method should be told which ones exist, not handed a 404 suggesting the path
// is wrong.
func TestUnsupportedMethodSaysWhatIsAllowed(t *testing.T) {
	w := httptest.NewRecorder()
	entriesHandler(w, httptest.NewRequest("PATCH", "/entries/a.txt", nil))

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want %d", w.Code, http.StatusMethodNotAllowed)
	}
	if allow := w.Header().Get("Allow"); allow == "" {
		t.Error("no Allow header on a 405")
	}
}

// TestPutOfFileContentIsNotImplemented documents the one real gap in the
// surface. When PUT starts storing bytes this test should fail, which is the
// point: the 501 is a promise to be broken deliberately.
func TestPutOfFileContentIsNotImplemented(t *testing.T) {
	w := httptest.NewRecorder()
	putEntry(w, requestWithPath("/entries/a.txt", "a.txt"))

	if w.Code != http.StatusNotImplemented {
		t.Errorf("status = %d, want %d", w.Code, http.StatusNotImplemented)
	}
	// A 501 that doesn't say what to do instead is a dead end.
	if body := w.Body.String(); body == "" {
		t.Error("501 with no explanation of the alternative")
	}
}

func TestRootCannotBeDeleted(t *testing.T) {
	w := httptest.NewRecorder()
	r := mux.SetURLVars(httptest.NewRequest("DELETE", "/entries/", nil), map[string]string{"path": ""})
	deleteEntry(w, r)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}
