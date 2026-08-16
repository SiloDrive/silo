// Command silo is the unified Silo binary. It dispatches to the fileserver
// daemon (`silo serve`), the Bubble Tea TUI (`silo tui`), or one of the
// non-interactive CLI subcommands (`silo ls`, `silo get`, …).
package main

import (
	"fmt"
	"os"

	"github.com/dkam/silo/fileserver" // package silod
	"github.com/dkam/silo/internal/cli"
	"github.com/dkam/silo/internal/tui"
)

const defaultServerURL = "http://localhost:8082"

// Version is stamped at build time via -ldflags "-X main.Version=...".
// The default is the current source-tree version; CI overrides it with
// `git describe --tags --always --dirty` so tagged builds report the tag.
var Version = "0.3.22"

func main() {
	args := os.Args[1:]
	if len(args) == 0 {
		printUsage(os.Stderr)
		os.Exit(2)
	}

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
	case "backup-db":
		if err := silod.RunBackupDB(rest); err != nil {
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
	_, _ = fmt.Fprint(w, `silo — Seafile-compatible server and client in one binary

Usage:
  silo serve [flags]              Run the file server daemon
  silo gc [-delete]               Reclaim disk from deleted libraries
  silo backup-db <dir>            Snapshot the databases (server may be running)
  silo token list <email>         Show a user's sync and API tokens
  silo token revoke <email> [tok] Revoke every token a user holds, or just one
  silo tui [url]                  Launch the interactive terminal UI
  silo repos [--json]             List libraries
  silo repo create <name>         Create a library (prints ID)
  silo repo rm <repo-id>          Delete a library
  silo ls <repo-id> [path] [--json]
  silo get <repo-id> <remote> [local]
  silo put <repo-id> <local> [remote-dir]
  silo mkdir <repo-id> <path>
  silo rm <repo-id> <path>
  silo mv <repo-id> <src> <dst>
  silo rename <repo-id> <path> <new-name>
  silo version                    Print the build version

Server environment:
  SILO_DATA_DIR          Data directory (default: ~/.local/share/silo)
  SILO_HOST              Listen address (default: 0.0.0.0)
  SILO_PORT              Listen port (default: 8082)
  SILO_ADMIN_EMAIL       Bootstrap admin email (first run)
  SILO_ADMIN_PASSWORD    Bootstrap admin password (first run)
  SILO_JWT_SECRET        JWT signing key (auto-generated if unset)
  SILO_LOG_LEVEL         Log level: debug, info, warn, error

Client environment:
  SILO_URL               Server base URL (default: http://localhost:8082)
  SILO_EMAIL             Account email for TUI/CLI
  SILO_PASSWORD          Account password for TUI/CLI

Run "silo serve -h" for server-side flags.

"silo gc" reports what deleting a library left behind and reclaims it with
-delete. It only ever touches libraries that are already deleted, but stop
the server first: nothing locks the data directory.

"silo backup-db" writes a consistent snapshot of ccnet.db and seafile.db,
safely while the server runs — copying them with cp loses everything since
the last WAL checkpoint. Copy storage/ afterwards, never before; see
docs/backup.md.
`)
}
