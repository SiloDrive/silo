// Package cli implements the non-interactive Silo command-line subcommands.
//
// It wraps client.APIClient with flag parsing, plain-text (default) or JSON
// output, and minimal positional-argument validation. The top-level
// dispatcher in cmd/silo routes any non-{serve,tui,help} argument here.
package cli

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/SiloDrive/silo/client"
)

func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard) // subcommand usage isn't printed; errors bubble up
	return fs
}

// commands is the client-side verb set. It is a table rather than a switch so
// that a name can be checked before anything is done about it: the credential
// gate below used to answer first, which reported `silo service` -- serve,
// misspelt, arriving here because everything that is not a daemon verb does --
// as a missing account rather than as a word that is not a command.
var commands = map[string]func(*client.APIClient, []string) error{
	"libraries": cmdLibraries,
	"library":   cmdLibrary,
	"ls":        cmdLs,
	"get":       cmdGet,
	"put":       cmdPut,
	"mkdir":     cmdMkdir,
	"rm":        cmdRm,
	"mv":        cmdMv,
	"rename":    cmdRename,
	"changes":   cmdChanges,
	// Self-service, and distinct from `silo token`, which is the operator
	// reading the same table on the host. See credential.go.
	"credential": cmdCredential,
}

// Run executes a single CLI subcommand. args[0] is the subcommand name; the
// rest are its arguments and flags. It logs in using email+password before
// each operation.
func Run(serverURL, email, password string, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("no subcommand given")
	}

	sub, rest := args[0], args[1:]
	cmd, ok := commands[sub]
	if !ok {
		return fmt.Errorf("unknown subcommand: %s (run \"silo help\" for the list)", sub)
	}

	if email == "" || password == "" {
		return fmt.Errorf("SILO_EMAIL and SILO_PASSWORD must be set")
	}

	c := client.NewClient(serverURL)
	if err := c.Login(email, password); err != nil {
		return fmt.Errorf("login: %w", err)
	}

	return cmd(c, rest)
}

func cmdLibraries(c *client.APIClient, args []string) error {
	fs := newFlagSet("libraries")
	jsonOut := fs.Bool("json", false, "output as JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	libraries, err := c.ListLibraries()
	if err != nil {
		return err
	}
	if *jsonOut {
		return printJSON(os.Stdout, libraries)
	}
	printLibrariesText(os.Stdout, libraries)
	return nil
}

func cmdLibrary(c *client.APIClient, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: silo library <create|rm>")
	}
	switch args[0] {
	case "create":
		if len(args) < 2 {
			return fmt.Errorf("usage: silo library create <name>")
		}
		library, err := c.CreateLibrary(args[1])
		if err != nil {
			return err
		}
		fmt.Println(library.ID)
		return nil
	case "rm":
		if len(args) < 2 {
			return fmt.Errorf("usage: silo library rm <library-id>")
		}
		libraryID, err := resolveLibrary(c, args[1])
		if err != nil {
			return err
		}
		return c.DeleteLibrary(libraryID)
	default:
		return fmt.Errorf("unknown library subcommand: %s", args[0])
	}
}

// libraryIDPattern is the shape CreateLibrary hands out: a UUID. Anything matching
// it is taken as an id and used directly, and anything else is looked up as a
// name — so a library really called "4e5d525b-38a0-4198-95c7-2fde63a9b91d"
// would be unreachable by name, which is a trade worth making against sending
// every argument through an extra request.
var libraryIDPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// resolveLibrary turns what someone typed into a library id.
//
// Names are not unique — nothing stops two libraries being called "Photos" —
// so an ambiguous name is an error that lists the candidates rather than a
// guess. Picking the first would work for months and then quietly write to the
// wrong library on the day a second one appeared.
func resolveLibrary(c *client.APIClient, arg string) (string, error) {
	if libraryIDPattern.MatchString(arg) {
		return arg, nil
	}
	libraries, err := c.ListLibraries()
	if err != nil {
		return "", err
	}
	var matches []client.Library
	for _, library := range libraries {
		if library.Name == arg {
			matches = append(matches, library)
		}
	}
	switch len(matches) {
	case 1:
		return matches[0].ID, nil
	case 0:
		return "", fmt.Errorf("no library called %q; `silo libraries` lists them", arg)
	default:
		ids := make([]string, 0, len(matches))
		for _, library := range matches {
			ids = append(ids, library.ID)
		}
		return "", fmt.Errorf("%d libraries are called %q; name one by id: %s",
			len(matches), arg, strings.Join(ids, ", "))
	}
}

func cmdLs(c *client.APIClient, args []string) error {
	fs := newFlagSet("ls")
	jsonOut := fs.Bool("json", false, "output as JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	rest := fs.Args()
	if len(rest) < 1 {
		return fmt.Errorf("usage: silo ls [--json] <library-id> [path]")
	}
	libraryID, err := resolveLibrary(c, rest[0])
	if err != nil {
		return err
	}
	path := "/"
	if len(rest) >= 2 {
		path = rest[1]
	}
	entries, err := c.ListDir(libraryID, path)
	if err != nil {
		return err
	}
	if *jsonOut {
		return printJSON(os.Stdout, entries)
	}
	printDirText(os.Stdout, entries)
	return nil
}

func cmdGet(c *client.APIClient, args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("usage: silo get <library-id> <remote-path> [local-path]")
	}
	libraryID, err := resolveLibrary(c, args[0])
	if err != nil {
		return err
	}
	remote := args[1]
	local := filepath.Base(remote)
	if len(args) >= 3 {
		local = args[2]
	}
	return c.DownloadFile(libraryID, remote, local)
}

