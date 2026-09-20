package client

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/SiloDrive/silo/store"
)

// Sending many chunks in one request.
//
// The per-chunk PUT is a round trip each way for a megabyte of payload, and at
// the chunker's 1 MiB target a 1 GB file is about a thousand of them. On a link
// with any latency at all that is the dominant cost of an upload — the bytes
// are not the problem, the waiting is. This sends them in one framed body
// instead, which is the same framing chunks/fetch answers with.
//
// It is a straight optimisation with a fallback: a server that does not
// advertise `chunks-upload` still gets the one-at-a-time loop, because the
// client ships separately from the server it is pointed at.

// Batches are bounded at both ends. The server caps a request at 256 frames
// and 256 MiB (fileserver/chunks_upload.go); these are smaller on purpose.
//
// A batch is the unit of progress, not the unit of durability — chunks that
// landed before a failure stay landed, and the retry asks chunks/missing and
// gets a shorter list. So the size trades round trips against how much a
// failure has to re-ask for, and against how coarse the progress a caller sees
// becomes. 64 frames at the 1 MiB target is a 64 MiB request and a sixteenth
// of the round trips on a 1 GB file, which is most of the win with none of the
// bluntness of the server's own ceiling.
const (
	maxChunksPerUpload = 64
	maxUploadBatchSize = 64 << 20
)

// uploadResult is what the server reports about one framed request.
type uploadResult struct {
	// Stored is what it did not already have. Present is what it did — not an
	// error, and worth reading: it is a chunks/missing answer that went stale
	// under this client, which happens whenever something else uploaded the
	// same content in between.
	Stored  int `json:"stored"`
	Present int `json:"present"`
}

// PutChunks uploads several chunks in one request and reports what landed.
//
// The ids are named by the caller and verified by the server against the bytes
// that arrive under them, so a successful call is proof the content crossed
// intact — the same end-to-end check the single PUT gives, once for the batch.
func (c *APIClient) PutChunks(libraryID string, ids []string, sources map[string]chunkSource) (uploadResult, error) {
	var frames []framedChunk
	var size int64
	for _, id := range ids {
		src, ok := sources[id]
		if !ok {
			return uploadResult{}, fmt.Errorf("chunk %.8s is not part of this upload", id)
		}
		parsed, err := store.ParseID(id)
		if err != nil {
			return uploadResult{}, fmt.Errorf("chunk %.8s: %w", id, err)
		}
		frames = append(frames, framedChunk{id: parsed, src: src})
		size += int64(store.ChunkFrameHeaderSize) + src.size
	}
	// The terminator, which is what tells the server the body arrived whole.
	size += int64(store.ChunkFrameHeaderSize)

	resp, err := c.doStream("POST", "/api/silo/v1/libraries/"+libraryID+"/chunks",
		chunkStreamMediaType, func() (io.ReadCloser, int64, error) {
			// A factory, not a reader: doStream re-sends the body after a 401,
			// and a stream already drained cannot be sent twice.
			return &framedUpload{frames: frames}, size, nil
		})
	if err != nil {
		return uploadResult{}, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		msg, _ := io.ReadAll(resp.Body)
		return uploadResult{}, fmt.Errorf("uploading %d chunks: %s: %s", len(frames), resp.Status, string(msg))
	}
	var out uploadResult
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return uploadResult{}, fmt.Errorf("uploading %d chunks: %w", len(frames), err)
	}
	if out.Stored+out.Present != len(frames) {
		// The counts are the receipt. A server that accepted the body and
		// accounted for fewer chunks than it contains has done something this
		// client cannot reason about, and continuing would build an entry out
		// of ids that may not all be there.
		return out, fmt.Errorf("sent %d chunks and the server accounted for %d",
			len(frames), out.Stored+out.Present)
	}
	return out, nil
}

// chunkStreamMediaType names the framing. The same constant as the server's,
// spelled here because `store` holds the format and neither side imports the
// other.
const chunkStreamMediaType = "application/vnd.silo.chunks"

// framedChunk is one chunk to send: its id, and where its bytes are.
type framedChunk struct {
	id  store.ID
	src chunkSource
}

// framedUpload is the request body: chunk frames, produced one at a time.
//
// Lazy rather than assembled, so a batch costs one chunk of memory however
// many it carries — the point of batching is round trips, and buying them with
// a 64 MiB buffer would be a poor trade. The length is still known exactly in
// advance, because a frame's size is its header plus its bytes and both are
// known before anything is read, which is what lets the request declare a
// Content-Length and the server refuse an over-quota upload before it starts.
type framedUpload struct {
	frames []framedChunk
	next   int
	buf    []byte // the current frame, fully formed
	off    int    // how much of it has been read
	ended  bool   // the terminator has been produced
}

func (f *framedUpload) Read(p []byte) (int, error) {
	for f.off >= len(f.buf) {
		if f.ended {
			return 0, io.EOF
		}
		if f.next == len(f.frames) {
			f.buf, f.off, f.ended = store.AppendChunkStreamEnd(f.buf[:0]), 0, true
			continue
		}
		frame := f.frames[f.next]
		data, err := readChunkSource(frame.src)
		if err != nil {
			return 0, err
		}
		f.buf, err = store.AppendChunkFrame(f.buf[:0], frame.id, data)
		if err != nil {
			return 0, err
		}
		f.off, f.next = 0, f.next+1
	}
	n := copy(p, f.buf[f.off:])
	f.off += n
	return n, nil
}

// Close is here because the body is an io.ReadCloser. There is nothing to
// close: every file this opened was closed by the read that opened it.
func (f *framedUpload) Close() error { return nil }

// readChunkSource reads one chunk's bytes out of the file it came from.
func readChunkSource(src chunkSource) ([]byte, error) {
	if src.data != nil {
		return src.data, nil
	}
	file, err := os.Open(src.local)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	buf := make([]byte, src.size)
	if _, err := file.ReadAt(buf, src.off); err != nil {
		return nil, fmt.Errorf("reading %s at %d: %w", src.local, src.off, err)
	}
	return buf, nil
}

// sendChunks uploads every named chunk, batched when the server can take them
// that way and one at a time when it cannot, and reports how many chunks and
// bytes went up.
//
// This is the one place either upload path decides how content crosses the
// wire, which is why both call it: uploadChunks had the loop and
// uploadTreeBatched had a copy of it, and a batching rule in two places is a
// batching rule that will differ in two places.
func (c *APIClient) sendChunks(libraryID string, missing []string, sources map[string]chunkSource,
	onSent func(chunks int, bytes int64),
) error {
	if !c.capabilities().Has("chunks-upload") {
		for _, id := range missing {
			sent, err := c.putChunkFrom(libraryID, id, sources[id])
			if err != nil {
				return err
			}
			onSent(1, sent)
		}
		return nil
	}

	for start := 0; start < len(missing); {
		end, batchBytes := start, int64(0)
		// Grown one chunk at a time against both limits, and always by at
		// least one: a single chunk larger than the size budget still has to
		// be sent, and the server's own ceiling is far above any one chunk.
		for end < len(missing) && end-start < maxChunksPerUpload {
			src, ok := sources[missing[end]]
			if !ok {
				return fmt.Errorf("server asked for chunk %.8s, which is not part of this upload", missing[end])
			}
			if end > start && batchBytes+src.size > maxUploadBatchSize {
				break
			}
			batchBytes += src.size
			end++
		}
		if _, err := c.PutChunks(libraryID, missing[start:end], sources); err != nil {
			return err
		}
		onSent(end-start, batchBytes)
		start = end
	}
	return nil
}
