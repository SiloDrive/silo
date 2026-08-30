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

	"github.com/dkam/silo/store"
)

// ErrNotFound reports something the library does not hold: a path with no
// entry, or an object or chunk the server does not have.
var ErrNotFound = errors.New("client: not found")

// maxFetchChunks is the server's cap on one chunks/fetch request
// (fileserver/chunks_fetch.go). Asking for more is a 400, so a caller with a
// long list is batched here rather than at every call site.
const maxFetchChunks = 256

// ErrHeadMoved reports a PUT head that lost the compare-and-swap: something
// else committed since the head this write was built on.
//
// It is an answer rather than a failure. The move is to re-read the head,
// rebuild the change on the new root and try again, which is what the write
// path here does; a caller only sees it if the rebuild kept losing.
var ErrHeadMoved = errors.New("client: the head has moved")

// doBytes performs an authenticated request whose response is raw bytes.
func (c *APIClient) doBytes(method, path, contentType string, header http.Header, body []byte) ([]byte, error) {
	var newBody func() (io.ReadCloser, int64, error)
	if body != nil {
		newBody = func() (io.ReadCloser, int64, error) {
			return io.NopCloser(bytes.NewReader(body)), int64(len(body)), nil
		}
	}
	resp, err := c.doStreamHeaders(method, path, contentType, header, newBody)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		msg, _ := io.ReadAll(resp.Body)
		se := &StatusError{Code: resp.StatusCode, Status: resp.Status, Body: string(msg)}
		switch resp.StatusCode {
		case http.StatusNotFound:
			return nil, fmt.Errorf("%w: %s", ErrNotFound, se)
		case http.StatusPreconditionFailed:
			return nil, fmt.Errorf("%w: %s", ErrHeadMoved, se)
		}
		return nil, se
	}
	return io.ReadAll(resp.Body)
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
func (c *APIClient) FetchChunks(libraryID string, ids []store.ID) (map[store.ID][]byte, error) {
	out := make(map[store.ID][]byte, len(ids))
	for start := 0; start < len(ids); start += maxFetchChunks {
		end := min(start+maxFetchChunks, len(ids))
		if err := c.fetchChunkBatch(libraryID, ids[start:end], out); err != nil {
			return out, err
		}
	}
	return out, nil
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
