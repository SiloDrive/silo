package store

import (
	"encoding/binary"
	"errors"
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
	// chunkFrameEnd terminates a stream. Its id is zero and its length is
	// zero; only its presence carries meaning.
	//
	// It exists because a response body is not self-delimiting without it. A
	// server that fails half way through writes whole frames and then stops,
	// so the body is K complete frames — which decodes cleanly, under a 200
	// that was committed before the first byte moved. A client asking for 256
	// chunks and receiving 3 would see success. There is no way to retract the
	// status once streaming has begun, so completeness has to be something the
	// body states rather than something its length implies.
	chunkFrameEnd byte = 2
)

// MaxChunkFrameBytes bounds one frame's payload. It is far above any chunker's
// max_size — the largest this format's parameters admit is 4 MiB.
//
// It is a policy cap, and on a 32-bit build it is also a safety one: length is
// a uint32, int is 32 bits signed there, and a declared length above MaxInt32
// would convert to a negative slice bound. Checking it before the conversion is
// what makes the conversion safe, so the check is not redundant with the
// remaining-bytes check below it even though that one looks stricter.
const MaxChunkFrameBytes = 64 << 20

// ChunkFrame is one decoded frame.
type ChunkFrame struct {
	ID ID
	// Present is false when the server does not hold this chunk. Bytes is
	// then empty, and the caller has learned it without a second request.
	Present bool
	Bytes   []byte
}

// AppendChunkFrame appends one present chunk's frame to dst.
//
// Appending rather than writing is what lets a caller emit a frame in a single
// Write. Two Writes per frame costs more than it looks: the ResponseWriter
// buffers before chunking, so a 37-byte header followed by a megabyte payload
// leaves as two chunked-transfer records and two syscalls, and at 256 chunks a
// response that is one flush per chunk becomes two.
func AppendChunkFrame(dst []byte, id ID, chunk []byte) ([]byte, error) {
	if len(chunk) > MaxChunkFrameBytes {
		return dst, fmt.Errorf("store: chunk %s is %d bytes, over the %d frame limit", id, len(chunk), MaxChunkFrameBytes)
	}
	dst = append(dst, id[:]...)
	dst = append(dst, chunkFramePresent)
	dst = binary.BigEndian.AppendUint32(dst, uint32(len(chunk)))
	return append(dst, chunk...), nil
}

// AppendAbsentChunkFrame records that the store does not hold this id.
func AppendAbsentChunkFrame(dst []byte, id ID) []byte {
	dst = append(dst, id[:]...)
	dst = append(dst, chunkFrameAbsent)
	return binary.BigEndian.AppendUint32(dst, 0)
}

// AppendChunkStreamEnd closes a stream. Every complete response ends with it,
// and a reader that does not find it has been cut off.
func AppendChunkStreamEnd(dst []byte) []byte {
	var zero ID
	dst = append(dst, zero[:]...)
	dst = append(dst, chunkFrameEnd)
	return binary.BigEndian.AppendUint32(dst, 0)
}

// WriteChunkFrame writes one present chunk.
//
// The caller has the bytes in hand, so the frame is written whole and a torn
// frame is not a state this can reach — which is why the server reads a chunk
// fully before starting to write it rather than streaming through.
func WriteChunkFrame(w io.Writer, id ID, chunk []byte) error {
	buf, err := AppendChunkFrame(make([]byte, 0, ChunkFrameHeaderSize+len(chunk)), id, chunk)
	if err != nil {
		return err
	}
	_, err = w.Write(buf)
	return err
}

// WriteAbsentChunkFrame records that the store does not hold this id.
func WriteAbsentChunkFrame(w io.Writer, id ID) error {
	_, err := w.Write(AppendAbsentChunkFrame(nil, id))
	return err
}

// WriteChunkStreamEnd closes a stream.
func WriteChunkStreamEnd(w io.Writer) error {
	_, err := w.Write(AppendChunkStreamEnd(nil))
	return err
}

// ErrChunkStreamTruncated reports a stream that ended without its terminator.
//
// It is a named error because it is the one failure a caller must not treat as
// an empty answer: the frames that did arrive are good and can be kept, and the
// ids that did not are the ones to ask for again.
var ErrChunkStreamTruncated = errors.New("store: chunk stream ended without its terminator")

