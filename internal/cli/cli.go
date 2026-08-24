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

	"github.com/dkam/silo/client"
)

func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard) // subcommand usage isn't printed; errors bubble up
	return fs
}

// Run executes a single CLI subcommand. args[0] is the subcommand name; the
// rest are its arguments and flags. It logs in using email+password before
// each operation.
func Run(serverURL, email, password string, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("no subcommand given")
	}
	if email == "" || password == "" {
		return fmt.Errorf("SILO_EMAIL and SILO_PASSWORD must be set")
	}

	c := client.NewClient(serverURL)
	if err := c.Login(email, password); err != nil {
		return fmt.Errorf("login: %w", err)
	}

	sub, rest := args[0], args[1:]
	switch sub {
	case "repos":
		return cmdRepos(c, rest)
	case "repo":
		return cmdRepo(c, rest)
	case "ls":
		return cmdLs(c, rest)
	case "get":
		return cmdGet(c, rest)
	case "put":
		return cmdPut(c, rest)
	case "mkdir":
		return cmdMkdir(c, rest)
	case "rm":
		return cmdRm(c, rest)
	case "mv":
		return cmdMv(c, rest)
	case "rename":
		return cmdRename(c, rest)
	case "changes":
		return cmdChanges(c, rest)
	default:
		return fmt.Errorf("unknown subcommand: %s", sub)
	}
}

func cmdRepos(c *client.APIClient, args []string) error {
	fs := newFlagSet("repos")
	jsonOut := fs.Bool("json", false, "output as JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	repos, err := c.ListRepos()
	if err != nil {
		return err
	}
	if *jsonOut {
		return printJSON(os.Stdout, repos)
	}
	printReposText(os.Stdout, repos)
	return nil
}

func cmdRepo(c *client.APIClient, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: silo repo <create|rm>")
	}
	switch args[0] {
	case "create":
		if len(args) < 2 {
			return fmt.Errorf("usage: silo repo create <name>")
		}
		repo, err := c.CreateRepo(args[1])
		if err != nil {
			return err
		}
		fmt.Println(repo.ID)
		return nil
	case "rm":
		if len(args) < 2 {
			return fmt.Errorf("usage: silo repo rm <repo-id>")
		}
		return c.DeleteRepo(args[1])
	default:
		return fmt.Errorf("unknown repo subcommand: %s", args[0])
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
		return fmt.Errorf("usage: silo ls [--json] <repo-id> [path]")
	}
	repoID := rest[0]
	path := "/"
	if len(rest) >= 2 {
		path = rest[1]
	}
	entries, err := c.ListDir(repoID, path)
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
		return fmt.Errorf("usage: silo get <repo-id> <remote-path> [local-path]")
	}
	repoID := args[0]
	remote := args[1]
	local := filepath.Base(remote)
	if len(args) >= 3 {
		local = args[2]
	}
	return c.DownloadFile(repoID, remote, local)
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
		return fmt.Errorf("usage: silo put [-r] [-q] [--json] <repo-id> <local-path> [remote-dir]")
	}
	repoID := rest[0]
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
		return c.UploadFile(repoID, parentDir, local)
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

	up, err := c.UploadDir(repoID, parentDir, local, onFile)
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
		fmt.Fprintf(w, "skipped %s: not a regular file\n", path)
	}
	fmt.Fprintf(w, "%s in %s, %s across %s\n",
		plural(up.Files, "file"), plural(up.Dirs, "directory", "directories"),
		plural(up.ChunksSent, "chunk"), plural(up.Commits, "commit"))
	if up.ChunksHeld > 0 {
		fmt.Fprintf(w, "%s already on the server, not sent\n", plural(up.ChunksHeld, "chunk"))
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
		return fmt.Errorf("usage: silo mkdir <repo-id> <path>")
	}
	return c.Mkdir(args[0], args[1])
}

func cmdRm(c *client.APIClient, args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("usage: silo rm <repo-id> <path>")
	}
	return c.DeleteFile(args[0], args[1])
}

func cmdMv(c *client.APIClient, args []string) error {
	if len(args) < 3 {
		return fmt.Errorf("usage: silo mv <repo-id> <src> <dst>")
	}
	return c.MoveFile(args[0], args[1], args[2])
}

func cmdRename(c *client.APIClient, args []string) error {
	if len(args) < 3 {
		return fmt.Errorf("usage: silo rename <repo-id> <path> <new-name>")
	}
	return c.RenameFile(args[0], args[1], args[2])
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
		return fmt.Errorf("usage: silo changes [--json] <repo-id> <since-commit>")
	}
	resp, err := c.Changes(rest[0], rest[1])
	if err != nil {
		return err
	}
	if *jsonOut {
		return printJSON(os.Stdout, resp)
	}
	printChangesText(os.Stdout, resp)
	return nil
}
