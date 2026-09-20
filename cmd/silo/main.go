// Command silo is the unified Silo binary. It dispatches to the fileserver
// daemon (`silo serve`), the Bubble Tea TUI (`silo tui`), or one of the
// non-interactive CLI subcommands (`silo ls`, `silo get`, …).
package main

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/SiloDrive/silo/fileserver" // package silod
	"github.com/SiloDrive/silo/fileserver/option"
	"github.com/SiloDrive/silo/internal/cli"
	"github.com/SiloDrive/silo/internal/observability"
	"github.com/SiloDrive/silo/internal/tui"
	"github.com/SiloDrive/silo/internal/upgrade"
)

const defaultServerURL = "http://localhost:8082"

// Version is stamped at build time via -ldflags "-X main.Version=...".
// The default is the current source-tree version; CI overrides it with
// `git describe --tags --always --dirty` so tagged builds report the tag.
var Version = "0.7.0"

// InstallMethod is stamped at build time via -ldflags
// "-X main.InstallMethod=..." by whatever produced this binary: the Makefile
// says "tarball", packaging/nfpm.yaml says "deb" or "rpm", the PKGBUILD says
// "aur", a Homebrew formula says "homebrew". `silo upgrade` uses it to name the
// package manager that owns this file.
//
// It is stamped rather than detected because nothing observable at runtime
// separates a binary dpkg put at /usr/bin/silo from one somebody copied there,
// and an unstamped build -- plain `go build ./cmd/silo` -- correctly gets the
// general advice rather than a guess.
var InstallMethod = ""

// normalizeVersion drops the leading "v" a git tag carries, so the version this
// binary reports does not depend on how it was built.
//
// The source default is bare — "0.7.0" — while CI stamps `git describe
// --tags`, which for the same commit is "v0.7.0". Without this,
// /api/silo/v1/server-info answers one spelling from a development build and
// the other from a release, and a client
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

// upgradeExit maps a comparison to a process exit status.
//
// Only --check reports through the status at all, and only Behind is a
// non-zero: ComparisonUnknown is what a rate-limited API or an untagged
// development build produces, and neither is something to wake anybody for.
func upgradeExit(check bool, c upgrade.Comparison) int {
	if check && c == upgrade.Behind {
		return 1
	}
	return 0
}

