package silod

import (
	"context"
	"fmt"
	"sort"
	"text/tabwriter"

	"os"

	"github.com/dkam/silo/fileserver/libmgr"
	"github.com/dkam/silo/fileserver/objmgr"
	"github.com/dkam/silo/fileserver/option"
	storefmt "github.com/dkam/silo/store"
)

// RunDF reports where a server's disk has gone, per library.
//
// Quota answers a different question and answers it correctly: it charges
// logical size at head, so a user can predict it and free it by deleting
// files. What it cannot do is explain a full disk. A library whose one file is
// rewritten every day reports a flat usage forever while the store grows every
// day, and until this there was nothing to point at.
//
// Three columns, because they want three different actions:
//
//   - head — what the library holds now. Reclaimed by deleting files.
//   - history — reachable from an older commit but not from head. Reclaimed
//     only by a retention policy, which does not exist yet; this is the number
//     that says whether one is worth having.
//   - unreferenced — reachable from nothing. Interrupted uploads and abandoned
//     commits. Reclaimable now, with no policy attached, which is why it is a
//     column of its own rather than part of history.
//
// It reports and never deletes. That is deliberate for a first cut: the walk
// this depends on is the same mark phase a collector needs, and running it in
// a form that only ever prints is how the numbers get checked against reality
// before anything is removed on the strength of them.
//
// The figures are stored bytes, not logical ones, and will not match
// account/usage. They are not meant to — see docs/quota.md.
func RunDF(args []string) error {
	flags := commandFlags("df")
	quiet := flags.Bool("q", false, "only print the totals")
	rest, done, err := parseCommandArgs("df", flags, args)
	if err != nil || done {
		return err
	}
	if len(rest) > 1 {
		return fmt.Errorf("usage: silo df [-d datadir] [-C config] [-q] [library-id]")
	}

	if err := openStores(); err != nil {
		return err
	}

	ids, err := libraryIDsForDF(rest)
	if err != nil {
		return err
	}
	if len(ids) == 0 {
		fmt.Println("No libraries.")
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	if !*quiet {
		fmt.Fprintln(w, "LIBRARY\tHEAD\tHISTORY\tUNREFERENCED\tTOTAL")
	}

	var total objmgr.Census
	var failed int
	for _, id := range ids {
		c, err := censusOf(id)
		if err != nil {
			// One unreadable library must not cost the report for the others:
			// a census is what somebody runs when they already suspect
			// something is wrong, and the run that refuses to say anything
			// about the rest is the least useful possible response to that.
			fmt.Fprintf(w, "%s\t--\t--\t--\t(%v)\n", id, err)
			failed++
			continue
		}
		total.Head = addExtent(total.Head, c.Head)
		total.History = addExtent(total.History, c.History)
		total.Unreferenced = addExtent(total.Unreferenced, c.Unreferenced)
		if !*quiet {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", id,
				formatBytes(c.Head.Bytes), formatBytes(c.History.Bytes),
				formatBytes(c.Unreferenced.Bytes), formatBytes(censusTotal(c)))
		}
	}

	if !*quiet {
		fmt.Fprintln(w, "\t\t\t\t")
	}
	fmt.Fprintf(w, "TOTAL\t%s\t%s\t%s\t%s\n",
		formatBytes(total.Head.Bytes), formatBytes(total.History.Bytes),
		formatBytes(total.Unreferenced.Bytes), formatBytes(censusTotal(total)))
	if err := w.Flush(); err != nil {
		return err
	}

	if total.History.Bytes > 0 || total.Unreferenced.Bytes > 0 {
		fmt.Printf("\nhistory is what a retention policy would reclaim; unreferenced is reclaimable now.\n" +
			"Neither is collected yet -- see docs/quota.md.\n")
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d libraries could not be measured", failed, len(ids))
	}
	return nil
}

func addExtent(a, b objmgr.Extent) objmgr.Extent {
	return objmgr.Extent{Bytes: a.Bytes + b.Bytes, Objects: a.Objects + b.Objects}
}

func censusTotal(c objmgr.Census) int64 {
	return c.Head.Bytes + c.History.Bytes + c.Unreferenced.Bytes
}

// censusOf measures one library by id.
func censusOf(id string) (objmgr.Census, error) {
	library, err := libmgr.GetWithReason(id)
	if err != nil {
		return objmgr.Census{}, err
	}
	st, err := library.Store()
	if err != nil {
		return objmgr.Census{}, err
	}
	head, err := storefmt.ParseID(library.HeadCommitID)
	if err != nil {
		return objmgr.Census{}, fmt.Errorf("head commit %q: %w", library.HeadCommitID, err)
	}
	return st.Census(head)
}

// libraryIDsForDF is every library the server owns, or the one that was named.
func libraryIDsForDF(rest []string) ([]string, error) {
	if len(rest) == 1 {
		return rest, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), option.DBOpTimeout*2)
	defer cancel()

	rows, err := siloPair.Read.QueryContext(ctx, "SELECT library_id FROM LibraryOwner")
	if err != nil {
		return nil, fmt.Errorf("failed to list libraries: %v", err)
	}
	defer func() { _ = rows.Close() }()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("failed to scan a library row: %v", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to list libraries: %v", err)
	}
	sort.Strings(ids)
	return ids, nil
}
