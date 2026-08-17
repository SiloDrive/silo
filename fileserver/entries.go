package silod

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	upath "path"
	"path/filepath"
	"strings"
	"time"

	"github.com/dkam/silo/fileserver/api"
	"github.com/dkam/silo/fileserver/fsmgr"
	"github.com/dkam/silo/fileserver/middleware"
	"github.com/dkam/silo/fileserver/option"
	"github.com/dkam/silo/fileserver/repomgr"
	"github.com/dkam/silo/fileserver/share"
	"github.com/gorilla/mux"
	log "github.com/sirupsen/logrus"
)

// The entries API: one addressable noun for everything in a library, with the
// HTTP methods as the verbs.
//
//	GET    /api/silo/v1/repos/{repo}/entries/{path}   dir -> listing, file -> bytes
//	HEAD   /api/silo/v1/repos/{repo}/entries/{path}   headers only
//	PUT    /api/silo/v1/repos/{repo}/entries/{path}   body -> file, or ?type=dir
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
// Every operation is one request, authenticated by the bearer header on that
// request. Reads stream the bytes back and writes carry them up; neither
// redirects to a URL bearing a credential the way the Seafile lane does, and
// docs/capability-urls.md records why that difference is deliberate.
//
// Where an operation already had an implementation, these handlers resolve and
// validate and then delegate to it rather than reimplementing the commit
// machinery: two copies would drift, and the copy with fewer callers would
// drift silently.

// etagPrefix versions the *representation*, not the object. The id identifies
// the bytes; the ETag identifies what this API makes of them. Without the
// prefix, changing the listing JSON would leave clients holding cache entries
// that validate against a body shape that no longer exists.
const etagPrefix = "v1-"

// siloTextCharset is empty on purpose: this lane declares no charset for text
// files. The server does not know how a file it is handing back is encoded, and
// the Seafile lane's inherited "gbk" is a guess that mangles anything else. Not
// saying is the only honest answer available.
const siloTextCharset = ""

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

	// Delegated by rewriting the query the older handler reads. It re-checks
	// the permission and reloads the repo; that is a little wasted work in
	// exchange for there being exactly one implementation of the listing.
	if entry.isDir {
		setQuery(r, url.Values{"path": {path}})
		api.ListDirHandler(w, r)
		return
	}
	serveFile(w, r, repo, entry.id, upath.Base(path), user)
}

// serveFile streams a file's bytes on this request, rather than redirecting to
// /files/{token}/{name} the way the Seafile lane does.
//
// That redirect exists because its consumers — a browser following a download
// link, a document server fetching a file — cannot set an Authorization header,
// so the URL has to carry the credential, and the token is spent on first use
// so a leaked URL is worth one request. Every caller of this lane sets a bearer
// header on every request, so there is nothing to solve: minting a one-time
// capability here would only mean two round trips and a URL that stops working
// after one of them. That is the difference between a usable ranged read and an
// unusable one, since a client reading at offsets pays it on every read.
//
// See docs/capability-urls.md. The redirect is still correct for the Seafile
// lane and is untouched there.
func serveFile(w http.ResponseWriter, r *http.Request, repo *repomgr.Repo, fileID, fileName, user string) {
	// Advertised even when this request has no Range, so a client learns it can
	// seek without having to try one and see.
	w.Header().Set("Accept-Ranges", "bytes")

	var cryptKey *seafileCrypt
	if repo.IsEncrypted {
		key, err := parseCryptKey(w, repo.ID, user, repo.EncVersion)
		if err != nil {
			http.Error(w, err.Message, err.Code)
			return
		}
		cryptKey = key
	}

	// Ranges are not served from an encrypted repo: the blocks are encrypted,
	// so a byte range of the plaintext is not a byte range of what is stored.
	// The whole-file path decrypts as it streams. This mirrors accessCB.
	byteRanges := strings.Join(r.Header["Range"], "")
	if !repo.IsEncrypted && byteRanges != "" {
		if e := doFileRange(w, r, repo, fileID, fileName, "download", byteRanges, user, siloTextCharset); e != nil {
			http.Error(w, e.Message, e.Code)
		}
		return
	}
	if e := doFile(w, r, repo, fileID, fileName, "download", cryptKey, user, siloTextCharset); e != nil {
		http.Error(w, e.Message, e.Code)
	}
}

// putEntry stores a file, or creates a directory with ?type=dir.
func putEntry(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	path := entryPath(vars["path"])

	if path == "/" {
		http.Error(w, "The library root already exists", http.StatusConflict)
		return
	}

	if !wantsDirectory(r) {
		putEntryFile(w, r, vars["repoid"], path)
		return
	}

	if !preconditionsHold(w, r, vars["repoid"], path) {
		return
	}
	setQuery(r, url.Values{"path": {path}})
	mkdirHandler(w, r)
}

