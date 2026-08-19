package client

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// Paging on this lane is opt-in and lives in a Link header, so the page
// functions here are the only ones that need to know it exists. ListDir and
// Changes ask for everything and get everything, exactly as before.
//
// A next link is an opaque path: hand it back verbatim rather than building the
// next URL from parts. It carries a cursor that pins the commit or directory
// object the first page came from, which is what keeps a sequence of pages
// consistent while the library is being written to.

// nextLink returns the URL of the next page, or "" at the end of a sequence.
//
// Only rel="next" is looked for. The header can carry several links and this
// endpoint sends one, but a parser that takes the first URL it sees would break
// silently the day a second relation is added.
func nextLink(h http.Header) string {
	for _, header := range h.Values("Link") {
		for _, part := range strings.Split(header, ",") {
			fields := strings.Split(part, ";")
			if len(fields) < 2 {
				continue
			}
			target := strings.TrimSpace(fields[0])
			if !strings.HasPrefix(target, "<") || !strings.HasSuffix(target, ">") {
				continue
			}
			for _, param := range fields[1:] {
				k, v, found := strings.Cut(strings.TrimSpace(param), "=")
				if !found || strings.TrimSpace(k) != "rel" {
					continue
				}
				if strings.Trim(strings.TrimSpace(v), `"`) == "next" {
					return target[1 : len(target)-1]
				}
			}
		}
	}
	return ""
}

// ListDirPage reads one page of a directory listing. Pass next="" to start;
// pass the returned next to continue, and stop when it comes back empty.
//
// The entries are a window on the directory as it was when the sequence began.
// A directory written to mid-listing does not shift items across the boundary —
// the client finishes reading the version it started on.
func (c *APIClient) ListDirPage(repoID, path, next string, limit int) ([]DirEntry, string, error) {
	target := next
	if target == "" {
		target = fmt.Sprintf("%s?limit=%d", entriesURL(repoID, path), limit)
	}
	var entries []DirEntry
	h, err := c.doRequestHeaders("GET", target, nil, &entries)
	if err != nil {
		return nil, "", err
	}
	return entries, nextLink(h), nil
}

// ChangesPage reads one page of the delta feed. Pass next="" to start with a
// since anchor; pass the returned next to continue.
//
// **Anchor is empty until the last page**, which is the contract rather than an
// oversight: recording it early would mark the client up to date for changes it
// has not seen. A caller that saves the anchor whenever it is non-empty is
// correct without having to track where in the sequence it is.
func (c *APIClient) ChangesPage(repoID, since, next string, limit int) (*ChangesResponse, string, error) {
	target := next
	if target == "" {
		target = fmt.Sprintf("/api/silo/v1/repos/%s/changes?since=%s&limit=%d",
			url.PathEscape(repoID), url.QueryEscape(since), limit)
	}
	var resp ChangesResponse
	h, err := c.doRequestHeaders("GET", target, nil, &resp)
	if err != nil {
		return nil, "", err
	}
	return &resp, nextLink(h), nil
}
