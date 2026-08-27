package silod

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/dkam/silo/fileserver/objmgr"
	"github.com/dkam/silo/fileserver/objstore"
	"github.com/dkam/silo/store"
	"github.com/gorilla/mux"
	log "github.com/sirupsen/logrus"
)

// The block surface: the Silo lane's answer to "don't send what the server
// already has, and don't start over when a transfer dies".
//
//	POST /api/silo/v1/libraries/{library}/blocks/missing              {"blocks":[…]} -> {"missing":[…]}
//	PUT  /api/silo/v1/libraries/{library}/blocks/{id}                 the chunk's bytes
//	PUT  /api/silo/v1/libraries/{library}/entries/{path}?type=blocks  {"blocks":[…]}
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

// maxBlockListBody bounds a block-id list. At roughly 43 bytes per quoted id
// and comma this is some 380,000 blocks, which at the default block size is
// several terabytes in one file — far past any real request, which is what a
// limit is for.
const maxBlockListBody = 16 << 20

// blocksMissingHandler answers which of the offered blocks the store does not
// already hold, in the order they were offered.
//
// Write permission, not read, even though this only reads. Its whole purpose
// is to precede an upload, and a store's block ids are content: answering
// "yes, I have that one" to anyone with read access to any library turns this
// into an oracle for whether a given file exists somewhere on the server.
func blocksMissingHandler(w http.ResponseWriter, r *http.Request) {
	// A chunk is addressed by its content hash, not by a path, so this is a
	// library-level check: a path-scoped credential cannot use the chunk
	// surface, because a chunk id says nothing about where it will be linked.
	library := entryLibrary(w, r, mux.Vars(r)["libraryid"], "", true)
	if library == nil {
		return
	}

	var body struct {
		Blocks []string `json:"blocks"`
	}
	if !decodeJSONBody(w, r, maxBlockListBody, &body, `Expected a JSON body such as {"blocks":["<64 hex characters>",…]}`) {
		return
	}

	ids := make([]store.ID, 0, len(body.Blocks))
	for _, raw := range body.Blocks {
		id, err := store.ParseID(raw)
		if err != nil {
			// Says what it wanted, because whoever reads this has just been
			// surprised. The message used to name only what arrived, while
			// the example three lines above told them to send a SHA-1 — so a
			// client author who followed the error text was pointed at the
			// bug rather than away from it. A chunk id is the SHA-256 of the
			// chunk's bytes; a 40-character SHA-1 is refused here, not
			// converted.
			http.Error(w, "Not a chunk id: "+raw+
				" (want 64 lowercase hex characters, the SHA-256 of the chunk)",
				http.StatusBadRequest)
			return
		}
		ids = append(ids, id)
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