// DecodeChunkFrames reads a whole chunk stream, verifying every present frame
// against the id it arrived under.
//
// The verification is the point rather than a nicety. A chunk fetched by id is
// exactly as trustworthy as the hash check on arrival, and that check is what
// makes a source other than this server — a cache of uncertain provenance
// after an unclean shutdown, a peer, a mirror — legal at all. A decoder that
// skipped it would hand the caller bytes it had no reason to believe in.
//
// Returned frames ALIAS b rather than copying it. That is the right trade for
// throughput and the wrong one to be surprised by: retaining one 1 MiB frame
// keeps the whole response body reachable, which at this endpoint's cap is up
// to a gigabyte. Copy what you keep past the response.
//
// It decodes from a slice because a caller holding the whole body is the
// simplest thing to get right. A caller that does not want to hold a whole
// response buffers nothing and calls ReadChunkFrame instead, which is the same
// verification over an io.Reader.
func DecodeChunkFrames(b []byte) ([]ChunkFrame, error) {
	// The header is fixed-width, so the frame count is bounded by the body
	// length and one allocation replaces the nine that growing to 256 costs.
	frames := make([]ChunkFrame, 0, len(b)/ChunkFrameHeaderSize+1)
	for p := 0; p < len(b); {
		if len(b)-p < ChunkFrameHeaderSize {
			return frames, fmt.Errorf("%w: %d bytes short of a header", ErrChunkStreamTruncated, ChunkFrameHeaderSize-(len(b)-p))
		}
		var f ChunkFrame
		copy(f.ID[:], b[p:p+IDSize])
		status := b[p+IDSize]
		length := binary.BigEndian.Uint32(b[p+IDSize+1:])
		p += ChunkFrameHeaderSize

		switch status {
		case chunkFrameEnd:
			if length != 0 {
				return frames, fmt.Errorf("store: the terminator declares %d bytes", length)
			}
			if p != len(b) {
				return frames, fmt.Errorf("store: %d bytes follow the terminator", len(b)-p)
			}
			return frames, nil
		case chunkFrameAbsent:
			if length != 0 {
				return frames, fmt.Errorf("store: chunk %s is marked absent and declares %d bytes", f.ID, length)
			}
			frames = append(frames, f)
			continue
		case chunkFramePresent:
		default:
			return frames, fmt.Errorf("store: chunk %s has unknown frame status %d", f.ID, status)
		}

		if length > MaxChunkFrameBytes {
			return frames, fmt.Errorf("store: chunk %s declares %d bytes, over the %d limit", f.ID, length, MaxChunkFrameBytes)
		}
		if uint64(len(b)-p) < uint64(length) {
			return frames, fmt.Errorf("%w: chunk %s declares %d bytes and %d remain", ErrChunkStreamTruncated, f.ID, length, len(b)-p)
		}
		f.Bytes = b[p : p+int(length)]
		p += int(length)

		if got := ChunkID(f.Bytes); got != f.ID {
			return frames, fmt.Errorf("store: chunk offered as %s hashes to %s", f.ID, got)
		}
		f.Present = true
		frames = append(frames, f)
	}
	// Ran out of body without meeting the terminator.
	return frames, ErrChunkStreamTruncated
}

// ReadChunkFrame reads one frame from r, verifying a present frame against its
// id, and reports io.EOF only once the terminator has been consumed.
//
// It exists because the endpoint this format serves streams: the server flushes
// per chunk so a client can lay one down while the next is still being read, and
// a client that can only call DecodeChunkFrames has to buffer a whole response —
// up to a gigabyte — to use a format designed not to need that. Handing the
// streaming path to callers in a comment meant every port would re-derive the
// hash check by hand, and that check is the point rather than a nicety.
//
// scratch is reused for the payload when it is large enough; pass the frame's
// Bytes back in on the next call to read a whole response with one buffer. The
// returned frame aliases scratch, so copy what you keep.
func ReadChunkFrame(r io.Reader, scratch []byte) (ChunkFrame, error) {
	var hdr [ChunkFrameHeaderSize]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		if errors.Is(err, io.EOF) {
			// A clean EOF here is still truncation: the terminator is a frame,
			// so a complete stream never ends by simply running out.
			return ChunkFrame{}, ErrChunkStreamTruncated
		}
		return ChunkFrame{}, err
	}
	var f ChunkFrame
	copy(f.ID[:], hdr[:IDSize])
	status := hdr[IDSize]
	length := binary.BigEndian.Uint32(hdr[IDSize+1:])

	switch status {
	case chunkFrameEnd:
		if length != 0 {
			return ChunkFrame{}, fmt.Errorf("store: the terminator declares %d bytes", length)
		}
		return ChunkFrame{}, io.EOF
	case chunkFrameAbsent:
		if length != 0 {
			return ChunkFrame{}, fmt.Errorf("store: chunk %s is marked absent and declares %d bytes", f.ID, length)
		}
		return f, nil
	case chunkFramePresent:
	default:
		return ChunkFrame{}, fmt.Errorf("store: chunk %s has unknown frame status %d", f.ID, status)
	}

	if length > MaxChunkFrameBytes {
		return ChunkFrame{}, fmt.Errorf("store: chunk %s declares %d bytes, over the %d limit", f.ID, length, MaxChunkFrameBytes)
	}
	if uint32(cap(scratch)) < length {
		scratch = make([]byte, length)
	}
	f.Bytes = scratch[:length]
	if _, err := io.ReadFull(r, f.Bytes); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return ChunkFrame{}, fmt.Errorf("%w: chunk %s ended early", ErrChunkStreamTruncated, f.ID)
		}
		return ChunkFrame{}, err
	}
	if got := ChunkID(f.Bytes); got != f.ID {
		return ChunkFrame{}, fmt.Errorf("store: chunk offered as %s hashes to %s", f.ID, got)
	}
	f.Present = true
	return f, nil
}
