// Package share manages share relations.
// share: manages personal shares and provide high level permission check functions.
package share

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"

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

// Init sets the database handles, the group table name and cloud mode.
//
// Two handles now. Checking a permission is a read and this package held only
// a read handle for as long as that was all it did; recording a grant is a
// write, and the grant model is the first thing here that writes.
func Init(readDB, siloWriteDB *sql.DB, grpTableName string, clMode bool) {
	db = readDB
	writeDB = siloWriteDB
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
func checkLibrarySharePerm(libraryID string, user account.ID) string {
	owner, err := libmgr.GetLibraryOwner(libraryID)
	if err != nil {
		log.Errorf("Failed to get library owner: %v", err)
	}
	if !owner.IsZero() && owner == user {
		return "rw"
	}

	ctx, cancel := ctxWithTimeout()
	defer cancel()

	perm, err := permFor(ctx, libraryID, rootPath, PrincipalsFor(user))
	if err != nil {
		log.Errorf("Failed to read grants on library %s: %v", libraryID, err)
		return ""
	}
	if perm != "" {
		return perm
	}
	if cloudMode {
		return ""
	}
	perm, err = permFor(ctx, libraryID, rootPath, []Principal{Anon})
	if err != nil {
		log.Errorf("Failed to read the anonymous grant on library %s: %v", libraryID, err)
		return ""
	}
	return perm
}

// grantedDirs maps each shared subfolder of an origin library to what these
// principals may do in it.
//
// A subfolder share is a grant on a virtual library -- the entity that gives a
// folder its own id -- so in the grant model this is an ordinary whole-library
// grant joined back to the path the virtual library stands for. Two functions
// became one because the only thing that differed between them was which
// principals were being asked about, which is now a parameter rather than a
// second query.
func grantedDirs(ctx context.Context, originLibraryID string, principals []Principal) (map[string]string, error) {
	dirs := make(map[string]string)
	if len(principals) == 0 {
		return dirs, nil
	}
	q := `SELECT v.path, g.perm
	      FROM LibraryGrant g JOIN VirtualLibrary v ON v.library_id = g.library_id
	      WHERE v.origin_library = ? AND g.path = ? AND g.principal IN (` +
		placeholders(len(principals)) + `)`
	args := make([]any, 0, len(principals)+2)
	args = append(args, originLibraryID, rootPath)
	for _, p := range principals {
		args = append(args, p)
	}
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to get shared directories in %s: %v", originLibraryID, err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var path, perm string
		if err := rows.Scan(&path, &perm); err != nil {
			return nil, err
		}
		// A folder reached through two principals takes the stronger, the same
		// rule two groups sharing one library already followed.
		dirs[path] = stronger(dirs[path], perm)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to get shared directories in %s: %v", originLibraryID, err)
	}
	return dirs, nil
}

// checkPermOnParentLibrary answers for a path inside a library, by finding the
// nearest shared folder above it.
//
// The precedence is the same one permFor applies to a whole library, and for
// the same reason: a grant naming you is a decision about you, and one naming a
// group you belong to is not. Kept as two lookups rather than one because the
// answer is a nearest-ancestor walk per principal kind, not a strongest-wins
// over a set -- a folder shared to you directly must answer even when a
// shallower folder was shared to a group you are in.
// getDirPerm walks up from a path to the nearest folder that was shared,
// because a share on a folder reaches everything under it.
//
// If the path is empty, filepath.Dir returns "."; if it is all separators, it
// returns a single separator. Both terminate the loop.
func getDirPerm(perms map[string]string, path string) string {
	tmp := path
	for tmp != "/" && tmp != "." && tmp != "" {
		if perm, exists := perms[tmp]; exists {
			return perm
		}
		tmp = filepath.Dir(tmp)
	}
	return ""
}

func checkPermOnParentLibrary(originLibraryID string, user account.ID, vPath string) string {
	ctx, cancel := ctxWithTimeout()
	defer cancel()

	userDirs, err := grantedDirs(ctx, originLibraryID, []Principal{UserPrincipal(user)})
	if err != nil {
		log.Errorf("Failed to get shared folders in %.8s for user %s: %v", originLibraryID, user, err)
		return ""
	}
	if perm := getDirPerm(userDirs, vPath); perm != "" {
		return perm
	}

	groups, err := getGroupsByUser(user, false)
	if err != nil {
		log.Errorf("Failed to get groups by user %s: %v", user, err)
		return ""
	}
	if len(groups) == 0 {
		return ""
	}
	principals := make([]Principal, 0, len(groups))
	for _, g := range groups {
		principals = append(principals, GroupPrincipal(g.id))
	}
	groupDirs, err := grantedDirs(ctx, originLibraryID, principals)
	if err != nil {
		log.Errorf("Failed to get shared folders in %.8s for the groups of %s: %v",
			originLibraryID, user, err)
		return ""
	}
	return getDirPerm(groupDirs, vPath)
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
