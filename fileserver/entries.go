package silod

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/dkam/silo/fileserver/api"
	"github.com/dkam/silo/fileserver/fsmgr"
	"github.com/dkam/silo/fileserver/middleware"
	"github.com/dkam/silo/fileserver/repomgr"
	"github.com/dkam/silo/fileserver/share"
	"github.com/gorilla/mux"
)

// The entries API: one addressable noun for everything in a library, with the
// HTTP methods as the verbs.
//
//	GET    /api/silo/v1/repos/{repo}/entries/{path}   dir -> listing, file -> bytes
//	HEAD   /api/silo/v1/repos/{repo}/entries/{path}   headers only
//	PUT    /api/silo/v1/repos/{repo}/entries/{path}   create a directory
//	DELETE /api/silo/v1/repos/{repo}/entries/{path}
//	POST   /api/silo/v1/repos/{repo}/entries/{path}   {"op":"move","to":"/x/y"}
//
// It exists beside the older dir/download/file/mkdir/rename/move endpoints
// rather than replacing them: those have in-tree callers, and keeping both
// lets a new client be written against one shape while the TUI and CLI move
// over at their own pace. Nothing outside this repository has ever used the
// older spelling, so it stays deletable.
//
// Rename is absent on purpose — renaming is moving, and a client that has to
// tell them apart can compare the parent directories itself.
//
// The handlers here resolve, validate and answer conditional requests, then
// delegate the actual mutation to the existing handlers by rewriting the query
// string they read. That keeps one implementation of the commit machinery: two
// copies would drift, and the copy with fewer callers would drift silently.

// etagPrefix versions the *representation*, not the object. The id identifies
// the bytes; the ETag identifies what this API makes of them. Without the
// prefix, changing the listing JSON would leave clients holding cache entries
// that validate against a body shape that no longer exists.
const etagPrefix = "v1-"

// entriesHandler dispatches on method. Registered for one route so that an
// unsupported method gets a 405 naming what is allowed, rather than a 404
// suggesting the path is wrong.
func entriesHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		getEntry(w, r)
	case http.MethodPut:
		putEntry(w, r)
	case http.MethodDelete:
		deleteEntry(w, r)
	case http.MethodPost:
		postEntry(w, r)
	default:
		w.Header().Set("Allow", "GET, HEAD, PUT, POST, DELETE")
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

// entryPath turns the matched path variable into the rooted, cleaned form the
// rest of the API speaks. The empty match — /entries/ with nothing after it —
// is the library root.
func entryPath(raw string) string {
	raw = strings.TrimRight(raw, "/")
	if raw == "" {
		return "/"
	}
	if raw[0] != '/' {
		return "/" + raw
	}
	return raw
}

// resolved is what a path points at right now: enough to answer a conditional
// request without reading any content.
type resolved struct {
	id    string
	isDir bool
	mtime int64
	size  int64
}

// resolve looks up a path in the current head tree. The root has no dirent of
// its own — nothing contains it — so its id is the commit's root id, which is
// exactly as good an ETag: it changes whenever anything in the library does.
func resolve(repo *repomgr.Repo, path string) (*resolved, error) {
	if path == "/" {
		return &resolved{id: repo.RootID, isDir: true}, nil
	}
	dent, err := fsmgr.GetDirentByPath(repo.StoreID, repo.RootID, path)
	if err != nil {
		return nil, err
	}
	return &resolved{
		id:    dent.ID,
		isDir: fsmgr.IsDir(dent.Mode),
		mtime: dent.Mtime,
		size:  dent.Size,
	}, nil
}

// getEntry answers a read, conditionally.
//
// The 304 is the point of the whole design: an object's id is its content
// hash, so it is already a strong ETag, and validating one costs a single
// dirent lookup in the parent directory — no blocks are read at all. A client
// re-checking a materialised file pays almost nothing to learn it is current.
func getEntry(w http.ResponseWriter, r *http.Request) {
	user := middleware.GetUserEmail(r)
	vars := mux.Vars(r)
	repoID := vars["repoid"]
	path := entryPath(vars["path"])

	// Read-only access is enough here, unlike the mutating paths which
	// require "rw", so the permission check is spelled out rather than
	// borrowed from loadRepoAndCommit.
	if perm := share.CheckPerm(repoID, user); perm == "" {
		http.Error(w, "Permission denied", http.StatusForbidden)
		return
	}
	repo := repomgr.Get(repoID)
	if repo == nil {
		http.Error(w, "Repo not found", http.StatusNotFound)
		return
	}

	entry, err := resolve(repo, path)
	if err != nil {
		http.Error(w, "Not found", http.StatusNotFound)
		return
	}

	etag := `"` + etagPrefix + entry.id + `"`
	w.Header().Set("ETag", etag)
	if entry.mtime > 0 {
		w.Header().Set("Last-Modified", time.Unix(entry.mtime, 0).UTC().Format(http.TimeFormat))
	}
	if matchesETag(r.Header.Get("If-None-Match"), etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}

	// Delegated by rewriting the query the older handler reads. Both of them
	// re-check the permission and reload the repo; that is a little wasted
	// work in exchange for there being exactly one implementation of each
	// operation.
	if entry.isDir {
		setQuery(r, url.Values{"path": {path}})
		api.ListDirHandler(w, r)
		return
	}
	setQuery(r, url.Values{"path": {path}})
	downloadFileHandler(w, r)
}

