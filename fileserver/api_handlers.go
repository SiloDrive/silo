package silod

import (
	"fmt"
	"net/http"
	"net/url"
	upath "path"
	"strings"
	"syscall"
	"time"

	"github.com/dkam/silo/fileserver/commitmgr"
	"github.com/dkam/silo/fileserver/fsmgr"
	"github.com/dkam/silo/fileserver/middleware"
	"github.com/dkam/silo/fileserver/repomgr"
	"github.com/dkam/silo/fileserver/share"
	"github.com/gorilla/mux"
	log "github.com/sirupsen/logrus"
)

// loadRepoAndCommit loads the repo and its head commit, with rw permission check.
func loadRepoAndCommit(w http.ResponseWriter, repoID, user string) (*repomgr.Repo, *commitmgr.Commit, bool) {
	perm := share.CheckPerm(repoID, user)
	if perm != "rw" {
		http.Error(w, "Permission denied", http.StatusForbidden)
		return nil, nil, false
	}
	repo, err := repomgr.GetWithReason(repoID)
	if err != nil {
		code, msg := repomgr.StatusFor(err)
		http.Error(w, msg, code)
		return nil, nil, false
	}
	head, err := commitmgr.Load(repo.ID, repo.HeadCommitID)
	if err != nil {
		log.Errorf("Failed to load head commit: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return nil, nil, false
	}
	return repo, head, true
}

// checkEntryName applies the same name guard the upload path uses before a
// dirent reaches the tree, replying 400 when the name is rejected. Syncing
// clients write dirent names straight to disk relative to the library root,
// so "..", embedded separators, invalid UTF-8 and over-long names must never
// be stored.
func checkEntryName(w http.ResponseWriter, name string) bool {
	if name == "" || name == "." || shouldIgnoreFile(name) {
		http.Error(w, "Invalid name", http.StatusBadRequest)
		return false
	}
	return true
}

// movesIntoOwnSubtree reports whether dstDir sits at or beneath srcPath. A
// move is add-then-delete, so relocating a directory beneath itself would put
// the copy inside the tree that the delete then removes, destroying it. The
// trailing separator keeps "/ab" from matching a move into "/abc".
func movesIntoOwnSubtree(srcPath, dstDir string) bool {
	src := upath.Clean(srcPath)
	dst := upath.Clean(dstDir)
	if dst == src {
		return true
	}
	// Everything is beneath the root. moveHandler cannot reach this (it trims
	// the trailing slash and rejects the resulting empty src), but src+"/"
	// below would be "//" and match nothing, so handle it explicitly.
	if src == "/" {
		return true
	}
	return strings.HasPrefix(dst, src+"/")
}

// destructiveCollision reports why moving an entry of mode srcMode onto dst
// would destroy data, or "" when the move is safe. dst is the dirent already at
// the destination path, or nil when nothing is there.
//
// A move replaces its destination, so the type of what is being replaced decides
// how much is lost. Replacing a directory unlinks its whole subtree; replacing a
// file with a directory is the same trade in the other direction. Only
// file-onto-file loses nothing the caller did not name, which is why it is the
// one collision left to proceed — PUT entries/{path} already replaces rather
// than renaming, and a move should not be stricter than a write.
func destructiveCollision(srcMode uint32, dst *fsmgr.SeafDirent) string {
	if dst == nil {
		return ""
	}
	if fsmgr.IsDir(dst.Mode) {
		return "Destination exists and is a directory"
	}
	if fsmgr.IsDir(srcMode) {
		return "Destination exists and is a file"
	}
	return ""
}

func renameRepoHandler(w http.ResponseWriter, r *http.Request) {
	user := middleware.GetUserEmail(r)
	repoID := mux.Vars(r)["repoid"]

	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid form data", http.StatusBadRequest)
		return
	}
	newName := r.FormValue("repo_name")
	if newName == "" {
		http.Error(w, "repo_name is required", http.StatusBadRequest)
		return
	}

	repo, head, ok := loadRepoAndCommit(w, repoID, user)
	if !ok {
		return
	}

	repo.Name = newName
	desc := fmt.Sprintf("Renamed library to \"%s\"", newName)
	_, err := GenNewCommit(repo, head, head.RootID, user, desc, false, "", false)
	if err != nil {
		writeCommitErr(w, r, err, "library rename")
		return
	}

	w.WriteHeader(http.StatusOK)
}

func mkdirHandler(w http.ResponseWriter, r *http.Request) {
	user := middleware.GetUserEmail(r)
	vars := mux.Vars(r)
	repoID := vars["repoid"]

	path, _ := url.QueryUnescape(r.URL.Query().Get("path"))
	if path == "" {
		http.Error(w, "path is required", http.StatusBadRequest)
		return
	}

	parentDir := upath.Dir(path)
	dirName := upath.Base(path)
	if !checkEntryName(w, dirName) {
		return
	}

	repo, head, ok := loadRepoAndCommit(w, repoID, user)
	if !ok {
		return
	}

	mode := uint32(syscall.S_IFDIR | 0644)
	dent := fsmgr.NewDirent(fsmgr.EmptySha1, dirName, mode, time.Now().Unix(), "", 0)

	var names []string
	newRootID, err := DoPostMultiFiles(repo, head.RootID, parentDir, []*fsmgr.SeafDirent{dent}, user, false, &names)
	if err != nil {
		log.Errorf("Failed to create directory: %v", err)
		http.Error(w, "Failed to create directory", http.StatusInternalServerError)
		return
	}

	desc := fmt.Sprintf("Added directory \"%s\"", dirName)
	_, err = GenNewCommit(repo, head, newRootID, user, desc, false, "", false)
	if err != nil {
		writeCommitErr(w, r, err, "mkdir")
		return
	}

	w.WriteHeader(http.StatusCreated)
}

