package store

import (
	"bytes"
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
	full := buf.Bytes()
	if _, err := DecodeChunkFrames(full[:len(full)-1]); err == nil {
		t.Error("a truncated frame decoded without error")
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
