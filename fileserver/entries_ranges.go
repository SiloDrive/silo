package silod

import (
	"fmt"
	"net/http"

	"github.com/SiloDrive/silo/store"
	"github.com/gorilla/mux"
	log "github.com/sirupsen/logrus"
)

// The range read, in one round trip.
//
//	QUERY /api/silo/v1/libraries/{library}/entries/{path}
//	{"ranges": [[offset, length], …]}   ->   manifest frame, then chunk frames
//
// A client reading part of a file it does not hold needs two things: the
// manifest, to learn which chunks cover the bytes it wants, and those chunks.
// Asked separately those are serialised — GET objects/{id} then POST
// chunks/fetch — because the second cannot name its ids until the first lands,
// and the first fetches nothing at all. It asks where to look. On the host
// that motivated this it is 165/312/738 ms, which is a third of a second of
// pure latency in front of every cold file, paid before anything appears on
// screen. See Silo/silo#81.
//
// Four decisions are worth stating, because each had an obvious alternative.
//
// **QUERY, not POST.** POST entries/{path} is taken and means mutate: it is
// the move and copy verb. Putting a safe, idempotent read on the same route
// distinguished only by a body or a ?type= is the thing QUERY exists to avoid,
// and a proxy or a retry that cannot tell the two apart is how that goes
// wrong. Go's server takes any valid method token and mux routes on it, so
// this costs nothing but the name.
//
// **Not a Range header.** That header is scoped to the representation being
// returned, so on this route it would have to mean "part of the manifest" —
// ?type=manifest is a representation of the same resource. Asking for two
// representations of one resource at once is what makes this a method and a
// body rather than a header.
//
// **Path-addressed, not id-addressed.** The obvious shape takes the manifest
// id, and would be unusable by the client that asked for it: objects/{id} is
// refused to a credential narrowed to a subtree, because an id says nothing
// about where it is linked and so cannot be checked against a scope. That is
// the same reasoning that put entries/{path}?type=manifest beside objects/{id}
// in the first place.
//
// **No "first chunk" bundled with the manifest.** That shape was considered
// and refused: it is a bet on the read being at offset 0, which is fair for a
// header and wrong for a seek into the middle or for an MP4 whose moov atom
// sits at the tail — which are exactly the reads partial hydration exists for.
// The covering chunks are the answer; the first chunk is a guess.

// methodQuery is the HTTP method this answers. net/http has no constant for
// it: QUERY reached RFC in June 2026 and Go has not adopted a name, so the
// string is written once here rather than at each comparison.
const methodQuery = "QUERY"

// maxQueryRanges caps how many spans one request may name. It is the frame cap
// rather than a number of its own: more ranges than that cannot resolve to
// distinct chunks anyway, so a body carrying more is a client that has not
// understood the endpoint rather than one with a large read to do.
//
// Checked before anything is resolved. Resolving a hundred thousand spans to
// discover they were refusable is the work the cap exists to bound.
const maxQueryRanges = maxFetchChunks

// maxRangeBody bounds the request. Sized from the cap the way maxFetchBody is:
// two decimal int64s, brackets and a comma run to about 45 bytes, plus slack
// for the envelope. decodeJSONBody decodes the whole body before the count
// check can refuse it, so borrowing a larger limit would mean parsing
// megabytes on the way to a 400.
const maxRangeBody = maxQueryRanges*48 + 1024

// rangeStreamMediaType names what this answers with. It is the chunk stream's
// framing exactly — store.DecodeChunkFrames reads it, and the frames verify
// against their ids as they always do — under a second name, because the
// contract differs: here the first frame is the file's manifest and the rest
// are its chunks, where application/vnd.silo.chunks promises chunks only.
//
// A second name rather than a widened one, for the reason a feature name is
// never reused: a client built for chunks/fetch would still parse this, and
// would file the manifest away as a chunk. It is not a corruption — a manifest
// id is the same SHA-256 over the same kind of bytes, so it would be stored
// correctly under a correct id — but it is a thing the client did not mean to
// do, and the media type is the only place to say so before it happens.
const rangeStreamMediaType = "application/vnd.silo.ranges"

