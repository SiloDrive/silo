package silod

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/dkam/silo/fileserver/libmgr"
	"github.com/dkam/silo/fileserver/option"
)

// The quota commands: read an account's ceiling and what it is using, and set
// or remove the ceiling.
//
// Per account, which is worth saying because it is neither of the two things
// people assume. It is not per library -- libmgr.AccountUsage totals every
// library an account owns, and a library shared with somebody is charged to
// its owner and not to them. And it is not per server -- option.DefaultQuota
// is only the fallback for an account with no row of its own, so a server-wide
// figure in silo.conf sets the floor and this sets the exception.
//
// A CLI rather than an API for the reasons RunUser gives. docs/future-features.md
// wants GET/PUT /users/{email}/quota, and they are meant to be these functions
// over HTTP once there is an admin role to gate them with.

// setUserQuota gives an account a ceiling, or removes the one it has.
//
// The write is an upsert because the table is keyed by account: a plain INSERT
// works exactly once per account and fails afterwards, so an operator raising
// somebody's quota would be told nothing and leave the old number in force.
func setUserQuota(email, size string) error {
	acct, err := resolveAccount(email)
	if err != nil {
		return err
	}

	quota, remove, err := parseQuotaSize(size)
	if err != nil {
		return err
	}

	ctx, cancel := option.WithDBTimeout(context.Background())
	defer cancel()

	if remove {
		if _, err := siloPair.Write.ExecContext(ctx,
			"DELETE FROM UserQuota WHERE account_id = ?", acct.ID); err != nil {
			return fmt.Errorf("removing the quota of %s: %v", acct.Email, err)
		}
		fmt.Printf("Removed the quota on %s. Their writes are no longer capped", acct.Email)
		if option.DefaultQuota > 0 {
			fmt.Printf(" by an account setting; the server default of %s now applies",
				formatBytes(option.DefaultQuota))
		}
		fmt.Println(".")
		return nil
	}

	if _, err := siloPair.Write.ExecContext(ctx,
		"INSERT INTO UserQuota (account_id, quota) VALUES (?, ?) "+
			"ON CONFLICT(account_id) DO UPDATE SET quota = excluded.quota",
		acct.ID, quota); err != nil {
		return fmt.Errorf("setting the quota of %s: %v", acct.Email, err)
	}

	fmt.Printf("Quota for %s set to %s.\n", acct.Email, formatBytes(quota))

	// Said now rather than discovered later. A cap set below what somebody
	// already holds is legitimate -- it is how you stop a runaway account
	// growing further -- but it refuses their next write, and an operator who
	// did not mean that should hear it here and not from the user.
	if usage, err := libmgr.AccountUsage(acct.ID); err == nil && usage.Size > quota {
		fmt.Printf("\nThey are already using %s, which is over that cap. Nothing is deleted,\n"+
			"but their next write is refused with 507 until they free %s.\n",
			formatBytes(usage.Size), formatBytes(usage.Size-quota))
	}
	return nil
}

// reportUserQuota prints an account's ceiling and its usage.
//
// Both numbers, always. A ceiling on its own does not answer the question
// anybody types this to ask, and a usage figure on its own cannot be judged.
func reportUserQuota(email string) error {
	acct, err := resolveAccount(email)
	if err != nil {
		return err
	}

	quota, err := libmgr.AccountQuota(acct.ID)
	if err != nil {
		return fmt.Errorf("reading the quota of %s: %v", acct.Email, err)
	}
	usage, err := libmgr.AccountUsage(acct.ID)
	if err != nil {
		return fmt.Errorf("totalling the usage of %s: %v", acct.Email, err)
	}

	ceiling := "unlimited"
	if quota > 0 {
		ceiling = formatBytes(quota)
	}
	fmt.Printf("%s\n  usage  %s in %d files\n  quota  %s\n",
		acct.Email, formatBytes(usage.Size), usage.FileCount, ceiling)

	// The label the API puts on the same number, spelled out here for the same
	// reason api.go's usageKind exists: the first question this figure
	// generates is why it disagrees with du, and the answer is that they
	// measure different things. History is not in this number and is not
	// reclaimed by anything yet -- see RunGC.
	fmt.Println("\nUsage is logical size at head: the sizes of the files the current commit\n" +
		"reaches. Not bytes on disk, and it does not include history.")
	return nil
}

// parseQuotaSize reads a size an operator typed. remove is true for the words
// that mean "no ceiling".
//
// It is not option.parseQuota, and the difference is the whole point: that one
// answers InfiniteQuota for anything it cannot read, which is right for a
// config file nobody is watching and wrong for a command somebody is typing.
// "silo user quota alice 100gigs" would report success and remove the ceiling
// it was called to impose.
//
// The unit is required for the same class of reason. A bare 100 means 100 GB
// in silo.conf, which nobody guesses; an operator who reads it as 100 bytes
// has set a cap four orders of magnitude from the one they meant, and neither
// the command nor the table would say so.
func parseQuotaSize(size string) (quota int64, remove bool, err error) {
	s := strings.ToLower(strings.TrimSpace(size))
	switch s {
	case "none", "unlimited":
		return 0, true, nil
	case "":
		return 0, false, errors.New("no size given; use a size like 100gb, or \"none\" to remove the cap")
	}

	units := []struct {
		suffix string
		scale  int64
	}{
		{"kb", option.KB}, {"mb", option.MB}, {"gb", option.GB}, {"tb", option.TB},
	}
	for _, u := range units {
		digits, ok := strings.CutSuffix(s, u.suffix)
		if !ok {
			continue
		}
		n, err := strconv.ParseInt(strings.TrimSpace(digits), 10, 64)
		if err != nil || n <= 0 {
			return 0, false, fmt.Errorf("%q is not a size; write a positive whole number of %s, like 100%s",
				size, strings.ToUpper(u.suffix), u.suffix)
		}
		// A quota that overflows int64 is not a large quota, it is a negative
		// one, and AccountQuota reads anything <= 0 as no ceiling at all.
		if n > (1<<63-1)/u.scale {
			return 0, false, fmt.Errorf("%q is too large to store as a number of bytes", size)
		}
		return n * u.scale, false, nil
	}
	return 0, false, fmt.Errorf("%q has no unit; write kb, mb, gb or tb, like 100gb, or \"none\" to remove the cap", size)
}

// formatBytes renders a size the way the units it was typed in are written.
//
// Decimal, because option.KB is 1000 and a figure that came from "100gb" has
// to read back as 100 GB. Rendering it as 93.1 GiB would be accurate and would
// still look like the command had misunderstood.
func formatBytes(n int64) string {
	switch {
	case n >= option.TB:
		return trimZeros(float64(n)/option.TB) + " TB"
	case n >= option.GB:
		return trimZeros(float64(n)/option.GB) + " GB"
	case n >= option.MB:
		return trimZeros(float64(n)/option.MB) + " MB"
	case n >= option.KB:
		return trimZeros(float64(n)/option.KB) + " KB"
	default:
		return strconv.FormatInt(n, 10) + " B"
	}
}

// trimZeros prints one decimal place unless it would be a zero, so that a
// quota somebody set as a round number reads back as one.
func trimZeros(v float64) string {
	s := strconv.FormatFloat(v, 'f', 1, 64)
	return strings.TrimSuffix(s, ".0")
}
