package client

import (
	"testing"
)

// What batching is for: the round trips, not the bytes. These count requests
// rather than inspect content, because the number of requests IS the feature —
// a 1 GB file at the chunker's 1 MiB target was about a thousand PUTs, and the
// bytes it moved were never the complaint.

func TestATreeUploadSendsItsChunksInOneRequest(t *testing.T) {
	ts, c := newTreeServer(t, "batch", "chunks", "chunks-upload")
	root := sampleTree(t)

	result, err := c.UploadDir("r1", "/", root, nil)
	if err != nil {
		t.Fatal(err)
	}

	if len(ts.uploads) != 1 {
		t.Fatalf("%d framed uploads, want 1 — the point of the surface is that they travel together", len(ts.uploads))
	}
	// Two distinct chunks: a.txt and dup.txt hold the same content, so the
	// tree names it once. b.txt is the other.
	if len(ts.uploads[0]) != 2 {
		t.Errorf("the request carried %d chunks, want 2", len(ts.uploads[0]))
	}
	if len(ts.chunksPut) != 0 {
		t.Errorf("%d chunks went up one at a time as well: %v", len(ts.chunksPut), ts.chunksPut)
	}
	if result.ChunksSent != 2 {
		t.Errorf("reported %d chunks sent, want 2", result.ChunksSent)
	}
}

// A server that does not advertise the name still gets a working upload. The
// client ships separately from the server it is pointed at, so every capability
// on this surface has to degrade rather than fail.
func TestAServerWithoutBatchedUploadStillGetsItsChunks(t *testing.T) {
	ts, c := newTreeServer(t, "batch", "chunks")
	root := sampleTree(t)

	if _, err := c.UploadDir("r1", "/", root, nil); err != nil {
		t.Fatal(err)
	}

	if len(ts.uploads) != 0 {
		t.Errorf("%d framed uploads to a server that never advertised the name", len(ts.uploads))
	}
	if len(ts.chunksPut) != 2 {
		t.Errorf("%d single-chunk PUTs, want 2: %v", len(ts.chunksPut), ts.chunksPut)
	}
}

// Content the server already holds is reported, not resent — and it is not an
// error. It is what a client sees whenever its chunks/missing answer went
// stale under it, which is ordinary on a library more than one client writes.
func TestChunksTheServerAlreadyHoldsAreCounted(t *testing.T) {
	ts, c := newTreeServer(t, "batch", "chunks", "chunks-upload")
	root := sampleTree(t)

	if _, err := c.UploadDir("r1", "/", root, nil); err != nil {
		t.Fatal(err)
	}
	held := len(ts.uploads)

	// The same tree again, into a server that now holds all of it. Nothing is
	// missing, so nothing is framed and no request is made at all.
	if _, err := c.UploadDir("r1", "/", root, nil); err != nil {
		t.Fatal(err)
	}
	if len(ts.uploads) != held {
		t.Errorf("a second upload of content the server holds made %d more requests, want 0",
			len(ts.uploads)-held)
	}
}