// queryEntry answers a QUERY on entries/{path}: the manifest, then the chunks
// covering the ranges the body asked for.
//
// Read permission on the path, like the GET beside it — which is the whole
// point of the route being path-addressed. The ordering here is deliberate:
// the body is validated before the tree is walked, so a malformed request
// costs no I/O and does not disclose whether a path exists to a caller who
// could not have read it anyway.
func queryEntry(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	libraryID := vars["libraryid"]
	path := entryPath(vars["path"])

	library := entryLibrary(w, r, libraryID, path, false)
	if library == nil {
		return
	}

	var body struct {
		Ranges [][]int64 `json:"ranges"`
	}
	if !decodeJSONBody(w, r, maxRangeBody, &body,
		`Expected a JSON body such as {"ranges":[[<offset>,<length>],…]}`) {
		return
	}
	ranges, ok := parseByteRanges(w, body.Ranges)
	if !ok {
		return
	}

	// ?at= reads an old tree, exactly as it does on the GET. A point-in-time
	// read costs nothing extra here for the same reason it costs nothing
	// there: an old root id names a tree as immutable as the current one, and
	// the manifest it resolves to is as complete.
	root, ok := rootFor(w, r, library)
	if !ok {
		return
	}
	entry, err := resolveUnder(library, root, path)
	if err != nil {
		resolveErr(w, err, "Not found")
		return
	}
	// A directory is a real entry with no manifest, so this is the wrong
	// question rather than a missing path — the distinction serveManifest
	// already draws, and for the same reason: 404 would be a claim about the
	// library rather than about the request.
	if entry.isDir {
		http.Error(w, "Not a file; a directory has no manifest", http.StatusBadRequest)
		return
	}

	st, id, ok := storeAndID(w, r, library, entry.id)
	if !ok {
		return
	}

	// The manifest is held whole, so it is charged to the object budget before
	// it is read — the same accounting serveFile does, and the size is a stat
	// rather than a read, so nothing is held to find out. The chunks behind it
	// are not charged: streamChunks reads them one at a time into a buffer it
	// reuses.
	size, err := st.ObjectSize(id)
	if err != nil {
		objectReadError(w, r, err, "manifest", id)
		return
	}
	release, ok := holdForObject(w, size)
	if !ok {
		return
	}
	defer release()

	// The encoded object rather than a decoded one, because the encoding is
	// what goes on the wire: the frame's id is the hash of these exact bytes,
	// so re-encoding a decoded manifest would be a second expression of the
	// format with its own chance to differ. It is decoded beside that only to
	// do the arithmetic.
	encoded, err := st.GetObject(id)
	if err != nil {
		objectReadError(w, r, err, "manifest", id)
		return
	}
	m, err := store.DecodeManifest(encoded)
	if err != nil {
		// The object resolved from the tree as a file and does not parse as a
		// manifest. That is this server's problem, not the caller's.
		log.WithContext(r.Context()).WithError(err).
			Errorf("manifest %s in library %s does not decode", id, library.ID)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	covering, ok := m.ChunksCovering(ranges, maxFetchChunks)
	if !ok {
		http.Error(w, fmt.Sprintf(
			"Those ranges touch more than %d of the file's chunks; read less at a time",
			maxFetchChunks), http.StatusBadRequest)
		return
	}
	// Distinct ids, in the order the file lists them. A run of zeroes names one
	// chunk at many offsets, and frames are matched by id rather than by
	// position, so sending those bytes once per mention would spend exactly
	// the bandwidth this endpoint exists to save — the same dedup
	// parseChunkIDs does for the ids a client names itself.
	ids := make([]store.ID, 0, len(covering))
	seen := make(map[store.ID]struct{}, len(covering))
	for _, i := range covering {
		cid := m.Chunks[i].ID
		if _, dup := seen[cid]; dup {
			continue
		}
		seen[cid] = struct{}{}
		ids = append(ids, cid)
	}

	// The manifest leads, and it is an ordinary present frame carrying its own
	// id — which is the id the client did not have and could not have asked
	// with. Everything after it is matched by id in the usual way.
	//
	// Framed BEFORE the status is written, which is the whole reason this is
	// not one line further down. A manifest may legally be up to 1 GiB
	// (store.MaxManifestBytes) and a frame may carry 64 MiB
	// (store.MaxChunkFrameBytes), and at some 67 bytes per chunk the gap opens
	// at around a terabyte of file — reachable, not hypothetical. Discovering
	// that after a 200 has gone out would end the body without a terminator,
	// which a client reads as a truncated response and retries, forever.
	frame, err := store.AppendChunkFrame(nil, id, encoded)
	if err != nil {
		// 501 rather than 500: nothing is broken and a retry will not help.
		// This route cannot express this file, and the answer is to name the
		// one that can — the same shape as the refusal to share an E2EE
		// library, which is the other place this surface says "not here".
		log.WithContext(r.Context()).WithError(err).
			Warnf("manifest %s in library %s is too large to frame", id, library.ID)
		http.Error(w, fmt.Sprintf(
			"This file's manifest is %d bytes, past the %d a frame can carry; "+
				"read it with GET entries/{path}?type=manifest, which streams, and fetch its chunks with POST chunks/fetch",
			len(encoded), store.MaxChunkFrameBytes), http.StatusNotImplemented)
		return
	}

	// No Content-Length, for streamChunks's reason: the body is written as it
	// is read, and what makes a short one legible is the terminator at the end
	// rather than a length up front.
	w.Header().Set("Content-Type", rangeStreamMediaType)
	w.WriteHeader(http.StatusOK)

	if _, err := w.Write(frame); err != nil {
		return
	}
	// Flushed before the chunks so the client can start on the manifest while
	// the first chunk is still being read off disk. That is the round trip
	// this endpoint removed, given back as overlap rather than as a wait.
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}

	streamChunks(w, r, st, library.ID, ids)
}

