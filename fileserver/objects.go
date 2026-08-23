package silod

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/dkam/silo/fileserver/middleware"
	"github.com/dkam/silo/fileserver/objmgr"
	"github.com/dkam/silo/fileserver/objstore"
	"github.com/dkam/silo/fileserver/repomgr"
	"github.com/dkam/silo/store"
	"github.com/gorilla/mux"
	log "github.com/sirupsen/logrus"
)

// The id-addressed surface: how a client reads and writes a library the server
// cannot read.
//
//	GET  /api/silo/v1/repos/{repo}/objects/{id}   manifest, directory or commit
//	PUT  /api/silo/v1/repos/{repo}/objects/{id}   the same, verified and stored
//	GET  /api/silo/v1/repos/{repo}/blocks/{id}    a chunk, as stored
//	PUT  /api/silo/v1/repos/{repo}/blocks/{id}    a chunk, verified and stored
//	PUT  /api/silo/v1/repos/{repo}/head           If-Match: <current head>
//
// This exists because of one fact and its consequences. The server holds no
// content key for an E2EE library, so it cannot chunk a file, cannot build a
// manifest, and cannot name an entry — every write is therefore something the
// client computes and the server merely stores. `entries/{path}` keeps working
// for *structure* on such a library, because names route as ciphertext; what
// cannot go through it is content and every write.
//
// The surface is deliberately the same for both library types. A plain library
// could be written either way, and a client that implements this one does not
// need a second implementation for the encrypted case — which is the whole
// point of porter building one interface with two backends rather than
// branching on e2ee at every call site.
//
// What the server verifies, and it is a short list because it is everything it
// can: that an object's id is the SHA-256 of the bytes offered under it, and
// that the bytes decode as some store-v2 object. It cannot check that a
// manifest describes a real file, that a directory's names are well formed, or
// that a commit says anything true — those need the key. What it can do is
// refuse to store something that would break its own walks later, which is
// what the decode check buys: GC's mark and changes?since= both read public
// sections, and an object that never parsed would fail there instead of here.

// maxObjectBody bounds an object PUT. store.MaxManifestBytes is the format's
// own ceiling for the largest object type, so this is that plus room for the
// framing — an object larger than this is not a manifest that grew, it is a
// request that is not an object at all.
const maxObjectBody = store.MaxManifestBytes + (1 << 20)

// storeV2Repo loads a library for the id-addressed surface, refusing one that
// is not store-v2.
//
// The refusal is a 404 on the route rather than an error about formats: a
// Seafile library has no objects addressed this way, so the honest answer to
// "give me object X in this library" is that there is no such thing here.
func storeV2Repo(w http.ResponseWriter, r *http.Request, write bool) (*repomgr.Repo, *objmgr.Store, bool) {
	repo := entryRepo(w, mux.Vars(r)["repoid"], middleware.GetAccountID(r), write)
	if repo == nil {
		return nil, nil, false
	}
	if !repo.IsStoreV2() {
		http.Error(w, "This library does not have an object surface", http.StatusNotFound)
		return nil, nil, false
	}
	st, err := repo.Store()
	if err != nil {
		log.WithContext(r.Context()).WithError(err).Errorf("failed to open store for repo %s", repo.ID)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return nil, nil, false
	}
	return repo, st, true
}

// objectID parses the id out of the route. The route regex already constrains
// it, so a failure here means the regex and this parser disagree — which is
// worth an answer rather than a panic.
func objectID(w http.ResponseWriter, r *http.Request) (store.ID, bool) {
	id, err := store.ParseID(mux.Vars(r)["id"])
	if err != nil {
		http.Error(w, "Not a store-v2 object id", http.StatusBadRequest)
		return store.ID{}, false
	}
	return id, true
}

// getObjectHandler serves one object's bytes exactly as stored.
//
// Immutable, so the caching is unconditional: the id IS the content hash, so
// the representation at this URL can never change. That is why the ETag is
// strong and the max-age is a year — a client that has these bytes never needs
// to ask again, which matters most to the client that has to walk a whole
// object graph to resolve one path.
func getObjectHandler(w http.ResponseWriter, r *http.Request) {
	_, st, ok := storeV2Repo(w, r, false)
	if !ok {
		return
	}
	id, ok := objectID(w, r)
	if !ok {
		return
	}

	data, err := st.GetObject(id)
	if err != nil {
		objectReadError(w, r, err, "object", id)
		return
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("ETag", `"`+id.String()+`"`)
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	if matchesETag(r.Header.Get("If-None-Match"), `"`+id.String()+`"`) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(data)))
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(data)
	}
}

