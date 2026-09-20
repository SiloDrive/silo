package silod

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"

	"github.com/SiloDrive/silo/fileserver/objstore"
	"github.com/SiloDrive/silo/fileserver/option"
	"github.com/SiloDrive/silo/internal/format"
	log "github.com/sirupsen/logrus"
)

// garbageLibrary is one row of GarbageLibraries together with what GC decided to do
// about it. A non-empty skip means GC refused to touch it and why.
type garbageLibrary struct {
	libraryID string
	// stores are the object stores that hold anything for it, carried from
	// measure so that reclaim removes exactly what was measured and reported.
	stores []*objstore.ObjectStore
	bytes  int64
	files  int
	skip   string
}

// RunGC reclaims the object-store directories of deleted libraries.
//
// DeleteLibrary removes a library's database rows and records its ID in
// GarbageLibraries, but nothing has ever reclaimed the objects, so deleted
// libraries leak disk indefinitely. This walks GarbageLibraries and removes each
// dead library's directory from the commit, fs and chunk stores.
//
// It is deliberately not a mark-and-sweep over live libraries: it never inspects
// or deletes anything belonging to a library that still exists, which is what
// lets it run without reasoning about concurrent writes. Reclaiming
// unreferenced history *within* a live library is a separate, harder job.
//
// Reporting is the default; -delete is required to remove anything.
func RunGC(args []string) error {
	flags := commandFlags("gc")
	del := flags.Bool("delete", false, "remove the objects (default: report what would be removed)")
	quiet := flags.Bool("q", false, "only print the summary")
	orphans := flags.Bool("orphans", false, "sweep unreferenced objects inside live libraries too")
	expire := flags.Bool("expire-history", false, "expire history per each library's retention policy, keeping the head")
	expireWindow := flags.Duration("expire-window", 0, "with -expire-history, override every library's policy with this window")
	// These three carry no default of their own: what they fall back to is the
	// [storage] section, which is not loaded until openStores runs below. A
	// flag default evaluated here would be the compiled-in number and would
	// silently beat the configured one -- so the zero values are sentinels and
	// the block after openStores fills in whatever was not typed.
	minAge := flags.Duration("min-age", 0, "with -orphans or -compact, how long an object or pack must have sat there before it is a candidate (default: [storage] orphan_age, compact_min_age)")
	compact := flags.Bool("compact", false, "rewrite sealed packs without the frames nothing reaches (offline: -delete refuses while a server holds the data dir)")
	thresholdFlag := flags.String("compact-threshold", "", "with -compact, the dead fraction at which a pack is worth rewriting, as in 0.5 (default: [storage] compact_threshold)")
	budgetFlag := flags.String("compact-budget", "", "with -compact, the live bytes one run may copy, as a size like 10gb (0: no cap) (default: [storage] compact_budget)")
	rest, done, err := parseCommandArgs("gc", flags, args)
	if err != nil || done {
		return err
	}
	if len(rest) != 0 {
		return fmt.Errorf("usage: silo gc [-d datadir] [-C config] [-delete] [-q]")
	}

	// What was actually typed, so that a value taken from the config file can
	// be told apart from the same value typed on the command line.
	given := map[string]bool{}
	flags.Visit(func(f *flag.Flag) { given[f.Name] = true })

	// A typed value is checked before any database is opened, so a typo costs
	// an error rather than a startup, and by the same reader as the config
	// key, so the two accept the same spellings. A configured one was already
	// checked when the section was read.
	var threshold float64
	var budget int64
	if given["compact-threshold"] {
		if threshold, err = option.ParseFraction(*thresholdFlag); err != nil {
			return fmt.Errorf("-compact-threshold: %w", err)
		}
	}
	if given["compact-budget"] {
		if budget, err = parseCompactBudget(*budgetFlag); err != nil {
			return err
		}
	}

	// A pass that deletes takes the data directory, the way the server does.
	//
	// This was two warnings and a hope. GC only ever touches libraries that
	// are already deleted, which a running server will not write to, but a
	// server midway through DeleteLibrary is a genuine race -- and a
	// compacting pass is worse than a race: it renames a new pack into place
	// and deletes the one the running server holds in memory, so the next
	// read of a frame that moved is a 404 to a client that stored it.
	//
	// Reporting takes nothing, because reporting changes nothing and an
	// operator asking what a running server would reclaim is a fair question.
	//
	// After resolvePaths so absDataDir exists, and before openStores so the
	// refusal comes out before any pack is opened.
	if *del {
		if err := resolvePaths(); err != nil {
			return err
		}
		lock, err := objstore.LockDataDir(absDataDir)
		if err != nil {
			if errors.Is(err, objstore.ErrDataDirLocked) {
				return fmt.Errorf("%w\nStop the server before running gc -delete", err)
			}
			return err
		}
		defer func() { _ = lock.Release() }()
	}

	if err := openStores(); err != nil {
		return err
	}

	// The config is loaded now, so the flags that were left off can take their
	// defaults from it. -min-age serves both passes, so typing it sets both;
	// leaving it off lets each take its own key, which is the point of there
	// being two -- an orphan's age guard protects an upload in flight, and a
	// pack's protects the frames inside it, and an operator may reasonably
	// want them different.
	orphanAge, compactAge := option.OrphanAge, option.CompactMinAge
	if given["min-age"] {
		orphanAge, compactAge = *minAge, *minAge
	}
	if !given["compact-threshold"] {
		threshold = option.CompactThreshold
	}
	if !given["compact-budget"] {
		budget = option.CompactBudget
	}

	// Expiry runs first so that what it releases is collectable by the sweep
	// in the same invocation: an operator who asked for both meant "reclaim
	// what retention allows", not "reclaim it next time".
	if *expire {
		if err := runHistoryExpiry(*expireWindow, *del, *quiet); err != nil {
			return err
		}
		fmt.Println()
	}

	if *orphans {
		if err := runOrphanSweep(orphanAge, *del, *quiet); err != nil {
			return err
		}
		fmt.Println()
	}

	// Last of the three, because it is what finishes the other two. Expiry and
	// the sweep both leave frames they could not delete inside sealed packs;
	// this is the pass that reclaims them, so a run that asked for all three
	// frees what retention allows without waiting for the next one.
	if *compact {
		if err := runCompaction(compactOpts{
			threshold:    threshold,
			minAge:       compactAge,
			budget:       budget,
			expire:       *expire,
			expireWindow: *expireWindow,
			del:          *del,
		}, *quiet); err != nil {
			return err
		}
		fmt.Println()
	}

	libraries, err := collectGarbageLibraries()
	if err != nil {
		return err
	}
	if len(libraries) == 0 {
		fmt.Println("Nothing to reclaim: GarbageLibraries is empty.")
		return nil
	}

	var totalBytes int64
	var totalFiles, skipped int
	for _, r := range libraries {
		if r.skip != "" {
			skipped++
			fmt.Printf("skip %s: %s\n", r.libraryID, r.skip)
			continue
		}
		totalBytes += r.bytes
		totalFiles += r.files
		if !*quiet {
			verb := "would reclaim"
			if *del {
				verb = "reclaiming"
			}
			fmt.Printf("%s %s: %d objects, %s\n", verb, r.libraryID, r.files, format.Bytes(r.bytes))
		}
	}

	if !*del {
		fmt.Printf("\n%d librar%s reclaimable, %d objects, %s. Re-run with -delete to remove.\n",
			len(libraries)-skipped, pluralY(len(libraries)-skipped), totalFiles, format.Bytes(totalBytes))
		if skipped > 0 {
			fmt.Printf("%d skipped — see above.\n", skipped)
		}
		return nil
	}

	var removed int
	for _, r := range libraries {
		if r.skip != "" {
			continue
		}
		if err := reclaim(r); err != nil {
			// Keep going: one unreadable directory should not strand every
			// other dead library. The row stays in GarbageLibraries for a retry.
			log.Errorf("Failed to reclaim %s: %v", r.libraryID, err)
			continue
		}
		removed++
	}

	fmt.Printf("\nReclaimed %d librar%s, %d objects, %s.\n",
		removed, pluralY(removed), totalFiles, format.Bytes(totalBytes))
	if skipped > 0 {
		fmt.Printf("%d skipped — see above.\n", skipped)
	}
	return nil
}

