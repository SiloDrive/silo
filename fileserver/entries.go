package silod

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	upath "path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/dkam/silo/fileserver/account"
	"github.com/dkam/silo/fileserver/api"
	"github.com/dkam/silo/fileserver/fsmgr"
	"github.com/dkam/silo/fileserver/middleware"
	"github.com/dkam/silo/fileserver/objmgr"
	"github.com/dkam/silo/fileserver/option"
	"github.com/dkam/silo/fileserver/repomgr"
	"github.com/dkam/silo/fileserver/share"
	"github.com/dkam/silo/fileserver/utils"
	"github.com/dkam/silo/store"
	"github.com/gorilla/mux"
	log "github.com/sirupsen/logrus"
)

// The entries API: one addressable noun for everything in a library, with the
// HTTP methods as the verbs.
//
//	GET    /api/silo/v1/repos/{repo}/entries/{path}   dir -> listing, file -> bytes
//	HEAD   /api/silo/v1/repos/{repo}/entries/{path}   headers only
//	PUT    /api/silo/v1/repos/{repo}/entries/{path}   body -> file, ?type=dir,
//	                                                  or ?type=blocks (blocks.go)
//	DELETE /api/silo/v1/repos/{repo}/entries/{path}
//	POST   /api/silo/v1/repos/{repo}/entries/{path}   {"op":"move"|"copy","to":"/x/y"}
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

// entryRepo checks permission and loads the repository, answering itself and
// returning nil when it has. write asks for "rw"; otherwise any permission
// will do, since reading is allowed to anyone who can see the library at all.
//
// The lookup failure is not flattened to 404. A client that sees 404 here is
// entitled to conclude the library was deleted and remove its local copy —
// that is how a deletion propagates — so only a genuinely missing row may say
// it. A library whose objects the server has lost answers 500, which a client
// reads as "something is broken", not as "act on this".
//
// Both lookups are uncached — CheckPerm is two or more queries and the repo
// lookup is a query plus a commit read — so the result is passed down rather
// than re-derived by each function that needs it.
func entryRepo(w http.ResponseWriter, repoID string, user account.ID, write bool) *repomgr.Repo {
	perm := share.CheckPerm(repoID, user)
	if perm == "" || (write && perm != "rw") {
		http.Error(w, "Permission denied", http.StatusForbidden)
		return nil
	}
	repo, err := repomgr.GetWithReason(repoID)
	if err != nil {
		code, msg := repomgr.StatusFor(err)
		http.Error(w, msg, code)
		return nil
	}
	return repo
}

// resolved is what a path points at right now: enough to answer a conditional
// request without reading any content.
type resolved struct {
	id    string
	isDir bool
	mtime int64
}

// resolve looks up a path in the current head tree. The root has no dirent of
// its own — nothing contains it — so its id is the commit's root id, which is
// exactly as good an ETag: it changes whenever anything in the library does.
func resolve(repo *repomgr.Repo, path string) (*resolved, error) {
	if path == "/" {
		return &resolved{id: repo.RootID, isDir: true}, nil
	}
	if repo.IsStoreV2() {
		return resolveV2(repo, path)
	}
	dent, err := fsmgr.GetDirentByPath(repo.StoreID, repo.RootID, path)
	if err != nil {
		return nil, err
	}
	return &resolved{
		id:    dent.ID,
		isDir: fsmgr.IsDir(dent.Mode),
		mtime: dent.Mtime,
	}, nil
}

// resolveV2 walks a store-v2 tree to a path.
//
// It refuses an E2EE library rather than failing further in. The server has no
// content key, so it cannot encrypt the path segments it would have to match
// on — a resolve by path is a client operation there, and the id-addressed
// surface is how such a library is read. Saying so here keeps the refusal next
// to the reason instead of surfacing as "not found", which would be a lie
// about whether the file exists.
func resolveV2(repo *repomgr.Repo, path string) (*resolved, error) {
	st, err := repo.Store()
	if err != nil {
		return nil, err
	}
	root, err := store.ParseID(repo.RootID)
	if err != nil {
		return nil, err
	}
	node, err := st.Resolve(root, path)
	if err != nil {
		return nil, err
	}
	return &resolved{
		id:    node.ID.String(),
		isDir: node.IsDir(),
		mtime: node.Mtime,
	}, nil
}

