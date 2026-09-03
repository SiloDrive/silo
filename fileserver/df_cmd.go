package silod

import (
	"fmt"
	"github.com/dkam/silo/fileserver/diskfree"
	"github.com/dkam/silo/fileserver/libmgr"
	"github.com/dkam/silo/fileserver/option"
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
	packs := flags.Bool("packs", false, "report per-pack occupancy and dead fraction instead")
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
	if *packs {
		return reportPacks(ids, *quiet)
	}
	if len(ids) == 0 {
		// Still worth the footer: a server with no libraries has a disk, and
		// "how much room is there" is a fair question to ask before putting
		// anything on it.
		fmt.Println("No libraries.")
		printServerHeadroom()
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

	printServerHeadroom()

	if total.History.Bytes > 0 || total.Unreferenced.Bytes > 0 {
		fmt.Printf("\nhistory is what a retention policy would reclaim; unreferenced is reclaimable now.\n" +
			"Neither is collected yet -- see docs/quota.md.\n")
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d libraries could not be measured", failed, len(ids))
	}
	return nil
}

// printServerHeadroom says how much more this server will accept, which is
// the question the three columns above do not answer.
//
// It is here rather than in a command of its own because an operator asking
// where the disk went is one refusal away from asking why a sync stopped, and
// the answer to the second is not in a census: a write is refused at the lower
// of the configured ceiling and free space less the reserve, and neither of
// those is a figure any column above reports.
//
// The numbers are deliberately in two currencies and labelled as such. Stored
// bytes above, logical-at-head against the ceiling, and physical free space --
// see checkServerLimits for why they are not reconciled.
func printServerHeadroom() {
	fmt.Println()
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)

	if free, err := diskfree.Available(absDataDir); err != nil {
		fmt.Fprintf(w, "free on disk\t--\t(%v)\n", err)
	} else if option.DiskReserve > 0 {
		fmt.Fprintf(w, "free on disk\t%s\t(keeping %s back, so %s admissible)\n",
			formatBytes(free), formatBytes(option.DiskReserve),
			formatBytes(max(free-option.DiskReserve, 0)))
	} else {
		fmt.Fprintf(w, "free on disk\t%s\t(no reserve set)\n", formatBytes(free))
	}

	if option.ServerQuota > 0 {
		used, err := libmgr.ServerUsage()
		if err != nil {
			fmt.Fprintf(w, "server ceiling\t%s\t(usage unreadable: %v)\n", formatBytes(option.ServerQuota), err)
		} else {
			fmt.Fprintf(w, "server ceiling\t%s\t(%s used, %s admissible; logical-at-head)\n",
				formatBytes(option.ServerQuota), formatBytes(used.Size),
				formatBytes(max(option.ServerQuota-used.Size, 0)))
		}
	} else {
		fmt.Fprintf(w, "server ceiling\tnone\t(set [quota] server to bound what everybody together may hold)\n")
	}
	_ = w.Flush()
}

func addExtent(a, b objmgr.Extent) objmgr.Extent {
	return objmgr.Extent{Bytes: a.Bytes + b.Bytes, Objects: a.Objects + b.Objects}
}

func censusTotal(c objmgr.Census) int64 {
	return c.Head.Bytes + c.History.Bytes + c.Unreferenced.Bytes
}

// censusOf measures one library by id.
// reportPacks is "where did the disk go" asked one pack at a time.
//
// The same three-way partition the table above prints, attributed to the packs
// holding it, plus the dead fraction — which is the number compaction is
// scheduled on and the only one here an operator can act on directly.
//
// Deliberately a separate view rather than more columns on the main table. A
// library has one census and many packs, so the two do not share a row, and a
// store that has not been packed has nothing to say here at all.
func reportPacks(ids []string, quiet bool) error {
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	if !quiet {
		fmt.Fprintln(w, "LIBRARY\tPACK\tHEAD\tLIVE\tDEAD\tDEAD%\tON DISK")
	}

	var frames, live, dead, onDisk int64
	var counted, failed int
	for _, id := range ids {
		_, st, head, err := openLibraryAtHead(id)
		if err != nil {
			fmt.Fprintf(w, "%s\t--\t--\t--\t--\t--\t(%v)\n", id, err)
			failed++
			continue
		}
		stats, err := st.PackCensus(head)
		if err != nil {
			fmt.Fprintf(w, "%s\t--\t--\t--\t--\t--\t(%v)\n", id, err)
			failed++
			continue
		}
		for _, p := range stats {
			counted++
			frames += p.FrameBytes
			live += p.LiveBytes
			dead += p.DeadBytes()
			onDisk += p.FileBytes
			if !quiet {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%.0f%%\t%s\n",
					id, p.PackID[:12],
					formatBytes(p.HeadBytes), formatBytes(p.LiveBytes),
					formatBytes(p.DeadBytes()), p.DeadFraction()*100,
					formatBytes(p.FileBytes))
			}
		}
	}

	if counted == 0 && failed == 0 {
		if err := w.Flush(); err != nil {
			return err
		}
		// Not an error, and worth saying plainly rather than printing an empty
		// table: this is every server until packs become the write path.
		fmt.Println("No sealed packs. This store writes one file per object; " +
			"see docs/plans/packs.md.")
		return nil
	}

	if !quiet {
		fmt.Fprintln(w, "\t\t\t\t\t\t")
	}
	var pct float64
	if frames > 0 {
		pct = float64(dead) / float64(frames) * 100
	}
	fmt.Fprintf(w, "TOTAL\t%d packs\t\t%s\t%s\t%.0f%%\t%s\n",
		counted, formatBytes(live), formatBytes(dead), pct, formatBytes(onDisk))
	if err := w.Flush(); err != nil {
		return err
	}

	if dead > 0 {
		// Said out loud because the number is otherwise misleading: these
		// bytes are reclaimable in principle and nothing reclaims them yet.
		fmt.Printf("\ndead bytes are frames no commit reaches, still inside sealed packs.\n" +
			"Stop the server and run `silo gc -compact -delete` to rewrite those packs without them.\n")
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d libraries could not be measured", failed, len(ids))
	}
	return nil
}

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