// putEntry creates a directory.
//
// File content is not accepted yet: the upload path indexes blocks straight
// out of a multipart part (indexBlocks takes a *multipart.FileHeader), so a
// raw body needs a reader-shaped variant of it before this can be honest about
// storing bytes. Returning 501 says which half is missing; pretending to
// accept an upload and dropping it would be worse than not offering it.
func putEntry(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	path := entryPath(vars["path"])

	if !wantsDirectory(r) {
		w.Header().Set("Allow", "GET, HEAD, POST, DELETE")
		http.Error(w,
			"PUT of file content is not implemented yet; use POST /api/silo/v1/access-tokens with op=upload, "+
				"then POST the file to /upload-api/{token}. PUT with ?type=dir creates a directory.",
			http.StatusNotImplemented)
		return
	}
	if path == "/" {
		http.Error(w, "The library root already exists", http.StatusConflict)
		return
	}

	setQuery(r, url.Values{"path": {path}})
	mkdirHandler(w, r)
}

// wantsDirectory reports whether a PUT is asking for a directory rather than a
// file. Both spellings are accepted because both are natural: a trailing slash
// is how a directory is written in a path, and an explicit ?type=dir is how a
// client that builds URLs from components says the same thing without having
// to remember the convention.
func wantsDirectory(r *http.Request) bool {
	if strings.EqualFold(r.URL.Query().Get("type"), "dir") {
		return true
	}
	return strings.HasSuffix(mux.Vars(r)["path"], "/")
}

func deleteEntry(w http.ResponseWriter, r *http.Request) {
	path := entryPath(mux.Vars(r)["path"])
	if path == "/" {
		http.Error(w, "The library root cannot be deleted; delete the library instead", http.StatusBadRequest)
		return
	}
	setQuery(r, url.Values{"path": {path}})
	deleteFileHandler(w, r)
}

// postEntry performs an operation on an existing entry. Only "move" so far,
// which covers renaming.
func postEntry(w http.ResponseWriter, r *http.Request) {
	path := entryPath(mux.Vars(r)["path"])

	var body struct {
		Op string `json:"op"`
		To string `json:"to"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&body); err != nil {
		http.Error(w, `Expected a JSON body such as {"op":"move","to":"/new/path"}`, http.StatusBadRequest)
		return
	}
	if body.Op != "move" {
		http.Error(w, `Unsupported op; the only operation is {"op":"move","to":"/new/path"}`, http.StatusBadRequest)
		return
	}
	to := entryPath(strings.TrimPrefix(body.To, "/"))
	if to == "/" {
		http.Error(w, "to is required and cannot be the library root", http.StatusBadRequest)
		return
	}
	if path == "/" {
		http.Error(w, "The library root cannot be moved", http.StatusBadRequest)
		return
	}

	setQuery(r, url.Values{"src": {path}, "dst": {to}})
	moveHandler(w, r)
}

// setQuery replaces the request's query string with what a delegate expects.
// The delegates read their arguments with url.QueryUnescape on top of Query(),
// so Encode's escaping is what they are written against.
func setQuery(r *http.Request, v url.Values) {
	r.URL.RawQuery = v.Encode()
}

// matchesETag reports whether an If-None-Match header covers etag. It handles
// the list form and "*", and compares weakly — RFC 9110 requires the weak
// comparison for If-None-Match, so a W/-prefixed candidate matches the strong
// tag with the same opaque part.
func matchesETag(header, etag string) bool {
	header = strings.TrimSpace(header)
	if header == "" {
		return false
	}
	if header == "*" {
		return true
	}
	for _, candidate := range strings.Split(header, ",") {
		if weakETag(candidate) == weakETag(etag) {
			return true
		}
	}
	return false
}

func weakETag(tag string) string {
	return strings.TrimPrefix(strings.TrimSpace(tag), "W/")
}
