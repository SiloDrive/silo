package middleware

import "net/http"

// DefaultCachePolicy marks every authenticated response uncacheable unless its
// handler says otherwise.
//
// An ETag and a Last-Modified with no Cache-Control is the one combination RFC
// 9111 §4.2.2 licenses a cache to invent a freshness lifetime for, and the
// conventional heuristic — CFNetwork's — is a tenth of the document's age. It
// cost a SiloDrive user three days: a directory listing was served from an
// on-disk cache with no request at all, the newly added child was not in that
// stale body, so the change was filtered out as a path the listing does not
// mention and the sync anchor advanced past the commit. The delta is never
// requested again after that, so the file was invisible permanently rather
// than until the next sweep.
//
// The routes that had no validator — libraries, changes, commits, account —
// escaped that only by accident, because a heuristic needs something to
// measure age from. That is a trap rather than a policy: the day one of them
// grows an ETag for good reasons it becomes cacheable in silence.
// changes?since={commit} is the sharpest, because the same URL legitimately
// returns *more* as commits land, so a stale answer is wrong in a way
// indistinguishable from "nothing has changed".
//
// This runs *before* the handler and sets rather than defends the header, so a
// route with a real policy of its own simply overwrites it — objects/{id} is
// content-addressed and keeps its year of immutability. A defaulting layer
// that ran afterwards would have replaced those deliberate headers with this
// generic one, which is the opposite of the point.
//
// private, not public: every route under here required a credential to reach,
// so no shared cache may serve the result to somebody who did not present one.
func DefaultCachePolicy(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "private, no-cache")
		next.ServeHTTP(w, r)
	})
}
