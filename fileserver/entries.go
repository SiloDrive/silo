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
	"github.com/dkam/silo/fileserver/libmgr"
	"github.com/dkam/silo/fileserver/middleware"
	"github.com/dkam/silo/fileserver/objmgr"
	"github.com/dkam/silo/fileserver/option"
	"github.com/dkam/silo/fileserver/share"
	"github.com/dkam/silo/store"
	"github.com/gorilla/mux"
	log "github.com/sirupsen/logrus"
)

// The entries API: one addressable noun for everything in a library, with the
// HTTP methods as the verbs.
//
//	GET    /api/silo/v1/libraries/{library}/entries/{path}   dir -> listing, file -> bytes
//	HEAD   /api/silo/v1/libraries/{library}/entries/{path}   headers only
//	PUT    /api/silo/v1/libraries/{library}/entries/{path}   body -> file, ?type=dir,
//	                                                  or ?type=blocks (blocks.go)
//	DELETE /api/silo/v1/libraries/{library}/entries/{path}
//	POST   /api/silo/v1/libraries/{library}/entries/{path}   {"op":"move"|"copy","to":"/x/y"}
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
// redirects to a URL bearing a credential, and docs/capability-urls.md
// records why that is deliberate.
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

// No charset is declared for text files, on purpose. The server does not know
// how a file it is handing back is encoded; the inherited answer was a fixed
// "gbk", which is a guess that mangles anything else. Not saying is the only
// honest answer available.

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

// entryLibrary checks permission and loads the repository, answering itself and
// returning nil when it has. write asks for "rw"; otherwise any permission
// will do, since reading is allowed to anyone who can see the library at all.
//
// The lookup failure is not flattened to 404. A client that sees 404 here is
// entitled to conclude the library was deleted and remove its local copy —
// that is how a deletion propagates — so only a genuinely missing row may say
// it. A library whose objects the server has lost answers 500, which a client
// reads as "something is broken", not as "act on this".
//
// Both lookups are uncached — CheckPerm is two or more queries and the library
// lookup is a query plus a commit read — so the result is passed down rather
// than re-derived by each function that needs it.
func entryLibrary(w http.ResponseWriter, libraryID string, user account.ID, write bool) *libmgr.Library {
	perm := share.CheckPerm(libraryID, user)
	if perm == "" || (write && perm != "rw") {
		http.Error(w, "Permission denied", http.StatusForbidden)
		return nil
	}
	library, err := libmgr.GetWithReason(libraryID)
	if err != nil {
		code, msg := libmgr.StatusFor(err)
		http.Error(w, msg, code)
		return nil
	}
	return library
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
//
// It refuses an E2EE library rather than failing further in. The server has no
// content key, so it cannot encrypt the path segments it would have to match
// on — a resolve by path is a client operation there, and the id-addressed
// surface is how such a library is read. Saying so here keeps the refusal next
// to the reason instead of surfacing as "not found", which would be a lie
// about whether the file exists.
func resolve(library *libmgr.Library, path string) (*resolved, error) {
	if path == "/" {
		return &resolved{id: library.RootID, isDir: true}, nil
	}
	st, err := library.Store()
	if err != nil {
		return nil, err
	}
	root, err := store.ParseID(library.RootID)
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
	vars := mux.Vars(r)
	libraryID := vars["libraryid"]
	path := entryPath(vars["path"])

	library := entryLibrary(w, libraryID, acct.ID, false)
	if library == nil {
		return
	}

	entry, err := resolve(library, path)
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
		api.ListDirByID(w, r, library, entry.id)
		return
	}
	serveFile(w, r, library, entry.id, upath.Base(path))
}

// isPaged reports whether a request is asking for a window of a listing rather
// than the whole of it. Both spellings count: limit opens a paged sequence and
// cursor continues one.
func isPaged(r *http.Request) bool {
	q := r.URL.Query()
	return q.Get("limit") != "" || q.Get("cursor") != ""
}

