package silod

import (
	"fmt"
	"os"
	"strconv"
	"text/tabwriter"

	"github.com/dkam/silo/fileserver/libmgr"
	"github.com/dkam/silo/fileserver/option"
)

// RunRetention reads and sets how long a library keeps history.
//
// A CLI rather than an API for the reason RunUser gives: there is no admin
// role to gate an endpoint with, and it is a server-side setting rather than
// something a library's owner decides — retention is what bounds the
// operator's disk.
//
// One name, three operations, the same shape `silo user quota` settled on:
// asking and answering are the same question with and without an answer, and
// splitting them means an operator who typed the reading form with a value
// gets a usage message instead of the change they asked for.
func RunRetention(args []string) error {
	flags := commandFlags("retention")
	rest, done, err := parseCommandArgs("retention", flags, args)
	if err != nil || done {
		return err
	}
	if len(rest) > 2 {
		return fmt.Errorf("usage: silo retention [library-id [days|keep-all|default]]")
	}

	if err := openStores(); err != nil {
		return err
	}

	if len(rest) == 0 {
		return reportAllRetention()
	}
	libraryID := rest[0]
	if len(rest) == 1 {
		return reportRetention(libraryID)
	}
	return setRetention(libraryID, rest[1])
}

func setRetention(libraryID, value string) error {
	if _, err := libmgr.GetWithReason(libraryID); err != nil {
		return err
	}

	switch value {
	case "default", "inherit":
		if err := libmgr.ClearRetentionDays(libraryID); err != nil {
			return err
		}
		fmt.Printf("%s now follows the server default (%s).\n", libraryID, describeDays(option.DefaultKeepDays))
		return nil
	case "keep-all", "forever", "all":
		if err := libmgr.SetRetentionDays(libraryID, 0); err != nil {
			return err
		}
		fmt.Printf("%s now keeps all history, whatever the server default says.\n", libraryID)
		return nil
	}

	// A unit is not required here, unlike a quota size, because there is only
	// one unit history is ever expressed in and the command name says it. What
	// is refused is anything that is not a whole number of days: a value the
	// parser cannot read must never fall through to a number, because every
	// number here deletes something.
	days, err := strconv.Atoi(value)
	if err != nil || days < 0 {
		return fmt.Errorf("%q is not a number of days; write a whole number like 30, "+
			"or \"keep-all\" to keep everything, or \"default\" to follow the server setting", value)
	}
	if days == 0 {
		// Rejected rather than silently meaning keep-all, because an operator
		// typing 0 could as easily mean "keep nothing", and the two are
		// opposites. keep-all says which one out loud.
		return fmt.Errorf("0 is ambiguous here: write \"keep-all\" to keep every commit, " +
			"or a number of days to expire what is older")
	}
	if err := libmgr.SetRetentionDays(libraryID, days); err != nil {
		return err
	}
	fmt.Printf("%s now keeps %d day%s of history.\n", libraryID, days, plural(days))
	fmt.Println("Nothing expires until you run: silo gc -expire-history -delete")
	return nil
}

func reportRetention(libraryID string) error {
	if _, err := libmgr.GetWithReason(libraryID); err != nil {
		return err
	}
	days, err := libmgr.RetentionDays(libraryID)
	if err != nil {
		return err
	}
	fmt.Printf("%s\n  history  %s\n", libraryID, describeDays(days))
	return nil
}

func reportAllRetention() error {
	ids, err := liveLibraryIDs()
	if err != nil {
		return err
	}
	if len(ids) == 0 {
		fmt.Println("No libraries.")
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "LIBRARY\tHISTORY")
	for _, id := range ids {
		days, err := libmgr.RetentionDays(id)
		if err != nil {
			fmt.Fprintf(w, "%s\t(%v)\n", id, err)
			continue
		}
		fmt.Fprintf(w, "%s\t%s\n", id, describeDays(days))
	}
	if err := w.Flush(); err != nil {
		return err
	}
	fmt.Printf("\nServer default: %s. Nothing expires until you run silo gc -expire-history -delete.\n",
		describeDays(option.DefaultKeepDays))
	return nil
}

func describeDays(days int) string {
	if days <= 0 {
		return "kept in full"
	}
	return fmt.Sprintf("%d day%s", days, plural(days))
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
