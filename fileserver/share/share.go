// Package share manages share relations.
// share: manages personal shares and provide high level permission check functions.
package share

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/dkam/silo/fileserver/account"
	"github.com/dkam/silo/fileserver/option"
	"github.com/dkam/silo/fileserver/repomgr"
	log "github.com/sirupsen/logrus"
)

type group struct {
	id            int
	groupName     string
	creator       account.ID
	timestamp     int64
	parentGroupID int
}

// db is the read handle on the one Silo database. Groups and repositories
// used to live in separate databases, so the permission checks below were
// split across two handles and could never join across them; they now can.
var db *sql.DB
var groupTableName string
var cloudMode bool

// Init sets the database handle, the group table name and cloud mode.
func Init(readDB *sql.DB, grpTableName string, clMode bool) {
	db = readDB
	groupTableName = grpTableName
	cloudMode = clMode
}

// CheckPerm get user's repo permission
func CheckPerm(repoID string, user account.ID) string {
	var perm string
	vInfo, err := repomgr.GetVirtualRepoInfo(repoID)
	if err != nil {
		log.Errorf("Failed to get virtual repo info by repo id %s: %v", repoID, err)
	}
	if vInfo != nil {
		perm = checkVirtualRepoPerm(repoID, vInfo.OriginRepoID, user, vInfo.Path)
		return perm
	}

	perm = checkRepoSharePerm(repoID, user)

	return perm
}

func checkVirtualRepoPerm(repoID, originRepoID string, user account.ID, vPath string) string {
	owner, err := repomgr.GetRepoOwner(originRepoID)
	if err != nil {
		log.Errorf("Failed to get repo owner: %v", err)
	}
	var perm string
	if !owner.IsZero() && owner == user {
		perm = "rw"
		return perm
	}
	perm = checkPermOnParentRepo(originRepoID, user, vPath)
	if perm != "" {
		return perm
	}
	perm = checkRepoSharePerm(originRepoID, user)
	return perm
}

func getUserGroups(sqlStr string, args ...interface{}) ([]group, error) {
	ctx, cancel := option.WithDBTimeout(context.Background())
	defer cancel()
	rows, err := db.QueryContext(ctx, sqlStr, args...)
	if err != nil {
		return nil, err
	}

	defer func() { _ = rows.Close() }()

	var groups []group
	var g group
	for rows.Next() {
		if err := rows.Scan(&g.id, &g.groupName,
			&g.creator, &g.timestamp,
			&g.parentGroupID); err == nil {

			groups = append(groups, g)
		}
	}

	if err := rows.Err(); err != nil {
		return nil, err
	}
	return groups, nil
}

func getGroupsByUser(user account.ID, returnAncestors bool) ([]group, error) {
	sqlStr := fmt.Sprintf("SELECT g.group_id, group_name, creator_account_id, timestamp, parent_group_id FROM "+
		"`%s` g, GroupUser u WHERE g.group_id = u.group_id AND u.account_id=? ORDER BY g.group_id DESC",
		groupTableName)
	groups, err := getUserGroups(sqlStr, user)
	if err != nil {
		err := fmt.Errorf("failed to get groups by user %s: %v", user, err)
		return nil, err
	}
	if !returnAncestors {
		return groups, nil
	}

	sqlStr = ""
	var ret []group
	for _, group := range groups {
		parentGroupID := group.parentGroupID
		groupID := group.id
		if parentGroupID != 0 {
			if sqlStr == "" {
				sqlStr = fmt.Sprintf("SELECT path FROM GroupStructure WHERE group_id IN (%d",
					groupID)
			} else {
				sqlStr += fmt.Sprintf(", %d", groupID)
			}
		} else {
			ret = append(ret, group)
		}
	}
	if sqlStr != "" {
		sqlStr += ")"
		paths, err := getGroupPaths(sqlStr)
		if err != nil {
			log.Errorf("Failed to get group paths: %v", err)
		}
		if paths == "" {
			err := fmt.Errorf("failed to get groups path for user %s", user)
			return nil, err
		}

		sqlStr = fmt.Sprintf("SELECT g.group_id, group_name, creator_account_id, timestamp, parent_group_id FROM "+
			"`%s` g WHERE g.group_id IN (%s) ORDER BY g.group_id DESC",
			groupTableName, paths)
		groups, err := getUserGroups(sqlStr)
		if err != nil {
			return nil, err
		}
		ret = append(ret, groups...)
	}
	return ret, nil
}

