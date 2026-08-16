package silod

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/dkam/silo/fileserver/option"
	"github.com/dkam/silo/fileserver/repomgr"
	log "github.com/sirupsen/logrus"
)

// Object stores are laid out as <data-dir>/storage/<type>/<store-id>/…, so a
// repo's objects can be reclaimed by removing its directory from each store.
var objectStoreTypes = []string{"commits", "fs", "blocks"}

// garbageRepo is one row of GarbageRepos together with what GC decided to do
// about it. A non-empty skip means GC refused to touch it and why.
type garbageRepo struct {
	repoID string
	dirs   []string
	bytes  int64
	files  int
	skip   string
}

// RunGC reclaims the object-store directories of deleted libraries.
//
// DeleteRepo removes a library's database rows and records its ID in
// GarbageRepos, but nothing has ever reclaimed the objects, so deleted
// libraries leak disk indefinitely. This walks GarbageRepos and removes each
// dead library's directory from the commit, fs and block stores.
//
// It is deliberately not a mark-and-sweep over live repos: it never inspects
// or deletes anything belonging to a library that still exists, which is what
// lets it run without reasoning about concurrent writes. Reclaiming
// unreferenced history *within* a live library is a separate, harder job.
//
// Reporting is the default; -delete is required to remove anything.
func RunGC(args []string) error {
	flags := flag.NewFlagSet("silo gc", flag.ContinueOnError)
	flags.StringVar(&configFile, "C", "", "path to config file (optional)")
	flags.StringVar(&dataDir, "d", "", "data directory (default: $SILO_DATA_DIR or ~/.local/share/silo)")
	del := flags.Bool("delete", false, "remove the objects (default: report what would be removed)")
	quiet := flags.Bool("q", false, "only print the summary")
	if err := flags.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return nil
		}
		return err
	}

	if err := resolvePaths(); err != nil {
		return err
	}

	// The server keeps no lock on the data directory, so GC cannot detect a
	// running instance. It only ever touches libraries that are already
	// deleted, which a running server will not write to, but a server midway
	// through DeleteRepo is a genuine race.
	if *del {
		log.Warn("Stop the server before running gc -delete.")
	}

	option.LoadFileServerOptions(configFile)
	loadDatabases()
	repomgr.Init(seafilePair.Read, seafilePair.Write)

	repos, err := collectGarbageRepos()
	if err != nil {
		return err
	}
	if len(repos) == 0 {
		fmt.Println("Nothing to reclaim: GarbageRepos is empty.")
		return nil
	}

	var totalBytes int64
	var totalFiles, skipped int
	for _, r := range repos {
		if r.skip != "" {
			skipped++
			fmt.Printf("skip %s: %s\n", r.repoID, r.skip)
			continue
		}
		totalBytes += r.bytes
		totalFiles += r.files
		if !*quiet {
			verb := "would reclaim"
			if *del {
				verb = "reclaiming"
			}
			fmt.Printf("%s %s: %d objects, %s\n", verb, r.repoID, r.files, humanBytes(r.bytes))
		}
	}

	if !*del {
		fmt.Printf("\n%d librar%s reclaimable, %d objects, %s. Re-run with -delete to remove.\n",
			len(repos)-skipped, plural(len(repos)-skipped), totalFiles, humanBytes(totalBytes))
		if skipped > 0 {
			fmt.Printf("%d skipped — see above.\n", skipped)
		}
		return nil
	}

	var removed int
	for _, r := range repos {
		if r.skip != "" {
			continue
		}
		if err := reclaim(r); err != nil {
			// Keep going: one unreadable directory should not strand every
			// other dead library. The row stays in GarbageRepos for a retry.
			log.Errorf("Failed to reclaim %s: %v", r.repoID, err)
			continue
		}
		removed++
	}

	fmt.Printf("\nReclaimed %d librar%s, %d objects, %s.\n",
		removed, plural(removed), totalFiles, humanBytes(totalBytes))
	if skipped > 0 {
		fmt.Printf("%d skipped — see above.\n", skipped)
	}
	return nil
}

