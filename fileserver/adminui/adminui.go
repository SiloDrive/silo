// Package adminui serves the administrative page.
//
// One embedded file off the binary, and no build step: a server that needed npm
// run before it could show an operator their own disk usage would be a server
// nobody could debug from a shell. The page asks the same admin endpoints an
// operator could curl, which is also what keeps it honest -- it can render
// nothing the API does not already say.
//
// It is served unauthenticated because it carries no data. Every number on it
// arrives from a fetch the browser makes with a credential the person typed in;
// the HTML itself is a form and a table with nothing in them. Gating the shell
// would only mean an operator could not reach the login box.
//
// docs/plans/admin.md § The page, and what it can honestly show is the owning
// document, and its rule about panels that would have to lie is the reason
// three of the five sections here say "not measured yet" instead of a number.
package adminui

import (
	_ "embed"
	"net/http"
	"strconv"
	"time"
)

//go:embed admin.html
var page []byte

// Handler serves the page.
func Handler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// No inline scripts or styles from anywhere else, no framing, and no
	// network egress: everything the page needs is in the file, so the policy
	// that says so costs nothing and closes the gap where a future edit quietly
	// adds a CDN.
	w.Header().Set("Content-Security-Policy",
		"default-src 'none'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; "+
			"connect-src 'self'; form-action 'none'; frame-ancestors 'none'; base-uri 'none'")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	// The page changes with the binary and nothing else, so it is cacheable for
	// a moment and revalidated after -- long enough that a reload during a
	// session is free, short enough that an upgraded server does not serve a
	// stale panel to somebody who never closed the tab.
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Content-Length", strconv.Itoa(len(page)))
	http.ServeContent(w, r, "admin.html", time.Time{}, newReader(page))
}