// putObjectHandler stores one manifest, directory or commit.
//
// Idempotent by construction: the id is the content, so a repeated PUT of the
// same object is the same object, and a client retrying after a dropped
// connection needs no special case. It answers 200 rather than 201 when the
// object was already there, which is the one bit of information the retry
// might want and costs an Exists call to provide.
func putObjectHandler(w http.ResponseWriter, r *http.Request) {
	_, st, ok := storeV2Repo(w, r, true)
	if !ok {
		return
	}
	id, ok := objectID(w, r)
	if !ok {
		return
	}

	data, ok := readObjectBody(w, r)
	if !ok {
		return
	}
	if err := decodesAsAnObject(data); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	existed, err := st.HasObject(id)
	if err != nil {
		log.WithContext(r.Context()).WithError(err).Errorf("failed to stat object %s", id)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
	if err := st.PutObject(id, data); err != nil {
		putObjectError(w, r, err, "object", id)
		return
	}

	w.Header().Set("ETag", `"`+id.String()+`"`)
	if existed {
		w.WriteHeader(http.StatusOK)
		return
	}
	w.WriteHeader(http.StatusCreated)
}

// getChunkHandler serves one chunk as stored — plaintext in a plain library,
// the sealed frame in an E2EE one. The server does not know which it is
// handing over and does not need to: the client that asked holds the key, or
// does not need one.
func getChunkHandler(w http.ResponseWriter, r *http.Request) {
	_, st, ok := storeV2Repo(w, r, false)
	if !ok {
		return
	}
	id, ok := objectID(w, r)
	if !ok {
		return
	}

	data, err := st.GetChunk(id)
	if err != nil {
		objectReadError(w, r, err, "chunk", id)
		return
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("ETag", `"`+id.String()+`"`)
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	if matchesETag(r.Header.Get("If-None-Match"), `"`+id.String()+`"`) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(data)))
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(data)
	}
}

// putChunkHandler stores one chunk, verified against its id.
//
// No decode check here, and there cannot be one: a chunk is opaque bytes in a
// plain library and a sealed frame in an E2EE one, and neither has a structure
// the server can check without the key. The id is the whole verification, and
// for a plain library it is a real one — SHA-256 of exactly these bytes.
func putChunkHandler(w http.ResponseWriter, r *http.Request) {
	_, st, ok := storeV2Repo(w, r, true)
	if !ok {
		return
	}
	id, ok := objectID(w, r)
	if !ok {
		return
	}

	data, ok := readObjectBody(w, r)
	if !ok {
		return
	}

	existed, err := st.HasChunk(id)
	if err != nil {
		log.WithContext(r.Context()).WithError(err).Errorf("failed to stat chunk %s", id)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
	if err := st.PutChunk(id, data); err != nil {
		putObjectError(w, r, err, "chunk", id)
		return
	}

	w.Header().Set("ETag", `"`+id.String()+`"`)
	if existed {
		w.WriteHeader(http.StatusOK)
		return
	}
	w.WriteHeader(http.StatusCreated)
}

// readObjectBody reads a bounded request body.
func readObjectBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxObjectBody))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			http.Error(w, "Object is too large", http.StatusRequestEntityTooLarge)
			return nil, false
		}
		// A client that hung up mid-body has not asked for an answer.
		return nil, false
	}
	if len(data) == 0 {
		http.Error(w, "Empty body", http.StatusBadRequest)
		return nil, false
	}
	return data, true
}

// decodesAsAnObject reports whether the bytes parse as some store-v2 object.
//
// It tries all three because the format carries no type tag — a manifest, a
// directory and a commit each start with a version and a flags byte, and
// nothing in the bytes says which one this is. That is fine for what this
// check is for: the question is not "which type is it" but "would the server's
// own walks be able to read it later", and any of the three answers that.
//
// The public decoders specifically, because they are the ones the server will
// actually use — GC's mark and changes?since= read public sections and hold no
// key. Checking with a decoder the server can never run would prove the wrong
// thing.
func decodesAsAnObject(data []byte) error {
	if _, err := store.DecodeManifestPublic(data); err == nil {
		return nil
	}
	if _, err := store.DecodeDirectoryPublic(data); err == nil {
		return nil
	}
	if _, err := store.DecodeCommitPublic(data); err == nil {
		return nil
	}
	return errors.New("these bytes do not decode as a manifest, a directory or a commit")
}

// objectReadError maps a failed read to a status. A missing object is a 404
// and anything else is damage worth logging: the distinction matters because
// a client acts on the first and must not act on the second.
func objectReadError(w http.ResponseWriter, r *http.Request, err error, kind string, id store.ID) {
	if errors.Is(err, objstore.ErrNotFound) {
		http.Error(w, "Not found", http.StatusNotFound)
		return
	}
	log.WithContext(r.Context()).WithError(err).Errorf("failed to read %s %s", kind, id)
	http.Error(w, "Internal server error", http.StatusInternalServerError)
}