// parseByteRanges validates the body's spans, answering the request itself and
// reporting false when it will not serve them.
//
// The wire is stricter than store.ChunksCovering, which clamps everything it
// is given: a length of zero or below is refused here rather than resolved to
// nothing, because a client that computed one has a bug and an empty answer is
// how that bug reaches production. Running off the END of the file is a
// different case and is clamped, because the client's idea of the length can
// be honestly stale — and the manifest in the same response is what corrects
// it, which is why that is not a 416.
func parseByteRanges(w http.ResponseWriter, raw [][]int64) ([]store.ByteRange, bool) {
	if len(raw) == 0 {
		http.Error(w, "Expected at least one range; "+
			"GET entries/{path}?type=manifest is the call for a manifest on its own",
			http.StatusBadRequest)
		return nil, false
	}
	if len(raw) > maxQueryRanges {
		http.Error(w, fmt.Sprintf("Asked for %d ranges; at most %d may be read in one request",
			len(raw), maxQueryRanges), http.StatusBadRequest)
		return nil, false
	}
	ranges := make([]store.ByteRange, 0, len(raw))
	for _, pair := range raw {
		if len(pair) != 2 {
			http.Error(w, fmt.Sprintf("A range is [offset, length]; got %d values", len(pair)),
				http.StatusBadRequest)
			return nil, false
		}
		off, length := pair[0], pair[1]
		if off < 0 {
			http.Error(w, fmt.Sprintf("Range offset %d is negative", off), http.StatusBadRequest)
			return nil, false
		}
		if length <= 0 {
			http.Error(w, fmt.Sprintf("Range length %d asks for nothing; a length must be positive", length),
				http.StatusBadRequest)
			return nil, false
		}
		ranges = append(ranges, store.ByteRange{Offset: off, Length: length})
	}
	return ranges, true
}
