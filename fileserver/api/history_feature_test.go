package api

import (
	"slices"
	"testing"
)

// The point-in-time surface shipped without a name, twice over: commits and
// entries?at= were built, documented in protocol.md, and listed nowhere a
// client branches on. Both times a client following "start with features, not
// the version" read a 404 where a working endpoint stood. This is the first
// ask on that lane filed before the endpoint existed, so the name lands in the
// same change as the route.
func TestFeaturesNameTheHistorySurface(t *testing.T) {
	got := features()
	if !slices.Contains(got, "history") {
		t.Errorf("feature %q (GET commits, GET entries/{path}?at=, GET entries/{path}?type=history) is missing from %v", "history", got)
	}
}
