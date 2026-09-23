package client

// The id-addressed surface: objects and chunks fetched by name.
//
// It is the same surface for both library types -- a plain library could be
// read this way too -- which is why these methods hang off APIClient rather
// than off anything that holds a key. What differs is only whether the bytes
// that come back mean anything without one.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"

	"github.com/SiloDrive/silo/store"
)

// ErrNotFound reports something the library does not hold: a path with no
// entry, or an object or chunk the server does not have.
var ErrNotFound = errors.New("client: not found")

// maxFetchChunks is the server's cap on one chunks/fetch request
// (fileserver/chunks_fetch.go). Asking for more is a 400, so a caller with a
// long list is batched here rather than at every call site.
const maxFetchChunks = 256

// fetchConcurrency is how many chunk batches FetchChunks will have in flight
// at once.
//
// Bounded rather than one-per-batch, and the bound is about memory rather than
// about being polite to the server. Each request in flight holds its own
// response buffer live, and frames alias that buffer -- so N concurrent
// batches is N batch bodies resident, which is the peak that opening frames
// one at a time was there to remove. Four is enough to hide the round trips
// behind each other on a link where latency is what costs, and small enough
// that the peak stays a multiple nobody has to think about.
const fetchConcurrency = 4

// ErrHeadMoved reports a PUT head that lost the compare-and-swap: something
// else committed since the head this write was built on.
//
// It is an answer rather than a failure. The move is to re-read the head,
// rebuild the change on the new root and try again, which is what the write
// path here does; a caller only sees it if the rebuild kept losing.
var ErrHeadMoved = errors.New("client: the head has moved")

// doBytes performs an authenticated request whose response is raw bytes.
func (c *APIClient) doBytes(method, path, contentType string, header http.Header, body []byte) ([]byte, error) {
	out, _, err := c.doBytesHeaders(method, path, contentType, header, body)
	return out, err
}

// doBytesHeaders is doBytes with the response headers, for the callers that
// have to know what they were given -- a GET on the path surface answers a
// listing or a file body at the same address, and only Content-Type says
// which.
func (c *APIClient) doBytesHeaders(method, path, contentType string, header http.Header, body []byte) ([]byte, http.Header, error) {
	var newBody func() (io.ReadCloser, int64, error)
	if body != nil {
		newBody = func() (io.ReadCloser, int64, error) {
			return io.NopCloser(bytes.NewReader(body)), int64(len(body)), nil
		}
	}
	resp, err := c.doStreamHeaders(method, path, contentType, header, newBody)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		msg, _ := io.ReadAll(resp.Body)
		se := &StatusError{Code: resp.StatusCode, Status: resp.Status, Body: string(msg)}
		switch resp.StatusCode {
		case http.StatusNotFound:
			return nil, resp.Header, fmt.Errorf("%w: %w", ErrNotFound, se)
		case http.StatusPreconditionFailed:
			return nil, resp.Header, fmt.Errorf("%w: %w", ErrHeadMoved, se)
		}
		return nil, resp.Header, se
	}
	out, err := io.ReadAll(resp.Body)
	return out, resp.Header, err
}

// Object fetches one object -- a manifest, a directory or a commit -- exactly
// as it is stored.
//
// The bytes are returned rather than decoded because the server does not know
// which of the three it holds and neither does this method; the caller does,
// because it followed a reference that said so.
func (c *APIClient) Object(libraryID string, id store.ID) ([]byte, error) {
	return c.doBytes("GET",
		"/api/silo/v1/libraries/"+libraryID+"/objects/"+id.String(), "", nil, nil)
}

// PutObject stores one object under the id its bytes hash to.
//
// The server verifies that id against the bytes and that they decode as one of
// the store's object kinds, and nothing else, because there is nothing else it
// can check without the content key. So a successful PUT is proof the bytes
// crossed intact and no statement at all about whether they mean anything.
func (c *APIClient) PutObject(libraryID string, id store.ID, body []byte) error {
	_, err := c.doBytes("PUT",
		"/api/silo/v1/libraries/"+libraryID+"/objects/"+id.String(),
		"application/octet-stream", nil, body)
	return err
}

