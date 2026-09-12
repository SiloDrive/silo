package silod

import (
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/dkam/silo/fileserver/option"
)

// parseBindAddr reads the argument to -b, which is either a host on its own or
// a host and port together:
//
//	0.0.0.0              every interface, port unchanged
//	127.0.0.1:8082       host and port
//	:8003                every interface, port 8003
//	[::1]:8082           IPv6 host and port
//	::1                  bare IPv6 host, port unchanged
//
// portSet is false for the host-only forms, and the caller leaves option.Port
// as SILO_PORT or the config file left it. That is what keeps the flag
// backward compatible: -b has only ever taken a host, and an install passing
// one keeps whatever port it had.
//
// The bare-IPv6 forms are the reason this is not a strings.Split on ":".
// SplitHostPort rejects "::1" as "too many colons", and that rejection is
// precisely how an address with no port is told from one with a port —
// brackets are what make a colon-bearing host unambiguous, which is why the
// only way to give an IPv6 host a port is to bracket it.
func parseBindAddr(s string) (host string, port uint32, portSet bool, err error) {
	h, p, splitErr := net.SplitHostPort(s)
	if splitErr != nil {
		// No port here, so the whole argument is the host. Brackets are still
		// stripped: "[::1]" is a legal way to write a host, and JoinHostPort
		// would otherwise bracket it a second time.
		return strings.TrimSuffix(strings.TrimPrefix(s, "["), "]"), 0, false, nil
	}

	// The port is checked before the host so that ":" is reported as the missing
	// port it is, rather than being sent to the message below to recommend
	// "127.0.0.1:" as the fix.
	n, err := strconv.ParseUint(p, 10, 32)
	if err != nil {
		return "", 0, false, fmt.Errorf("bind address %q: %q is not a port number", s, p)
	}
	// Port 0 asks the kernel for any free port. net.Listen would accept it, but
	// the startup line reports the port as configured, so the server would
	// announce ":0" and be listening somewhere the log does not name.
	if n == 0 || n > 65535 {
		return "", 0, false, fmt.Errorf("bind address %q: port %d is out of range (1-65535)", s, n)
	}

	// ":8003" has two honest readings and no way to tell them apart. Everywhere
	// else in Go it is the wildcard; here, where the default host is loopback,
	// an operator who only means to move the port reads it as "same host, new
	// port". Guessing wildcard puts a server on the network nobody asked to put
	// there, and guessing loopback strands one that was meant to be reachable.
	// So it is refused, and the message names both addresses rather than making
	// the reader work out what to type. See parseCommandArgs for the same call:
	// being pointed somewhere you did not mean, without being told, is worse
	// than being stopped.
	if h == "" {
		return "", 0, false, fmt.Errorf(
			"bind address %q: give a host too — 127.0.0.1:%d for this machine only, "+
				"or 0.0.0.0:%d for every interface", s, n, n)
	}

	return h, uint32(n), true, nil
}

// listenAddr renders the configured host and port as an address net.Listen
// accepts. JoinHostPort rather than "%s:%d" because an IPv6 host has to be
// bracketed: "::1" and port 8082 is "[::1]:8082", and "::1:8082" is an address
// net.Listen refuses with "too many colons".
func listenAddr() string {
	return net.JoinHostPort(option.Host, strconv.FormatUint(uint64(option.Port), 10))
}

// displayAddr renders the bind address for the startup line — the string an
// operator copies into a browser or a client config.
//
// It differs from listenAddr in one place: the empty host, which means every
// interface, is reported as 0.0.0.0 rather than left off. "http://:8003" is not
// an address anyone can use, and the whole point of the line is to be usable.
func displayAddr() string {
	host := option.Host
	if host == "" {
		host = "0.0.0.0"
	}
	return net.JoinHostPort(host, strconv.FormatUint(uint64(option.Port), 10))
}

// exposedWithoutTLS says whether the configured bind address is reachable from
// off this machine, which is what decides whether the plaintext warning fires.
//
// The empty host — "-b :8003", the form someone reaches for meaning only to
// change the port — is every interface, so it is exposed. That is the case the
// warning exists for.
//
// "localhost" is loopback but is not an IP, so ParseIP cannot say so and the
// warning used to fire on it. A security warning that cries wolf on the safest
// possible configuration is one people learn to scroll past, which costs more
// than it saves.
func exposedWithoutTLS() bool {
	if option.Host == "localhost" {
		return false
	}
	ip := net.ParseIP(option.Host)
	return ip == nil || !ip.IsLoopback()
}