// serveFile streams a file's bytes on this request, rather than redirecting to
// a one-time capability URL.
//
// That redirect existed because its consumers — a browser following a download
// link, a document server fetching a file — cannot set an Authorization header,
// so the URL has to carry the credential, and the token is spent on first use
// so a leaked URL is worth one request. Every caller of this lane sets a bearer
// header on every request, so there is nothing to solve: minting a one-time
// capability here would only mean two round trips and a URL that stops working
// after one of them. That is the difference between a usable ranged read and an
// unusable one, since a client reading at offsets pays it on every read. See
// docs/capability-urls.md.
//
// The ranged path is arithmetic rather than I/O planning, which is the
// manifest earning its keep: chunk sizes are recorded as PLAINTEXT lengths, so
// the run of chunks a range touches is computable from the manifest alone.
//
// An E2EE library is refused rather than served here, inside GetManifest and
// ReadFile: the server holds no key, so what it could stream is ciphertext,
// and a client that asked for a file and received sealed bytes has no way to
// tell that apart from the file. Such a library is read through the
// id-addressed surface, where the client opens the chunks itself.
func serveFile(w http.ResponseWriter, r *http.Request, library *libmgr.Library, fileID, fileName string) {
	st, err := library.Store()
	if err != nil {
		log.Errorf("failed to open store for library %s: %v", library.ID, err)
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
		log.Errorf("failed to read manifest %s in library %s: %v", fileID, library.ID, err)
		http.Error(w, "Not found", http.StatusNotFound)
		return
	}

	w.Header().Set("Accept-Ranges", "bytes")
	if parseContentType(fileName) == "image/svg+xml" {
		w.Header().Set("Content-Security-Policy", "sandbox")
	}
	setCommonHeaders(w, r, "download", fileName)

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
			log.Errorf("failed to stream %s in library %s: %v", fileID, library.ID, err)
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
		log.Errorf("failed to stream range of %s in library %s: %v", fileID, library.ID, err)
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
		putEntryBlocks(w, r, vars["libraryid"], path)
		return
	}

	if !wantsDirectory(r) {
		putEntryFile(w, r, vars["libraryid"], path)
		return
	}

	if !checkPreconditions(w, r, vars["libraryid"], middleware.GetAccountID(r), path) {
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
func checkPreconditions(w http.ResponseWriter, r *http.Request, libraryID string, user account.ID, path string) bool {
	if r.Header.Get("If-Match") == "" && r.Header.Get("If-None-Match") == "" {
		return true
	}
	library := entryLibrary(w, libraryID, user, true)
	if library == nil {
		return false
	}
	return preconditionsHold(w, r, library, path)
}

func preconditionsHold(w http.ResponseWriter, r *http.Request, library *libmgr.Library, path string) bool {
	ifMatch := r.Header.Get("If-Match")
	ifNoneMatch := r.Header.Get("If-None-Match")
	if ifMatch == "" && ifNoneMatch == "" {
		return true
	}

	// An empty tag means the path holds nothing right now. That is a state a
	// precondition can legitimately be asserted about, so it is not an error.
	var etag string
	if entry, err := resolve(library, path); err == nil {
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
// "file (1)" beside it — which is what an upload endpoint modelled on a person
// dragging things into a folder does, rather than one modelled on a client
// asserting a desired state.
//
// The body streams straight into the chunker rather than through a spool file:
// WriteFile chunks a stream and the manifest records what it found, so nothing
// needs to know the size in advance and there is no temporary file, no cleanup
// and no disk for it.
//
// An E2EE library is refused, and inside WriteFile rather than here: the
// server cannot chunk what it cannot read, and chunking under the wrong seed
// would be worse than refusing. Writing such a library is the id-addressed
// surface's job, where the client chunks, seals and names every object.
func putEntryFile(w http.ResponseWriter, r *http.Request, libraryID, path string) {
	acct := middleware.GetAccount(r)

	library := entryLibrary(w, libraryID, acct.ID, true)
	if library == nil {
		return
	}

	// Checked before the body is spooled: a doomed upload should be refused
	// before it is transferred, not after.
	if !preconditionsHold(w, r, library, path) {
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
		parent, err := resolve(library, parentDir)
		if err != nil || !parent.isDir {
			http.Error(w, "Parent directory does not exist", http.StatusNotFound)
			return
		}
	}

	st, err := library.Store()
	if err != nil {
		log.WithContext(r.Context()).WithError(err).Errorf("failed to open store for library %s", library.ID)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	// Refused before the body is read when the request says how big it is.
	// Receiving forty gigabytes and then declining them wastes the transfer on
	// both ends, and the client learns nothing it could not have been told
	// first.
	if refuseOverQuota(w, library, declaredLength(r)) {
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
		log.WithContext(r.Context()).WithError(err).Errorf("failed to store %s in library %s", path, library.ID)
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
	// And asked again with the size the bytes actually were, because a chunked
	// request declared nothing and a lying one declared whatever it liked.
	if refuseOverQuota(w, library, m.FileSize) {
		return
	}

	manifestID, err := st.PutManifest(m)
	if err != nil {
		log.WithContext(r.Context()).WithError(err).Errorf("failed to store manifest for %s in library %s", path, library.ID)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	if _, _, err := mutateTree(library, acct.Email, func(st *objmgr.Store, root store.ID, now int64) (store.ID, error) {
		return st.PutNode(root, path, objmgr.Node{
			ID: manifestID, Type: store.NodeFile, Name: fileName,
			Mtime: now, Mode: defaultFileMode,
		}, now)
	}); err != nil {
		writeTreeErr(w, r, err, "Parent directory does not exist",
			fmt.Sprintf("commit of %s in library %s", path, library.ID))
		return
	}

	sendStatisticMsg(library.ID, acct.Email, "web-file-upload", uint64(m.FileSize))

	// The ETag is the new content, so a client can record it without a
	// follow-up GET — which is the whole point of returning it here.
	w.Header().Set("ETag", `"`+etagPrefix+manifestID.String()+`"`)
	writeEntryJSON(w, http.StatusCreated, map[string]any{
		"name": fileName, "type": "file", "id": manifestID.String(), "size": m.FileSize,
	})
}

// putEntryChunks commits a store-v2 file whose chunks are already on the
// server, named in order by the client.
//
// This is the resumable upload's second half, and it transfers no content: the
// chunks went up one at a time through the block surface, and this names them.
//
// The size is never taken from the request. A client-supplied length that
// disagreed with the chunks would produce a file whose recorded size is a lie,
// and nothing downstream would notice — reads take their length from the
// manifest, not from the chunks.
func putEntryChunks(w http.ResponseWriter, r *http.Request, library *libmgr.Library, path, fileName string, ids []string) {
	acct := middleware.GetAccount(r)

	st, err := library.Store()
	if err != nil {
		log.WithContext(r.Context()).WithError(err).Errorf("failed to open store for library %s", library.ID)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	manifestID, size, missing, fail := chunkManifest(st, ids)
	// 424 rather than 400: the request is well formed and the content it names
	// is simply not here yet, and the recovery is exact — upload these, then
	// send this same request again.
	if len(missing) > 0 {
		writeEntryJSON(w, http.StatusFailedDependency, map[string]any{
			"error":   "Some chunks are not on the server; upload them and retry",
			"missing": idStrings(missing),
		})
		return
	}
	if fail != nil {
		http.Error(w, fail.message, fail.code)
		return
	}
	// The chunks are already here, so this refusal costs no transfer — but it
	// still has to happen before the head moves, because until it does the file
	// does not exist and the bytes are the collector's to reclaim.
	if refuseOverQuota(w, library, size) {
		return
	}

	if _, _, err := mutateTree(library, acct.Email, func(st *objmgr.Store, root store.ID, now int64) (store.ID, error) {
		return st.PutNode(root, path, objmgr.Node{
			ID: manifestID, Type: store.NodeFile, Name: fileName,
			Mtime: now, Mode: defaultFileMode,
		}, now)
	}); err != nil {
		writeTreeErr(w, r, err, "Parent directory does not exist",
			fmt.Sprintf("commit of %s in library %s", path, library.ID))
		return
	}

	w.Header().Set("ETag", `"`+etagPrefix+manifestID.String()+`"`)
	writeEntryJSON(w, http.StatusCreated, map[string]any{
		"name": fileName, "type": "file", "id": manifestID.String(), "size": size,
	})
}

// chunkManifest turns a client's ordered list of chunk ids into a stored
// manifest id, and is the whole of what the single-file path and a batch's
// create operation have in common.
//
// Written out at both call sites it was seven decisions duplicated — the
// refusal, the id parsing, the size bound, the assembly, the write — and they
// had already drifted where a client could see it. What differs is only how
// each caller reports a refusal, so that is all each caller keeps: missing
// chunks come back as a list rather than a rendered answer, because the
// single-file path names them all and a batch names a count and the first.
//
// The upload bound stays here rather than in objmgr because it is server
// policy and not a rule about the format: another deployment sets it
// differently, and a manifest that is legal is legal at any size.
func chunkManifest(st *objmgr.Store, blocks []string) (store.ID, int64, []store.ID, *batchFailure) {
	ids := make([]store.ID, 0, len(blocks))
	for _, raw := range blocks {
		id, err := store.ParseID(raw)
		if err != nil {
			return store.ID{}, 0, nil, &batchFailure{http.StatusBadRequest, "Not a chunk id: " + raw}
		}
		ids = append(ids, id)
	}

	m, missing, err := st.ManifestFromChunks(ids)
	if err != nil {
		if fail := treeFailure(err, "A chunk is not on the server"); fail != nil {
			return store.ID{}, 0, nil, fail
		}
		return store.ID{}, 0, nil, &batchFailure{http.StatusInternalServerError, "Failed to assemble the manifest"}
	}
	if len(missing) > 0 {
		return store.ID{}, 0, missing, nil
	}
	if overBound(m.FileSize) {
		return store.ID{}, 0, nil, &batchFailure{http.StatusRequestEntityTooLarge, "File is too large"}
	}

	id, err := st.PutManifest(m)
	if err != nil {
		return store.ID{}, 0, nil, &batchFailure{http.StatusInternalServerError, "Failed to write the manifest"}
	}
	return id, m.FileSize, nil, nil
}

// idStrings renders ids for a JSON reply.
func idStrings(ids []store.ID) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, id.String())
	}
	return out
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
func putEntryBlocks(w http.ResponseWriter, r *http.Request, libraryID, path string) {
	acct := middleware.GetAccount(r)

	library := entryLibrary(w, libraryID, acct.ID, true)
	if library == nil {
		return
	}

	if !preconditionsHold(w, r, library, path) {
		return
	}

	parentDir, fileName := upath.Split(path)
	parentDir = entryPath(parentDir)
	if !checkEntryName(w, fileName) {
		return
	}
	if parentDir != "/" {
		parent, err := resolve(library, parentDir)
		if err != nil || !parent.isDir {
			http.Error(w, "Parent directory does not exist", http.StatusNotFound)
			return
		}
	}

	var body struct {
		Blocks []string `json:"blocks"`
	}
	if !decodeJSONBody(w, r, maxBlockListBody, &body, `Expected a JSON body such as {"blocks":["<sha1>",…]}`) {
		return
	}

	putEntryChunks(w, r, library, path, fileName, body.Blocks)
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
	if !checkPreconditions(w, r, vars["libraryid"], middleware.GetAccountID(r), path) {
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
	if !decodeJSONBody(w, r, 64<<10, &body, `Expected a JSON body such as {"op":"move","to":"/new/path"}`) {
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
	if !checkPreconditions(w, r, vars["libraryid"], middleware.GetAccountID(r), path) {
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

// contentType = "application/octet-stream"
func parseContentType(fileName string) string {
	var contentType string

	parts := strings.Split(fileName, ".")
	if len(parts) >= 2 {
		suffix := parts[len(parts)-1]
		suffix = strings.ToLower(suffix)
		switch suffix {
		case "txt":
			contentType = "text/plain"
		case "doc":
			contentType = "application/vnd.ms-word"
		case "docx":
			contentType = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
		case "ppt":
			contentType = "application/vnd.ms-powerpoint"
		case "xls":
			contentType = "application/vnd.ms-excel"
		case "xlsx":
			contentType = "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
		case "pdf":
			contentType = "application/pdf"
		case "zip":
			contentType = "application/zip"
		case "mp3":
			contentType = "audio/mp3"
		case "mpeg":
			contentType = "video/mpeg"
		case "mp4":
			contentType = "video/mp4"
		case "ogv":
			contentType = "video/ogg"
		case "mov":
			contentType = "video/mp4"
		case "webm":
			contentType = "video/webm"
		case "mkv":
			contentType = "video/x-matroska"
		case "jpeg", "JPEG", "jpg", "JPG":
			contentType = "image/jpeg"
		case "png", "PNG":
			contentType = "image/png"
		case "gif", "GIF":
			contentType = "image/gif"
		case "svg", "SVG":
			contentType = "image/svg+xml"
		case "heic":
			contentType = "image/heic"
		case "ico":
			contentType = "image/x-icon"
		case "bmp":
			contentType = "image/bmp"
		case "tif", "tiff":
			contentType = "image/tiff"
		case "psd":
			contentType = "image/vnd.adobe.photoshop"
		case "webp":
			contentType = "image/webp"
		case "jfif":
			contentType = "image/jpeg"
		}
	}

	return contentType
}

// setCommonHeaders sets the content type and disposition for a file response.
//
// No charset is declared, even on text/*: see the note above the constants.
func setCommonHeaders(rsp http.ResponseWriter, r *http.Request, operation, fileName string) {
	fileType := parseContentType(fileName)
	if fileType != "" {
		rsp.Header().Set("Content-Type", fileType)
	} else {
		rsp.Header().Set("Content-Type", "application/octet-stream")
	}

	var contFileName string
	if operation == "download" || operation == "download-link" ||
		operation == "downloadblks" {
		// Since the file name downloaded by safari will be garbled, we need to encode the filename.
		// Safari cannot parse unencoded utf8 characters.
		contFileName = fmt.Sprintf("attachment;filename*=utf-8''%s;filename=\"%s\"", url.PathEscape(fileName), fileName)
	} else {
		contFileName = fmt.Sprintf("inline;filename*=utf-8''%s;filename=\"%s\"", url.PathEscape(fileName), fileName)
	}
	rsp.Header().Set("Content-Disposition", contFileName)

	if fileType != "image/jpg" {
		rsp.Header().Set("X-Content-Type-Options", "nosniff")
	}
}

// parseRange reads a single byte range out of a Range header.
//
// One range only: a multi-range request is answered as if it had asked for
// nothing, because multipart/byteranges is a response format no client of this
// server sends and half-implementing it is worse than not offering it.
func parseRange(byteRanges string, fileSize uint64) (uint64, uint64, bool) {
	start := strings.Index(byteRanges, "=")
	end := strings.Index(byteRanges, "-")

	if end < 0 {
		return 0, 0, false
	}

	var startByte, endByte uint64

	if start+1 == end {
		retByte, err := strconv.ParseUint(byteRanges[end+1:], 10, 64)
		if err != nil || retByte == 0 {
			return 0, 0, false
		}
		startByte = fileSize - retByte
		endByte = fileSize - 1
	} else if end+1 == len(byteRanges) {
		firstByte, err := strconv.ParseUint(byteRanges[start+1:end], 10, 64)
		if err != nil {
			return 0, 0, false
		}

		startByte = firstByte
		endByte = fileSize - 1
	} else {
		firstByte, err := strconv.ParseUint(byteRanges[start+1:end], 10, 64)
		if err != nil {
			return 0, 0, false
		}
		lastByte, err := strconv.ParseUint(byteRanges[end+1:], 10, 64)
		if err != nil {
			return 0, 0, false
		}

		if lastByte > fileSize-1 {
			lastByte = fileSize - 1
		}

		startByte = firstByte
		endByte = lastByte
	}

	if startByte > endByte {
		return 0, 0, false
	}

	return startByte, endByte, true
}

// decodeJSONBody reads a JSON request body into v, refusing anything past
// limit, and answers the request itself when it cannot. It reports whether the
// caller should carry on.
//
// The decoder streams, so the limit bounds the allocation rather than only
// rejecting it after the fact. expected is what a well-formed body looks like,
// and is shown only for a malformed one — a body that was merely too large
// gets the size complaint, where repeating the shape would be noise.
func decodeJSONBody(w http.ResponseWriter, r *http.Request, limit int64, v any, expected string) bool {
	err := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit)).Decode(v)
	if err == nil {
		return true
	}
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		http.Error(w, fmt.Sprintf("Request body is limited to %d bytes", limit), http.StatusRequestEntityTooLarge)
		return false
	}
	http.Error(w, expected, http.StatusBadRequest)
	return false
}