func getGroupPaths(sqlStr string) (string, error) {
	var paths string
	ctx, cancel := option.WithDBTimeout(context.Background())
	defer cancel()
	rows, err := db.QueryContext(ctx, sqlStr)
	if err != nil {
		return paths, err
	}

	defer func() { _ = rows.Close() }()

	var path string
	for rows.Next() {
		if err := rows.Scan(&path); err != nil {
			return "", err
		}
		if paths == "" {
			paths = path
		} else {
			paths += fmt.Sprintf(", %s", path)
		}
	}

	if err := rows.Err(); err != nil {
		return "", err
	}
	return paths, nil
}

func checkGroupPermByUser(repoID string, user account.ID) (string, error) {
	groups, err := getGroupsByUser(user, false)
	if err != nil {
		return "", err
	}
	if len(groups) == 0 {
		return "", nil
	}

	var sqlBuilder strings.Builder
	sqlBuilder.WriteString("SELECT permission FROM RepoGroup WHERE repo_id = ? AND group_id IN (")
	for i := 0; i < len(groups); i++ {
		sqlBuilder.WriteString(strconv.Itoa(groups[i].id))
		if i+1 < len(groups) {
			sqlBuilder.WriteString(",")
		}
	}
	sqlBuilder.WriteString(")")

	ctx, cancel := option.WithDBTimeout(context.Background())
	defer cancel()
	rows, err := db.QueryContext(ctx, sqlBuilder.String(), repoID)
	if err != nil {
		err := fmt.Errorf("failed to get group permission by user %s: %v", user, err)
		return "", err
	}

	defer func() { _ = rows.Close() }()

	var perm string
	var origPerm string
	for rows.Next() {
		if err := rows.Scan(&perm); err == nil {
			if perm == "rw" {
				origPerm = perm
			} else if perm == "r" && origPerm == "" {
				origPerm = perm
			}
		}
	}

	if err := rows.Err(); err != nil {
		err := fmt.Errorf("failed to get group permission for user %s: %v", user, err)
		return "", err
	}

	return origPerm, nil
}

func checkSharedRepoPerm(repoID string, to account.ID) (string, error) {
	sqlStr := "SELECT permission FROM SharedRepo WHERE repo_id=? AND to_account_id=?"
	ctx, cancel := option.WithDBTimeout(context.Background())
	defer cancel()
	row := db.QueryRowContext(ctx, sqlStr, repoID, to)

	var perm string
	if err := row.Scan(&perm); err != nil {
		if err != sql.ErrNoRows {
			err := fmt.Errorf("failed to check shared repo permission: %v", err)
			return "", err
		}
	}
	return perm, nil
}

func checkInnerPubRepoPerm(repoID string) (string, error) {
	sqlStr := "SELECT permission FROM InnerPubRepo WHERE repo_id=?"
	ctx, cancel := option.WithDBTimeout(context.Background())
	defer cancel()
	row := db.QueryRowContext(ctx, sqlStr, repoID)

	var perm string
	if err := row.Scan(&perm); err != nil {
		if err != sql.ErrNoRows {
			err := fmt.Errorf("failed to check inner public repo permission: %v", err)
			return "", err
		}
	}

	return perm, nil
}

func checkRepoSharePerm(repoID string, user account.ID) string {
	owner, err := repomgr.GetRepoOwner(repoID)
	if err != nil {
		log.Errorf("Failed to get repo owner: %v", err)
	}
	if !owner.IsZero() && owner == user {
		perm := "rw"
		return perm
	}
	perm, err := checkSharedRepoPerm(repoID, user)
	if err != nil {
		log.Errorf("Failed to get shared repo permission: %v", err)
	}
	if perm != "" {
		return perm
	}
	perm, err = checkGroupPermByUser(repoID, user)
	if err != nil {
		log.Errorf("Failed to get group permission by user %s: %v", user, err)
	}
	if perm != "" {
		return perm
	}
	if !cloudMode {
		perm, err = checkInnerPubRepoPerm(repoID)
		if err != nil {
			log.Errorf("Failed to get inner pulic repo permission by repo id %s: %v", repoID, err)
			return ""
		}
		return perm
	}
	return ""
}

