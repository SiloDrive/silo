package utils

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func withXFF(value string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/api/silo/v1/auth/login", nil)
	r.RemoteAddr = "127.0.0.1:41000" // the proxy, on loopback
	if value != "" {
		r.Header.Set("X-Forwarded-For", value)
	}
	return r
}

// A proxy APPENDS the address it saw to whatever the client sent. nginx's
// $proxy_add_x_forwarded_for does it, Caddy does it, and it is what the header
// is for -- the list is the path a request took, oldest hop first.
//
// So the first entry is the one furthest from the server and nearest to
// nobody: on a request that arrived through one proxy, it is whatever the
// client wrote there. Reading it meant an attacker chose their own rate-limit
// key and got a fresh one on every request, which is the whole of what the
// per-address buckets exist to stop -- and it only bites when
// trustProxyHeaders is on, which every proxied deployment must set.
//
// The last entry is the one the nearest proxy wrote, and it is the only one
// this server has grounds to believe.
func TestClientIPReadsTheEntryTheProxyWrote(t *testing.T) {
	for _, tc := range []struct {
		name, header, want string
	}{
		{"one hop, nothing forged", "203.0.113.9", "203.0.113.9"},
		{"one hop, a forged entry in front", "1.2.3.4, 203.0.113.9", "203.0.113.9"},
		{"a whole forged chain in front", "1.2.3.4, 5.6.7.8, 9.10.11.12, 203.0.113.9", "203.0.113.9"},
		{"spacing the client chose", "1.2.3.4,203.0.113.9", "203.0.113.9"},
		{"junk in the trusted position falls through", "203.0.113.9, not-an-ip", "127.0.0.1"},
		{"no header at all", "", "127.0.0.1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClientIP(withXFF(tc.header), true); got != tc.want {
				t.Errorf("ClientIP(%q) = %q, want %q", tc.header, got, tc.want)
			}
		})
	}
}

// Untrusted, the header is not read at all, whatever it says.
func TestClientIPIgnoresTheHeaderWhenItIsNotTrusted(t *testing.T) {
	if got := ClientIP(withXFF("1.2.3.4, 203.0.113.9"), false); got != "127.0.0.1" {
		t.Errorf("ClientIP with trust off = %q, want the peer address", got)
	}
}

// Two proxies means the last entry is the inner proxy and the client is the
// one before it. The count never grows on its own, because guessing high hands
// the client a key they wrote.
func TestClientIPCountsTrustedHopsFromTheRight(t *testing.T) {
	old := TrustedProxyHops
	TrustedProxyHops = 2
	t.Cleanup(func() { TrustedProxyHops = old })

	for _, tc := range []struct {
		name, header, want string
	}{
		{"two real hops", "203.0.113.9, 198.51.100.2", "203.0.113.9"},
		{"a forged entry in front of two hops", "1.2.3.4, 203.0.113.9, 198.51.100.2", "203.0.113.9"},
		// Fewer entries than the install said there were hops: the request did
		// not arrive the way this server was told it would, so none of the
		// list is believed.
		{"fewer entries than hops", "203.0.113.9", "127.0.0.1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClientIP(withXFF(tc.header), true); got != tc.want {
				t.Errorf("ClientIP(%q) with 2 hops = %q, want %q", tc.header, got, tc.want)
			}
		})
	}
}
