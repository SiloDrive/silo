package silod

import (
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"

	"github.com/SiloDrive/silo/fileserver/objstore"
	"github.com/SiloDrive/silo/store"
	log "github.com/sirupsen/logrus"
)

// uploadBatchBytes is how much of a request is held before it is stored.
//
// A bound is needed in both directions. Storing each chunk as it arrives —
// what this did before — means a file's frames interleave with any concurrent
// upload to the same library, and once interleaved nothing downstream can
// separate them: only this layer knows which chunks arrived together, so
// compaction can preserve their order but never recover it. Holding the whole
// request instead would mean up to 256 chunks resident, which at the sizes the
// chunker produces is hundreds of megabytes per upload in flight.
//
// 16 MB is the compromise: a run of frames long enough to be worth having
// contiguous, and small enough that many concurrent uploads cost bounded
// memory. It is not a tuning knob; it is a memory ceiling with a locality
// benefit, and it should be measured before it is moved.
const uploadBatchBytes = 16 << 20

// The write half of the batched chunk surface.
//
//	POST /api/silo/v1/libraries/{library}/chunks   a chunk stream -> {"stored":N,"present":M}
//
// `PUT chunks/{id}` still exists and is still the right call for one chunk: it
// is cacheable, it is the shape a retry takes, and it needs no framing. What it
// cannot do is amortise a round trip. The chunker targets 1 MiB, so a 1 GB file
// is roughly a thousand PUTs — a thousand round trips to move content that a
// single connection could stream in one. That is the last asymmetry on this
// surface: the read side has answered many-at-once since `chunks/fetch`, and
// the write side, whose whole purpose is bulk, had no equivalent.
//
// The framing is the fetch side's, read rather than written — store's own
// comment on the format said as much before either end used it that way. One
// format means a client has one encoder and one decoder rather than two of
// each, and it means the id check that makes a chunk trustworthy is the same
// code in both directions.
//
// What this does NOT do is create anything. Chunks are content, and content is
// nameless until a tree points at it: the entry still arrives through
// `PUT entries/{path}?type=chunks` or through `batch`. That separation is what
// makes an interrupted upload resumable with no session to keep — see the note
// on chunks/missing next door.

// maxUploadChunks bounds the frame count in one request.
//
// The download side's cap is about a response with no cursor to resume from.
// This is a different argument and lands in a similar place: a batch is
// all-or-nothing in what it reports, so an oversized one costs a client the
// bytes it already sent when the last frame fails. 256 at the 1 MiB target is
// a quarter-gigabyte request, which is the most that is sensible to lose.
const maxUploadChunks = 256

// maxUploadBody bounds the same request by size, because the count alone does
// not. A library chunking at the format's 4 MiB maximum reaches this at 64
// frames rather than 256, which is correct — the limit that binds should be
// whichever comes first, and a client batching by bytes never notices either.
const maxUploadBody = 256 << 20

