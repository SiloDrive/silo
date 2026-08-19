package client

import (
	"net/http"
	"testing"
)

func TestNextLinkFindsTheRelationAndNotJustAURL(t *testing.T) {
	cases := []struct {
		name   string
		header []string
		want   string
	}{
		{"absent", nil, ""},
		{"one link", []string{`</api/silo/v1/x?cursor=abc>; rel="next"`}, "/api/silo/v1/x?cursor=abc"},
		{"unquoted rel", []string{`</x>; rel=next`}, "/x"},
		{"spaces", []string{`</x> ;  rel = "next" `}, "/x"},
		// The relation is what is being looked for, not the first URL in the
		// header. A parser that takes position for meaning works right up
		// until the day a second relation is added ahead of it.
		{"other relation only", []string{`</first>; rel="prev"`}, ""},
		{"next after another", []string{`</first>; rel="prev", </second>; rel="next"`}, "/second"},
		{"repeated header", []string{`</a>; rel="prev"`, `</b>; rel="next"`}, "/b"},
		// A bare URL with no relation says nothing about what it is.
		{"no relation", []string{`</x>`}, ""},
		{"not bracketed", []string{`/x; rel="next"`}, ""},
	}
	for _, c := range cases {
		h := http.Header{}
		for _, v := range c.header {
			h.Add("Link", v)
		}
		if got := nextLink(h); got != c.want {
			t.Errorf("%s: nextLink = %q, want %q", c.name, got, c.want)
		}
	}
}
