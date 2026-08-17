package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/dkam/silo/client"
	"github.com/dkam/silo/internal/format"
)

func printJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func printReposText(w io.Writer, repos []client.Repo) {
	for _, r := range repos {
		name := r.Name
		if name == "" {
			name = "(unnamed)"
		}
		updated := ""
		if r.UpdateTime > 0 {
			updated = time.Unix(r.UpdateTime, 0).Format("2006-01-02 15:04")
		}
		enc := ""
		if r.Encrypted {
			enc = " [encrypted]"
		}
		_, _ = fmt.Fprintf(w, "%s  %-16s  %s%s\n", r.ID, updated, name, enc)
	}
}

func printDirText(w io.Writer, entries []client.DirEntry) {
	for _, e := range entries {
		kind := "f"
		if e.Type == "dir" {
			kind = "d"
		}
		size := "-"
		if e.Type != "dir" {
			size = format.Bytes(e.Size)
		}
		mtime := ""
		if e.Mtime > 0 {
			mtime = time.Unix(e.Mtime, 0).Format("2006-01-02 15:04")
		}
		_, _ = fmt.Fprintf(w, "%s  %10s  %-16s  %s\n", kind, size, mtime, e.Name)
	}
}

// printChangesText prints one change per line, with the anchor last so it is
// still on screen after a long list — it is the one value the caller has to
// keep for the next call.
func printChangesText(w io.Writer, resp *client.ChangesResponse) {
	for _, ch := range resp.Changes {
		kind := "f"
		if ch.IsDir {
			kind = "d"
		}
		path := ch.Path
		if ch.OldPath != "" {
			path = ch.OldPath + " -> " + ch.Path
		}
		_, _ = fmt.Fprintf(w, "%-6s %s  %s\n", ch.Op, kind, path)
	}
	_, _ = fmt.Fprintf(w, "\nanchor: %s (%d change(s))\n", resp.Anchor, len(resp.Changes))
}
