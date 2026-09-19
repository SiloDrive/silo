package main

import (
	"strings"
	"testing"
)

// TestTUIRefusesAnythingThatIsNotAServerURL is the guard for a silent
// misunderstanding between an operator and this command.
//
// `silo tui` takes a server URL, because it is a client: it logs in over HTTP
// and holds a session. Every other subcommand that names a deployment names it
// with -d, as a data directory on disk, and the one that does not is the one an
// operator reaches for when the others have not worked. So `silo tui -d share02`
// gets typed, and before this it was accepted: "-d" became the URL, the value
// after it was never looked at, and the failure arrived as a connection error
// against a host named "-d" rather than as a complaint about the command.
//
// The rule is that a positional here is an http or https URL or it is a
// mistake. Saying so costs one comparison and saves the operator debugging a
// server that was never contacted.
func TestTUIRefusesAnythingThatIsNotAServerURL(t *testing.T) {
	const fallback = "http://localhost:8082"

	t.Run("no argument uses the environment or the default", func(t *testing.T) {
		got, err := tuiURL(nil, fallback)
		if err != nil {
			t.Fatalf("tuiURL(nil) errored: %v", err)
		}
		if got != fallback {
			t.Errorf("tuiURL(nil) = %q, want the fallback %q", got, fallback)
		}
	})

	t.Run("an empty argument is not an override", func(t *testing.T) {
		got, err := tuiURL([]string{""}, fallback)
		if err != nil {
			t.Fatalf(`tuiURL([""]) errored: %v`, err)
		}
		if got != fallback {
			t.Errorf(`tuiURL([""]) = %q, want the fallback %q`, got, fallback)
		}
	})

	good := []string{
		"http://localhost:8082",
		"https://silo.example.com",
		"https://silo.example.com:8443/",
		"HTTP://localhost:8082", // a scheme is case-insensitive per RFC 3986
	}
	for _, in := range good {
		if got, err := tuiURL([]string{in}, fallback); err != nil {
			t.Errorf("tuiURL(%q) errored: %v", in, err)
		} else if got != in {
			t.Errorf("tuiURL(%q) = %q, want it unchanged", in, got)
		}
	}

	// Each of these is something an operator has typed or will: the flag this
	// command does not take, a data directory, an absolute path, a host with no
	// scheme, and a scheme it cannot speak. The error has to name what to type
	// instead, so each case says which word it must contain.
	bad := map[string]string{
		"-d":            "flag",
		"--data-dir":    "flag",
		"share02":       "http",
		"/srv/silo":     "http",
		"localhost:808": "http",
		"ftp://host":    "http",
	}
	for in, want := range bad {
		got, err := tuiURL([]string{in}, fallback)
		if err == nil {
			t.Errorf("tuiURL(%q) = %q with no error, but it is not a server URL", in, got)
			continue
		}
		if !strings.Contains(err.Error(), want) {
			t.Errorf("tuiURL(%q) said %q, which never mentions %q -- the operator cannot tell what to type instead",
				in, err, want)
		}
	}

	// Two URLs is a typo, not a request to connect to both.
	if _, err := tuiURL([]string{"http://a", "http://b"}, fallback); err == nil {
		t.Error("tuiURL accepted two URLs without complaint; the second would have been ignored silently")
	}

	// The command that prompted all of this, typed exactly as it was typed.
	// It is two arguments, so an arity complaint is what it would earn by
	// argument count alone -- and "takes one server URL" is the least useful
	// true thing that can be said to somebody who has just reached for the
	// flag every other subcommand accepts. The flag is the diagnosis.
	_, err := tuiURL([]string{"-d", "share02"}, fallback)
	if err == nil {
		t.Fatal(`tuiURL(["-d","share02"]) was accepted`)
	}
	if !strings.Contains(err.Error(), "flag") {
		t.Errorf("`silo tui -d share02` said %q, which never says the problem is the flag", err)
	}
	if !strings.Contains(err.Error(), "silo user -d") {
		t.Errorf("`silo tui -d share02` said %q, without naming the command that does take -d", err)
	}
}
