// Command silo is the unified Silo binary. It dispatches to the fileserver
// daemon (`silo serve`), the Bubble Tea TUI (`silo tui`), or one of the
// non-interactive CLI subcommands (`silo ls`, `silo get`, …).
package main

import (
	"fmt"
	"os"

	"github.com/dkam/silo/fileserver" // package silod
	"github.com/dkam/silo/fileserver/option"
	"github.com/dkam/silo/internal/cli"
	"github.com/dkam/silo/internal/observability"
	"github.com/dkam/silo/internal/tui"
)

const defaultServerURL = "http://localhost:8082"

// Version is stamped at build time via -ldflags "-X main.Version=...".
// The default is the current source-tree version; CI overrides it with
// `git describe --tags --always --dirty` so tagged builds report the tag.
var Version = "0.4.6"

// normalizeVersion drops the leading "v" a git tag carries, so the version this
// binary reports does not depend on how it was built.
//
// The source default is a bare "0.4.3"; CI stamps `git describe --tags`, which
// for the same commit is "v0.4.3". Without this, /api/silo/v1/server-info answers
// spelling from a development build and the other from a release, and a client
// comparing versions has to guess which it got. One did, decided the value was
// uncomparable, and silently fell back to probing behaviour instead — the gate
// still present, still correct, and no longer deciding anything.
//
// Only a "v" immediately before a digit is dropped, so the suffixes describe
// adds for untagged or dirty trees ("v0.4.1-3-gabc1234", "-dirty") survive
// intact: they carry real information about what is running.
func normalizeVersion(v string) string {
	if len(v) > 1 && (v[0] == 'v' || v[0] == 'V') && v[1] >= '0' && v[1] <= '9' {
		return v[1:]
	}
	return v
}

func main() {
	args := os.Args[1:]
	if len(args) == 0 {
		printUsage(os.Stderr)
		os.Exit(2)
	}

	Version = normalizeVersion(Version)
	option.Version = Version

	sub, rest := args[0], args[1:]
	switch sub {
	case "serve":
		if err := silod.Run(rest); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	case "gc":
		if err := silod.RunGC(rest); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	case "token":
		if err := silod.RunToken(rest); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	case "user":
		if err := silod.RunUser(rest); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	case "backup-db":
		if err := silod.RunBackupDB(rest); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	case "sentry-test":
		if err := observability.SelfTest(Version, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	case "tui":
		url := serverURL()
		if len(rest) > 0 && rest[0] != "" {
			url = rest[0]
		}
		if err := tui.Run(url, email(), password()); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	case "help", "-h", "--help":
		printUsage(os.Stdout)
	case "version", "-v", "--version":
		fmt.Println(Version)
	default:
		if err := cli.Run(serverURL(), email(), password(), args); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
}

func serverURL() string {
	if v := os.Getenv("SILO_URL"); v != "" {
		return v
	}
	return defaultServerURL
}

func email() string {
	return os.Getenv("SILO_EMAIL")
}

func password() string {
	return os.Getenv("SILO_PASSWORD")
}

func printUsage(w *os.File) {
	_, _ = fmt.Fprint(w, `silo — file sync server and client in one binary

Usage:
  silo serve [-b addr] [flags]    Run the file server daemon
  silo gc [-delete]               Reclaim disk from deleted libraries
  silo backup-db <dir>            Snapshot the databases (server may be running)
  silo sentry-test                Send a test event to $SILO_SENTRY_DSN and report
  silo user list [-json]          Show every account (see "silo user -h")
  silo user add <email>           Create an account
  silo user passwd <email>        Set a password
  silo user disable <email>       Stop every credential the account holds
  silo user enable <email>        Undo a disable
  silo token list <email>         Show a user's sync and API tokens
  silo token revoke <email> [tok] Revoke every token a user holds, or just one
  silo tui [url]                  Launch the interactive terminal UI
  silo repos [--json]             List libraries
  silo repo create <name>         Create a library (prints ID)
  silo repo rm <repo-id>          Delete a library
  silo ls <repo-id> [path] [--json]
  silo get <repo-id> <remote> [local]
  silo put [-r] <repo-id> <local> [remote-dir]
  silo mkdir <repo-id> <path>
  silo rm <repo-id> <path>
  silo mv <repo-id> <src> <dst>
  silo rename <repo-id> <path> <new-name>
  silo changes <repo-id> <since-commit> [--json]
  silo version                    Print the build version

Server environment:
  SILO_DATA_DIR          Data directory (default: ~/.local/share/silo)
  SILO_HOST              Listen address (default: 127.0.0.1)
  SILO_PORT              Listen port (default: 8082)
  SILO_ADMIN_EMAIL       Bootstrap admin email (first run)
  SILO_ADMIN_PASSWORD    Bootstrap admin password (first run)
  SILO_JWT_SECRET        JWT signing key (auto-generated if unset)
  SILO_LOG_LEVEL         Log level: debug, info, warn, error
  SILO_SENTRY_DSN        Send errors, panics and request timings here
                         (SENTRY_DSN also works; unset means send nothing)

Client environment:
  SILO_URL               Server base URL (default: http://localhost:8082)
  SILO_EMAIL             Account email for TUI/CLI
  SILO_PASSWORD          Account password for TUI/CLI

Run "silo serve -h" for server-side flags.

"silo gc" reports what deleting a library left behind and reclaims it with
-delete. It only ever touches libraries that are already deleted, but stop
the server first: nothing locks the data directory.

"silo sentry-test" sends one error and one transaction to the configured DSN
and reports what the receiver said, so a silent tracker can be told apart from
a healthy server that has had nothing to report.

"silo user" is the account lifecycle on the host: creating people, setting
passwords and disabling them, none of which had any path before it but editing
silo.db by hand. Passwords are never taken as flags — a terminal is prompted
with echo off, a pipe is read from stdin, and -generate invents one and prints
it once. Flags come before the subcommand: "silo user -generate add a@b.c".
Disabling an account stops every credential it holds at once; the tokens
themselves survive and work again if it is re-enabled.

"silo backup-db" writes a consistent snapshot of silo.db, safely while the
server runs — copying it with cp loses everything since the last WAL
checkpoint. Copy storage/ afterwards, never before; see docs/backup.md.
`)
}
