package store

import (
	"encoding/binary"
	"fmt"
	"io"
)

// The chunk stream: many chunks in one response body, each one named.
//
// It exists because the chunk surface was built one-directional. A client has
// been able to ask "which of these do you hold?" and send only the answer
// since 0.4.5; reading them back was one request per chunk, so a client
// holding a previous version of a 1 GiB file uploaded a few chunks and
// downloaded a thousand times to build the next one. This is the response
// shape that closes that, and the same framing is what a batched upload would
// take.
//
// The framing is in this package rather than in the server for the reason the
// chunker and the manifest codec are: `store` is the dependency-free module
// both sides build from, so there is exactly one expression of the format to
// be compatible with. A JSON envelope with base64 bodies would have been
// easier to write and would have been a second way to name the same bytes —
// the mistake the write path already refuses when it takes chunk ids from
// ChunkID rather than from a local sha256-and-hex.
//
// One frame:
//
//	id      32 bytes   the chunk's id, so a frame is self-describing
//	status   1 byte    0 present, 1 absent
//	length   4 bytes   big-endian, the chunk's stored length; 0 when absent
//	bytes    length    the chunk exactly as stored
//
// Three properties are worth naming because each is a decision rather than a
// consequence.
//
// **The id leads.** A reader matches frames by id rather than by position, so
// the server may drop a repeat, and a client may lay chunks down as they
// arrive without holding the request list open beside the response.
//
// **Absence is a status byte, not a zero length.** A zero-length chunk cannot
// occur today — a file small enough to have no bytes inlines instead — but
// inferring "absent" from a length is a rule that holds until the day it does
// not, and it fails silently when it breaks.
//
// **The length is fixed-width, not a varint.** Everything else in this format
// that counts uses a varint; this does not, because a frame header wants to be
// a fixed offset a port can read without a loop, and a chunk is bounded by the
// library's max_size anyway. The saving would be two bytes per megabyte.
const (
	// ChunkFrameHeaderSize is id + status + length.
	ChunkFrameHeaderSize = IDSize + 1 + 4

	chunkFramePresent byte = 0
	chunkFrameAbsent  byte = 1
)

// MaxChunkFrameBytes bounds one frame's payload. It is far above any chunker's
// max_size — the largest this format's parameters admit is 4 MiB — and exists
// so a decoder sizes an allocation from a bound rather than from the number a
// stranger sent it.
const MaxChunkFrameBytes = 64 << 20

// ChunkFrame is one decoded frame.
type ChunkFrame struct {
	ID ID
	// Present is false when the server does not hold this chunk. Bytes is
	// then empty, and the caller has learned it without a second request.
	Present bool
	Bytes   []byte
}

// WriteChunkFrame writes one present chunk. The caller has the bytes in hand,
// so the frame is written whole and a torn frame is not a state this can
// reach — which is why the server reads a chunk fully before starting to
// write it rather than streaming through.
func WriteChunkFrame(w io.Writer, id ID, chunk []byte) error {
	if len(chunk) > MaxChunkFrameBytes {
		return fmt.Errorf("store: chunk %s is %d bytes, over the %d frame limit", id, len(chunk), MaxChunkFrameBytes)
	}
	var hdr [ChunkFrameHeaderSize]byte
	copy(hdr[:IDSize], id[:])
	hdr[IDSize] = chunkFramePresent
	binary.BigEndian.PutUint32(hdr[IDSize+1:], uint32(len(chunk)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err := w.Write(chunk)
	return err
}

// WriteAbsentChunkFrame records that the store does not hold this id.
func WriteAbsentChunkFrame(w io.Writer, id ID) error {
	var hdr [ChunkFrameHeaderSize]byte
	copy(hdr[:IDSize], id[:])
	hdr[IDSize] = chunkFrameAbsent
	_, err := w.Write(hdr[:])
	return err
}

// DecodeChunkFrames reads a whole chunk stream, verifying every present frame
// against the id it arrived under.
//
// The verification is the point rather than a nicety. A chunk fetched by id is
// exactly as trustworthy as the hash check on arrival, and that check is what
// makes a source other than this server — a cache of uncertain provenance
// after an unclean shutdown, a peer, a mirror — legal at all. A decoder that
// skipped it would hand the caller bytes it had no reason to believe in.
//
// It decodes from a slice because a caller with the whole body is the common
// case and the simplest thing to get right. A client streaming a very large
// response reads ChunkFrameHeaderSize bytes, then the payload, and does the
// same check itself; the header is fixed-width so that loop is four lines.
func DecodeChunkFrames(b []byte) ([]ChunkFrame, error) {
	var frames []ChunkFrame
	for p := 0; p < len(b); {
		if len(b)-p < ChunkFrameHeaderSize {
			return nil, fmt.Errorf("store: chunk stream ends mid-header, %d bytes short", ChunkFrameHeaderSize-(len(b)-p))
		}
		var f ChunkFrame
		copy(f.ID[:], b[p:p+IDSize])
		status := b[p+IDSize]
		length := binary.BigEndian.Uint32(b[p+IDSize+1:])
		p += ChunkFrameHeaderSize

		switch status {
		case chunkFrameAbsent:
			if length != 0 {
				return nil, fmt.Errorf("store: chunk %s is marked absent and declares %d bytes", f.ID, length)
			}
			frames = append(frames, f)
			continue
		case chunkFramePresent:
		default:
			return nil, fmt.Errorf("store: chunk %s has unknown frame status %d", f.ID, status)
		}

		if length > MaxChunkFrameBytes {
			return nil, fmt.Errorf("store: chunk %s declares %d bytes, over the %d limit", f.ID, length, MaxChunkFrameBytes)
		}
		if uint64(len(b)-p) < uint64(length) {
			return nil, fmt.Errorf("store: chunk %s declares %d bytes and %d remain", f.ID, length, len(b)-p)
		}
		f.Bytes = b[p : p+int(length)]
		p += int(length)

		if got := ChunkID(f.Bytes); got != f.ID {
			return nil, fmt.Errorf("store: chunk offered as %s hashes to %s", f.ID, got)
		}
		f.Present = true
		frames = append(frames, f)
	}
	return frames, nil
}
