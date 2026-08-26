package silod

import (
	"fmt"
	"text/tabwriter"

	"os"

	"github.com/dkam/silo/fileserver/objmgr"
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
//     only by `silo retention` and the expiry pass behind it; this is the
//     number that says what a shorter window would buy.
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
	_, st, head, err := openLibraryAtHead(id)
	if err != nil {
		return objmgr.Census{}, err
	}
	return st.Census(head)
}

// libraryIDsForDF is every library that exists, or the one that was named.
//
// A named library is taken as given rather than looked up, because naming one
// is how an operator measures a library the catalog has lost track of, and a
// lookup would refuse exactly that case.
//
// Everything else defers to liveLibraryIDs, which is the collector's list. It
// used to be its own query over LibraryOwner, which agrees with Library on any
// healthy server -- the two rows are written in one transaction and deleted in
// another -- and that agreement is what made the duplication safe to keep and
// impossible to notice. It is not worth keeping: df and gc reporting on
// different sets of libraries is the one thing an operator reading both
// outputs cannot be expected to catch.
func libraryIDsForDF(rest []string) ([]string, error) {
	if len(rest) == 1 {
		return rest, nil
	}
	return liveLibraryIDs()
}
