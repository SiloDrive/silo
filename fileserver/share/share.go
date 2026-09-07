// Package share answers who may do what in a library: the grant model, and
// the permission check over it.
package share

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"

	"github.com/dkam/silo/fileserver/account"
	"github.com/dkam/silo/fileserver/libmgr"
	log "github.com/sirupsen/logrus"
)

// db is the read handle on the one Silo database.
var db *sql.DB

// Init sets the database handles.
//
// Two handles. Checking a permission is a read and this package held only a
// read handle for as long as that was all it did; recording a grant is a
// write, and the grant model is the first thing here that writes.
func Init(readDB, siloWriteDB *sql.DB) {
	db = readDB
	writeDB = siloWriteDB
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

	dirs, err := grantedDirs(ctx, originLibraryID, PrincipalsFor(user))
	if err != nil {
		log.Errorf("Failed to get shared folders in %.8s for user %s: %v", originLibraryID, user, err)
		return ""
	}
	return getDirPerm(dirs, vPath)
}
