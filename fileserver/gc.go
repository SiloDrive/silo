package silod

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/dkam/silo/fileserver/objstore"
	"github.com/dkam/silo/fileserver/option"
	"github.com/dkam/silo/internal/format"
	log "github.com/sirupsen/logrus"
)

// garbageLibrary is one row of GarbageLibraries together with what GC decided to do
// about it. A non-empty skip means GC refused to touch it and why.
type garbageLibrary struct {
	libraryID string
	dirs      []string
	bytes     int64
	files     int
	skip      string
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
	minAge := flags.Duration("min-age", DefaultOrphanAge, "with -orphans, how long an object must have sat unreferenced before it is a candidate")
	rest, done, err := parseCommandArgs("gc", flags, args)
	if err != nil || done {
		return err
	}
	if len(rest) != 0 {
		return fmt.Errorf("usage: silo gc [-d datadir] [-C config] [-delete] [-q]")
	}

	// The server keeps no lock on the data directory, so GC cannot detect a
	// running instance. It only ever touches libraries that are already
	// deleted, which a running server will not write to, but a server midway
	// through DeleteLibrary is a genuine race.
	if *del {
		log.Warn("Stop the server before running gc -delete.")
	}

	if err := openStores(); err != nil {
		return err
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
		if err := runOrphanSweep(*minAge, *del, *quiet); err != nil {
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

// measure records which store directories exist for a library and how much they
// hold, so a dry run can report the same set the delete pass would remove.
func measure(r *garbageLibrary) error {
	// The layout and the type names come from objstore rather than being
	// spelled again here: a directory this does not find is silently nothing
	// to reclaim, so a disagreement would make gc report success and remove
	// nothing.
	for _, objType := range objstore.Types {
		dir := objstore.LibraryDir(absDataDir, objType, r.libraryID)
		info, err := os.Stat(dir)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("failed to stat %s: %v", dir, err)
		}
		if !info.IsDir() {
			continue
		}

		err = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			fi, err := d.Info()
			if err != nil {
				return err
			}
			r.files++
			r.bytes += fi.Size()
			return nil
		})
		if err != nil {
			return fmt.Errorf("failed to walk %s: %v", dir, err)
		}

		r.dirs = append(r.dirs, dir)
	}
	return nil
}

// reclaim removes a dead library's store directories, then clears its
// GarbageLibraries row. The row is cleared last so a failure part-way leaves the
// library queued for the next run rather than forgotten with objects on disk.
func reclaim(r *garbageLibrary) error {
	for _, dir := range r.dirs {
		if err := os.RemoveAll(dir); err != nil {
			return fmt.Errorf("failed to remove %s: %v", dir, err)
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
