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

// The read half of the chunk surface.
//
//	POST /api/silo/v1/libraries/{library}/chunks/fetch   {"chunks":[id,…]} -> a chunk stream
//
// `GET chunks/{id}` already serves one chunk, immutably and cacheably, and
// stays the right call for one. This is the same answer for many, and it is
// asked for by the same argument that produced chunks/missing: a client that
// can name content by hash should be able to say the whole list at once.
//
// The framing is store.WriteChunkFrame — see store/chunkstream.go for the
// layout and for why the format lives in the shared module rather than here.
//
// Why not a range over one endpoint, or a multipart body: a chunk is addressed
// by content, so the response needs to name what each part is, and both of
// those spellings would carry that naming in a second vocabulary. The frame's
// id field is the same 32 bytes the request asked with.

// maxFetchChunks caps one request. It is a round-trip amortiser rather than a
// bulk-download API: nearly all of the win from batching is in the first
// couple of hundred, and past that the cost is a long response with no cursor
// to resume from.
//
// Over the cap is a 400 and not a short answer, for the reason pagination is
// opt-in with no default — a truncated response that looks complete is worse
// than a large one, and here the client cannot even tell, because a chunk it
// did not receive is indistinguishable from one the store does not hold.
const maxFetchChunks = 256

// chunkStreamMediaType names the framing so a proxy or a client library cannot
// mistake it for an opaque download.
const chunkStreamMediaType = "application/vnd.silo.chunks"

// chunksFetchHandler streams the requested chunks, in the order asked, each
// frame naming the id it answers.
//
// Read permission, not write — the opposite of chunks/missing next door, and
// the difference is worth stating because the two sit together. `missing`
// takes write because answering "yes, I hold that one" to anyone with read
// access turns it into an oracle for whether a given file exists somewhere on
// the server. This one hands over the bytes, so a caller who can use the
// answer can already read the library, and there is no oracle left to close:
// GET chunks/{id} makes the same disclosure one chunk at a time.
func chunksFetchHandler(w http.ResponseWriter, r *http.Request) {
	// Library-level, like every other id-addressed call: a chunk id says
	// nothing about where the chunk is linked, so a path-scoped credential
	// cannot be checked against it. Such a client reads a manifest through
	// entries/{path}?type=manifest and the bytes through the path.
	library := entryLibrary(w, r, mux.Vars(r)["libraryid"], "", false)
	if library == nil {
		return
	}

	var body struct {
		Chunks []string `json:"chunks"`
	}
	if !decodeJSONBody(w, r, maxChunkListBody, &body, `Expected a JSON body such as {"chunks":["<64 hex characters>",…]}`) {
		return
	}
	if len(body.Chunks) > maxFetchChunks {
		http.Error(w, fmt.Sprintf("Asked for %d chunks; at most %d may be fetched in one request",
			len(body.Chunks), maxFetchChunks), http.StatusBadRequest)
		return
	}

	// Deduplicated in the order first asked. A file with a run of zeroes names
	// one chunk many times, and sending those bytes once per mention would
	// spend exactly the bandwidth this endpoint exists to save. Safe to drop
	// the repeats because a frame is matched by its id and not by its
	// position — see the layout note in store/chunkstream.go.
	ids := make([]store.ID, 0, len(body.Chunks))
	seen := make(map[store.ID]struct{}, len(body.Chunks))
	for _, raw := range body.Chunks {
		id, err := store.ParseID(raw)
		if err != nil {
			http.Error(w, "Not a chunk id: "+raw+
				" (want 64 lowercase hex characters, the SHA-256 of the chunk)",
				http.StatusBadRequest)
			return
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}

	st, err := library.Store()
	if err != nil {
		log.WithContext(r.Context()).WithError(err).Errorf("failed to open store for library %s", library.ID)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", chunkStreamMediaType)
	// No Content-Length: the body is streamed, so its size is not known until
	// it has been sent. That is deliberate — buffering to compute a length
	// would hold up to maxFetchChunks chunks in memory on the one endpoint
	// whose purpose is to move a lot of them.
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	streamChunks(w, r, st, library.ID, ids)
}

// streamChunks writes one frame per id.
//
// Each chunk is read whole before its frame is started, so a read that fails
// cannot leave a header promising bytes that never arrive. The failure that
// remains is a body that ends early, which is what a client's decoder reports
// as a truncated stream — the status is long gone by then, which is the same
// bargain serveFile makes and for the same reason.
func streamChunks(w http.ResponseWriter, r *http.Request, st *objmgr.Store, libraryID string, ids []store.ID) {
	flusher, _ := w.(http.Flusher)
	for _, id := range ids {
		data, err := st.GetChunk(id)
		if err != nil {
			if errors.Is(err, objstore.ErrNotFound) {
				if err := store.WriteAbsentChunkFrame(w, id); err != nil {
					return
				}
				continue
			}
			log.WithContext(r.Context()).WithError(err).
				Errorf("failed to read chunk %s in library %s mid-stream", id, libraryID)
			return
		}
		if err := store.WriteChunkFrame(w, id, data); err != nil {
			// The client hung up, or the write failed. Either way there is
			// nobody left to tell.
			return
		}
		// Flushed per chunk so a client can lay one down while the next is
		// still being read, which is the whole latency win over N requests.
		if flusher != nil {
			flusher.Flush()
		}
	}
}
