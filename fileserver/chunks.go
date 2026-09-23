package silod

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/SiloDrive/silo/fileserver/objmgr"
	"github.com/SiloDrive/silo/fileserver/objstore"
	"github.com/SiloDrive/silo/store"
	"github.com/gorilla/mux"
	log "github.com/sirupsen/logrus"
)

// The chunk surface: the Silo lane's answer to "don't send what the server
// already has, and don't start over when a transfer dies".
//
//	POST /api/silo/v1/libraries/{library}/chunks/missing              {"chunks":[…]} -> {"missing":[…]}
//	PUT  /api/silo/v1/libraries/{library}/chunks/{id}                 the chunk's bytes
//	PUT  /api/silo/v1/libraries/{library}/entries/{path}?type=chunks  {"chunks":[…]}
//
// Whole-file PUT still exists and is still the right call for one small file:
// it is one request, and it needs no hashing on the client. What it cannot do
// is resume, skip content the server already holds, or upload in parallel, and
// all three matter to a sync client the moment a file is large or a link is
// unreliable.
//
// This works because a client can compute chunk ids without asking. The
// library's chunker parameters are in the catalog and reported to the client,
// and a chunk's id is the SHA-256 of its bytes, so a client that chunks the
// same way arrives at exactly the names the server would. That is what makes
// "which of these do you have?" a question a client can pose before
// transferring anything.
//
// Content-defined boundaries are why this dedups where a fixed-offset scheme
// did not: inserting a byte near the front of a file used to shift every
// boundary after it, so nothing matched and the whole file went up again.

// maxChunkListBody bounds a chunk-id list. At roughly 43 bytes per quoted id
// and comma this is some 380,000 chunks, which at the default chunk size is
// several terabytes in one file — far past any real request, which is what a
// limit is for.
const maxChunkListBody = 16 << 20

// chunksMissingHandler answers which of the offered chunks the store does not
// already hold, in the order they were offered.
//
// Write permission, not read, even though this only reads. Its whole purpose
// is to precede an upload, and a store's chunk ids are content: answering
// "yes, I have that one" to anyone with read access to any library turns this
// into an oracle for whether a given file exists somewhere on the server.
func chunksMissingHandler(w http.ResponseWriter, r *http.Request) {
	// A chunk is addressed by its content hash, not by a path, so this is a
	// library-level check: a path-scoped credential cannot use the chunk
	// surface, because a chunk id says nothing about where it will be linked.
	library := entryLibrary(w, r, mux.Vars(r)["libraryid"], "", true)
	if library == nil {
		return
	}

	var body struct {
		Chunks []string `json:"chunks"`
	}
	if !decodeJSONBody(w, r, maxChunkListBody, &body, `Expected a JSON body such as {"chunks":["<64 hex characters>",…]}`) {
		return
	}

	ids, ok := parseChunkIDs(w, body.Chunks, false)
	if !ok {
		return
	}
	st, err := library.Store()
	if err != nil {
		log.WithContext(r.Context()).WithError(err).Errorf("failed to open store for library %s", library.ID)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
	missing, _, err := chunkInventory(st, ids)
	if err != nil {
		log.WithContext(r.Context()).WithError(err).Errorf("failed to inventory chunks in library %s", library.ID)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	writeEntryJSON(w, http.StatusOK, map[string]any{"missing": missing})
}

// parseChunkIDs turns a request's hex ids into store ids, answering 400 itself
// on the first one that is not an id and reporting false.
//
// One copy, because the message is part of the contract. It had already drifted
// once — the error named only what arrived while the example three lines above
// it told the reader to send a SHA-1 — and a second endpoint copying the fixed
// version is how that comes back.
//
// dedupe drops repeats, keeping first-asked order. The fetch path wants it: a
// file with a run of zeroes names one chunk many times, and frames are matched
// by id rather than by position, so sending those bytes once per mention would
// spend exactly the bandwidth that endpoint exists to save. The missing path
// does not, because it answers positionally.
func parseChunkIDs(w http.ResponseWriter, raw []string, dedupe bool) ([]store.ID, bool) {
	ids := make([]store.ID, 0, len(raw))
	var seen map[store.ID]struct{}
	if dedupe {
		seen = make(map[store.ID]struct{}, len(raw))
	}
	for _, hex := range raw {
		id, err := store.ParseID(hex)
		if err != nil {
			http.Error(w, "Not a chunk id: "+hex+
				" (want 64 lowercase hex characters, the SHA-256 of the chunk)",
				http.StatusBadRequest)
			return nil, false
		}
		if dedupe {
			if _, dup := seen[id]; dup {
				continue
			}
			seen[id] = struct{}{}
		}
		ids = append(ids, id)
	}
	return ids, true
}

// chunkInventory reports which of the offered ids the chunk store does not
// hold, in the order they were offered, and the total size of a file made of
// them.
//
// An id is inspected once however often it repeats, and reported missing once,
// because a file that repeats a chunk — a run of zeroes, a duplicated section
// — would otherwise have the client upload the same bytes twice. Its size
// still counts every time it appears: the repeat is a real part of the file's
// length even though it is one object on disk.
//
// Presence is asked of Exists rather than inferred from a failed Stat. They
// are not the same question: a stat that fails because the disk is unhappy
// would come back as "not present", the client would upload the chunk again,
// and the second write would fail the same way with the cause now two steps
// removed from where it happened.
//
// The size it returns is the STORED size, which under E2EE is sixteen bytes
// per chunk larger than the plaintext. That is correct for what this answers —
// how many bytes the client is about to not have to send — and it is why this
// number must never reach quota, which is logical size at head.
func chunkInventory(st *objmgr.Store, ids []store.ID) (missing []string, size int64, err error) {
	// Never nil: an empty list has to encode as [] and not null.
	missing = []string{}
	seen := make(map[store.ID]int64, len(ids))
	for _, id := range ids {
		known, ok := seen[id]
		if !ok {
			// ChunkStoredSize and not GetChunk: the only thing wanted is the
			// number, and reading a 4 MiB chunk to measure it costs some eight
			// hundred times a stat and allocates the whole chunk to throw it
			// away — on the one endpoint whose entire purpose is to avoid
			// moving those bytes. Stored and not plaintext, per the paragraph
			// above.
			sz, err := st.ChunkStoredSize(id)
			if err != nil {
				if errors.Is(err, objstore.ErrNotFound) {
					seen[id] = -1
					missing = append(missing, id.String())
					continue
				}
				return nil, 0, fmt.Errorf("failed to read chunk %s: %w", id, err)
			}
			known = sz
			seen[id] = known
		}
		if known < 0 {
			continue
		}
		size += known
	}
	return missing, size, nil
}
