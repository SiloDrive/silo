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

// TestPreconditionOutcomes covers the decision table without a database, by
// exercising the comparison directly: given what is at the path now (or
// nothing) and what the caller asserted, does the write proceed?
//
// The live behaviour is covered end-to-end against a real server; this pins the
// logic, including the case that makes the feature worth having — an If-Match
// naming content that has since been replaced.
func TestPreconditionOutcomes(t *testing.T) {
	const current = `"v1-aaa"`

	cases := []struct {
		name        string
		etag        string // what is at the path now; "" means nothing is
		ifMatch     string
		ifNoneMatch string
		wantOK      bool
	}{
		{name: "no preconditions, existing entry", etag: current, wantOK: true},
		{name: "no preconditions, absent entry", wantOK: true},

		// If-Match: replace only if this is still what I read.
		{name: "If-Match on unchanged content", etag: current, ifMatch: current, wantOK: true},
		{name: "If-Match on changed content", etag: `"v1-bbb"`, ifMatch: current, wantOK: false},
		{name: "If-Match on an absent entry", ifMatch: current, wantOK: false},
		{name: "If-Match * on an existing entry", etag: current, ifMatch: "*", wantOK: true},
		{name: "If-Match * on an absent entry", ifMatch: "*", wantOK: false},

		// If-None-Match: create only if nothing is there.
		{name: "If-None-Match * on an absent entry", ifNoneMatch: "*", wantOK: true},
		{name: "If-None-Match * on an existing entry", etag: current, ifNoneMatch: "*", wantOK: false},
		{name: "If-None-Match a different tag", etag: current, ifNoneMatch: `"v1-bbb"`, wantOK: true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := preconditionResult(c.etag, c.ifMatch, c.ifNoneMatch); got != c.wantOK {
				t.Errorf("got ok=%v, want %v", got, c.wantOK)
			}
		})
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
