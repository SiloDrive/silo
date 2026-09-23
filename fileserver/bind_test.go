package silod

import (
	"testing"

	"github.com/SiloDrive/silo/fileserver/option"
)

func TestParseBindAddr(t *testing.T) {
	cases := []struct {
		name     string
		in       string
		wantHost string
		wantPort uint32
		wantSet  bool
	}{
		// Host only: the form -b has always taken. The port is left alone, so
		// SILO_PORT and the config file still decide it.
		{"ipv4 host only", "0.0.0.0", "0.0.0.0", 0, false},
		{"loopback host only", "127.0.0.1", "127.0.0.1", 0, false},
		{"hostname only", "localhost", "localhost", 0, false},
		// Bare IPv6, no port. SplitHostPort rejects these as "too many colons",
		// which is exactly how they are told apart from host:port.
		{"bare ipv6 loopback", "::1", "::1", 0, false},
		{"bare ipv6 global", "2401:d002:440a:e300::1", "2401:d002:440a:e300::1", 0, false},
		{"bracketed ipv6, no port", "[::1]", "::1", 0, false},

		// Host and port together.
		{"ipv4 host and port", "127.0.0.1:8082", "127.0.0.1", 8082, true},
		{"hostname and port", "localhost:9000", "localhost", 9000, true},
		{"bracketed ipv6 and port", "[::1]:8082", "::1", 8082, true},
		{"bracketed ipv6 global and port", "[2401:d002:440a:e300::1]:9000", "2401:d002:440a:e300::1", 9000, true},

		{"wildcard and port", "0.0.0.0:8003", "0.0.0.0", 8003, true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			host, port, set, err := parseBindAddr(c.in)
			if err != nil {
				t.Fatalf("parseBindAddr(%q) returned error: %v", c.in, err)
			}
			if host != c.wantHost {
				t.Errorf("parseBindAddr(%q) host = %q, want %q", c.in, host, c.wantHost)
			}
			if set != c.wantSet {
				t.Errorf("parseBindAddr(%q) portSet = %v, want %v", c.in, set, c.wantSet)
			}
			if set && port != c.wantPort {
				t.Errorf("parseBindAddr(%q) port = %d, want %d", c.in, port, c.wantPort)
			}
		})
	}
}

func TestParseBindAddrRejects(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"port not a number", "127.0.0.1:http"},
		{"port out of range", "127.0.0.1:70000"},
		// Port 0 binds to a kernel-chosen port, and the startup line reports the
		// port as configured -- so the server would announce ":0" and listen
		// somewhere nobody can find.
		{"port zero", "127.0.0.1:0"},
		{"empty port", "127.0.0.1:"},
		{"colon only", ":"},
		// A bare ":port" is the one form with two honest readings. Every other
		// Go server takes it for the wildcard; silo defaults to loopback, so an
		// operator who means "same host, new port" reads it the other way -- and
		// that reading, resolved silently the wrong way, is a server on the
		// network nobody meant to put there. Refused rather than guessed, the
		// way parseCommandArgs refuses a trailing flag.
		{"port only", ":8003"},
		{"port only, low port", ":80"},
		{"negative port", "127.0.0.1:-1"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, _, _, err := parseBindAddr(c.in); err == nil {
				t.Errorf("parseBindAddr(%q) returned no error, want one", c.in)
			}
		})
	}
}

// The startup line is what an operator copies into a browser or a client
// config, so it has to be a URL that works: bracketed for IPv6, and naming an
// actual host for the every-interface bind rather than "http://:8003".
func TestDisplayAddr(t *testing.T) {
	origHost, origPort := option.Host, option.Port
	t.Cleanup(func() { option.Host, option.Port = origHost, origPort })

	cases := []struct {
		name string
		host string
		port uint32
		want string
	}{
		{"ipv4", "127.0.0.1", 8082, "127.0.0.1:8082"},
		{"wildcard", "0.0.0.0", 8003, "0.0.0.0:8003"},
		{"every interface", "", 8003, "0.0.0.0:8003"},
		{"ipv6 loopback", "::1", 8082, "[::1]:8082"},
		{"hostname", "localhost", 8082, "localhost:8082"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			option.Host, option.Port = c.host, c.port
			if got := displayAddr(); got != c.want {
				t.Errorf("displayAddr() = %q, want %q", got, c.want)
			}
		})
	}
}

// The address handed to net.Listen has to be a valid one for an IPv6 host, and
// "%s:%d" is not: it renders ::1 as "::1:8082", which net.Listen rejects with
// "too many colons". Anyone who set SILO_HOST to a bare IPv6 address got a
// server that would not start.
func TestListenAddr(t *testing.T) {
	origHost, origPort := option.Host, option.Port
	t.Cleanup(func() { option.Host, option.Port = origHost, origPort })

	cases := []struct {
		name string
		host string
		port uint32
		want string
	}{
		{"ipv4", "127.0.0.1", 8082, "127.0.0.1:8082"},
		{"wildcard", "0.0.0.0", 8003, "0.0.0.0:8003"},
		{"every interface", "", 8003, ":8003"},
		{"ipv6 loopback", "::1", 8082, "[::1]:8082"},
		{"ipv6 global", "2401:d002:440a:e300::1", 9000, "[2401:d002:440a:e300::1]:9000"},
		{"hostname", "localhost", 8082, "localhost:8082"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			option.Host, option.Port = c.host, c.port
			if got := listenAddr(); got != c.want {
				t.Errorf("listenAddr() = %q, want %q", got, c.want)
			}
		})
	}
}

// Whether the server is reachable from off the machine decides whether the
// plaintext warning fires, so the every-interface bind has to count as exposed.
// "-b :8003" is the form someone reaches for when they only mean to change the
// port, and it is exactly the one that must not go quiet.
func TestExposedWithoutTLS(t *testing.T) {
	origHost := option.Host
	t.Cleanup(func() { option.Host = origHost })

	cases := []struct {
		name string
		host string
		want bool
	}{
		{"ipv4 loopback", "127.0.0.1", false},
		{"ipv4 loopback, other address in block", "127.0.0.53", false},
		{"ipv6 loopback", "::1", false},
		{"localhost", "localhost", false},
		{"every interface", "", true},
		{"ipv4 wildcard", "0.0.0.0", true},
		{"ipv6 wildcard", "::", true},
		{"lan address", "192.168.1.50", true},
		{"global ipv6", "2401:d002:440a:e300::1", true},
		{"hostname", "silo.example.com", true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			option.Host = c.host
			if got := exposedWithoutTLS(); got != c.want {
				t.Errorf("exposedWithoutTLS() with host %q = %v, want %v", c.host, got, c.want)
			}
		})
	}
}