// preconditionsHold evaluates If-Match and If-None-Match on a mutating request,
// answering 412 and returning false when the caller's assumption about the
// current state is wrong.
//
// This is optimistic concurrency, and it is the difference between two clients
// racing on a file and one of them silently losing an edit. Without it every
// write is last-writer-wins: read a file, edit it, PUT it, and whatever someone
// else committed in between is gone with no error anywhere. S3 added the same
// two preconditions in 2024 for the same reason.
//
//	If-Match: "v1-<id>"   replace only if this is still the content I read
//	If-None-Match: *      create only if nothing is there
//
// Both are opt-in: a request carrying neither header behaves exactly as before.
// A client that wants last-writer-wins can still have it, but now it has to
// choose it rather than get it by default.
//
// On the read path If-None-Match means "skip the body if unchanged" and yields
// 304. Here it means "fail if it exists". Same header, different question,
// because the method is different — that is what RFC 9110 specifies.
func preconditionsHold(w http.ResponseWriter, r *http.Request, repoID, path string) bool {
	ifMatch := r.Header.Get("If-Match")
	ifNoneMatch := r.Header.Get("If-None-Match")
	if ifMatch == "" && ifNoneMatch == "" {
		return true
	}

	repo := repomgr.Get(repoID)
	if repo == nil {
		http.Error(w, "Repo not found", http.StatusNotFound)
		return false
	}

	// An empty tag means the path holds nothing right now. That is a state a
	// precondition can legitimately be asserted about, so it is not an error.
	var etag string
	if entry, err := resolve(repo, path); err == nil {
		etag = `"` + etagPrefix + entry.id + `"`
	}

	if !preconditionResult(etag, ifMatch, ifNoneMatch) {
		http.Error(w,
			"Precondition failed: the entry is not in the state the request asserted. "+
				"Re-read it and reapply your change.",
			http.StatusPreconditionFailed)
		return false
	}
	return true
}

// preconditionResult is the comparison itself, split out so the decision table
// can be tested without a repository behind it. etag is what is at the path
// now, or "" when nothing is.
func preconditionResult(etag, ifMatch, ifNoneMatch string) bool {
	// If-Match fails against an absent entry: there is nothing that could be
	// the content the caller claims to be replacing. "*" is the same question
	// asked loosely — does anything exist here at all.
	if ifMatch != "" && (etag == "" || !matchesETag(ifMatch, etag)) {
		return false
	}
	// If-None-Match fails when the entry is there and matches, which for the
	// usual "*" means simply: something is already here.
	if ifNoneMatch != "" && etag != "" && matchesETag(ifNoneMatch, etag) {
		return false
	}
	return true
}