func getSharedDirsToUser(originRepoID string, to account.ID) (map[string]string, error) {
	dirs := make(map[string]string)
	sqlStr := "SELECT v.path, s.permission FROM SharedRepo s, VirtualRepo v WHERE " +
		"s.repo_id = v.repo_id AND s.to_account_id = ? AND v.origin_repo = ?"

	ctx, cancel := option.WithDBTimeout(context.Background())
	defer cancel()
	rows, err := db.QueryContext(ctx, sqlStr, to, originRepoID)
	if err != nil {
		err := fmt.Errorf("failed to get shared directories by user %s: %v", to, err)
		return nil, err
	}

	defer func() { _ = rows.Close() }()

	var path string
	var perm string
	for rows.Next() {
		if err := rows.Scan(&path, &perm); err == nil {
			dirs[path] = perm
		}
	}
	if err := rows.Err(); err != nil {
		err := fmt.Errorf("failed to get shared directories by user %s: %v", to, err)
		return nil, err
	}

	return dirs, nil
}

func getDirPerm(perms map[string]string, path string) string {
	tmp := path
	var perm string
	// If the path is empty, filepath.Dir returns ".". If the path consists entirely of separators,
	// filepath.Dir returns a single separator.
	for tmp != "/" && tmp != "." && tmp != "" {
		if perm, exists := perms[tmp]; exists {
			return perm
		}
		tmp = filepath.Dir(tmp)
	}
	return perm
}

func convertGroupListToStr(groups []group) string {
	var groupIDs strings.Builder

	for i, group := range groups {
		groupIDs.WriteString(strconv.Itoa(group.id))
		if i+1 < len(groups) {
			groupIDs.WriteString(",")
		}
	}
	return groupIDs.String()
}

func getSharedDirsToGroup(originRepoID string, groups []group) (map[string]string, error) {
	dirs := make(map[string]string)
	groupIDs := convertGroupListToStr(groups)

	sqlStr := fmt.Sprintf("SELECT v.path, s.permission "+
		"FROM RepoGroup s, VirtualRepo v WHERE "+
		"s.repo_id = v.repo_id AND v.origin_repo = ? "+
		"AND s.group_id in (%s)", groupIDs)

	ctx, cancel := option.WithDBTimeout(context.Background())
	defer cancel()
	rows, err := db.QueryContext(ctx, sqlStr, originRepoID)
	if err != nil {
		err := fmt.Errorf("failed to get shared directories: %v", err)
		return nil, err
	}

	defer func() { _ = rows.Close() }()

	var path string
	var perm string
	for rows.Next() {
		if err := rows.Scan(&path, &perm); err == nil {
			dirs[path] = perm
		}
	}

	if err := rows.Err(); err != nil {
		err := fmt.Errorf("failed to get shared directories: %v", err)
		return nil, err
	}

	return dirs, nil
}

func checkPermOnParentRepo(originRepoID string, user account.ID, vPath string) string {
	var perm string
	userPerms, err := getSharedDirsToUser(originRepoID, user)
	if err != nil {
		log.Errorf("Failed to get all shared folder perms in parent repo %.8s for user %s", originRepoID, user)
		return ""
	}
	if len(userPerms) > 0 {
		perm = getDirPerm(userPerms, vPath)
		if perm != "" {
			return perm
		}
	}

	groups, err := getGroupsByUser(user, false)
	if err != nil {
		log.Errorf("Failed to get groups by user %s: %v", user, err)
	}
	if len(groups) == 0 {
		return perm
	}

	groupPerms, err := getSharedDirsToGroup(originRepoID, groups)
	if err != nil {
		log.Errorf("Failed to get all shared folder perm from parent repo %.8s to all user groups", originRepoID)
		return ""
	}
	if len(groupPerms) == 0 {
		return ""
	}

	perm = getDirPerm(groupPerms, vPath)

	return perm
}

