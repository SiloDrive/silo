package api

import (
	"slices"
	"testing"
)

// The range read is one round trip only if a client knows it is there. Asked
// for and answered in the same change, so this lane does not repeat what
// history and the id surface both did: built, documented, and named nowhere a
// client branches on, which is a capability that does not exist to the client
// that needed it.
//
// The fallback matters to the name's shape. A client that cannot see this can
// still collapse the two requests by issuing them concurrently, so the check
// is a real fork in its code rather than a formality — and a client that
// probed by calling the route would pay the round trip it was trying to save
// to learn the answer.
func TestFeaturesNameTheRangeRead(t *testing.T) {
	got := features()
	if !slices.Contains(got, "entries-ranges") {
		t.Errorf("feature %q (QUERY entries/{path} with byte ranges) is missing from %v", "entries-ranges", got)
	}
}
