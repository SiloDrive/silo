// Package share answers who may do what in a library: the grant model, and
// the permission check over it.
package share

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"

	"github.com/SiloDrive/silo/fileserver/account"
	"github.com/SiloDrive/silo/fileserver/libmgr"
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

// CheckPerm answers what a user may do in a library: "rw", "r", or "" for
// nothing at all.
//
// A virtual library -- the entity a shared subfolder gets its own id from -- is
// answered against the library it was cut out of, since that is where both the
// ownership and the grants actually live. A lookup that fails leaves vInfo nil
// and the library is treated as an ordinary one, which is the conservative
// reading: it asks about grants on the id in hand rather than inheriting an
// origin's.
func CheckPerm(libraryID string, user account.ID) string {
	vInfo, err := libmgr.GetVirtualLibraryInfo(libraryID)
	if err != nil {
		log.Errorf("Failed to get virtual library info by library id %s: %v", libraryID, err)
	}
	if vInfo != nil {
		return checkVirtualLibraryPerm(vInfo.OriginLibraryID, user, vInfo.Path)
	}
	return checkLibrarySharePerm(libraryID, user)
}

// checkVirtualLibraryPerm answers for a subfolder share, in the order the
// answers get weaker: the origin's owner may do anything, then a grant on a
// folder at or above this one, then a grant on the origin library as a whole.
func checkVirtualLibraryPerm(originLibraryID string, user account.ID, vPath string) string {
	owner, err := libmgr.GetLibraryOwner(originLibraryID)
	if err != nil {
		log.Errorf("Failed to get library owner: %v", err)
	}
	if !owner.IsZero() && owner == user {
		return "rw"
	}
	if perm := checkPermOnParentLibrary(originLibraryID, user, vPath); perm != "" {
		return perm
	}
	return checkLibrarySharePerm(originLibraryID, user)
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
	dirs := map[string]string{}
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

// nearestGrant walks up from a path to the closest folder that was shared,
// because a share on a folder reaches everything beneath it. It answers "" when
// no folder on the way up was shared.
//
// The three values it stops on are the fixed points filepath.Dir converges to:
// "/" from an absolute path, "." from a relative one, and "" only if it is
// handed "" to begin with. Reaching one of them means the walk is at the top
// with nothing found -- and the library root is deliberately not consulted
// here, because a grant on the whole library is permFor's question and is asked
// separately.
//
// The map may hold a folder shared to any of the principals asked about, so the
// answer is the nearest ancestor's permission and not the strongest one found
// on the way. That is the right reading while a principal stands for exactly
// one account: a folder shared to you more specifically is a later decision
// than one shared further up. It is worth revisiting if a principal ever stands
// for a set of accounts, since "shared to a group above" and "shared to you
// below" would then be two different kinds of claim on the same path.
func nearestGrant(perms map[string]string, path string) string {
	for dir := path; dir != "/" && dir != "." && dir != ""; dir = filepath.Dir(dir) {
		if perm, ok := perms[dir]; ok {
			return perm
		}
	}
	return ""
}

// checkPermOnParentLibrary answers for a path inside a library by finding the
// nearest shared folder at or above it.
func checkPermOnParentLibrary(originLibraryID string, user account.ID, vPath string) string {
	ctx, cancel := ctxWithTimeout()
	defer cancel()

	dirs, err := grantedDirs(ctx, originLibraryID, PrincipalsFor(user))
	if err != nil {
		log.Errorf("Failed to get shared folders in %.8s for user %s: %v", originLibraryID, user, err)
		return ""
	}
	return nearestGrant(dirs, vPath)
}