func deleteFileHandler(w http.ResponseWriter, r *http.Request) {
	user := middleware.GetUserEmail(r)
	vars := mux.Vars(r)
	repoID := vars["repoid"]

	path, _ := url.QueryUnescape(r.URL.Query().Get("path"))
	if path == "" {
		http.Error(w, "path is required", http.StatusBadRequest)
		return
	}

	repo, head, ok := loadRepoAndCommit(w, repoID, user)
	if !ok {
		return
	}

	parentDir := upath.Dir(path)
	filename := upath.Base(path)

	newRootID, err := DelFileFromTree(repo.StoreID, head.RootID, parentDir, filename)
	if err != nil {
		log.Errorf("Failed to delete %s: %v", path, err)
		http.Error(w, fmt.Sprintf("Failed to delete: %v", err), http.StatusNotFound)
		return
	}

	desc := fmt.Sprintf("Deleted \"%s\"", filename)
	_, err = GenNewCommit(repo, head, newRootID, user, desc, false, "", false)
	if err != nil {
		writeCommitErr(w, r, err, "delete")
		return
	}

	w.WriteHeader(http.StatusOK)
}

func moveHandler(w http.ResponseWriter, r *http.Request) {
	user := middleware.GetUserEmail(r)
	vars := mux.Vars(r)
	repoID := vars["repoid"]

	srcPath, _ := url.QueryUnescape(r.URL.Query().Get("src"))
	dstPath, _ := url.QueryUnescape(r.URL.Query().Get("dst"))
	srcPath = strings.TrimRight(srcPath, "/")
	dstPath = strings.TrimRight(dstPath, "/")
	if srcPath == "" || dstPath == "" {
		http.Error(w, "src and dst are required", http.StatusBadRequest)
		return
	}
	if srcPath == dstPath {
		http.Error(w, "src and dst are the same", http.StatusBadRequest)
		return
	}
	if !checkEntryName(w, upath.Base(dstPath)) {
		return
	}

	repo, head, ok := loadRepoAndCommit(w, repoID, user)
	if !ok {
		return
	}

	// Get the existing entry
	srcEntry, err := fsmgr.GetDirentByPath(repo.StoreID, head.RootID, srcPath)
	if err != nil || srcEntry == nil {
		http.Error(w, "Source not found", http.StatusNotFound)
		return
	}

	srcDir := upath.Dir(srcPath)
	srcName := upath.Base(srcPath)
	dstDir := upath.Dir(dstPath)
	dstName := upath.Base(dstPath)

	if fsmgr.IsDir(srcEntry.Mode) && movesIntoOwnSubtree(srcPath, dstDir) {
		http.Error(w, "Cannot move a directory into itself", http.StatusBadRequest)
		return
	}

	// Phase 1 replaces whatever already sits at the destination, and until this
	// guard nothing established what that was. Overwriting a directory swaps its
	// dirent for the source's, which unreachables every descendant in a single
	// commit — silent data loss reported as 200. Refuse before phase 1 runs, the
	// same shape of guard and for the same reason as movesIntoOwnSubtree.
	//
	// File-onto-file is deliberately still allowed: it is the one collision that
	// destroys nothing the caller did not name, and it matches PUT entries/{path},
	// which replaces rather than renaming the collision.
	dstEntry, err := fsmgr.GetDirentByPath(repo.StoreID, head.RootID, dstPath)
	if err != nil {
		dstEntry = nil // absent, which is the ordinary case
	}
	if reason := destructiveCollision(srcEntry.Mode, dstEntry); reason != "" {
		http.Error(w, reason, http.StatusConflict)
		return
	}

	// Phase 1: Add to destination
	newDent := fsmgr.NewDirent(srcEntry.ID, dstName, srcEntry.Mode, time.Now().Unix(), srcEntry.Modifier, srcEntry.Size)
	var names []string
	rootAfterAdd, err := DoPostMultiFiles(repo, head.RootID, dstDir, []*fsmgr.SeafDirent{newDent}, user, true, &names)
	if err != nil {
		log.Errorf("Failed to add to destination: %v", err)
		http.Error(w, "Failed to move: destination error", http.StatusInternalServerError)
		return
	}

	// Phase 2: Remove from source
	newRootID, err := DelFileFromTree(repo.StoreID, rootAfterAdd, srcDir, srcName)
	if err != nil {
		log.Errorf("Failed to remove from source: %v", err)
		http.Error(w, "Failed to move: source error", http.StatusInternalServerError)
		return
	}

	desc := fmt.Sprintf("Moved \"%s\"", srcName)
	_, err = GenNewCommit(repo, head, newRootID, user, desc, false, "", false)
	if err != nil {
		writeCommitErr(w, r, err, "move")
		return
	}

	w.WriteHeader(http.StatusOK)
}
