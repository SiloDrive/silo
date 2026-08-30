package silod

import (
	"fmt"
	"net/http"
	"net/url"
	upath "path"
	"strings"
	"unicode/utf8"

	"github.com/dkam/silo/fileserver/libmgr"
	"github.com/dkam/silo/fileserver/middleware"
	"github.com/dkam/silo/fileserver/objmgr"
	"github.com/dkam/silo/store"
	"github.com/gorilla/mux"
	log "github.com/sirupsen/logrus"
)

// checkEntryName applies the same name guard the upload path uses before a
// dirent reaches the tree, replying 400 when the name is rejected. Syncing
// clients write dirent names straight to disk relative to the library root,
// so "..", embedded separators, invalid UTF-8 and over-long names must never
// be stored.
func checkEntryName(w http.ResponseWriter, name string) bool {
	if !validEntryName(name) {
		http.Error(w, "Invalid name", http.StatusBadRequest)
		return false
	}
	return true
}

// validEntryName is the same rule without a response to write, for callers
// that report their failures somewhere other than straight down the wire — a
// batch names the operation that failed, so it cannot let the check answer for
// it.
func validEntryName(name string) bool {
	return name != "" && name != "." && !shouldIgnoreFile(name)
}

// shouldIgnoreFile is the rule for a single path component: no traversal, no
// invalid UTF-8, nothing absurdly long, and no separator smuggled inside what
// is supposed to be one name.
func shouldIgnoreFile(name string) bool {
	if strings.Contains(name, "/") || strings.Contains(name, "\\") {
		return true
	}
	for _, part := range strings.Split(name, "/") {
		if part == ".." {
			return true
		}
	}
	if !utf8.ValidString(name) {
		log.Warnf("file name %s contains non-UTF8 characters, skip", name)
		return true
	}
	return len(name) >= 256
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

// patchLibraryHandler handles PATCH /api/silo/v1/libraries/{libraryid}. Renaming is the
// only field so far, which is why this is a PATCH and not a PUT: the body names
// what changes, and everything unmentioned is left alone, so adding a second
// mutable field later does not change what an existing client's request means.
//
// The operation itself already existed, form-encoded, on /api2/ — but reaching
// it meant a Silo-lane client holding a second credential on a frozen lane to
// rename a library it can already create and delete. That is the crossing this
// lane exists to remove.
func patchLibraryHandler(w http.ResponseWriter, r *http.Request) {
	libraryID := mux.Vars(r)["libraryid"]

	var body struct {
		Name string `json:"name"`
	}
	if !decodeJSONBody(w, r, 64<<10, &body, `Expected a JSON body such as {"name":"New name"}`) {
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" {
		http.Error(w, `name is required, as {"name":"New name"}`, http.StatusBadRequest)
		return
	}
	// A library name is not a dirent, but it reaches the same places: clients
	// create a directory named after it. The guard the tree uses is the guard
	// it needs.
	if !checkEntryName(w, name) {
		return
	}

	// Renaming a library is about the library, not a path inside it.
	library := entryLibrary(w, r, libraryID, "", true)
	if library == nil {
		return
	}
	if !renameLibrary(w, r, library, name) {
		return
	}

	writeEntryJSON(w, http.StatusOK, map[string]any{"id": library.ID, "name": name})
}

// renameLibrary gives a library a new name, answering the request
// itself on failure. It reports whether the caller should carry on.
// renameLibrary changes a library's display name.
//
// It is one UPDATE and mints no commit. The old spelling wrote a commit whose
// only change was the name it carried, which moved the head for a change that
// was not in the tree: every client saw a new head, fetched it, diffed two
// identical roots and found nothing. The name is catalog data now, and under
// E2EE the server could not write that commit at all.
func renameLibrary(w http.ResponseWriter, r *http.Request, library *libmgr.Library, newName string) bool {
	if err := libmgr.SetLibraryName(library.ID, newName); err != nil {
		log.WithContext(r.Context()).WithError(err).Errorf("library rename failed")
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return false
	}
	return true
}

// mkdirHandler creates one directory in a library.
//
// Mkdir rather than MkdirAll: the parent must exist. A typo in a path should
// not silently build a directory tree, and a client that wants mkdir -p asks
// for it a directory at a time — the same rule putEntryFile applies to a
// file's parent, stated once in each place it is enforced.
func mkdirHandler(w http.ResponseWriter, r *http.Request) {
	acct := middleware.GetAccount(r)
	vars := mux.Vars(r)
	libraryID := vars["libraryid"]

	path, _ := url.QueryUnescape(r.URL.Query().Get("path"))
	if path == "" {
		http.Error(w, "path is required", http.StatusBadRequest)
		return
	}

	dirName := upath.Base(path)
	if !checkEntryName(w, dirName) {
		return
	}

	library := entryLibrary(w, r, libraryID, path, true)
	if library == nil {
		return
	}
	newRoot, _, err := mutateTree(library, acct.Email, func(st *objmgr.Store, root store.ID, now int64) (store.ID, error) {
		return st.Mkdir(root, path, defaultDirMode, now)
	})
	if err != nil {
		writeTreeErr(w, r, err, "Parent directory does not exist",
			fmt.Sprintf("mkdir %s in library %s", path, library.ID))
		return
	}

	// The id of the directory that was made. A client models a write's answer
	// on a listing row, where id is always present, so omitting it does not
	// read as a directory whose id is unknown — it fails to decode at all,
	// which is what made this the one create verb a client had to special-case.
	//
	// Resolved against the root this commit published rather than against the
	// head, which may already have moved: a writer that landed in between
	// would otherwise have this report their directory, or none.
	st, err := library.Store()
	if err != nil {
		log.WithContext(r.Context()).WithError(err).Errorf("failed to open store for library %s", library.ID)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
	made, err := st.Resolve(newRoot, path)
	if err != nil {
		log.WithContext(r.Context()).WithError(err).Errorf("mkdir %s in library %s committed but did not resolve", path, library.ID)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	// The same ETag a file write and a copy return, for the same reason: the
	// directory is empty and its id says so, so a client can file it in its
	// cache without a follow-up GET. A directory carries no size — the listing
	// omits it there too, and the sum of what is under it is a different
	// question with a different endpoint.
	w.Header().Set("ETag", `"`+etagPrefix+made.ID.String()+`"`)
	writeEntryJSON(w, http.StatusCreated, map[string]any{
		"name": dirName, "type": "dir", "id": made.ID.String(),
	})
}

func deleteFileHandler(w http.ResponseWriter, r *http.Request) {
	acct := middleware.GetAccount(r)
	vars := mux.Vars(r)
	libraryID := vars["libraryid"]

	path, _ := url.QueryUnescape(r.URL.Query().Get("path"))
	if path == "" {
		http.Error(w, "path is required", http.StatusBadRequest)
		return
	}

	library := entryLibrary(w, r, libraryID, path, true)
	if library == nil {
		return
	}
	// A directory goes with everything under it — the tree is content-addressed,
	// so dropping the edge drops the subtree, and what that leaves unreferenced
	// is the collector's business rather than this request's.
	if _, _, err := mutateTree(library, acct.Email, func(st *objmgr.Store, root store.ID, now int64) (store.ID, error) {
		return st.Remove(root, path, now)
	}); err != nil {
		writeTreeErr(w, r, err, "Not found",
			fmt.Sprintf("delete %s in library %s", path, library.ID))
		return
	}

	w.WriteHeader(http.StatusOK)
}

func moveHandler(w http.ResponseWriter, r *http.Request) { moveOrCopy(w, r, false) }

// copyHandler serves {"op":"copy"}. Copying is the same tree edit as moving
// with the delete left off: the new dirent points at the object the source
// already names, so no bytes are read, nothing new reaches the chunk store, and
// a copy costs one dirent and one commit whether it is an empty file or a
// hundred-gigabyte subtree. A client emulating it with a download followed by an
// upload pays the entire content twice for the one operation the store gives
// away.
func copyHandler(w http.ResponseWriter, r *http.Request) { moveOrCopy(w, r, true) }

func moveOrCopy(w http.ResponseWriter, r *http.Request, isCopy bool) {
	acct := middleware.GetAccount(r)
	vars := mux.Vars(r)
	libraryID := vars["libraryid"]

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
	verb := "move"
	if isCopy {
		verb = "copy"
	}
	dstName := upath.Base(dstPath)
	if !checkEntryName(w, dstName) {
		return
	}

	// Both ends. A credential that may write the destination but not read the
	// source would otherwise pull content out of a subtree it cannot reach.
	if middleware.Perm(r, libraryID, srcPath) == "" {
		http.Error(w, "Permission denied", http.StatusForbidden)
		return
	}
	library := entryLibrary(w, r, libraryID, dstPath, true)
	if library == nil {
		return
	}
	st, err := library.Store()
	if err != nil {
		log.WithContext(r.Context()).WithError(err).Errorf("failed to open store for library %s", libraryID)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
	root, err := store.ParseID(library.RootID)
	if err != nil {
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	src, err := st.Resolve(root, srcPath)
	if err != nil {
		writeTreeErr(w, r, err, "Source not found",
			fmt.Sprintf("resolve %s in library %s", srcPath, libraryID))
		return
	}

	// Copying is exempt: the destination entry holds the source's id, which
	// names the subtree as it stands at this commit, so a copy into its own
	// subtree is a snapshot of a finite thing and terminates. A move cannot be,
	// because its delete would take the copy with it.
	if !isCopy && src.IsDir() && movesIntoOwnSubtree(srcPath, upath.Dir(dstPath)) {
		http.Error(w, "Cannot move a directory into itself", http.StatusBadRequest)
		return
	}

	// A destination that already holds a directory would lose its whole
	// subtree to the replacement — silent data loss reported as 200. Refused
	// before anything is written. File-onto-file is deliberately still
	// allowed: it is the one collision that destroys nothing the caller did
	// not name, and it matches PUT entries/{path}, which replaces.
	if dst, err := st.Resolve(root, dstPath); err == nil {
		switch {
		case dst.IsDir():
			http.Error(w, "Destination exists and is a directory", http.StatusConflict)
			return
		case src.IsDir():
			http.Error(w, "Destination exists and is a file", http.StatusConflict)
			return
		}
	}

	if _, _, err := mutateTree(library, acct.Email, func(st *objmgr.Store, root store.ID, now int64) (store.ID, error) {
		if !isCopy {
			return st.Rename(root, srcPath, dstPath, now)
		}
		// A copy is a move with the delete left off: the new entry points at
		// the object the source already names, so no content moves and a
		// directory copies in constant time however large it is.
		node, err := st.Resolve(root, srcPath)
		if err != nil {
			return store.ID{}, err
		}
		return st.PutNode(root, dstPath, objmgr.Node{
			ID: node.ID, Type: node.Type, Name: dstName,
			Mtime: now, Mode: node.Mode,
		}, now)
	}); err != nil {
		writeTreeErr(w, r, err, "Parent directory does not exist",
			fmt.Sprintf("%s %s in library %s", verb, srcPath, libraryID))
		return
	}

	if !isCopy {
		w.WriteHeader(http.StatusOK)
		return
	}

	// A copy creates a resource where there was none, so it answers 201 and
	// describes what it made — the same shape PUT entries/{path} returns, and
	// with the same ETag, because a copy is content-addressed and shares the
	// source's id. A client can file the destination in its cache without a
	// follow-up GET, and will find it already has the content.
	entryType := "file"
	if src.IsDir() {
		entryType = "dir"
	}
	w.Header().Set("ETag", `"`+etagPrefix+src.ID.String()+`"`)
	writeEntryJSON(w, http.StatusCreated, map[string]any{
		"name": dstName, "type": entryType, "id": src.ID.String(),
	})
}