// collectGarbageLibraries reads GarbageLibraries and decides, per entry, whether its
// store directories are safe to remove.
func collectGarbageLibraries() ([]*garbageLibrary, error) {
	ctx, cancel := context.WithTimeout(context.Background(), option.DBOpTimeout*2)
	defer cancel()

	rows, err := siloPair.Read.QueryContext(ctx, "SELECT library_id FROM GarbageLibraries")
	if err != nil {
		return nil, fmt.Errorf("failed to read GarbageLibraries: %v", err)
	}
	defer func() { _ = rows.Close() }()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("failed to scan GarbageLibraries row: %v", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to read GarbageLibraries: %v", err)
	}

	libraries := make([]*garbageLibrary, 0, len(ids))
	for _, id := range ids {
		r := &garbageLibrary{libraryID: id}
		skip, err := unsafeToReclaim(ctx, id)
		if err != nil {
			return nil, err
		}
		r.skip = skip
		if skip == "" {
			if err := measure(r); err != nil {
				return nil, err
			}
		}
		libraries = append(libraries, r)
	}
	return libraries, nil
}

// unsafeToReclaim returns a reason when libraryID's store directories must not be
// removed, or "" when they are safe to reclaim.
//
// The dangerous case is store sharing. A virtual library's objects live in its
// origin's store, because libmgr sets StoreID to origin_library rather than to
// the library's own ID. So a directory named for a dead library can still hold a
// live library's only copy of its data, and a dead virtual library owns no directory
// of its own at all.
func unsafeToReclaim(ctx context.Context, libraryID string) (string, error) {
	for _, b := range reclaimBlockers {
		found, err := rowExists(ctx, b.query, libraryID)
		if err != nil {
			return "", err
		}
		if found {
			return b.reason, nil
		}
	}
	return "", nil
}

