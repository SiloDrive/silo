// The [storage] section: how large a pack grows, how long it may stay open,
// and what the collector considers old enough to touch.
//
// These were compiled-in constants scattered across four packages, each landed
// on once and never revisited. Collecting them here is not only about letting
// an operator move them -- it is about there being one place that says what
// the numbers are, so that "five minutes" is a decision somebody can find and
// argue with rather than a literal in a writer.
//
// This file imports objstore for its defaults rather than restating them. That
// direction is safe as long as objstore does not read configuration itself,
// which is the rule objstore.Tuning exists to keep: objstore is handed numbers
// by whoever loaded them. If that ever stops being true the import cycle will
// say so immediately, which is the right place to find out.
package option

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/dkam/silo/fileserver/objstore"
	log "github.com/sirupsen/logrus"
	"gopkg.in/ini.v1"
)

// DefaultOrphanAge is how long an unreferenced object must have sat there
// before a sweep will consider it.
//
// A day is orders of magnitude longer than any client takes to go from its
// first chunk to its head move, including one on a bad connection retrying,
// and short enough that a server does not carry a week of dead uploads.
//
// It is a safety margin before it is a tuning knob, and lowering it is the one
// change in this section that can destroy data rather than merely cost I/O: an
// upload still in progress and an upload that died halfway leave the same
// trace, and elapsed time is the only thing that tells them apart. It is
// configurable because an operator who knows their clients can reasonably
// shorten it -- and doing so logs a warning, because the failure it guards
// against is silent.
const DefaultOrphanAge = 24 * time.Hour

// DefaultCompactThreshold is the dead fraction at which a pack is worth
// rewriting. Half, because a rewrite copies what is live and reclaims what is
// not: at 0.5 the two are the same number of bytes, and below it the run costs
// more I/O than it gives back.
const DefaultCompactThreshold = 0.5

// DefaultCommitAttempts is how many times a commit retries the head CAS before
// giving up. The loop exists for concurrent writers to the same branch, so the
// number is about contention depth rather than about reliability.
const DefaultCommitAttempts = 5

// DefaultLibraryFaultInterval is how long a library fault is held before it is
// logged again. A fault is persistent and every retrying client rediscovers
// it, so without this one broken library becomes a log full of one line.
const DefaultLibraryFaultInterval = 5 * time.Minute

var (
	// Packs is the pack writer's behaviour, handed to objstore.Configure at
	// startup. The one of its three numbers worth knowing about is PackMaxAge:
	// it decides how large packs actually get, because a server that is not
	// ingesting a pack's worth of bytes within that window seals on age every
	// time and the size target is never reached. Raising it produces fewer,
	// larger packs and leaves recent frames unsealed -- still durable, since
	// the sidecar is fsynced, but not yet replicable, because a tier copies
	// sealed packs.
	Packs objstore.Tuning

	// OrphanAge is the sweep's age guard. See DefaultOrphanAge.
	OrphanAge time.Duration

	// CompactThreshold is the dead fraction at which a pack is worth
	// rewriting, and CompactMinAge is the age below which a pack is left alone
	// whatever its dead fraction. CompactBudget caps the live bytes one run
	// copies; 0 is no cap, which is what a nightly run wants so that it
	// catches up rather than falling further behind.
	CompactThreshold float64
	CompactMinAge    time.Duration
	CompactBudget    int64

	// CommitAttempts is the head CAS retry count. A var rather than a const
	// only so a test can lower it to force exhaustion without racing a
	// scheduler; nothing in the server writes it.
	CommitAttempts int

	// LibraryFaultInterval throttles repeated fault logging per library.
	LibraryFaultInterval time.Duration
)

// The defaults are in force from package init, not only after a config file
// is loaded: CommitAttempts and LibraryFaultInterval are read on paths a test
// exercises without ever loading options, and a zero retry budget is a server
// that refuses every write as contention.
func init() { initStorageDefaults() }

// initStorageDefaults is the compiled-in answer, before any file or
// environment is read.
func initStorageDefaults() {
	Packs = objstore.DefaultTuning()
	OrphanAge = DefaultOrphanAge
	CompactThreshold = DefaultCompactThreshold
	CompactMinAge = DefaultOrphanAge
	CompactBudget = 0
	CommitAttempts = DefaultCommitAttempts
	LibraryFaultInterval = DefaultLibraryFaultInterval
}

