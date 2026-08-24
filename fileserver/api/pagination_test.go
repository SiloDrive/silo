package api

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

func TestCursorRoundTrips(t *testing.T) {
	want := pageCursor{Since: "abc", Target: "def", Offset: 250}
	got, ok := decodeCursor(encodeCursor(want))
	if !ok {
		t.Fatal("a cursor this package encoded would not decode")
	}
	if got.Since != want.Since || got.Target != want.Target || got.Offset != want.Offset {
		t.Fatalf("round trip changed the cursor: %+v -> %+v", want, got)
	}
	if got.Version != cursorVersion {
		t.Errorf("version = %d, want %d", got.Version, cursorVersion)
	}
}

func TestCursorRejectsWhatItDidNotIssue(t *testing.T) {
	cases := []struct {
		name   string
		cursor string
	}{
		{"not base64", "!!!!"},
		{"not JSON", "aGVsbG8"},
		// A cursor from a future or past encoding decodes into plausible
		// values rather than failing, which is exactly why the version is
		// checked rather than trusted to structure.
		{"wrong version", encodeVersionedCursor(t, 99, 10)},
		// An offset cannot be negative, and a negative one would slice
		// backwards off the front of a page.
		{"negative offset", encodeVersionedCursor(t, cursorVersion, -1)},
	}
	for _, c := range cases {
		if _, ok := decodeCursor(c.cursor); ok {
			t.Errorf("%s was accepted", c.name)
		}
	}
}

// encodeVersionedCursor builds a cursor the encoder would never produce — a
// version it does not use, an offset it would not emit — so the decoder's
// checks are exercised rather than assumed.
func encodeVersionedCursor(t *testing.T, version, offset int) string {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"v": version, "since": "a", "target": "b", "off": offset})
	if err != nil {
		t.Fatalf("failed to build a test cursor: %v", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

func TestWindowBounds(t *testing.T) {
	cases := []struct {
		name                 string
		total, offset, limit int
		from, to             int
		more                 bool
	}{
		// No limit is the unpaged contract: everything, and nothing follows.
		{"unpaged", 100, 0, 0, 0, 100, false},
		{"first page", 100, 0, 10, 0, 10, true},
		{"middle page", 100, 40, 10, 40, 50, true},
		// The last full page must not claim there is more behind it, or the
		// client makes one wasted request and — worse — never sees an anchor.
		{"exact last page", 100, 90, 10, 90, 100, false},
		{"short last page", 95, 90, 10, 90, 95, false},
		{"limit past the end", 5, 0, 10, 0, 5, false},
		// A cursor that outlived what it pointed into: an empty final page,
		// not an error, because "there is nothing more" is true.
		{"offset past the end", 5, 50, 10, 5, 5, false},
		{"offset exactly at the end", 5, 5, 10, 5, 5, false},
		{"empty", 0, 0, 10, 0, 0, false},
	}
	for _, c := range cases {
		from, to, more := window(c.total, c.offset, c.limit)
		if from != c.from || to != c.to || more != c.more {
			t.Errorf("%s: window(%d,%d,%d) = %d,%d,%v; want %d,%d,%v",
				c.name, c.total, c.offset, c.limit, from, to, more, c.from, c.to, c.more)
		}
	}
}

func TestParseLimit(t *testing.T) {
	cases := []struct {
		query string
		want  int
		ok    bool
	}{
		// Absent is not zero-with-an-error: it is the whole-response contract.
		{"", 0, true},
		{"limit=10", 10, true},
		{"limit=" + strconv.Itoa(maxPageLimit), maxPageLimit, true},
		// Refused rather than clamped. A client asking for -1 has a bug, and
		// serving it something reasonable is how that bug survives.
		{"limit=0", 0, false},
		{"limit=-1", 0, false},
		{"limit=many", 0, false},
		{"limit=" + strconv.Itoa(maxPageLimit+1), 0, false},
	}
	for _, c := range cases {
		w := httptest.NewRecorder()
		got, ok := parseLimit(w, httptest.NewRequest("GET", "/x?"+c.query, nil))
		if got != c.want || ok != c.ok {
			t.Errorf("?%s = %d,%v; want %d,%v", c.query, got, ok, c.want, c.ok)
		}
		if !ok && w.Code != http.StatusBadRequest {
			t.Errorf("?%s answered %d, want 400", c.query, w.Code)
		}
	}
}

func TestNextLinkSupersedesTheOpeningParameters(t *testing.T) {
	r := httptest.NewRequest("GET", "/api/silo/v1/libraries/x/changes?since=aaa&limit=10", nil)
	w := httptest.NewRecorder()
	setNextLink(w, r, "CURSOR")

	link := w.Header().Get("Link")
	if link == "" {
		t.Fatal("no Link header")
	}
	if link[0] != '<' || !strings.HasSuffix(link, `>; rel="next"`) {
		t.Fatalf("Link is not RFC 8288 shaped: %s", link)
	}

	target := link[1 : len(link)-len(`>; rel="next"`)]
	u, err := url.Parse(target)
	if err != nil {
		t.Fatalf("the link is not a URL: %v", err)
	}
	q := u.Query()
	// since is dropped: the cursor already carries it, and two sources for one
	// fact is how they come to disagree.
	if q.Has("since") {
		t.Errorf("since survived into the next page: %s", target)
	}
	if q.Get("cursor") != "CURSOR" {
		t.Errorf("cursor = %q", q.Get("cursor"))
	}
	// limit does not: it is the client's page size, not part of what is being
	// paged over, so it has to be repeated or the next page is unbounded.
	if q.Get("limit") != "10" {
		t.Errorf("limit = %q, want 10 — without it the next page is the whole rest", q.Get("limit"))
	}
	if u.Path != "/api/silo/v1/libraries/x/changes" {
		t.Errorf("path = %q", u.Path)
	}
}
