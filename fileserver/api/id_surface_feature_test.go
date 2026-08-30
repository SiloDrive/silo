package api

import (
	"slices"
	"testing"
)

// The id-addressed surface shipped without a name, and a capability a client
// cannot discover is a capability that does not exist to it.
//
// This is not a hypothetical. porter-fuse asked for `GET chunks/{id}` and for
// a way to read a manifest, and both had already been built — it could not see
// them, because `features` listed only the three write calls of the block
// surface and said nothing about the read half or about objects at all. The
// names are pinned here, separately from the general feature test, because the
// reason they exist is a specific failure rather than a general rule.
func TestFeaturesNameTheIDAddressedSurface(t *testing.T) {
	got := features()
	for name, why := range map[string]string{
		"chunks":           "chunks/missing, PUT chunks/{id}, PUT entries?type=chunks",
		"objects":          "GET/PUT objects/{id}, GET/HEAD chunks/{id}, PUT head",
		"chunks-fetch":     "POST chunks/fetch — many chunks in one framed response",
		"chunks-upload":    "POST chunks — many chunks in one framed request",
		"entries-manifest": "GET entries/{path}?type=manifest",
	} {
		if !slices.Contains(got, name) {
			t.Errorf("feature %q (%s) is missing from %v", name, why, got)
		}
	}
	// And the inherited spelling is gone rather than kept alongside. A client
	// branches on a feature name, so leaving the old one advertised would keep
	// the old vocabulary alive in the one place it is load-bearing.
	for _, gone := range []string{"blocks", "blocks-fetch"} {
		if slices.Contains(got, gone) {
			t.Errorf("feature %q is still advertised; the chunk surface is spelled chunks now", gone)
		}
	}
}