// SharedRepo is a shared repo object
type SharedRepo struct {
	Version      int    `json:"version"`
	ID           string `json:"id"`
	HeadCommitID string `json:"head_commit_id"`
	Name         string `json:"name"`
	MTime        int64  `json:"mtime"`
	Permission   string `json:"permission"`
	Type         string `json:"type"`
	Owner        string `json:"owner"`
	RepoType     string `json:"-"`
}

// GetReposByOwner get repos by owner
func GetReposByOwner(owner account.ID) ([]*SharedRepo, error) {
	var repos []*SharedRepo

	query := "SELECT o.repo_id, b.commit_id, i.name, " +
		"i.version, i.update_time, i.last_modifier, i.type FROM " +
		"RepoOwner o LEFT JOIN Branch b ON o.repo_id = b.repo_id " +
		"LEFT JOIN RepoInfo i ON o.repo_id = i.repo_id " +
		"LEFT JOIN VirtualRepo v ON o.repo_id = v.repo_id " +
		"WHERE o.account_id=? AND " +
		"v.repo_id IS NULL " +
		"ORDER BY i.update_time DESC, o.repo_id"

	ctx, cancel := option.WithDBTimeout(context.Background())
	defer cancel()
	stmt, err := db.PrepareContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer func() { _ = stmt.Close() }()

	rows, err := stmt.QueryContext(ctx, owner)

	if err != nil {
		return nil, err
	}

	defer func() { _ = rows.Close() }()

	for rows.Next() {
		repo := new(SharedRepo)
		var repoName, lastModifier, repoType sql.NullString
		if err := rows.Scan(&repo.ID, &repo.HeadCommitID,
			&repoName, &repo.Version, &repo.MTime,
			&lastModifier, &repoType); err == nil {

			if repo.HeadCommitID == "" {
				continue
			}
			if !repoName.Valid || !lastModifier.Valid {
				continue
			}
			if repoName.String == "" || lastModifier.String == "" {
				continue
			}
			repo.Name = repoName.String
			if repoType.Valid {
				repo.RepoType = repoType.String
			}
			repos = append(repos, repo)
		}
	}

	if err := rows.Err(); err != nil {
		return nil, err
	}

	return repos, nil
}

// ListInnerPubRepos get inner public repos
func ListInnerPubRepos() ([]*SharedRepo, error) {
	// Owner comes back as an address rather than an id: it is a field in a
	// JSON response, read by a client that has never heard of an account.
	// That is the whole shape of the identity split at the edge — the key is
	// an id everywhere inside, and the address is joined back on the way out.
	query := "SELECT InnerPubRepo.repo_id, " +
		"ae.email, permission, commit_id, i.name, " +
		"i.update_time, i.version, i.type " +
		"FROM InnerPubRepo " +
		"LEFT JOIN RepoInfo i ON InnerPubRepo.repo_id = i.repo_id, RepoOwner, Branch " +
		"LEFT JOIN AccountEmail ae ON ae.account_id = RepoOwner.account_id AND ae.is_primary = 1 " +
		"WHERE InnerPubRepo.repo_id=RepoOwner.repo_id AND " +
		"InnerPubRepo.repo_id = Branch.repo_id AND Branch.name = 'master'"

	ctx, cancel := option.WithDBTimeout(context.Background())
	defer cancel()
	stmt, err := db.PrepareContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer func() { _ = stmt.Close() }()

	rows, err := stmt.QueryContext(ctx)
	if err != nil {
		return nil, err
	}

	defer func() { _ = rows.Close() }()

	var repos []*SharedRepo
	for rows.Next() {
		repo := new(SharedRepo)
		var repoName, repoType, owner sql.NullString
		if err := rows.Scan(&repo.ID, &owner,
			&repo.Permission, &repo.HeadCommitID, &repoName,
			&repo.MTime, &repo.Version, &repoType); err == nil {

			if !repoName.Valid {
				continue
			}
			if repoName.String == "" {
				continue
			}
			repo.Name = repoName.String
			repo.Owner = owner.String
			if repoType.Valid {
				repo.RepoType = repoType.String
			}
			repos = append(repos, repo)
		}
	}

	if err := rows.Err(); err != nil {
		return nil, err
	}

	return repos, nil
}

