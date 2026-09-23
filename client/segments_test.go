package client

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/SiloDrive/silo/store"
)

// segments is the client's split, and objmgr.SplitPath is the server's. They
// have to agree about what a path may contain, because the client is what
// decides which requests are worth making: a split that resolves ".." into an
// ordinary name hands the server a path to refuse, and hands the caller an
// error about a name rather than about the path they passed.
func TestSegmentsRefusesDotAndDotDot(t *testing.T) {
	for _, p := range []string{"..", "a/../b", "../a", "a/..", "./..", "a/b/.."} {
		if _, err := segments(p); !errors.Is(err, store.ErrName) {
			t.Errorf("segments(%q) = %v, want a store.ErrName refusal", p, err)
		}
	}
	// "." on its own is the root and is dropped rather than refused, the way
	// SplitPath drops it: a caller asking about "." is asking about the tree
	// it already named, which is not an attempt to leave it.
	for _, p := range []string{".", "a/./b", "./a"} {
		if _, err := segments(p); err != nil {
			t.Errorf("segments(%q) = %v, want it tolerated", p, err)
		}
	}
}

// The tolerated shapes have to stay tolerated: this function is on every path
// the client resolves, and tightening it past what SplitPath accepts would
// refuse paths the server is perfectly willing to serve.
func TestSegmentsKeepsTheShapesItAlreadyAccepted(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"/", nil},
		{".", nil},
		{"a", []string{"a"}},
		{"/a/b/", []string{"a", "b"}},
		{"a//b", []string{"a", "b"}},
		{"a/./b", []string{"a", "b"}},
	} {
		got, err := segments(tc.in)
		if err != nil {
			t.Errorf("segments(%q) = %v", tc.in, err)
			continue
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("segments(%q) = %#v, want %#v", tc.in, got, tc.want)
		}
	}
}

// The point of refusing at the split rather than two layers down: a plain
// library never encrypts a name, so nothing below this checks one. Today the
// traversal reaches the wire and is refused by the server; it should not get
// that far, and the caller should not have to be online to be told.
func TestAPlainLibraryDoesNotSendATraversalToTheServer(t *testing.T) {
	var asked int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	}))
	t.Cleanup(srv.Close)
	lib := &plainLibrary{c: NewClient(srv.URL), ID: "r1"}

	if err := lib.MkdirAll("a/../../etc"); err == nil {
		t.Error("MkdirAll accepted a path that climbs out of the library")
	}
	if _, err := lib.Stat("a/../../etc/passwd"); err == nil {
		t.Error("Stat accepted a path that climbs out of the library")
	}
	if asked != 0 {
		t.Errorf("the client made %d request(s) for a path it should have refused itself", asked)
	}
}