// chunksUploadHandler stores every chunk in the request body, verified against
// the id each arrived under.
//
// Write permission, for the ordinary reason rather than chunks/missing's
// subtle one: this writes.
func chunksUploadHandler(w http.ResponseWriter, r *http.Request) {
	library, st, ok := idAddressedLibrary(w, r, true)
	if !ok {
		return
	}
	if !isChunkStream(w, r) {
		return
	}

	// The same soft ceiling PUT chunks/{id} applies, and with the same
	// reasoning — see putChunkHandler. Asked once here on the declared length
	// so a client that is already over its quota is turned away before it
	// spends a quarter-gigabyte upload finding out, and asked again per chunk
	// below because a chunked request declares nothing and a lying one
	// declares whatever it likes.
	if refuseOverQuota(w, library, declaredLength(r)) {
		return
	}

	body := http.MaxBytesReader(w, r.Body, maxUploadBody)
	var stored, present int
	// Chunks are stored in groups rather than one at a time, so that a file's
	// frames land together instead of interleaving with a concurrent upload's,
	// and so that one durability barrier covers many of them. first is kept
	// typed, for the error path's log line.
	var batch []objstore.Object
	var batchBytes int64
	var first store.ID
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		if err := st.PutChunks(batch); err != nil {
			return err
		}
		batch, batchBytes = batch[:0], 0
		return nil
	}
	// One buffer down the whole stream. ReadChunkFrame reads into it and grows
	// it only when a frame does not fit, so a 256-frame request allocates
	// about once rather than 256 times — the same bargain GetChunkInto makes
	// on the way out.
	var scratch []byte

	for {
		frame, err := store.ReadChunkFrame(body, scratch)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			uploadStreamError(w, r, err, stored+present)
			return
		}
		scratch = frame.Bytes

		// Absence is a status the read side uses to say "I do not hold this".
		// Going up it would have to mean something new, and a frame that means
		// nothing is refused rather than skipped: silently ignoring it would
		// let a client believe it had uploaded a chunk it had not.
		if !frame.Present {
			http.Error(w, "Chunk "+frame.ID.String()+" is framed as absent, which has no meaning in an upload",
				http.StatusBadRequest)
			return
		}
		if stored+present == maxUploadChunks {
			http.Error(w, fmt.Sprintf("More than %d chunks in one request; send them in several",
				maxUploadChunks), http.StatusRequestEntityTooLarge)
			return
		}

		existed, err := st.HasChunk(frame.ID)
		if err != nil {
			log.WithContext(r.Context()).WithError(err).Errorf("failed to stat chunk %s", frame.ID)
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}
		if existed {
			// Not an error, and worth counting separately: it is what a client
			// sees when its chunks/missing answer went stale under it, and the
			// count tells it that happened without a second request.
			present++
			continue
		}
		if refuseOverQuota(w, library, int64(len(frame.Bytes))) {
			return
		}
		// Collected rather than stored one at a time, so that the frames of one
		// request land together instead of interleaving with a concurrent
		// upload's. Only this layer knows which chunks arrived as one file, so
		// an order lost here cannot be recovered underneath.
		if len(batch) == 0 {
			first = frame.ID
		}
		// Copied, because ReadChunkFrame hands back a slice of the scratch
		// buffer it is about to reuse. The old code stored each chunk before
		// reading the next and never had to care.
		data := make([]byte, len(frame.Bytes))
		copy(data, frame.Bytes)
		batch = append(batch, objstore.Object{ID: frame.ID.String(), Data: data})
		batchBytes += int64(len(data))
		stored++

		if batchBytes >= uploadBatchBytes {
			if err := flush(); err != nil {
				putObjectError(w, r, err, "chunk", first)
				return
			}
		}
	}

	if err := flush(); err != nil {
		putObjectError(w, r, err, "chunk", first)
		return
	}

	// Chunks written before a failure are left where they are, deliberately.
	// They are content-addressed, so storing one twice is storing it once, and
	// nothing references them until an entry does — a client that retries asks
	// chunks/missing and gets a shorter list, which is this surface's whole
	// resume story. What is not left behind is a half-created file, because
	// this endpoint creates nothing.
	//
	// Storing in groups does not change that: a group either stores or does
	// not, and the groups before it stay stored. What a client resends is
	// bounded by uploadBatchBytes rather than by one chunk, which is the same
	// bargain as any batching.
	//
	// stored + present equals the number of frames the body carried, always,
	// and that invariant is the receipt: it is how a client confirms the server
	// saw every chunk it sent without asking a second time. Every frame is
	// therefore counted in exactly one of the two, and a request that cannot
	// count one answers an error instead of a smaller total. Anyone adding a
	// third outcome here — rejected, deferred, charged — has to add it to the
	// sum a client checks, or teach the client a new one; a third counter that
	// quietly makes the pair fall short breaks the check without failing a test.
	writeEntryJSON(w, http.StatusOK, map[string]any{"stored": stored, "present": present})
}

// isChunkStream reports whether the request declares the framing, answering
// 415 itself when it does not.
//
// Checked rather than assumed because the framing is not guessable from the
// bytes: a body of the wrong shape would be read as a 32-byte id, a status and
// a length, and would fail somewhere further in with an error about a chunk
// that was never sent. A client that forgot the header should be told that.
func isChunkStream(w http.ResponseWriter, r *http.Request) bool {
	ct := r.Header.Get("Content-Type")
	if ct == "" {
		http.Error(w, "Expected Content-Type: "+chunkStreamMediaType, http.StatusUnsupportedMediaType)
		return false
	}
	// Parsed rather than compared, so a charset or boundary parameter does not
	// turn a well-formed request away.
	media, _, err := mime.ParseMediaType(ct)
	if err != nil || media != chunkStreamMediaType {
		http.Error(w, "Expected Content-Type: "+chunkStreamMediaType+", not "+ct,
			http.StatusUnsupportedMediaType)
		return false
	}
	return true
}

// uploadStreamError answers a body that could not be read to the end.
//
// Truncation is called out separately from a malformed frame because they ask
// the client for different things. A truncated stream is the transfer's fault
// and the request should simply be made again; a frame that does not decode,
// or bytes that do not hash to the id they arrived under, is the client's own
// framing and retrying it will fail identically. Both are 400 — the server
// cannot act on either — but the message has to say which, or a client author
// debugs the wrong half.
func uploadStreamError(w http.ResponseWriter, r *http.Request, err error, read int) {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		http.Error(w, fmt.Sprintf("Upload is over the %d byte limit for one request", maxUploadBody),
			http.StatusRequestEntityTooLarge)
		return
	}
	if errors.Is(err, store.ErrChunkStreamTruncated) {
		http.Error(w, fmt.Sprintf(
			"The chunk stream ended without its terminator after %d chunks; the upload was cut short, send it again",
			read), http.StatusBadRequest)
		return
	}
	log.WithContext(r.Context()).WithError(err).Debug("malformed chunk stream on upload")
	http.Error(w, "Malformed chunk stream: "+err.Error(), http.StatusBadRequest)
}
