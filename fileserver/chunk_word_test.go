package silod

import (
	"net/http"
	"strings"
	"testing"
)

// The wire says chunk, because everything else already did.
//
// "block" was the last of the inherited vocabulary. The binding format spec
// says chunk thirty-seven times and block none; store/, objmgr.Store and the
// handlers themselves all say chunk; the client's own file is chunks.go. Only
// the URL said block, which put the wire in disagreement with the spec a
// client implements against — the same shape as repo/library, which
// internal/lexicon records.
//
// It was renamed rather than dual-answered because nothing outside this
// repository had ever called it. Both halves are asserted here: the new
// spelling is on the router, and the old one is gone rather than quietly
// still working.
func TestTheChunkSurfaceIsSpelledChunks(t *testing.T) {
	base, token := wire(t)
	id := makeLibrary(t, base, token)

	// Registration only. A 404 means the route is not mounted; anything else
	// means it is, including the refusals these calls earn for their empty
	// bodies.
	for _, c := range []struct{ method, path string }{
		{"POST", "/api/silo/v1/libraries/" + id + "/chunks/missing"},
		{"POST", "/api/silo/v1/libraries/" + id + "/chunks/fetch"},
	} {
		code, body := call(t, c.method, base+c.path, token, "")
		if code == http.StatusNotFound {
			t.Errorf("%s %s: 404, so the route is not registered (%s)",
				c.method, c.path, strings.TrimSpace(body))
		}
	}

	// The id-addressed route needs the 405 probe the sibling test explains:
	// this path 404s for a chunk nobody stored, which is the same status an
	// unregistered route gives for the opposite reason.
	const hexID = "0000000000000000000000000000000000000000000000000000000000000000"
	code, _ := call(t, "POST", base+"/api/silo/v1/libraries/"+id+"/chunks/"+hexID, token, "")
	if code == http.StatusNotFound {
		t.Errorf("POST /chunks/{id}: 404, so the route is not registered " +
			"(a registered route answers 405 to a method it does not serve)")
	}
}

// The old spelling is gone, not deprecated. Every rename in this API fails
// loudly — a client built for one spelling gets a 404 from a server speaking
// the other — and a route that quietly kept working would leave two names for
// one resource, which is what the entries cleanup existed to remove.
func TestTheOldBlockSpellingIsGone(t *testing.T) {
	base, token := wire(t)
	id := makeLibrary(t, base, token)

	const hexID = "0000000000000000000000000000000000000000000000000000000000000000"
	for _, c := range []struct{ method, path string }{
		{"POST", "/api/silo/v1/libraries/" + id + "/blocks/missing"},
		{"POST", "/api/silo/v1/libraries/" + id + "/blocks/fetch"},
		{"PUT", "/api/silo/v1/libraries/" + id + "/blocks/" + hexID},
	} {
		code, _ := call(t, c.method, base+c.path, token, "")
		if code != http.StatusNotFound {
			t.Errorf("%s %s answered %d; the old spelling must be a 404", c.method, c.path, code)
		}
	}
}
