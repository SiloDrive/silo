package api

import (
	"testing"
)

func TestAbsPath(t *testing.T) {
	cases := map[string]string{
		"":          "/",
		"a.txt":     "/a.txt",
		"/a.txt":    "/a.txt",
		"sub/a.txt": "/sub/a.txt",
	}
	for in, want := range cases {
		if got := absPath(in); got != want {
			t.Errorf("absPath(%q) = %q, want %q", in, got, want)
		}
	}
}