// putFile stores a request body as a file, replacing whatever was there.
//
// Replacing is what PUT means: the request says what the resource should
// contain afterwards, so sending it twice leaves one file, not a file and a
// "file (1)" beside it — which is what the Seafile lane's upload does, because
// it is modelled on a person dragging things into a folder rather than on a
// client asserting a desired state.
//
// The body is spooled to a temp file before indexing. That is not a shortcut
// around streaming: chunkFile seeks to each block boundary, so it needs a
// seekable source, and indexFileWorker already accepts a path for exactly this
// reason. A request body is neither seekable nor replayable.
func putEntryFile(w http.ResponseWriter, r *http.Request, repoID, path string) {
	user := middleware.GetUserEmail(r)

	// Writing needs "rw"; the read path is content with any permission.
	if share.CheckPerm(repoID, user) != "rw" {
		http.Error(w, "Permission denied", http.StatusForbidden)
		return
	}
	repo := repomgr.Get(repoID)
	if repo == nil {
		http.Error(w, "Repo not found", http.StatusNotFound)
		return
	}

	// Checked before the body is spooled: a doomed upload should be refused
	// before it is transferred, not after.
	if !preconditionsHold(w, r, repoID, path) {
		return
	}

	parentDir, fileName := upath.Split(path)
	parentDir = entryPath(parentDir)
	if !checkEntryName(w, fileName) {
		return
	}

	// The parent has to exist. Creating it implicitly would make a typo in a
	// path silently produce a directory tree rather than an error, and a client
	// that wants mkdir -p can ask for it a directory at a time.
	if parentDir != "/" {
		parent, err := resolve(repo, parentDir)
		if err != nil || !parent.isDir {
			http.Error(w, "Parent directory does not exist", http.StatusNotFound)
			return
		}
	}

	tmpPath, size, err := spoolBody(w, r, fileName)
	if err != nil {
		return // spoolBody has answered
	}
	defer func() { _ = os.Remove(tmpPath) }()

	var cryptKey *seafileCrypt
	if repo.IsEncrypted {
		key, appErr := parseCryptKey(w, repoID, user, repo.EncVersion)
		if appErr != nil {
			http.Error(w, appErr.Message, appErr.Code)
			return
		}
		cryptKey = key
	}

	// Read before indexing, so a GC that starts mid-upload is detected as a
	// conflict rather than racing the blocks this is about to write.
	gcID, err := repomgr.GetCurrentGCID(repo.StoreID)
	if err != nil {
		log.WithContext(r.Context()).WithError(err).Errorf("failed to get gc id for repo %s", repoID)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	id, indexedSize, err := indexBlocks(r.Context(), repo.StoreID, repo.Version, tmpPath, nil, cryptKey)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			// The client hung up mid-upload. Nothing was committed.
			return
		}
		log.WithContext(r.Context()).WithError(err).Errorf("failed to index blocks for %s in repo %s", path, repoID)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	if _, err := postFilesAndGenCommit([]string{fileName}, repo.ID, user, parentDir, true,
		[]string{id}, []int64{indexedSize}, 0, gcID); err != nil {
		if errors.Is(err, ErrGCConflict) {
			http.Error(w, "GC conflict; retry", http.StatusConflict)
			return
		}
		log.WithContext(r.Context()).WithError(err).Errorf("failed to commit %s in repo %s", path, repoID)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	sendStatisticMsg(repoID, user, "web-file-upload", uint64(size))

	// The ETag is the new content, so a client can record it without a
	// follow-up GET — which is the whole point of returning it here.
	w.Header().Set("ETag", `"`+etagPrefix+id+`"`)
	writeEntryJSON(w, http.StatusCreated, map[string]any{
		"name": fileName, "type": "file", "id": id, "size": indexedSize,
	})
}

// spoolBody writes the request body to a temp file, returning its path and
// size. On failure it has already answered the request.
func spoolBody(w http.ResponseWriter, r *http.Request, fileName string) (string, int64, error) {
	if option.MaxUploadSize > 0 && r.ContentLength > 0 && uint64(r.ContentLength) > option.MaxUploadSize {
		http.Error(w, "File is too large", http.StatusRequestEntityTooLarge)
		return "", 0, errTooLarge
	}

	tmpDir := filepath.Join(absDataDir, "httptemp", "cluster-shared")
	f, err := os.CreateTemp(tmpDir, "put-*")
	if err != nil {
		log.WithContext(r.Context()).WithError(err).Error("failed to create upload temp file")
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return "", 0, err
	}
	defer func() { _ = f.Close() }()

	body := io.Reader(r.Body)
	if option.MaxUploadSize > 0 {
		// A chunked upload has no Content-Length to check, so the limit is also
		// enforced on the way through.
		body = io.LimitReader(body, int64(option.MaxUploadSize)+1)
	}

	size, err := io.Copy(f, body)
	if err != nil {
		_ = os.Remove(f.Name())
		// A client that disconnected mid-body is not an error worth reporting.
		return "", 0, err
	}
	if option.MaxUploadSize > 0 && uint64(size) > option.MaxUploadSize {
		_ = os.Remove(f.Name())
		http.Error(w, "File is too large", http.StatusRequestEntityTooLarge)
		return "", 0, errTooLarge
	}
	return f.Name(), size, nil
}

var errTooLarge = errors.New("upload exceeds the configured maximum size")

func writeEntryJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Errorf("failed to encode entries response: %v", err)
	}
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
	vars := mux.Vars(r)
	path := entryPath(vars["path"])
	if path == "/" {
		http.Error(w, "The library root cannot be deleted; delete the library instead", http.StatusBadRequest)
		return
	}
	// If-Match on a delete means "only if this is still what I think it is",
	// which is how a client avoids deleting an edit it never saw.
	if !preconditionsHold(w, r, vars["repoid"], path) {
		return
	}
	setQuery(r, url.Values{"path": {path}})
	deleteFileHandler(w, r)
}

// postEntry performs an operation on an existing entry. Only "move" so far,
// which covers renaming.
func postEntry(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	path := entryPath(vars["path"])

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

	// The precondition is about the source — what is being moved — because that
	// is the thing the caller looked at before deciding to move it.
	if !preconditionsHold(w, r, vars["repoid"], path) {
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
