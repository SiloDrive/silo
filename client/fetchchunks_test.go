package client

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/dkam/silo/store"
)

// chunkFixtures builds ids the decoder will accept: a chunk's id is the
// SHA-256 of its bytes, and DecodeChunkFrames checks it, so a fake server has
// to serve content that hashes to what it was asked for.
func chunkFixtures(n int) ([]store.ID, map[store.ID][]byte) {
	ids := make([]store.ID, n)
	content := make(map[store.ID][]byte, n)
	for i := range ids {
		b := fmt.Appendf(nil, "chunk-%d", i)
		ids[i] = store.ID(sha256.Sum256(b))
		content[ids[i]] = b
	}
	return ids, content
}

// FetchChunks batches over the server's 256-chunk cap. The batches are
// independent requests over one connection pool, so a 1024-chunk read that
// sends them in turn is four sequential round trips where it could be one plus
// transfer -- 4x the latency on a link where latency is what costs.
//
// Concurrency is not observable from a timing measurement that would be honest
// on a fast local server, so this asks the question directly: the handler
// refuses to answer the first request until a second one has arrived. Run in
// turn, nothing ever arrives to release it and the barrier times out; run
// together, they release each other. Peak in-flight is the assertion.
func TestFetchChunksSendsItsBatchesTogether(t *testing.T) {
	const total = maxFetchChunks * 4

	ids, content := chunkFixtures(total)

	var (
		mu       sync.Mutex
		inFlight int
		peak     int
	)
	gate := make(chan struct{})
	var once sync.Once

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Chunks []string `json:"chunks"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			t.Errorf("undecodable request body: %v", err)
		}

		mu.Lock()
		inFlight++
		if inFlight > peak {
			peak = inFlight
		}
		reached := inFlight >= 2
		mu.Unlock()

		if reached {
			once.Do(func() { close(gate) })
		}
		select {
		case <-gate:
		case <-time.After(300 * time.Millisecond):
			// Sequential: nothing else will ever arrive to release this.
		}

		mu.Lock()
		inFlight--
		mu.Unlock()

		var out []byte
		for _, h := range req.Chunks {
			id, err := store.ParseID(h)
			if err != nil {
				t.Errorf("unparseable id %q: %v", h, err)
				continue
			}
			out, err = store.AppendChunkFrame(out, id, content[id])
			if err != nil {
				t.Errorf("encoding a frame: %v", err)
			}
		}
		out = store.AppendChunkStreamEnd(out)
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(out)
	}))
	t.Cleanup(srv.Close)

	got, err := NewClient(srv.URL).FetchChunks("r1", ids)
	if err != nil {
		t.Fatalf("FetchChunks returned %v", err)
	}
	if len(got) != total {
		t.Errorf("fetched %d chunks, want %d", len(got), total)
	}
	if peak < 2 {
		t.Errorf("peak in-flight requests was %d: the batches were sent one at a time", peak)
	}
}

// Bounded, not unbounded. Each batch in flight holds its own response buffer
// live, which is the peak that dropping frames as they are opened was there to
// remove -- so the concurrency is the lever that keeps it from coming back.
func TestFetchChunksBoundsHowManyBatchesAreInFlight(t *testing.T) {
	const total = maxFetchChunks * (fetchConcurrency + 3)

	ids, content := chunkFixtures(total)

	var (
		mu       sync.Mutex
		inFlight int
		peak     int
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Chunks []string `json:"chunks"`
		}
		_ = json.Unmarshal(body, &req)

		mu.Lock()
		inFlight++
		if inFlight > peak {
			peak = inFlight
		}
		mu.Unlock()

		// Long enough that every batch the client is willing to run at once
		// piles up here before any of them finishes.
		time.Sleep(50 * time.Millisecond)

		mu.Lock()
		inFlight--
		mu.Unlock()

		var out []byte
		for _, h := range req.Chunks {
			id, _ := store.ParseID(h)
			out, _ = store.AppendChunkFrame(out, id, content[id])
		}
		_, _ = w.Write(store.AppendChunkStreamEnd(out))
	}))
	t.Cleanup(srv.Close)

	if _, err := NewClient(srv.URL).FetchChunks("r1", ids); err != nil {
		t.Fatalf("FetchChunks returned %v", err)
	}
	if peak > fetchConcurrency {
		t.Errorf("peak in-flight was %d, over the %d bound: several response buffers are live at once",
			peak, fetchConcurrency)
	}
}
