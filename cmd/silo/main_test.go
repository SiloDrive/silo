package main

import "testing"

// TestNormalizeVersion pins the one format server-info reports, regardless of
// how the binary was built. A development build carries the source default
// ("0.4.1"); a CI build carries `git describe --tags` for the same commit
// ("v0.4.1"). A client comparing versions must not have to handle both.
func TestNormalizeVersion(t *testing.T) {
	cases := map[string]string{
		// The two spellings of the same release, which is the bug this exists
		// for: both must come out identical.
		"0.4.1":  "0.4.1",
		"v0.4.1": "0.4.1",
		"V0.4.1": "0.4.1",

		// git describe on an untagged or dirty tree. The suffix says what is
		// actually running, so only the prefix goes.
		"v0.4.1-3-gabc1234":       "0.4.1-3-gabc1234",
		"v0.4.1-dirty":            "0.4.1-dirty",
		"v0.4.1-3-gabc1234-dirty": "0.4.1-3-gabc1234-dirty",

		// --always with no tag in history gives a bare short SHA. Hex has no
		// "v", so there is nothing to strip, but check it survives anyway.
		"abc1234": "abc1234",

		// A "v" not introducing a number is part of the string, not a prefix.
		"version-two": "version-two",
		"v":           "v",
		"":            "",
	}

	for in, want := range cases {
		if got := normalizeVersion(in); got != want {
			t.Errorf("normalizeVersion(%q) = %q, want %q", in, got, want)
		}
	}
}