// putObjectError maps a failed write to a status. Bytes that do not hash to
// the id they were offered under are the client's error, not the server's —
// which is the whole reason objstore makes that a sentinel.
func putObjectError(w http.ResponseWriter, r *http.Request, err error, kind string, id store.ID) {
	if errors.Is(err, objmgr.ErrIDMismatch) || errors.Is(err, objstore.ErrContentMismatch) {
		http.Error(w, "These bytes do not hash to that id", http.StatusBadRequest)
		return
	}
	log.WithContext(r.Context()).WithError(err).Errorf("failed to store %s %s", kind, id)
	http.Error(w, "Internal server error", http.StatusInternalServerError)
}

// putHeadHandler moves a library's head, and is a compare-and-swap.
//
//	PUT /api/silo/v1/repos/{repo}/head
//	If-Match: <current head commit id>
//	<new head commit id>
//
// This is the operation server-side merge used to hide. A writer that loses
// the race gets 412 and rebuilds on the new head; it does not get its work
// merged for it, because merging trees means reading names and on an E2EE
// library the server cannot. The refusal is the feature.
//
// If-Match is required rather than optional. Every other mutating route in
// this API treats it as opt-in, and the default there is last-writer-wins on
// one path — recoverable, and visible in the file. Here the unit is the whole
// library: a blind head move discards every change made since the client last
// looked, silently. There is no sensible default for that, so there is no
// default.
func putHeadHandler(w http.ResponseWriter, r *http.Request) {
	repo, st, ok := storeV2Repo(w, r, true)
	if !ok {
		return
	}

	ifMatch := strings.TrimSpace(r.Header.Get("If-Match"))
	if ifMatch == "" {
		http.Error(w, "If-Match is required: name the head you are replacing", http.StatusBadRequest)
		return
	}
	expected := strings.Trim(ifMatch, `"`)

	body, ok := readObjectBody(w, r)
	if !ok {
		return
	}
	newHead, err := store.ParseID(strings.TrimSpace(string(body)))
	if err != nil {
		http.Error(w, "Body must be the new head commit id", http.StatusBadRequest)
		return
	}

	// Read the commit rather than merely checking it exists. It has to decode,
	// it has to name a root that is here, and it has to descend from the head
	// it claims to replace — a commit whose parent is not the current head is
	// a fork, and accepting one would let a client rewrite history by writing
	// a commit with no parents at all.
	commit, err := st.GetCommitPublic(newHead)
	if err != nil {
		if errors.Is(err, objstore.ErrNotFound) {
			http.Error(w, "That commit is not in this library; upload it first", http.StatusBadRequest)
			return
		}
		log.WithContext(r.Context()).WithError(err).Errorf("failed to read commit %s", newHead)
		http.Error(w, "That object is not a commit", http.StatusBadRequest)
		return
	}
	if hasRoot, err := st.HasObject(commit.Root); err != nil {
		log.WithContext(r.Context()).WithError(err).Errorf("failed to stat root %s", commit.Root)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	} else if !hasRoot {
		http.Error(w, "That commit's root directory is not in this library; upload it first", http.StatusBadRequest)
		return
	}
	if !descendsFrom(commit, expected) {
		http.Error(w, "That commit does not descend from the head you named", http.StatusConflict)
		return
	}

	gcID, err := repomgr.GetCurrentGCID(repo.StoreID)
	if err != nil {
		log.WithContext(r.Context()).WithError(err).Errorf("failed to read gc id for repo %s", repo.ID)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	acct := middleware.GetAccount(r)
	_, err = updateBranch(repo.ID, repo.StoreID, headMove{
		CommitID: newHead.String(),
		RootID:   commit.Root.String(),
		Author:   acct.Email,
		Ctime:    commit.CreatedAt,
	}, expected, "", true, gcID)
	if err != nil {
		if errors.Is(err, ErrGCConflict) {
			http.Error(w, "A collection is running; retry", http.StatusServiceUnavailable)
			return
		}
		// updateBranch refuses when the head is not what the caller named,
		// which is exactly what If-Match asked about.
		http.Error(w, "The head has moved; re-read it and rebuild", http.StatusPreconditionFailed)
		return
	}

	w.Header().Set("ETag", `"`+newHead.String()+`"`)
	w.WriteHeader(http.StatusOK)
}

// descendsFrom reports whether the commit names parent among its parents.
//
// One generation only, on purpose. Walking further would let a client skip
// history by presenting a descendant several commits along, and the point of
// the check is that the head advances one commit at a time so nothing in
// between is lost.
func descendsFrom(commit *store.PublicCommit, parent string) bool {
	for _, p := range commit.Parents {
		if p.String() == parent {
			return true
		}
	}
	return false
}
