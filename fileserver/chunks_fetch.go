package silod

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/SiloDrive/silo/fileserver/objmgr"
	"github.com/SiloDrive/silo/fileserver/objstore"
	"github.com/SiloDrive/silo/store"
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

// maxFetchBody bounds the request. maxChunkListBody is 16 MiB, sized for
// chunks/missing, which caps no count — and decodeJSONBody decodes the whole
// body before the count check below can refuse it, so borrowing that limit here
// meant a 16 MiB body decoding to some 390,000 strings on its way to a 400.
// Sized from the cap instead: 68 bytes per quoted id and comma, plus slack for
// the envelope.
const maxFetchBody = maxFetchChunks*68 + 1024

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
	library, st, ok := idAddressedLibrary(w, r, false)
	if !ok {
		return
	}

	var body struct {
		Chunks []string `json:"chunks"`
	}
	if !decodeJSONBody(w, r, maxFetchBody, &body, `Expected a JSON body such as {"chunks":["<64 hex characters>",…]}`) {
		return
	}
	if len(body.Chunks) > maxFetchChunks {
		http.Error(w, fmt.Sprintf("Asked for %d chunks; at most %d may be fetched in one request",
			len(body.Chunks), maxFetchChunks), http.StatusBadRequest)
		return
	}
	ids, ok := parseChunkIDs(w, body.Chunks, true)
	if !ok {
		return
	}

	w.Header().Set("Content-Type", chunkStreamMediaType)
	// No Content-Length: the body is streamed, so its size is not known until
	// it has been sent. That is deliberate — buffering to compute a length
	// would hold up to maxFetchChunks chunks in memory on the one endpoint
	// whose purpose is to move a lot of them. What makes a short body legible
	// instead is the terminator streamChunks writes at the end.
	w.WriteHeader(http.StatusOK)
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
	// One scratch buffer for the chunk and one for the frame, both reused down
	// the loop. Without them a 256-chunk response allocates 256 chunks, each
	// grown from 512 bytes — some thirteen reallocations and twice the chunk's
	// own size in memcpy apiece — on the one path whose whole purpose is bulk
	// transfer.
	var chunk, frame []byte
	for _, id := range ids {
		var err error
		chunk, err = st.GetChunkInto(id, chunk)
		switch {
		case errors.Is(err, objstore.ErrNotFound):
			if _, err := w.Write(store.AppendAbsentChunkFrame(frame[:0], id)); err != nil {
				return
			}
			continue
		case err != nil:
			log.WithContext(r.Context()).WithError(err).
				Errorf("failed to read chunk %s in library %s mid-stream", id, libraryID)
			// No terminator, so the client reads this as truncation rather
			// than as a short but complete answer.
			return
		}
		// A zero-length chunk is what a publish interrupted before its fsync
		// leaves behind, and objstore.Exists already calls that absent — so
		// chunks/missing would ask for it again. Read returns it as a
		// successful empty read, and framing it as present would fail its hash
		// check in the client's decoder and discard the whole batch with it.
		// Answering absent keeps the three spellings of "do you have this"
		// saying the same thing.
		if len(chunk) == 0 {
			if _, err := w.Write(store.AppendAbsentChunkFrame(frame[:0], id)); err != nil {
				return
			}
			continue
		}

		frame, err = store.AppendChunkFrame(frame[:0], id, chunk)
		if err != nil {
			// Not a write failure: the only error here is a chunk over the
			// frame limit, which is this server's problem and would otherwise
			// truncate the response with nothing in the log to say why.
			log.WithContext(r.Context()).WithError(err).
				Errorf("chunk %s in library %s cannot be framed", id, libraryID)
			return
		}
		if _, err := w.Write(frame); err != nil {
			// The client hung up. There is nobody left to tell.
			return
		}
		// Flushed per chunk so a client can lay one down while the next is
		// still being read, which is the whole latency win over N requests.
		if flusher != nil {
			flusher.Flush()
		}
	}
	// The terminator is what makes a complete response distinguishable from one
	// that stopped early. Everything above returns without writing it.
	if err := store.WriteChunkStreamEnd(w); err != nil {
		return
	}
	if flusher != nil {
		flusher.Flush()
	}
}