// loadStorageOptions reads the [storage] section and then the environment, so
// that an environment variable beats the file the same way it does everywhere
// else here.
//
// Every value that cannot be read keeps the default and says so. None of these
// have a sentinel that means "unset", so a silent fallback would leave an
// operator looking at a server that ignored the line they wrote.
func loadStorageOptions(section *ini.Section) {
	if section != nil {
		Packs.PackTarget = fromSection(section, "pack_size", Packs.PackTarget, ParseBytes)
		Packs.PackMaxAge = fromSection(section, "pack_max_age", Packs.PackMaxAge, parseDuration)
		Packs.PackSweep = fromSection(section, "pack_sweep", Packs.PackSweep, parseDuration)
		OrphanAge = fromSection(section, "orphan_age", OrphanAge, parseDuration)
		CompactThreshold = fromSection(section, "compact_threshold", CompactThreshold, ParseFraction)
		CompactMinAge = fromSection(section, "compact_min_age", CompactMinAge, parseDuration)
		CompactBudget = fromSection(section, "compact_budget", CompactBudget, ParseBytes)
		CommitAttempts = fromSection(section, "commit_attempts", CommitAttempts, parseCount)
		LibraryFaultInterval = fromSection(section, "library_fault_interval", LibraryFaultInterval, parseDuration)
	}

	Packs.PackTarget = fromEnv("SILO_PACK_SIZE", Packs.PackTarget, ParseBytes)
	Packs.PackMaxAge = fromEnv("SILO_PACK_MAX_AGE", Packs.PackMaxAge, parseDuration)
	Packs.PackSweep = fromEnv("SILO_PACK_SWEEP", Packs.PackSweep, parseDuration)
	OrphanAge = fromEnv("SILO_ORPHAN_AGE", OrphanAge, parseDuration)
	CompactThreshold = fromEnv("SILO_COMPACT_THRESHOLD", CompactThreshold, ParseFraction)
	CompactMinAge = fromEnv("SILO_COMPACT_MIN_AGE", CompactMinAge, parseDuration)
	CompactBudget = fromEnv("SILO_COMPACT_BUDGET", CompactBudget, ParseBytes)
	CommitAttempts = fromEnv("SILO_COMMIT_ATTEMPTS", CommitAttempts, parseCount)
	LibraryFaultInterval = fromEnv("SILO_LIBRARY_FAULT_INTERVAL", LibraryFaultInterval, parseDuration)

	if OrphanAge < DefaultOrphanAge {
		// Said out loud, because the damage is silent and permanent: a sweep
		// that runs inside the window a slow client is still uploading in
		// deletes chunks the client believes it has stored and will never
		// send again.
		log.Warnf("[storage] orphan_age is %s, below the %s default: an upload still in "+
			"progress can be swept as garbage.", OrphanAge, DefaultOrphanAge)
	}
}

// fromSection takes a key if it is there and readable, and keeps the default
// and warns if it is there and is not. fromEnv is the same over a variable.
// The parser is the whole difference between one setting and the next, so the
// range rule for a type is written once, in its parser, and the file and the
// environment cannot drift apart on what they accept.

func fromSection[T any](section *ini.Section, name string, def T, parse func(string) (T, error)) T {
	key, err := section.GetKey(name)
	if err != nil {
		return def
	}
	v, err := parse(key.String())
	if err != nil {
		log.Warnf("[storage] %s = %q: %v; keeping %v", name, key.String(), err, def)
		return def
	}
	return v
}

func fromEnv[T any](name string, def T, parse func(string) (T, error)) T {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return def
	}
	v, err := parse(raw)
	if err != nil {
		log.Warnf("Ignoring %s=%q: %v; keeping %v", name, raw, err, def)
		return def
	}
	return v
}

func parseDuration(s string) (time.Duration, error) {
	d, err := time.ParseDuration(strings.TrimSpace(s))
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("%q is not a positive duration like 5m", s)
	}
	return d, nil
}

// ParseFraction reads a number between 0 and 1 inclusive. It is exported for
// the flags that take the same value as a [storage] key, so a fraction typed
// at silo is checked by the rule that checked the configured one.
func ParseFraction(s string) (float64, error) {
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil || f < 0 || f > 1 {
		return 0, fmt.Errorf("%q is not a fraction between 0 and 1, as in 0.5", s)
	}
	return f, nil
}

func parseCount(s string) (int, error) {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n < 1 {
		return 0, fmt.Errorf("%q is not a whole number of at least 1", s)
	}
	return n, nil
}

// ParseBytes reads a size with its unit: "512mb", "10gb", or "0".
//
// The unit is required, and that is the difference from parseQuota, which
// reads a bare number as gigabytes. Both readings are defensible on their own
// and neither is defensible next to the other -- "default = 512" meaning half
// a terabyte while "pack_size = 512" means half a kilobyte is a footgun with
// no upside, so this one refuses the ambiguous form instead of picking a
// meaning. Zero is spelled without a unit because it is the same size in all
// of them.
//
// It is the parser behind the quota command's sizes and the gc flags as well
// as the [storage] keys, so a size means the same thing wherever it is typed.
// silo#49 tracks parseQuota's bare-gigabytes reading joining it.
func ParseBytes(s string) (int64, error) {
	v := strings.ToLower(strings.TrimSpace(s))
	if v == "0" {
		return 0, nil
	}
	for _, unit := range []struct {
		suffix string
		size   int64
	}{{"kb", KB}, {"mb", MB}, {"gb", GB}, {"tb", TB}} {
		digits, ok := strings.CutSuffix(v, unit.suffix)
		if !ok {
			continue
		}
		n, err := strconv.ParseInt(strings.TrimSpace(digits), 10, 64)
		if err != nil || n < 0 {
			return 0, fmt.Errorf("%q is not a size; write a whole number of %s, like 100%s",
				s, strings.ToUpper(unit.suffix), unit.suffix)
		}
		if n > (1<<63-1)/unit.size {
			// A size too large for an int64 is a negative one, and everything
			// downstream reads that as something other than enormous.
			return 0, fmt.Errorf("%q is larger than this server can address", s)
		}
		return n * unit.size, nil
	}
	return 0, fmt.Errorf("%q has no unit; write kb, mb, gb or tb, like 100gb", s)
}
