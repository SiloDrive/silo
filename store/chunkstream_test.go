package store

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

// The framing round-trips, including the absent case, which is the whole
// reason a frame carries a status byte rather than being inferred from a
// length of zero.
func TestChunkFrameRoundTrip(t *testing.T) {
	present := []byte("chunk bytes")
	presentID := ChunkID(present)
	absentID := ChunkID([]byte("nobody has this"))

	var buf bytes.Buffer
	if err := WriteChunkFrame(&buf, presentID, present); err != nil {
		t.Fatalf("WriteChunkFrame: %v", err)
	}
	if err := WriteAbsentChunkFrame(&buf, absentID); err != nil {
		t.Fatalf("WriteAbsentChunkFrame: %v", err)
	}
	if err := WriteChunkStreamEnd(&buf); err != nil {
		t.Fatalf("WriteChunkStreamEnd: %v", err)
	}

	got, err := DecodeChunkFrames(buf.Bytes())
	if err != nil {
		t.Fatalf("DecodeChunkFrames: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("decoded %d frames, want 2", len(got))
	}
	if got[0].ID != presentID || !got[0].Present || !bytes.Equal(got[0].Bytes, present) {
		t.Errorf("first frame = %+v, want the present chunk", got[0])
	}
	if got[1].ID != absentID || got[1].Present || len(got[1].Bytes) != 0 {
		t.Errorf("second frame = %+v, want the absent marker", got[1])
	}
}

// A frame whose declared length runs past the buffer is refused rather than
// read short. A decoder that trusts the length is how a truncated response
// becomes a chunk that hashes to nothing and a bug three layers away.
func TestATruncatedChunkFrameIsRefused(t *testing.T) {
	var buf bytes.Buffer
	body := []byte("some bytes")
	if err := WriteChunkFrame(&buf, ChunkID(body), body); err != nil {
		t.Fatal(err)
	}
	if err := WriteChunkStreamEnd(&buf); err != nil {
		t.Fatal(err)
	}
	full := buf.Bytes()
	if _, err := DecodeChunkFrames(full[:len(full)-1]); err == nil {
		t.Error("a truncated frame decoded without error")
	}
}

// The failure the terminator exists for: a server that dies part way through
// writes whole frames and stops, so the body is well-formed and short. Without
// an explicit end marker that decodes cleanly, under a 200 committed before the
// first byte moved — a client asking for many chunks and receiving a few would
// see success.
func TestAStreamCutBetweenFramesIsReportedAsTruncated(t *testing.T) {
	var buf bytes.Buffer
	for _, body := range [][]byte{[]byte("first"), []byte("second")} {
		if err := WriteChunkFrame(&buf, ChunkID(body), body); err != nil {
			t.Fatal(err)
		}
	}
	// Every frame complete, terminator never written.
	got, err := DecodeChunkFrames(buf.Bytes())
	if !errors.Is(err, ErrChunkStreamTruncated) {
		t.Fatalf("err = %v, want ErrChunkStreamTruncated", err)
	}
	// The frames that did arrive are still usable, which is what lets a caller
	// keep them and re-ask only for the rest.
	if len(got) != 2 {
		t.Errorf("got %d frames back alongside the error, want the 2 that arrived", len(got))
	}
}

// Anything after the terminator means the reader and the writer disagree about
// where the response ended.
func TestBytesAfterTheTerminatorAreRefused(t *testing.T) {
	var buf bytes.Buffer
	body := []byte("only chunk")
	if err := WriteChunkFrame(&buf, ChunkID(body), body); err != nil {
		t.Fatal(err)
	}
	if err := WriteChunkStreamEnd(&buf); err != nil {
		t.Fatal(err)
	}
	if err := WriteChunkFrame(&buf, ChunkID(body), body); err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeChunkFrames(buf.Bytes()); err == nil {
		t.Error("a frame after the terminator decoded without error")
	}
}

// ReadChunkFrame is the same verification over a stream, for a client that will
// not buffer a gigabyte to use a format built not to need it.
func TestReadChunkFrameStreamsAndVerifies(t *testing.T) {
	var buf bytes.Buffer
	present := []byte("streamed bytes")
	presentID := ChunkID(present)
	absentID := ChunkID([]byte("not held"))
	if err := WriteChunkFrame(&buf, presentID, present); err != nil {
		t.Fatal(err)
	}
	if err := WriteAbsentChunkFrame(&buf, absentID); err != nil {
		t.Fatal(err)
	}
	if err := WriteChunkStreamEnd(&buf); err != nil {
		t.Fatal(err)
	}

	var scratch []byte
	var got []ChunkFrame
	for {
		f, err := ReadChunkFrame(&buf, scratch)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("ReadChunkFrame: %v", err)
		}
		scratch = f.Bytes[:cap(f.Bytes)]
		got = append(got, f)
	}
	if len(got) != 2 {
		t.Fatalf("read %d frames, want 2", len(got))
	}
	if got[0].ID != presentID || !got[0].Present || !bytes.Equal(got[0].Bytes, present) {
		t.Error("the first frame is not the present chunk")
	}
	if got[1].ID != absentID || got[1].Present {
		t.Error("the second frame is not the absent marker")
	}
}

// The streaming reader must not report a clean EOF as a clean end: a complete
// stream always ends with a frame, so running out of bytes is truncation.
func TestReadChunkFrameTreatsAMissingTerminatorAsTruncation(t *testing.T) {
	var buf bytes.Buffer
	body := []byte("one chunk, no end")
	if err := WriteChunkFrame(&buf, ChunkID(body), body); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadChunkFrame(&buf, nil); err != nil {
		t.Fatalf("first frame: %v", err)
	}
	if _, err := ReadChunkFrame(&buf, nil); !errors.Is(err, ErrChunkStreamTruncated) {
		t.Errorf("err = %v, want ErrChunkStreamTruncated", err)
	}
}

// The id is not taken on trust in this direction either: the decoder is
// where a client learns that what arrived is not what it asked for.
func TestAChunkFrameWhoseBytesDoNotMatchItsIDIsRefused(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteChunkFrame(&buf, ChunkID([]byte("one thing")), []byte("another")); err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeChunkFrames(buf.Bytes()); err == nil {
		t.Error("a frame whose bytes do not hash to its id decoded without error")
	}
}