// getEntry answers a read, conditionally.
//
// The 304 is the point of the whole design: an object's id is its content
// hash, so it is already a strong ETag, and validating one costs a single
// dirent lookup in the parent directory — no blocks are read at all. A client
// re-checking a materialised file pays almost nothing to learn it is current.
func getEntry(w http.ResponseWriter, r *http.Request) {
	acct := middleware.GetAccount(r)
	user := acct.Email
	vars := mux.Vars(r)
	repoID := vars["repoid"]
	path := entryPath(vars["path"])

	repo := entryRepo(w, repoID, acct.ID, false)
	if repo == nil {
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
	// A paged request is answered on its merits: revalidating it against the
	// whole directory's id would answer 304 to a client that is asking for the
	// next window, not for the same one again.
	if !isPaged(r) && matchesETag(r.Header.Get("If-None-Match"), etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}

	// Both branches take the id resolved above rather than the path, so neither
	// re-walks the tree from the root — the listing and the file body are read
	// straight from the object the ETag was just computed from.
	if entry.isDir {
		api.ListDirByID(w, r, repo, entry.id)
		return
	}
	serveFile(w, r, repo, entry.id, upath.Base(path), user)
}

// isPaged reports whether a request is asking for a window of a listing rather
// than the whole of it. Both spellings count: limit opens a paged sequence and
// cursor continues one.
func isPaged(r *http.Request) bool {
	q := r.URL.Query()
	return q.Get("limit") != "" || q.Get("cursor") != ""
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
	if repo.IsStoreV2() {
		serveFileV2(w, r, repo, fileID, fileName)
		return
	}

	// Advertised even when this request has no Range, so a client learns it can
	// seek without having to try one and see — and *denied* on an encrypted
	// library, where the whole-file path below ignores Range and answers 200
	// with everything. Ignoring a Range is allowed; advertising support for one
	// and then ignoring it is not, and a client that trusts the header has no
	// way to tell the difference between the file it asked for and the file it
	// got. See docs/responses.md.
	if repo.IsEncrypted {
		w.Header().Set("Accept-Ranges", "none")
	} else {
		w.Header().Set("Accept-Ranges", "bytes")
	}

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

// serveFileV2 streams a store-v2 file, whole or ranged.
//
// The ranged path is arithmetic rather than I/O planning, which is the
// manifest earning its keep: chunk sizes are recorded as PLAINTEXT lengths, so
// the run of chunks a range touches is computable from the manifest alone. The
// Seafile path has to stat every block to learn the same thing, and caches the
// result to avoid doing it twice.
//
// An E2EE library is refused rather than served here, inside GetManifest and
// ReadFile: the server holds no key, so what it could stream is ciphertext,
// and a client that asked for a file and received sealed bytes has no way to
// tell that apart from the file. Such a library is read through the
// id-addressed surface, where the client opens the chunks itself.
func serveFileV2(w http.ResponseWriter, r *http.Request, repo *repomgr.Repo, fileID, fileName string) {
	st, err := repo.Store()
	if err != nil {
		log.Errorf("failed to open store for repo %s: %v", repo.ID, err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
	id, err := store.ParseID(fileID)
	if err != nil {
		http.Error(w, "Not found", http.StatusNotFound)
		return
	}
	m, err := st.GetManifest(id)
	if err != nil {
		log.Errorf("failed to read manifest %s in repo %s: %v", fileID, repo.ID, err)
		http.Error(w, "Not found", http.StatusNotFound)
		return
	}

	w.Header().Set("Accept-Ranges", "bytes")
	if parseContentType(fileName) == "image/svg+xml" {
		w.Header().Set("Content-Security-Policy", "sandbox")
	}
	setCommonHeaders(w, r, "download", fileName, siloTextCharset)

	byteRanges := strings.Join(r.Header["Range"], "")
	if byteRanges == "" {
		w.Header().Set("Content-Length", strconv.FormatInt(m.FileSize, 10))
		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodHead {
			return
		}
		if err := st.ReadFile(m, w); err != nil {
			// The status is already written, so this cannot become a 500. Log
			// it and let the body end short of Content-Length, which is what
			// tells the client it is incomplete.
			log.Errorf("failed to stream %s in repo %s: %v", fileID, repo.ID, err)
		}
		return
	}

	start, end, ok := parseRange(byteRanges, uint64(m.FileSize))
	if !ok {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", m.FileSize))
		http.Error(w, "", http.StatusRequestedRangeNotSatisfiable)
		return
	}
	w.Header().Set("Content-Length", strconv.FormatUint(end-start+1, 10))
	w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, m.FileSize))
	w.WriteHeader(http.StatusPartialContent)
	if r.Method == http.MethodHead {
		return
	}
	if err := st.ReadFileRange(m, int64(start), int64(end-start+1), w); err != nil {
		log.Errorf("failed to stream range of %s in repo %s: %v", fileID, repo.ID, err)
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

	// ?type=blocks is a file too, but one whose content is already on the
	// server: the body names blocks rather than carrying bytes.
	if strings.EqualFold(r.URL.Query().Get("type"), "blocks") {
		putEntryBlocks(w, r, vars["repoid"], path)
		return
	}

	if !wantsDirectory(r) {
		putEntryFile(w, r, vars["repoid"], path)
		return
	}

	if !checkPreconditions(w, r, vars["repoid"], middleware.GetAccountID(r), path) {
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
// checkPreconditions is the form for handlers that have not already loaded the
// repository. It loads one only when a precondition header is actually present,
// so an unconditional write costs nothing extra — the delegate it is about to
// call does its own permission check and load anyway.
func checkPreconditions(w http.ResponseWriter, r *http.Request, repoID string, user account.ID, path string) bool {
	if r.Header.Get("If-Match") == "" && r.Header.Get("If-None-Match") == "" {
		return true
	}
	repo := entryRepo(w, repoID, user, true)
	if repo == nil {
		return false
	}
	return preconditionsHold(w, r, repo, path)
}

func preconditionsHold(w http.ResponseWriter, r *http.Request, repo *repomgr.Repo, path string) bool {
	ifMatch := r.Header.Get("If-Match")
	ifNoneMatch := r.Header.Get("If-None-Match")
	if ifMatch == "" && ifNoneMatch == "" {
		return true
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
	acct := middleware.GetAccount(r)
	user := acct.Email

	repo := entryRepo(w, repoID, acct.ID, true)
	if repo == nil {
		return
	}

	// Checked before the body is spooled: a doomed upload should be refused
	// before it is transferred, not after.
	if !preconditionsHold(w, r, repo, path) {
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

	if repo.IsStoreV2() {
		putEntryFileV2(w, r, repo, fileName, path)
		return
	}

	// Resolved before the body is transferred: this is an in-memory key lookup
	// that can reject the request outright, and spooling gigabytes to disk only
	// to discover the library is locked is the case the ordering exists to
	// avoid.
	var cryptKey *seafileCrypt
	if repo.IsEncrypted {
		key, appErr := parseCryptKey(w, repoID, user, repo.EncVersion)
		if appErr != nil {
			http.Error(w, appErr.Message, appErr.Code)
			return
		}
		cryptKey = key
	}

	tmpPath, size, err := spoolBody(w, r, fileName)
	if err != nil {
		return // spoolBody has answered
	}
	defer func() { _ = os.Remove(tmpPath) }()

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
		writeCommitErr(w, r, err, fmt.Sprintf("commit of %s in repo %s", path, repoID))
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

// putEntryFileV2 stores an uploaded file in a store-v2 library.
//
// The body streams straight into the chunker rather than through a spool file.
// The Seafile path spools because it has to know the size before it indexes;
// this does not — WriteFile chunks a stream and the manifest records what it
// found — so the temporary file, its cleanup and the disk it needed all go.
//
// An E2EE library is refused, and inside WriteFile rather than here: the
// server cannot chunk what it cannot read, and chunking under the wrong seed
// would be worse than refusing. Writing such a library is the id-addressed
// surface's job, where the client chunks, seals and names every object.
func putEntryFileV2(w http.ResponseWriter, r *http.Request, repo *repomgr.Repo, fileName, path string) {
	acct := middleware.GetAccount(r)

	st, err := repo.Store()
	if err != nil {
		log.WithContext(r.Context()).WithError(err).Errorf("failed to open store for repo %s", repo.ID)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	// Content first, tree second. Chunks and the manifest are immutable and
	// addressed by content, so writing them commits to nothing — until the
	// head moves, the file does not exist and no path has changed. That is
	// also what makes the retry in mutateTree free of them.
	body, ok := boundedBody(w, r)
	if !ok {
		return
	}
	m, err := st.WriteFile(body)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return // the client hung up; nothing was committed
		}
		if errors.Is(err, objmgr.ErrNoContentKey) {
			http.Error(w, errE2EEWriteByID, http.StatusForbidden)
			return
		}
		log.WithContext(r.Context()).WithError(err).Errorf("failed to store %s in repo %s", path, repo.ID)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
	// Asked after the chunks are written rather than before, because on a
	// chunked upload there is nothing to ask until the body has been read.
	// What that leaves behind is chunks nothing names — which is what the
	// lost-race retry leaves behind too, and for the same reason it is the
	// collector's business rather than this request's. No manifest is minted
	// and no path changes, so the file does not exist.
	if overBound(m.FileSize) {
		http.Error(w, "File is too large", http.StatusRequestEntityTooLarge)
		return
	}

	manifestID, err := st.PutManifest(m)
	if err != nil {
		log.WithContext(r.Context()).WithError(err).Errorf("failed to store manifest for %s in repo %s", path, repo.ID)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	if _, err := mutateTree(repo, acct.Email, func(st *objmgr.Store, root store.ID, now int64) (store.ID, error) {
		return st.PutNode(root, path, objmgr.Node{
			ID: manifestID, Type: store.NodeFile, Name: fileName,
			Mtime: now, Mode: defaultFileMode,
		}, now)
	}); err != nil {
		writeTreeErr(w, r, err, "Parent directory does not exist",
			fmt.Sprintf("commit of %s in repo %s", path, repo.ID))
		return
	}

	sendStatisticMsg(repo.ID, acct.Email, "web-file-upload", uint64(m.FileSize))

	// The ETag is the new content, so a client can record it without a
	// follow-up GET — which is the whole point of returning it here.
	w.Header().Set("ETag", `"`+etagPrefix+manifestID.String()+`"`)
	writeEntryJSON(w, http.StatusCreated, map[string]any{
		"name": fileName, "type": "file", "id": manifestID.String(), "size": m.FileSize,
	})
}

// putEntryBlocks commits a file whose content is already in the store, named
// by the block ids the client uploaded to the block surface. It transfers no
// content: everything this reads is a few dozen bytes of JSON.
//
// This is the second half of a resumable upload, and it is the half that makes
// the first half safe to interrupt. Blocks are immutable and content-addressed,
// so uploading them commits to nothing — the file does not exist, and no path
// changes, until this call names them in order. A client can therefore upload
// blocks over hours, across restarts, in any order and in parallel, and still
// produce exactly one commit at the end.
//
// See blocks.go for the surface as a whole.
func putEntryBlocks(w http.ResponseWriter, r *http.Request, repoID, path string) {
	acct := middleware.GetAccount(r)
	user := acct.Email

	repo := entryRepo(w, repoID, acct.ID, true)
	if repo == nil {
		return
	}

	// An encrypted library stores ciphertext, so its block ids are the hashes
	// of encrypted bytes and a client cannot name one without doing the
	// encryption itself under the exact scheme in crypt.go. Refused rather
	// than half-supported: the whole-file PUT works there and does the
	// encryption server-side from the cached key.
	if repo.IsEncrypted {
		http.Error(w, "An encrypted library cannot be written block by block; PUT the file content instead", http.StatusBadRequest)
		return
	}

	if !preconditionsHold(w, r, repo, path) {
		return
	}

	parentDir, fileName := upath.Split(path)
	parentDir = entryPath(parentDir)
	if !checkEntryName(w, fileName) {
		return
	}
	if parentDir != "/" {
		parent, err := resolve(repo, parentDir)
		if err != nil || !parent.isDir {
			http.Error(w, "Parent directory does not exist", http.StatusNotFound)
			return
		}
	}

	var body struct {
		Blocks []string `json:"blocks"`
	}
	if appErr := decodeLimitedJSON(w, r, maxBlockListBody, &body); appErr != nil {
		if appErr.Code == http.StatusBadRequest {
			appErr.Message = `Expected a JSON body such as {"blocks":["<sha1>",…]}`
		}
		http.Error(w, appErr.Message, appErr.Code)
		return
	}

	// Read before the blocks are checked, so a GC that starts between the
	// check and the commit is caught as a conflict rather than leaving a
	// commit pointing at blocks that have just been reclaimed. The same
	// ordering as the whole-file path, for the same reason.
	gcID, err := repomgr.GetCurrentGCID(repo.StoreID)
	if err != nil {
		log.WithContext(r.Context()).WithError(err).Errorf("failed to get gc id for repo %s", repoID)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	for _, id := range body.Blocks {
		if !utils.IsObjectIDValid(id) {
			http.Error(w, "Not a block id: "+id, http.StatusBadRequest)
			return
		}
	}

	// The size is summed from the store rather than taken from the request. A
	// client-supplied length that disagreed with the blocks would produce a
	// file whose recorded size is a lie, and nothing downstream would notice —
	// reads take their length from here, not from the blocks.
	missing, size, err := blockInventory(repo.StoreID, body.Blocks)
	if err != nil {
		log.WithContext(r.Context()).WithError(err).Errorf("failed to inventory blocks for %s in repo %s", path, repoID)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	// 424 rather than 400, because the request is not wrong and the identical
	// one will succeed once its dependency is met — which is precisely what
	// 400 tells a client never to assume. The list is in the body so the fix
	// is exact: upload these, then send this same request again.
	if len(missing) > 0 {
		writeEntryJSON(w, http.StatusFailedDependency, map[string]any{
			"error":   "Some blocks are not on the server; upload them and retry",
			"missing": missing,
		})
		return
	}

	if option.MaxUploadSize > 0 && uint64(size) > option.MaxUploadSize {
		http.Error(w, "File is too large", http.StatusRequestEntityTooLarge)
		return
	}

	id, err := writeSeafile(repo.StoreID, repo.Version, size, body.Blocks)
	if err != nil {
		log.WithContext(r.Context()).WithError(err).Errorf("failed to write seafile for %s in repo %s", path, repoID)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	if _, err := postFilesAndGenCommit([]string{fileName}, repo.ID, user, parentDir, true,
		[]string{id}, []int64{size}, 0, gcID); err != nil {
		writeCommitErr(w, r, err, fmt.Sprintf("commit of %s in repo %s", path, repoID))
		return
	}

	w.Header().Set("ETag", `"`+etagPrefix+id+`"`)
	writeEntryJSON(w, http.StatusCreated, map[string]any{
		"name": fileName, "type": "file", "id": id, "size": size,
	})
}

// boundedBody applies the upload size limit to a request body, answering the
// request itself if the limit is already known to be exceeded.
//
// The limit is policy and spooling to a temp file is mechanism, so the policy
// lives here rather than inside spoolBody, where it started. A lane that does
// not spool — putEntryFileV2 chunks the stream straight into the object store
// — needs the same bound and none of the temp file, and taking only the
// LimitReader from spoolBody is how it came to enforce half the rule. The half
// it took is the half that silently truncates.
//
// Two checks, because neither alone is enough. Content-Length refuses the
// ordinary case before a byte is transferred. A chunked upload declares no
// length, so the reader is bounded one byte PAST the limit and the caller asks
// overBound whether that byte arrived — a LimitReader on its own refuses
// nothing, it ends the stream, and an ended stream is indistinguishable from a
// client that sent exactly that much.
func boundedBody(w http.ResponseWriter, r *http.Request) (io.Reader, bool) {
	if option.MaxUploadSize == 0 {
		return r.Body, true
	}
	if r.ContentLength > 0 && uint64(r.ContentLength) > option.MaxUploadSize {
		http.Error(w, "File is too large", http.StatusRequestEntityTooLarge)
		return nil, false
	}
	return io.LimitReader(r.Body, int64(option.MaxUploadSize)+1), true
}

// overBound reports whether a body read through boundedBody ran past the
// limit. The extra byte boundedBody allows is what makes this answerable.
func overBound(size int64) bool {
	return option.MaxUploadSize > 0 && uint64(size) > option.MaxUploadSize
}

// spoolBody writes the request body to a temp file, returning its path and
// size. On failure it has already answered the request.
func spoolBody(w http.ResponseWriter, r *http.Request, fileName string) (string, int64, error) {
	body, ok := boundedBody(w, r)
	if !ok {
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

	size, err := io.Copy(f, body)
	if err != nil {
		_ = os.Remove(f.Name())
		// A client that disconnected mid-body is not an error worth reporting.
		return "", 0, err
	}
	if overBound(size) {
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
	if !checkPreconditions(w, r, vars["repoid"], middleware.GetAccountID(r), path) {
		return
	}
	setQuery(r, url.Values{"path": {path}})
	deleteFileHandler(w, r)
}

// postEntry performs an operation on an existing entry: "move", which covers
// renaming, and "copy".
func postEntry(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	path := entryPath(vars["path"])

	var body struct {
		Op string `json:"op"`
		To string `json:"to"`
	}
	if appErr := decodeLimitedJSON(w, r, 64<<10, &body); appErr != nil {
		if appErr.Code == http.StatusBadRequest {
			appErr.Message = `Expected a JSON body such as {"op":"move","to":"/new/path"}`
		}
		http.Error(w, appErr.Message, appErr.Code)
		return
	}
	if body.Op != "move" && body.Op != "copy" {
		http.Error(w, `Unsupported op; the operations are {"op":"move","to":"/new/path"} and {"op":"copy","to":"/new/path"}`, http.StatusBadRequest)
		return
	}
	to := entryPath(strings.TrimPrefix(body.To, "/"))
	if to == "/" {
		http.Error(w, "to is required and cannot be the library root", http.StatusBadRequest)
		return
	}
	if path == "/" {
		http.Error(w, "The library root cannot be the source of a "+body.Op, http.StatusBadRequest)
		return
	}

	// The precondition is about the source — what is being moved or copied —
	// because that is the thing the caller looked at before deciding to act on
	// it. On a copy it means "copy this version, not whatever it became".
	if !checkPreconditions(w, r, vars["repoid"], middleware.GetAccountID(r), path) {
		return
	}

	setQuery(r, url.Values{"src": {path}, "dst": {to}})
	if body.Op == "copy" {
		copyHandler(w, r)
		return
	}
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