// ListSharedWithMe lists the libraries shared with an account, with the
// sharer's address on each row.
//
// There is deliberately no "shared by me" counterpart. One existed and no
// caller ever reached it, so it was a second query kept correct through every
// schema change on the strength of a symmetry argument alone. The outgoing
// direction is `sh.from_account_id = ?` with the join moved to
// `sh.to_account_id`, and is worth writing against the schema of the day
// something actually asks for it.
func ListSharedWithMe(id account.ID) ([]*SharedRepo, error) {
	var repos []*SharedRepo
	const query = "SELECT sh.repo_id, ae.email, " +
		"permission, commit_id, " +
		"i.name, i.update_time, i.version, i.type FROM " +
		"SharedRepo sh LEFT JOIN RepoInfo i ON sh.repo_id = i.repo_id " +
		"LEFT JOIN AccountEmail ae ON ae.account_id = sh.from_account_id AND ae.is_primary = 1, Branch b " +
		"WHERE sh.to_account_id=? AND " +
		"sh.repo_id = b.repo_id AND " +
		"b.name = 'master' " +
		"ORDER BY i.update_time DESC, sh.repo_id"

	ctx, cancel := option.WithDBTimeout(context.Background())
	defer cancel()
	stmt, err := db.PrepareContext(ctx, query)
	if err != nil {
		return nil, err
	}

	defer func() { _ = stmt.Close() }()

	rows, err := stmt.QueryContext(ctx, id)
	if err != nil {
		return nil, err
	}

	defer func() { _ = rows.Close() }()

	for rows.Next() {
		repo := new(SharedRepo)
		var repoName, repoType, other sql.NullString
		if err := rows.Scan(&repo.ID, &other,
			&repo.Permission, &repo.HeadCommitID,
			&repoName, &repo.MTime, &repo.Version, &repoType); err == nil {

			if !repoName.Valid {
				continue
			}
			if repoName.String == "" {
				continue
			}
			repo.Name = repoName.String
			repo.Owner = other.String
			if repoType.Valid {
				repo.RepoType = repoType.String
			}

			repos = append(repos, repo)
		}
	}

	if err := rows.Err(); err != nil {
		return nil, err
	}

	return repos, nil
}

// GetGroupReposByUser get group repos by user
func GetGroupReposByUser(user account.ID) ([]*SharedRepo, error) {
	groups, err := getGroupsByUser(user, true)
	if err != nil {
		return nil, err
	}
	if len(groups) == 0 {
		return nil, nil
	}

	var sqlBuilder strings.Builder
	sqlBuilder.WriteString("SELECT g.repo_id, " +
		"ae.email, permission, commit_id, " +
		"i.name, i.update_time, i.version, i.type " +
		"FROM RepoGroup g " +
		"LEFT JOIN RepoInfo i ON g.repo_id = i.repo_id " +
		"LEFT JOIN AccountEmail ae ON ae.account_id = g.account_id AND ae.is_primary = 1, " +
		"Branch b WHERE g.repo_id = b.repo_id AND " +
		"b.name = 'master' AND group_id IN (")

	for i := 0; i < len(groups); i++ {
		sqlBuilder.WriteString(strconv.Itoa(groups[i].id))
		if i+1 < len(groups) {
			sqlBuilder.WriteString(",")
		}
	}
	sqlBuilder.WriteString(" ) ORDER BY group_id")

	ctx, cancel := option.WithDBTimeout(context.Background())
	defer cancel()
	rows, err := db.QueryContext(ctx, sqlBuilder.String())
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var repos []*SharedRepo
	for rows.Next() {
		gRepo := new(SharedRepo)
		var repoType, sharer sql.NullString
		if err := rows.Scan(&gRepo.ID, &sharer,
			&gRepo.Permission, &gRepo.HeadCommitID,
			&gRepo.Name, &gRepo.MTime, &gRepo.Version, &repoType); err == nil {
			gRepo.Owner = sharer.String
			if repoType.Valid {
				gRepo.RepoType = repoType.String
			}
			repos = append(repos, gRepo)
		}
	}

	if err := rows.Err(); err != nil {
		return nil, err
	}

	return repos, nil
}