// runUpgrade reports what to type to move from this build to the latest
// release. It changes nothing on disk -- see package upgrade for why.
func runUpgrade(args []string, stdout, stderr io.Writer) int {
	check := false
	for _, a := range args {
		switch a {
		case "--check", "-check":
			check = true
		case "-h", "--help", "help":
			fmt.Fprint(stdout, upgradeUsage)
			return 0
		default:
			fmt.Fprintf(stderr, "silo upgrade: unknown argument %q\n\n%s", a, upgradeUsage)
			return 2
		}
	}

	// Same override install.sh takes, so both can be pointed at a Gitea
	// instance or a mirror without either one being special.
	latestURL := os.Getenv("SILO_LATEST_URL")
	if latestURL == "" {
		latestURL = upgrade.DefaultLatestURL
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	rel, err := upgrade.FetchLatest(ctx, nil, latestURL)
	if err != nil {
		fmt.Fprintf(stderr, "silo upgrade: %v\n", err)
		return 2
	}

	// The stamp alone is not enough: the .deb, the .rpm and silo-bin all ship
	// a binary built by the tarball path, so they carry "tarball". Resolve
	// prefers the marker the installing package manager left at
	// <prefix>/share/silo/install-method.
	exe, _ := os.Executable()
	method := upgrade.Resolve(InstallMethod, exe, upgrade.FileMarker)

	fmt.Fprint(stdout, upgrade.Advise(method, Version, rel, runtime.GOOS, runtime.GOARCH))
	return upgradeExit(check, upgrade.Compare(Version, rel.Tag))
}

const upgradeUsage = `silo upgrade — report how to move to the latest release

Usage:
  silo upgrade            Print the command for this install method
  silo upgrade --check    The same, exiting 1 when a newer release exists

It prints; it never replaces the binary. Where a package manager owns
/usr/bin/silo, writing over that file leaves its database describing something
that is no longer there, so the command belongs to the package manager.

Environment:
  SILO_LATEST_URL  Release API returning JSON with "tag_name" and "assets"
                   (default: the GitHub API; install.sh takes the same value)
`

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
	case "df":
		if err := silod.RunDF(rest); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	case "retention":
		if err := silod.RunRetention(rest); err != nil {
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
	// Local, and it has to have its own case: without one it would fall through
	// to the default and be sent to a server over HTTP, which is precisely what
	// it cannot be. See silod.RunSetupToken.
	case "setup-token":
		if err := silod.RunSetupToken(rest); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	case "migrate":
		if err := silod.RunMigrate(rest); err != nil {
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
		target, err := tuiURL(rest, serverURL())
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		if err := tui.Run(target, email(), password()); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	case "help", "-h", "--help":
		printUsage(os.Stdout)
	case "version", "-v", "--version":
		fmt.Println(Version)
	case "upgrade":
		os.Exit(runUpgrade(rest, os.Stdout, os.Stderr))
	default:
		if err := cli.Run(serverURL(), email(), password(), args); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
}

// tuiURL resolves the server `silo tui` will talk to, from its one optional
// positional and the environment behind it.
//
// It refuses anything that is not an http or https URL, because the mistake it
// is answering is not a typo. `silo tui` is the only subcommand that names a
// deployment by URL; every other one names it by data directory, with -d. So
// `silo tui -d share02` is a reasonable thing to type, and it used to be taken
// as a request to connect to a host called "-d" — which failed later, in the
// network layer, describing a server that was never contacted. A path, a bare
// host and a scheme the client cannot speak all fail the same way.
//
// The URL is returned as written rather than as url.String() renders it: a
// redirect or a certificate can turn on a trailing slash, and the operator's
// spelling is the one they can reason about.
func tuiURL(rest []string, fallback string) (string, error) {
	// An unset positional and an empty one mean the same thing, which is that
	// the environment or the default decides.
	if len(rest) == 0 || (len(rest) == 1 && rest[0] == "") {
		return fallback, nil
	}
	// The flag check runs before the arity check, and over every argument
	// rather than the first. `silo tui -d share02` is two arguments, so an
	// arity complaint is what it would earn, and "takes one server URL" is the
	// least useful of the three things that could be said to somebody who has
	// just typed the flag every other subcommand takes.
	for _, arg := range rest {
		if !strings.HasPrefix(arg, "-") {
			continue
		}
		return "", fmt.Errorf("silo tui takes no flags, and %q is one.\n"+
			"It is a client, so it names the server by URL rather than the data directory by path:\n"+
			"    silo tui https://silo.example.com\n"+
			"-d is how the host-side subcommands name a data directory, as in:\n"+
			"    silo user -d <dir> passwd <email>", arg)
	}

	if len(rest) > 1 {
		return "", fmt.Errorf("silo tui takes one server URL, and got %d arguments: %s",
			len(rest), strings.Join(rest, " "))
	}
	arg := rest[0]

	// url.Parse accepts almost anything, so the scheme and the host are what
	// get checked. Parse lowercases the scheme, which is what RFC 3986 says it
	// means, so HTTPS:// is the same request as https://.
	u, err := url.Parse(arg)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", fmt.Errorf("silo tui needs a server URL beginning with http:// or https://, and got %q.\n"+
			"If that is a data directory, the command that reads one is the host-side CLI, as in:\n"+
			"    silo user -d %s list", arg, arg)
	}
	return arg, nil
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

func printUsage(w io.Writer) {
	_, _ = fmt.Fprint(w, `silo — file sync server and client in one binary

Usage:
  silo serve [-b addr] [flags]    Run the file server daemon
  silo df [-q] [library-id]       Where the disk went: head, history, unreferenced
  silo retention [lib [days]]     Show or set how long a library keeps history
  silo gc [-orphans] [-delete]    Reclaim disk: dead libraries, orphans, old history
  silo gc -compact [-delete]      Rewrite packs without the frames nothing reaches
  silo backup-db <dir>            Snapshot the databases (server may be running)
  silo migrate [-n]               Bring the database to this build's schema (server stopped)
  silo sentry-test                Send a test event to $SILO_SENTRY_DSN and report
  silo setup-token                Print the token that creates the first account
  silo user list [-json]          Show every account (see "silo user -h")
  silo user add <email>           Create an account
  silo user passwd <email>        Set a password
  silo user disable <email>       Stop every credential the account holds
  silo user enable <email>        Undo a disable
  silo user quota <email> [size]  Show the storage cap and usage, or set it
  silo user grant <email> <caps>  Give administrative capabilities
  silo user revoke <email> <caps> Take administrative capabilities away
  silo token list <email>         Show a user's sync and API tokens
  silo token revoke <email> [tok] Revoke every token a user holds, or just one
  silo credential list [--json]   Show the credentials your own account holds
  silo credential revoke <id>     Revoke one of them
  silo credential revoke --others Revoke every other one, keeping this session
  silo tui [url]                  Launch the interactive terminal UI
  silo libraries [--json]         List libraries
  silo library create <name>      Create a library (prints ID)
  silo library rm <library-id>    Delete a library
  silo ls <library-id> [path] [--json]
  silo get <library-id> <remote> [local]
  silo put [-r] <library-id> <local> [remote-dir]
  silo mkdir <library-id> <path>
  silo rm <library-id> <path>
  silo mv <library-id> <src> <dst>
  silo rename <library-id> <path> <new-name>
  silo changes <library-id> <since-commit> [--json]
  silo version                    Print the build version
  silo upgrade [--check]          Report how to move to the latest release

Server environment:
  SILO_DATA_DIR          Data directory (default: ~/.local/share/silo)
  SILO_HOST              Listen address (default: 127.0.0.1)
  SILO_PORT              Listen port (default: 8082)
                         "silo serve -b" overrides both: -b takes a host
                         ("-b 0.0.0.0") or a host and port ("-b 0.0.0.0:8003").
                         A bare "-b :8003" is refused as ambiguous; name the
                         host, or use SILO_PORT to move only the port.
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

"silo gc -compact" rewrites sealed packs without the frames no commit reaches,
which is the only way space comes back from a pack: a sealed pack is immutable,
so expiry and the orphan sweep report what they could not delete and leave it.
A full reclaim is "silo gc -expire-history -orphans -compact -delete", in that
order, in one run. -compact-threshold sets the dead fraction worth rewriting
(default 0.5) and -compact-budget caps the live bytes one run copies.

-compact -delete is offline, and here that is a rule rather than advice: the
server holds its pack set in memory and opens a sealed pack by path, so a
rewrite from a second process moves frames the server still believes it can
find, and the next read of one is a 404 to a client that stored it.

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

"silo credential" is the self-service half of "silo token": the same table,
asked of the server over HTTP by the person who owns it rather than read from
silo.db by an operator on the host. That is the difference that matters when a
laptop is stolen -- the machine you want to revoke is the one you cannot revoke
it from, and this works from any machine with the binary and the password.
It signs its own session out when it finishes, so listing credentials does not
add one.

"--others" is what to reach for after losing a laptop: it signs every other
host out and leaves the machine you are typing on alone. Changing a password
does NOT do this any more -- rotating a password is not evidence anything was
stolen, and unmounting somebody's laptop, NAS and phone because they improved a
password is how people learn not to improve passwords. If you changed your
password because you were worried about something, run this as well.

"silo backup-db" writes a consistent snapshot of silo.db, safely while the
server runs — copying it with cp loses everything since the last WAL
checkpoint. Copy storage/ afterwards, never before; see docs/backup.md.
`)
}