// PutHead advances the library to a new commit, refusing unless the head is
// still the one named.
//
// ErrHeadMoved is the lost compare-and-swap. There is no server-side merge and
// there cannot be one: merging trees means reading names.
func (c *APIClient) PutHead(libraryID string, newHead, expected store.ID) error {
	header := http.Header{"If-Match": []string{`"` + expected.String() + `"`}}
	_, err := c.doBytes("PUT", "/api/silo/v1/libraries/"+libraryID+"/head",
		"text/plain", header, []byte(newHead.String()))
	return err
}

// FetchChunks fetches many chunks in one round trip, batching over the
// server's per-request cap.
//
// The answer is a map because the stream is id-labelled rather than ordered:
// the server drops a repeated id, so a manifest naming one chunk twice gets it
// once. A chunk the server does not hold is absent from the map rather than
// being an error -- which of the missing ones matters is the caller's
// question, and it can see all of them at once this way.
// Batches run together rather than in turn. They are independent requests over
// one connection pool, and sending them one after the other made a
// 1024-chunk read four sequential round trips where it is one plus transfer --
// four times the latency on the link where latency is what costs, and nothing
// on a local one. See fetchConcurrency for why together is bounded.
//
// Nothing has to be put back in order afterwards, which is the property that
// made the map the return type in the first place: the stream is id-labelled,
// so a batch's answer is self-describing wherever it lands.
func (c *APIClient) FetchChunks(libraryID string, ids []store.ID) (map[store.ID][]byte, error) {
	out := make(map[store.ID][]byte, len(ids))
	if len(ids) == 0 {
		return out, nil
	}

	type span struct{ start, end int }
	spans := make(chan span)

	var (
		mu    sync.Mutex
		first error
		wg    sync.WaitGroup
	)

	workers := min(fetchConcurrency, (len(ids)+maxFetchChunks-1)/maxFetchChunks)
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for s := range spans {
				// A batch's frames alias its own response buffer, so each one
				// accumulates into a map of its own and merges after: sharing
				// out would mean holding the lock across the request.
				got := make(map[store.ID][]byte, s.end-s.start)
				err := c.fetchChunkBatch(libraryID, ids[s.start:s.end], got)

				mu.Lock()
				for id, b := range got {
					out[id] = b
				}
				if err != nil && first == nil {
					first = err
				}
				mu.Unlock()
			}
		}()
	}

	for start := 0; start < len(ids); start += maxFetchChunks {
		// Stop feeding once something has failed. Sequentially this was free
		// -- the loop returned -- and here it is the difference between one
		// failed request against a server that is gone and forty.
		mu.Lock()
		failed := first != nil
		mu.Unlock()
		if failed {
			break
		}
		spans <- span{start, min(start+maxFetchChunks, len(ids))}
	}
	close(spans)
	wg.Wait()

	// Partial results with the error, as before: which chunks did arrive is
	// the caller's question to ask.
	return out, first
}

func (c *APIClient) fetchChunkBatch(libraryID string, ids []store.ID, out map[store.ID][]byte) error {
	hex := make([]string, len(ids))
	for i, id := range ids {
		hex[i] = id.String()
	}
	body, err := json.Marshal(map[string][]string{"chunks": hex})
	if err != nil {
		return err
	}
	stream, err := c.doBytes("POST",
		"/api/silo/v1/libraries/"+libraryID+"/chunks/fetch", "application/json", nil, body)
	if err != nil {
		return err
	}
	// Frames alias stream, and stream is this function's own buffer that
	// nothing else will touch, so what goes into the map is safe to keep.
	frames, err := store.DecodeChunkFrames(stream)
	if err != nil {
		return err
	}
	for _, f := range frames {
		if f.Present {
			out[f.ID] = f.Bytes
		}
	}
	return nil
}
