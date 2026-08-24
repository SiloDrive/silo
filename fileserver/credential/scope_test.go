package credential

import "testing"

func TestParseScopeRoundTrip(t *testing.T) {
	tests := []struct {
		in        string
		want      Scope
		canonical string // what String emits, and what storage writes
	}{
		{in: "", want: Scope{}, canonical: ""},
		{in: "abc-123", want: Scope{LibraryID: "abc-123"}, canonical: "abc-123"},
		{in: "abc-123:/photos", want: Scope{LibraryID: "abc-123", Path: "/photos"}, canonical: "abc-123:/photos"},

		// Normalisation: an untidy path is stored canonically.
		{in: "abc-123:photos", want: Scope{LibraryID: "abc-123", Path: "/photos"}, canonical: "abc-123:/photos"},
		{in: "abc-123:/photos/", want: Scope{LibraryID: "abc-123", Path: "/photos"}, canonical: "abc-123:/photos"},
		{in: "abc-123:/photos//2024", want: Scope{LibraryID: "abc-123", Path: "/photos/2024"}, canonical: "abc-123:/photos/2024"},
		{in: "abc-123:/photos/./2024", want: Scope{LibraryID: "abc-123", Path: "/photos/2024"}, canonical: "abc-123:/photos/2024"},

		// A path that cleans to the root is the whole library, which already
		// has an encoding. Keep one.
		{in: "abc-123:/", want: Scope{LibraryID: "abc-123"}, canonical: "abc-123"},

		// Rooted Clean cannot escape the library.
		{in: "abc-123:/a/../../etc", want: Scope{LibraryID: "abc-123", Path: "/etc"}, canonical: "abc-123:/etc"},
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

		if s := got.String(); s != tt.canonical {
			t.Errorf("ParseScope(%q).String() = %q, want %q", tt.in, s, tt.canonical)
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
		":",          // no library id
		":/photos",   // no library id
		"abc-123:",   // colon with no path: ambiguous truncation
		"abc/123",    // library ids do not contain path separators
		"abc 123:/p", // nor whitespace
	} {
		if got, err := ParseScope(in); err == nil {
			t.Errorf("ParseScope(%q) = %+v, want error", in, got)
		}
	}
}

func TestScopeCovers(t *testing.T) {
	const (
		library = "abc-123"
		other   = "def-456"
		photos  = library + ":/photos"
	)

	tests := []struct {
		scope   string
		library string
		path    string
		want    bool
		reason  string
	}{
		{"", library, "/anything", true, "unscoped reaches every library"},
		{"", other, "/anything", true, "unscoped reaches every library"},

		{library, library, "/", true, "library scope reaches the root"},
		{library, library, "/deep/inside", true, "library scope reaches everything"},
		{library, other, "/", false, "library scope does not cross libraries"},

		{photos, library, "/photos", true, "a scope covers its own path"},
		{photos, library, "/photos/2024", true, "and everything beneath it"},
		{photos, library, "/photos/2024/jan/x.jpg", true, "at any depth"},
		{photos, library, "/", false, "but not the root above it"},
		{photos, library, "/documents", false, "nor a sibling"},
		{photos, other, "/photos", false, "nor the same path elsewhere"},

		// The prefix bug this exists to prevent.
		{photos, library, "/photos-old", false, "a name is not a path prefix"},
		{photos, library, "/photosandmore", false, "a name is not a path prefix"},

		// The request path is normalised the same way the scope was, or a
		// caller could walk around a scope by spelling the path untidily.
		{photos, library, "photos/2024", true, "unrooted request path"},
		{photos, library, "/photos/", true, "trailing slash"},
		{photos, library, "/photos/2024/..", true, "cleans back inside"},
		{photos, library, "/photos/../documents", false, "cleans back outside"},

		// Case is not folded: encryption.md forbids the server deriving
		// behaviour from the shape of a name, and under E2EE these bytes are
		// ciphertext anyway.
		{library + ":/Photos", library, "/photos", false, "comparison is bytewise"},
	}

	for _, tt := range tests {
		s, err := ParseScope(tt.scope)
		if err != nil {
			t.Fatalf("ParseScope(%q): %v", tt.scope, err)
		}
		if got := s.Covers(tt.library, tt.path); got != tt.want {
			t.Errorf("Scope(%q).Covers(%q, %q) = %v, want %v — %s",
				tt.scope, tt.library, tt.path, got, tt.want, tt.reason)
		}
	}
}
