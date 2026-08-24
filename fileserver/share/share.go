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
	"github.com/dkam/silo/fileserver/libmgr"
	"github.com/dkam/silo/fileserver/option"
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

// CheckPerm get user's library permission
func CheckPerm(libraryID string, user account.ID) string {
	var perm string
	vInfo, err := libmgr.GetVirtualLibraryInfo(libraryID)
	if err != nil {
		log.Errorf("Failed to get virtual library info by library id %s: %v", libraryID, err)
	}
	if vInfo != nil {
		perm = checkVirtualLibraryPerm(libraryID, vInfo.OriginLibraryID, user, vInfo.Path)
		return perm
	}

	perm = checkLibrarySharePerm(libraryID, user)

	return perm
}

func checkVirtualLibraryPerm(libraryID, originLibraryID string, user account.ID, vPath string) string {
	owner, err := libmgr.GetLibraryOwner(originLibraryID)
	if err != nil {
		log.Errorf("Failed to get library owner: %v", err)
	}
	var perm string
	if !owner.IsZero() && owner == user {
		perm = "rw"
		return perm
	}
	perm = checkPermOnParentLibrary(originLibraryID, user, vPath)
	if perm != "" {
		return perm
	}
	perm = checkLibrarySharePerm(originLibraryID, user)
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

func checkGroupPermByUser(libraryID string, user account.ID) (string, error) {
	groups, err := getGroupsByUser(user, false)
	if err != nil {
		return "", err
	}
	if len(groups) == 0 {
		return "", nil
	}

	var sqlBuilder strings.Builder
	sqlBuilder.WriteString("SELECT permission FROM LibraryGroup WHERE library_id = ? AND group_id IN (")
	for i := 0; i < len(groups); i++ {
		sqlBuilder.WriteString(strconv.Itoa(groups[i].id))
		if i+1 < len(groups) {
			sqlBuilder.WriteString(",")
		}
	}
	sqlBuilder.WriteString(")")

	ctx, cancel := option.WithDBTimeout(context.Background())
	defer cancel()
	rows, err := db.QueryContext(ctx, sqlBuilder.String(), libraryID)
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

func checkSharedLibraryPerm(libraryID string, to account.ID) (string, error) {
	sqlStr := "SELECT permission FROM SharedLibrary WHERE library_id=? AND to_account_id=?"
	ctx, cancel := option.WithDBTimeout(context.Background())
	defer cancel()
	row := db.QueryRowContext(ctx, sqlStr, libraryID, to)

	var perm string
	if err := row.Scan(&perm); err != nil {
		if err != sql.ErrNoRows {
			err := fmt.Errorf("failed to check shared library permission: %v", err)
			return "", err
		}
	}
	return perm, nil
}

func checkInnerPubLibraryPerm(libraryID string) (string, error) {
	sqlStr := "SELECT permission FROM InnerPubLibrary WHERE library_id=?"
	ctx, cancel := option.WithDBTimeout(context.Background())
	defer cancel()
	row := db.QueryRowContext(ctx, sqlStr, libraryID)

	var perm string
	if err := row.Scan(&perm); err != nil {
		if err != sql.ErrNoRows {
			err := fmt.Errorf("failed to check inner public library permission: %v", err)
			return "", err
		}
	}

	return perm, nil
}

func checkLibrarySharePerm(libraryID string, user account.ID) string {
	owner, err := libmgr.GetLibraryOwner(libraryID)
	if err != nil {
		log.Errorf("Failed to get library owner: %v", err)
	}
	if !owner.IsZero() && owner == user {
		perm := "rw"
		return perm
	}
	perm, err := checkSharedLibraryPerm(libraryID, user)
	if err != nil {
		log.Errorf("Failed to get shared library permission: %v", err)
	}
	if perm != "" {
		return perm
	}
	perm, err = checkGroupPermByUser(libraryID, user)
	if err != nil {
		log.Errorf("Failed to get group permission by user %s: %v", user, err)
	}
	if perm != "" {
		return perm
	}
	if !cloudMode {
		perm, err = checkInnerPubLibraryPerm(libraryID)
		if err != nil {
			log.Errorf("Failed to get inner pulic library permission by library id %s: %v", libraryID, err)
			return ""
		}
		return perm
	}
	return ""
}

func getSharedDirsToUser(originLibraryID string, to account.ID) (map[string]string, error) {
	dirs := make(map[string]string)
	sqlStr := "SELECT v.path, s.permission FROM SharedLibrary s, VirtualLibrary v WHERE " +
		"s.library_id = v.library_id AND s.to_account_id = ? AND v.origin_library = ?"

	ctx, cancel := option.WithDBTimeout(context.Background())
	defer cancel()
	rows, err := db.QueryContext(ctx, sqlStr, to, originLibraryID)
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

func getSharedDirsToGroup(originLibraryID string, groups []group) (map[string]string, error) {
	dirs := make(map[string]string)
	groupIDs := convertGroupListToStr(groups)

	sqlStr := fmt.Sprintf("SELECT v.path, s.permission "+
		"FROM LibraryGroup s, VirtualLibrary v WHERE "+
		"s.library_id = v.library_id AND v.origin_library = ? "+
		"AND s.group_id in (%s)", groupIDs)

	ctx, cancel := option.WithDBTimeout(context.Background())
	defer cancel()
	rows, err := db.QueryContext(ctx, sqlStr, originLibraryID)
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

func checkPermOnParentLibrary(originLibraryID string, user account.ID, vPath string) string {
	var perm string
	userPerms, err := getSharedDirsToUser(originLibraryID, user)
	if err != nil {
		log.Errorf("Failed to get all shared folder perms in parent library %.8s for user %s", originLibraryID, user)
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

	groupPerms, err := getSharedDirsToGroup(originLibraryID, groups)
	if err != nil {
		log.Errorf("Failed to get all shared folder perm from parent library %.8s to all user groups", originLibraryID)
		return ""
	}
	if len(groupPerms) == 0 {
		return ""
	}

	perm = getDirPerm(groupPerms, vPath)

	return perm
}

// SharedLibrary is a shared library object
type SharedLibrary struct {
	Version      int    `json:"version"`
	ID           string `json:"id"`
	HeadCommitID string `json:"head_commit_id"`
	Name         string `json:"name"`
	MTime        int64  `json:"mtime"`
	Permission   string `json:"permission"`
	Type         string `json:"type"`
	Owner        string `json:"owner"`
	LibraryType  string `json:"-"`
}

// GetLibrariesByOwner get libraries by owner
func GetLibrariesByOwner(owner account.ID) ([]*SharedLibrary, error) {
	var libraries []*SharedLibrary

	query := "SELECT o.library_id, b.commit_id, i.name, " +
		"i.version, i.update_time, i.last_modifier, i.type FROM " +
		"LibraryOwner o LEFT JOIN Branch b ON o.library_id = b.library_id " +
		"LEFT JOIN LibraryInfo i ON o.library_id = i.library_id " +
		"LEFT JOIN VirtualLibrary v ON o.library_id = v.library_id " +
		"WHERE o.account_id=? AND " +
		"v.library_id IS NULL " +
		"ORDER BY i.update_time DESC, o.library_id"

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
		library := new(SharedLibrary)
		var libraryName, lastModifier, libraryType sql.NullString
		if err := rows.Scan(&library.ID, &library.HeadCommitID,
			&libraryName, &library.Version, &library.MTime,
			&lastModifier, &libraryType); err == nil {

			if library.HeadCommitID == "" {
				continue
			}
			if !libraryName.Valid || !lastModifier.Valid {
				continue
			}
			if libraryName.String == "" || lastModifier.String == "" {
				continue
			}
			library.Name = libraryName.String
			if libraryType.Valid {
				library.LibraryType = libraryType.String
			}
			libraries = append(libraries, library)
		}
	}

	if err := rows.Err(); err != nil {
		return nil, err
	}

	return libraries, nil
}

// ListInnerPubLibraries get inner public libraries
func ListInnerPubLibraries() ([]*SharedLibrary, error) {
	// Owner comes back as an address rather than an id: it is a field in a
	// JSON response, read by a client that has never heard of an account.
	// That is the whole shape of the identity split at the edge — the key is
	// an id everywhere inside, and the address is joined back on the way out.
	query := "SELECT InnerPubLibrary.library_id, " +
		"ae.email, permission, commit_id, i.name, " +
		"i.update_time, i.version, i.type " +
		"FROM InnerPubLibrary " +
		"LEFT JOIN LibraryInfo i ON InnerPubLibrary.library_id = i.library_id, LibraryOwner, Branch " +
		"LEFT JOIN AccountEmail ae ON ae.account_id = LibraryOwner.account_id AND ae.is_primary = 1 " +
		"WHERE InnerPubLibrary.library_id=LibraryOwner.library_id AND " +
		"InnerPubLibrary.library_id = Branch.library_id AND Branch.name = 'master'"

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

	var libraries []*SharedLibrary
	for rows.Next() {
		library := new(SharedLibrary)
		var libraryName, libraryType, owner sql.NullString
		if err := rows.Scan(&library.ID, &owner,
			&library.Permission, &library.HeadCommitID, &libraryName,
			&library.MTime, &library.Version, &libraryType); err == nil {

			if !libraryName.Valid {
				continue
			}
			if libraryName.String == "" {
				continue
			}
			library.Name = libraryName.String
			library.Owner = owner.String
			if libraryType.Valid {
				library.LibraryType = libraryType.String
			}
			libraries = append(libraries, library)
		}
	}

	if err := rows.Err(); err != nil {
		return nil, err
	}

	return libraries, nil
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
func ListSharedWithMe(id account.ID) ([]*SharedLibrary, error) {
	var libraries []*SharedLibrary
	const query = "SELECT sh.library_id, ae.email, " +
		"permission, commit_id, " +
		"i.name, i.update_time, i.version, i.type FROM " +
		"SharedLibrary sh LEFT JOIN LibraryInfo i ON sh.library_id = i.library_id " +
		"LEFT JOIN AccountEmail ae ON ae.account_id = sh.from_account_id AND ae.is_primary = 1, Branch b " +
		"WHERE sh.to_account_id=? AND " +
		"sh.library_id = b.library_id AND " +
		"b.name = 'master' " +
		"ORDER BY i.update_time DESC, sh.library_id"

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
		library := new(SharedLibrary)
		var libraryName, libraryType, other sql.NullString
		if err := rows.Scan(&library.ID, &other,
			&library.Permission, &library.HeadCommitID,
			&libraryName, &library.MTime, &library.Version, &libraryType); err == nil {

			if !libraryName.Valid {
				continue
			}
			if libraryName.String == "" {
				continue
			}
			library.Name = libraryName.String
			library.Owner = other.String
			if libraryType.Valid {
				library.LibraryType = libraryType.String
			}

			libraries = append(libraries, library)
		}
	}

	if err := rows.Err(); err != nil {
		return nil, err
	}

	return libraries, nil
}

// GetGroupLibrariesByUser get group libraries by user
func GetGroupLibrariesByUser(user account.ID) ([]*SharedLibrary, error) {
	groups, err := getGroupsByUser(user, true)
	if err != nil {
		return nil, err
	}
	if len(groups) == 0 {
		return nil, nil
	}

	var sqlBuilder strings.Builder
	sqlBuilder.WriteString("SELECT g.library_id, " +
		"ae.email, permission, commit_id, " +
		"i.name, i.update_time, i.version, i.type " +
		"FROM LibraryGroup g " +
		"LEFT JOIN LibraryInfo i ON g.library_id = i.library_id " +
		"LEFT JOIN AccountEmail ae ON ae.account_id = g.account_id AND ae.is_primary = 1, " +
		"Branch b WHERE g.library_id = b.library_id AND " +
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

	var libraries []*SharedLibrary
	for rows.Next() {
		gLibrary := new(SharedLibrary)
		var libraryType, sharer sql.NullString
		if err := rows.Scan(&gLibrary.ID, &sharer,
			&gLibrary.Permission, &gLibrary.HeadCommitID,
			&gLibrary.Name, &gLibrary.MTime, &gLibrary.Version, &libraryType); err == nil {
			gLibrary.Owner = sharer.String
			if libraryType.Valid {
				gLibrary.LibraryType = libraryType.String
			}
			libraries = append(libraries, gLibrary)
		}
	}

	if err := rows.Err(); err != nil {
		return nil, err
	}

	return libraries, nil
}
