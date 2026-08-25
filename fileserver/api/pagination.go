package api

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
)

// Pagination on the Silo lane, and the two decisions behind its shape.
//
// **It is opt-in.** No `limit`, no paging: the response is exactly what it has
// always been, whole and with its anchor. A default page size would silently
// truncate every client written before paging existed, and a truncated answer
// that looks complete is the worst failure available here — a sync client would
// apply half a diff and record the anchor for all of it.
//
// **A cursor pins the version it started on.** Paging through a diff or a
// directory while someone writes to the library would otherwise skip and repeat
// items as the underlying thing shifts under the offset. The objects are
// immutable and content-addressed, so the cursor names the exact commit or
// directory object the first page was computed from and every later page is
// served from that same object. The client enumerates a consistent snapshot and
// finds out about the writes on its next pass, which is the same bargain the
// delta feed already makes.
//
// The cursor is opaque on purpose. It carries an offset today; a later
// implementation may carry a key instead, and no client should have to be
// changed for that.

// maxPageLimit caps what a client can ask for in one page. A limit is a
// promise about response size, and a client that asks for a million items has
// unmade the promise without noticing.
const maxPageLimit = 10000

// cursorVersion is the shape of the encoded cursor. A cursor from a different
// version is rejected rather than misread — the fields are named, so an old
// cursor would decode into a plausible-looking value rather than an error.
const cursorVersion = 1

type pageCursor struct {
	Version int `json:"v"`
	// Since and Target pin a diff: the commit the client is coming from, and
	// the commit it is being brought up to. Target is what makes later pages
	// consistent — it is the head as of the first page, not as of now.
	Since  string `json:"since,omitempty"`
	Target string `json:"target,omitempty"`
	// Dir pins a directory listing to one directory object.
	Dir string `json:"dir,omitempty"`
	// From resumes a history walk at one commit. History is a linked list, so
	// the cursor names where to carry on rather than how far in to skip — an
	// offset would make each page re-walk everything before it, and the last
	// page of a long history would cost the whole of it.
	From string `json:"from,omitempty"`
	// Offset is how far into the pinned thing the next page starts.
	Offset int `json:"off"`
}

func encodeCursor(c pageCursor) string {
	c.Version = cursorVersion
	raw, err := json.Marshal(c)
	if err != nil {
		// The struct is three strings and an int. If this fails the process
		// has bigger problems than pagination.
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

func decodeCursor(s string) (pageCursor, bool) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return pageCursor{}, false
	}
	var c pageCursor
	if err := json.Unmarshal(raw, &c); err != nil {
		return pageCursor{}, false
	}
	if c.Version != cursorVersion || c.Offset < 0 {
		return pageCursor{}, false
	}
	return c, true
}

// parseLimit reads ?limit. A zero limit means the caller did not ask to page,
// which is not an error and not a default — it is the whole-response contract
// this endpoint has always had.
//
// On a bad value it has answered the request and returns false. Bad rather than
// clamped: a client that asks for -1 or "many" has a bug, and quietly serving
// it something reasonable is how that bug reaches production.
func parseLimit(w http.ResponseWriter, r *http.Request) (int, bool) {
	raw := r.URL.Query().Get("limit")
	if raw == "" {
		return 0, true
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		http.Error(w, "limit must be a positive integer", http.StatusBadRequest)
		return 0, false
	}
	if n > maxPageLimit {
		http.Error(w, "limit is above the maximum of "+strconv.Itoa(maxPageLimit), http.StatusBadRequest)
		return 0, false
	}
	return n, true
}

// window returns the slice of a page and whether anything follows it.
//
// An offset past the end is an empty final page rather than an error. It means
// the cursor outlived what it pointed into — a directory that shrank, a diff
// recomputed smaller — and "there is nothing more" is both true and the thing
// the client was going to do anyway.
func window(total, offset, limit int) (from, to int, more bool) {
	if offset >= total {
		return total, total, false
	}
	if limit <= 0 || offset+limit >= total {
		return offset, total, false
	}
	return offset, offset + limit, true
}

// setNextLink advertises the next page as an RFC 8288 Link header, so the body
// keeps the shape it had before paging existed. A client that follows the link
// needs to know nothing about how the cursor is built, which is the point of
// making it opaque.
//
// The URL is relative: Silo does not reliably know its own external address
// behind a proxy, and RFC 8288 permits a relative reference, resolved against
// the request URI.
func setNextLink(w http.ResponseWriter, r *http.Request, cursor string) {
	q := r.URL.Query()
	// The cursor carries everything the first request said about *what* to
	// page over, so the parameters that named it would be redundant at best
	// and contradictory at worst.
	q.Del("since")
	q.Set("cursor", cursor)
	next := url.URL{Path: r.URL.Path, RawQuery: q.Encode()}
	w.Header().Set("Link", "<"+next.String()+`>; rel="next"`)
}