// collectGarbageRepos reads GarbageRepos and decides, per entry, whether its
// store directories are safe to remove.
func collectGarbageRepos() ([]*garbageRepo, error) {
	ctx, cancel := context.WithTimeout(context.Background(), option.DBOpTimeout*2)
	defer cancel()

	rows, err := seafilePair.Read.QueryContext(ctx, "SELECT repo_id FROM GarbageRepos")
	if err != nil {
		return nil, fmt.Errorf("failed to read GarbageRepos: %v", err)
	}
	defer func() { _ = rows.Close() }()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("failed to scan GarbageRepos row: %v", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to read GarbageRepos: %v", err)
	}

	repos := make([]*garbageRepo, 0, len(ids))
	for _, id := range ids {
		r := &garbageRepo{repoID: id}
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
		repos = append(repos, r)
	}
	return repos, nil
}

// unsafeToReclaim returns a reason when repoID's store directories must not be
// removed, or "" when they are safe to reclaim.
//
// The dangerous case is store sharing. A virtual repo's objects live in its
// origin's store, because repomgr sets StoreID to origin_repo rather than to
// the repo's own ID. So a directory named for a dead repo can still hold a
// live repo's only copy of its data, and a dead virtual repo owns no directory
// of its own at all.
func unsafeToReclaim(ctx context.Context, repoID string) (string, error) {
	// The library came back, or the ID was never really dead.
	live, err := rowExists(ctx, "SELECT 1 FROM Repo WHERE repo_id = ?", repoID)
	if err != nil {
		return "", err
	}
	if live {
		return "still present in Repo — not a deleted library", nil
	}

	// Rows in Branch mean commits are still reachable through a head.
	branched, err := rowExists(ctx, "SELECT 1 FROM Branch WHERE repo_id = ?", repoID)
	if err != nil {
		return "", err
	}
	if branched {
		return "still has rows in Branch — a head still references its commits", nil
	}

	// A live virtual repo whose objects are written into this store.
	origin, err := rowExists(ctx, "SELECT 1 FROM VirtualRepo WHERE origin_repo = ?", repoID)
	if err != nil {
		return "", err
	}
	if origin {
		return "still the origin of a virtual repo, which stores its objects here", nil
	}

	// The dead repo is itself virtual: its objects are in the origin's store,
	// so there is nothing of its own to remove and any directory sharing its
	// name would belong to something else.
	virtual, err := rowExists(ctx, "SELECT 1 FROM VirtualRepo WHERE repo_id = ?", repoID)
	if err != nil {
		return "", err
	}
	if virtual {
		return "is a virtual repo — its objects live in the origin's store", nil
	}

	return "", nil
}

func rowExists(ctx context.Context, query, arg string) (bool, error) {
	var one int
	err := seafilePair.Read.QueryRowContext(ctx, query, arg).Scan(&one)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("failed to query %q: %v", query, err)
	}
	return true, nil
}

// measure records which store directories exist for a repo and how much they
// hold, so a dry run can report the same set the delete pass would remove.
func measure(r *garbageRepo) error {
	for _, objType := range objectStoreTypes {
		dir := filepath.Join(absDataDir, "storage", objType, r.repoID)
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

// reclaim removes a dead repo's store directories, then clears its
// GarbageRepos row. The row is cleared last so a failure part-way leaves the
// repo queued for the next run rather than forgotten with objects on disk.
func reclaim(r *garbageRepo) error {
	for _, dir := range r.dirs {
		if err := os.RemoveAll(dir); err != nil {
			return fmt.Errorf("failed to remove %s: %v", dir, err)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), option.DBOpTimeout)
	defer cancel()
	if _, err := seafilePair.Write.ExecContext(ctx, "DELETE FROM GarbageRepos WHERE repo_id = ?", r.repoID); err != nil {
		return fmt.Errorf("removed objects but failed to clear GarbageRepos row: %v", err)
	}
	return nil
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

func plural(n int) string {
	if n == 1 {
		return "y"
	}
	return "ies"
}