func cmdPut(c *client.APIClient, args []string) error {
	fs := newFlagSet("put")
	recursive := fs.Bool("r", false, "upload a directory and everything under it")
	quiet := fs.Bool("q", false, "with -r, print the summary but not each file")
	jsonOut := fs.Bool("json", false, "with -r, print the summary as JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	rest := fs.Args()
	if len(rest) < 2 {
		return fmt.Errorf("usage: silo put [-r] [-q] [--json] <library-id> <local-path> [remote-dir]")
	}
	libraryID, err := resolveLibrary(c, rest[0])
	if err != nil {
		return err
	}
	local := rest[1]
	parentDir := "/"
	if len(rest) >= 3 {
		parentDir = rest[2]
	}

	info, err := os.Stat(local)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return c.UploadFile(libraryID, parentDir, local)
	}
	// Without -r this used to reach the HTTP layer and come back as
	// "read <dir>: is a directory", which says what failed but not what to do.
	if !*recursive {
		return fmt.Errorf("%s is a directory; pass -r to upload it and everything under it", local)
	}

	onFile := func(remote string) { fmt.Println(remote) }
	if *quiet || *jsonOut {
		onFile = nil
	}

	up, err := c.UploadDir(libraryID, parentDir, local, onFile)
	// A failure that got partway still reports what landed — that is what tells
	// the caller whether to re-run or go looking. A failure that wrote nothing
	// says only why.
	if up != nil && (err == nil || up.Commits > 0) {
		if *jsonOut {
			if perr := printJSON(os.Stdout, up); perr != nil {
				return perr
			}
		} else {
			printTreeUpload(os.Stdout, up)
		}
	}
	return err
}

// printTreeUpload reports the shape of the transfer rather than only its size:
// chunks held is content the server already had, which is the whole reason
// this is not a loop over single-file uploads.
func printTreeUpload(w io.Writer, up *client.TreeUpload) {
	for _, path := range up.Skipped {
		_, _ = fmt.Fprintf(w, "skipped %s: not a regular file\n", path)
	}
	_, _ = fmt.Fprintf(w, "%s in %s, %s across %s\n",
		plural(up.Files, "file"), plural(up.Dirs, "directory", "directories"),
		plural(up.ChunksSent, "chunk"), plural(up.Commits, "commit"))
	if up.ChunksHeld > 0 {
		_, _ = fmt.Fprintf(w, "%s already on the server, not sent\n", plural(up.ChunksHeld, "chunk"))
	}
}

// plural spells a count with its noun, taking the plural form when the English
// is not just an "s" away.
func plural(n int, one string, many ...string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	if len(many) > 0 {
		return fmt.Sprintf("%d %s", n, many[0])
	}
	return fmt.Sprintf("%d %ss", n, one)
}

func cmdMkdir(c *client.APIClient, args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("usage: silo mkdir <library-id> <path>")
	}
	libraryID, err := resolveLibrary(c, args[0])
	if err != nil {
		return err
	}
	return c.Mkdir(libraryID, args[1])
}

func cmdRm(c *client.APIClient, args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("usage: silo rm <library-id> <path>")
	}
	libraryID, err := resolveLibrary(c, args[0])
	if err != nil {
		return err
	}
	return c.DeleteFile(libraryID, args[1])
}

func cmdMv(c *client.APIClient, args []string) error {
	if len(args) < 3 {
		return fmt.Errorf("usage: silo mv <library-id> <src> <dst>")
	}
	libraryID, err := resolveLibrary(c, args[0])
	if err != nil {
		return err
	}
	return c.MoveFile(libraryID, args[1], args[2])
}

func cmdRename(c *client.APIClient, args []string) error {
	if len(args) < 3 {
		return fmt.Errorf("usage: silo rename <library-id> <path> <new-name>")
	}
	libraryID, err := resolveLibrary(c, args[0])
	if err != nil {
		return err
	}
	return c.RenameFile(libraryID, args[1], args[2])
}

// cmdChanges exists mostly so the delta endpoint can be exercised by hand. A
// sync client is the real consumer, but an endpoint no human can call is an
// endpoint no human can debug.
func cmdChanges(c *client.APIClient, args []string) error {
	fs := newFlagSet("changes")
	jsonOut := fs.Bool("json", false, "output as JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	rest := fs.Args()
	if len(rest) < 2 {
		return fmt.Errorf("usage: silo changes [--json] <library-id> <since-commit>")
	}
	libraryID, err := resolveLibrary(c, rest[0])
	if err != nil {
		return err
	}
	resp, err := c.Changes(libraryID, rest[1])
	if err != nil {
		return err
	}
	if *jsonOut {
		return printJSON(os.Stdout, resp)
	}
	printChangesText(os.Stdout, resp)
	return nil
}
