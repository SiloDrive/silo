package credential

import "testing"

func TestParseScopeRoundTrip(t *testing.T) {
	tests := []struct {
		in     string
		want   Scope
		stored string // canonical form; "" means same as in
	}{
		{in: "", want: Scope{}},
		{in: "abc-123", want: Scope{RepoID: "abc-123"}},
		{in: "abc-123:/photos", want: Scope{RepoID: "abc-123", Path: "/photos"}},

		// Normalisation: all four spell the same scope.
		{in: "abc-123:photos", want: Scope{RepoID: "abc-123", Path: "/photos"}, stored: "abc-123:/photos"},
		{in: "abc-123:/photos/", want: Scope{RepoID: "abc-123", Path: "/photos"}, stored: "abc-123:/photos"},
		{in: "abc-123:/photos//2024", want: Scope{RepoID: "abc-123", Path: "/photos/2024"}, stored: "abc-123:/photos/2024"},
		{in: "abc-123:/photos/./2024", want: Scope{RepoID: "abc-123", Path: "/photos/2024"}, stored: "abc-123:/photos/2024"},

		// A path that cleans to the root is the whole library, which already
		// has an encoding. Keep one.
		{in: "abc-123:/", want: Scope{RepoID: "abc-123"}, stored: "abc-123"},

		// Rooted Clean cannot escape the library.
		{in: "abc-123:/a/../../etc", want: Scope{RepoID: "abc-123", Path: "/etc"}, stored: "abc-123:/etc"},
	}

	for _, tt := range tests {
		got, err := ParseScope(tt.in)
		if err != nil {
			t.Errorf("ParseScope(%q): unexpected error %v", tt.in, err)
			continue
		}
		if got != tt.want {
			t.Errorf("ParseScope(%q) = %+v, want %+v", tt.in, got, tt.want)
		}

		want := tt.stored
		if want == "" {
			want = tt.in
		}
		if s := got.String(); s != want {
			t.Errorf("ParseScope(%q).String() = %q, want %q", tt.in, s, want)
		}

		// The canonical form must parse to itself, or storage and comparison
		// disagree after a round trip through the column.
		again, err := ParseScope(got.String())
		if err != nil {
			t.Errorf("ParseScope(%q) (canonical): unexpected error %v", got.String(), err)
		} else if again != got {
			t.Errorf("ParseScope(%q) is not idempotent: %+v then %+v", tt.in, got, again)
		}
	}
}

func TestParseScopeRejects(t *testing.T) {
	for _, in := range []string{
		":",          // no repo id
		":/photos",   // no repo id
		"abc-123:",   // colon with no path: ambiguous truncation
		"abc/123",    // repo ids do not contain path separators
		"abc 123:/p", // nor whitespace
	} {
		if got, err := ParseScope(in); err == nil {
			t.Errorf("ParseScope(%q) = %+v, want error", in, got)
		}
	}
}

func TestScopeCovers(t *testing.T) {
	const repo = "abc-123"
	const other = "def-456"

	tests := []struct {
		scope  string
		repo   string
		path   string
		want   bool
		reason string
	}{
		{"", repo, "/anything", true, "unscoped reaches every library"},
		{"", other, "/anything", true, "unscoped reaches every library"},

		{repo, repo, "/", true, "library scope reaches the root"},
		{repo, repo, "/deep/inside", true, "library scope reaches everything"},
		{repo, other, "/", false, "library scope does not cross libraries"},

		{repo + ":/photos", repo, "/photos", true, "a scope covers its own path"},
		{repo + ":/photos", repo, "/photos/2024", true, "and everything beneath it"},
		{repo + ":/photos", repo, "/photos/2024/jan/x.jpg", true, "at any depth"},
		{repo + ":/photos", repo, "/", false, "but not the root above it"},
		{repo + ":/photos", repo, "/documents", false, "nor a sibling"},
		{repo + ":/photos", other, "/photos", false, "nor the same path elsewhere"},

		// The prefix bug this exists to prevent.
		{repo + ":/photos", repo, "/photos-old", false, "a name is not a path prefix"},
		{repo + ":/photos", repo, "/photosandmore", false, "a name is not a path prefix"},

		// The request path is normalised the same way the scope was, or a
		// caller could walk around a scope by spelling the path untidily.
		{repo + ":/photos", repo, "photos/2024", true, "unrooted request path"},
		{repo + ":/photos", repo, "/photos/", true, "trailing slash"},
		{repo + ":/photos", repo, "/photos/2024/..", true, "cleans back inside"},
		{repo + ":/photos", repo, "/photos/../documents", false, "cleans back outside"},

		// Case is not folded: encryption.md forbids the server deriving
		// behaviour from the shape of a name, and under E2EE these bytes are
		// ciphertext anyway.
		{repo + ":/Photos", repo, "/photos", false, "comparison is bytewise"},
	}

	for _, tt := range tests {
		s, err := ParseScope(tt.scope)
		if err != nil {
			t.Fatalf("ParseScope(%q): %v", tt.scope, err)
		}
		if got := s.Covers(tt.repo, tt.path); got != tt.want {
			t.Errorf("Scope(%q).Covers(%q, %q) = %v, want %v — %s",
				tt.scope, tt.repo, tt.path, got, tt.want, tt.reason)
		}
	}
}