// reclaimBlockers is the set of "someone still needs this" checks, in the
// order they are reported. A library is safe to reclaim only when none match.
var reclaimBlockers = []struct{ query, reason string }{
	// The library came back, or the ID was never really dead.
	{"SELECT 1 FROM Library WHERE library_id = ?",
		"still present in Library — not a deleted library"},
	// Rows in Branch mean commits are still reachable through a head.
	{"SELECT 1 FROM Branch WHERE library_id = ?",
		"still has rows in Branch — a head still references its commits"},
	// A live virtual library whose objects are written into this store.
	{"SELECT 1 FROM VirtualLibrary WHERE origin_library = ?",
		"still the origin of a virtual library, which stores its objects here"},
	// The dead library is itself virtual: its objects are in the origin's store,
	// so there is nothing of its own to remove and any directory sharing its
	// name would belong to something else.
	{"SELECT 1 FROM VirtualLibrary WHERE library_id = ?",
		"is a virtual library — its objects live in the origin's store"},
}

func rowExists(ctx context.Context, query, arg string) (bool, error) {
	var one int
	err := siloPair.Read.QueryRowContext(ctx, query, arg).Scan(&one)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("failed to query %q: %v", query, err)
	}
	return true, nil
}

// stores opens one ObjectStore per object type, which is how this file asks
// anything about what is on disk.
//
// Through the seam rather than around it. This used to walk the layout with
// filepath.WalkDir and delete with os.RemoveAll — the two things ObjectStore
// does behind the interface — which was a second implementation of the layout
// that would go wrong the moment the first one changed shape. It has: objects
// now live inside packs, in a directory the fan-out walk was never going to
// find, and a durable tier has no directories to walk at all.
func stores() []*objstore.ObjectStore {
	out := make([]*objstore.ObjectStore, 0, len(objstore.Types))
	for _, objType := range objstore.Types {
		out = append(out, objstore.New(absDataDir, objType))
	}
	return out
}

// measure records how much a dead library holds, so a dry run can report the
// same quantity the delete pass will free.
//
// LibraryUsage rather than List, because those two answer different questions
// and only one of them matches reclaim. A listing reports objects — well-formed
// ids and plaintext sizes — while this has to report storage: frame overhead,
// a pack's footer and filter, and the debris of an interrupted write. Reporting
// the listing's number would make gc quote a figure smaller than the space that
// actually came back, every time.
func measure(r *garbageLibrary) error {
	for _, store := range stores() {
		files, bytes, err := store.LibraryUsage(r.libraryID)
		if err != nil {
			return fmt.Errorf("failed to measure %s in the %s store: %v", r.libraryID, store.ObjType, err)
		}
		if files == 0 && bytes == 0 {
			continue
		}
		r.files += files
		r.bytes += bytes
		r.stores = append(r.stores, store)
	}
	return nil
}

// reclaim removes a dead library's objects, then clears its GarbageLibraries
// row. The row is cleared last so a failure part-way leaves the library queued
// for the next run rather than forgotten with objects on disk.
func reclaim(r *garbageLibrary) error {
	for _, store := range r.stores {
		if err := store.RemoveLibrary(r.libraryID); err != nil {
			return fmt.Errorf("failed to remove %s from the %s store: %v", r.libraryID, store.ObjType, err)
		}
	}

	ctx, cancel := option.WithDBTimeout(context.Background())
	defer cancel()
	if _, err := siloPair.Write.ExecContext(ctx, "DELETE FROM GarbageLibraries WHERE library_id = ?", r.libraryID); err != nil {
		return fmt.Errorf("removed objects but failed to clear GarbageLibraries row: %v", err)
	}
	return nil
}

func pluralY(n int) string {
	if n == 1 {
		return "y"
	}
	return "ies"
}
