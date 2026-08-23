package repomgr

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/dkam/silo/fileserver/account"
	"github.com/dkam/silo/fileserver/objmgr"
	"github.com/dkam/silo/fileserver/objstore"
	"github.com/dkam/silo/fileserver/option"
	storefmt "github.com/dkam/silo/store"
	log "github.com/sirupsen/logrus"
)

// Usage returns what a library holds, bringing the stored total forward if the
// library has moved since it was written.
//
// There is no hook on the write path, and that is the design rather than an
// omission. The row records the root its totals are true at, so a reader that
// finds a different root can compute the difference itself; a total that was
// never written, or that was lost to a crash between the commit and the
// update, costs the next reader one Merkle delta and then it is right again.
// Nothing can forget to call this, because everything that wants the number
// calls exactly this.
//
// The delta is what makes it affordable. It is not a walk of the library: a
// subtree whose id is unchanged contributes nothing and is skipped unread, so
// bringing a row forward across one commit reads the directories along one
// path. A library that has not moved costs one row read and no store at all.
func Usage(repo *Repo) (objmgr.Usage, error) {
	stored, at, err := readUsage(repo.ID)
	if err != nil {
		return objmgr.Usage{}, err
	}
	if at == repo.RootID {
		return stored, nil
	}

	st, err := repo.Store()
	if err != nil {
		return objmgr.Usage{}, err
	}
	root, err := storefmt.ParseID(repo.RootID)
	if err != nil {
		return objmgr.Usage{}, err
	}

	current, err := advance(st, stored, at, root)
	if err != nil {
		return objmgr.Usage{}, err
	}
	if err := writeUsage(repo.ID, current, at, repo.RootID); err != nil {
		return objmgr.Usage{}, err
	}
	return current, nil
}

// advance carries a stored total from the root it was true at to the one the
// library is on now.
//
// The delta path needs the old tree to still be there, and it will not always
// be: a library that is written once and then sits unread for longer than
// history retention has its old commits collected, and the walk meets an
// object that is gone. That is not a failure — it is the point at which the
// cheap answer stops being available — so it falls back to measuring the
// current tree outright. The same fallback covers an object lost to corruption
// or deleted by hand, which is the other way a delta can fail to find its
// footing.
func advance(st *objmgr.Store, stored objmgr.Usage, at string, root storefmt.ID) (objmgr.Usage, error) {
	if at != "" {
		old, err := storefmt.ParseID(at)
		if err == nil {
			delta, err := st.MeasureDelta(old, root)
			if err == nil {
				return stored.Add(delta), nil
			}
			if !errors.Is(err, objstore.ErrNotFound) {
				return objmgr.Usage{}, err
			}
		}
	}
	return st.Measure(root)
}

// readUsage returns a library's stored totals and the root they are true at.
// A library with no row yet reports zero at no root, which advance measures
// outright.
func readUsage(repoID string) (objmgr.Usage, string, error) {
	var u objmgr.Usage
	var at string
	ctx, cancel := option.WithDBTimeout(context.Background())
	defer cancel()
	row := seafileDB.QueryRowContext(ctx,
		"SELECT size, file_count, root_id FROM RepoUsage WHERE repo_id = ?", repoID)
	switch err := row.Scan(&u.Size, &u.FileCount, &at); {
	case err == sql.ErrNoRows:
		return objmgr.Usage{}, "", nil
	case err != nil:
		return objmgr.Usage{}, "", fmt.Errorf("failed to read usage of %s: %w", repoID, err)
	}
	return u, at, nil
}

// writeUsage publishes a total, but only over the root it was computed from.
//
// The WHERE clause is the whole safety of having no write-path hook: two
// readers can find the same stale row and race to bring it forward, and the
// loser must not add its delta on top of the winner's answer. A skipped update
// is the correct outcome and not an error — the row already holds a total at
// least as current as this one, computed the same way from the same trees.
func writeUsage(repoID string, u objmgr.Usage, from, to string) error {
	ctx, cancel := option.WithDBTimeout(context.Background())
	defer cancel()
	_, err := seafileWriteDB.ExecContext(ctx,
		"INSERT INTO RepoUsage (repo_id, size, file_count, root_id) VALUES (?, ?, ?, ?) "+
			"ON CONFLICT(repo_id) DO UPDATE SET size = excluded.size, "+
			"file_count = excluded.file_count, root_id = excluded.root_id "+
			"WHERE RepoUsage.root_id = ?",
		repoID, u.Size, u.FileCount, to, from)
	if err != nil {
		return fmt.Errorf("failed to record usage of %s: %w", repoID, err)
	}
	return nil
}

// AccountUsage totals the libraries an account owns.
//
// Owns, not sees: a library shared with you counts against its owner's quota
// and not yours, which is also why per-library sizes belong on the repos
// listing rather than under an account total they must not sum to.
//
// The common case is one query and no stores opened, because a library whose
// recorded root is the root it is on needs nothing computed. Only the ones
// that have moved since anyone last asked are loaded and brought forward, and
// a library that cannot be measured is skipped with its error rather than
// failing the whole total: one unreadable library must not make an account
// unable to see what it is using.
func AccountUsage(id account.ID) (objmgr.Usage, error) {
	ctx, cancel := option.WithDBTimeout(context.Background())
	defer cancel()
	rows, err := seafileDB.QueryContext(ctx,
		"SELECT o.repo_id, b.root_id, u.size, u.file_count, u.root_id "+
			"FROM RepoOwner o "+
			"JOIN Branch b ON b.repo_id = o.repo_id AND b.name = 'master' "+
			"LEFT JOIN RepoUsage u ON u.repo_id = o.repo_id "+
			"LEFT JOIN VirtualRepo v ON v.repo_id = o.repo_id "+
			"WHERE o.account_id = ? AND v.repo_id IS NULL",
		id)
	if err != nil {
		return objmgr.Usage{}, fmt.Errorf("failed to query owned libraries: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var total objmgr.Usage
	var stale []string
	for rows.Next() {
		var repoID, headRoot string
		var size, fileCount sql.NullInt64
		var at sql.NullString
		if err := rows.Scan(&repoID, &headRoot, &size, &fileCount, &at); err != nil {
			return objmgr.Usage{}, err
		}
		if at.Valid && at.String == headRoot {
			total = total.Add(objmgr.Usage{Size: size.Int64, FileCount: fileCount.Int64})
			continue
		}
		stale = append(stale, repoID)
	}
	if err := rows.Err(); err != nil {
		return objmgr.Usage{}, err
	}

	for _, repoID := range stale {
		repo := Get(repoID)
		if repo == nil {
			continue
		}
		u, err := Usage(repo)
		if err != nil {
			log.Warnf("Skipping library %s in account usage: %v", repoID, err)
			continue
		}
		total = total.Add(u)
	}
	return total, nil
}

// AccountQuota is the ceiling an account's usage is measured against, or
// option.InfiniteQuota for no ceiling at all.
//
// Anything that is not a positive number of bytes is no ceiling. An account
// with no row falls to the configured default, and a default that was never
// configured is unlimited — because the alternative is a server that has been
// told nothing about quotas refusing every write on the grounds that zero
// bytes are allowed. A quota only exists once somebody sets one.
func AccountQuota(id account.ID) (int64, error) {
	var quota int64
	ctx, cancel := option.WithDBTimeout(context.Background())
	defer cancel()
	row := seafileDB.QueryRowContext(ctx, "SELECT quota FROM UserQuota WHERE account_id = ?", id)
	if err := row.Scan(&quota); err != nil && err != sql.ErrNoRows {
		return 0, fmt.Errorf("failed to read quota: %w", err)
	}
	if quota <= 0 {
		quota = option.DefaultQuota
	}
	if quota <= 0 {
		return option.InfiniteQuota, nil
	}
	return quota, nil
}
