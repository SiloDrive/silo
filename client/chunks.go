package client

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"

	"github.com/dkam/silo/store"
)

// The client half of the chunk surface. See fileserver/chunks.go for the
// server's side and docs/protocol.md for the endpoints.
//
// The whole arrangement rests on one property: a chunk's id is the SHA-256 of
// its bytes, so this file computes exactly the names the server would without
// asking it anything. That is what lets a client say "here is what this file
// is made of — which parts do you not already have?" before transferring a
// byte.
//
// Where the cut points fall is the library's business, not this file's. The
// chunker is content-defined and its parameters are stored per library and
// served on the libraries listing, so a client that hardcodes them computes ids
// nothing else in the store shares. Chunking under the wrong parameters is
// still correct — the server verifies bytes against the id it was given — but
// it dedups against nothing, which is the entire point of the surface. So
// there is no default here and no guess: a library whose parameters cannot be
// read is uploaded whole.

// chunkLaneThreshold is the size above which a file is worth naming before
// sending.
//
// This is a property of the link rather than of the library, which is why it
// is a constant here and not one of the chunker's parameters. Below it the
// whole-file PUT is one request against the chunk lane's three, and neither
// resume nor dedup has enough to save to pay for the other two. Above it they
// do — an interrupted upload resumes from what landed, and content the server
// already holds never crosses the wire.
const chunkLaneThreshold = 8 << 20

// capabilities returns what the server said about itself, fetched once and
// remembered. A server that cannot be asked reads as one with no features,
// which routes every caller to the path that has always worked.
func (c *APIClient) capabilities() ServerInfo {
	c.mu.Lock()
	cached := c.serverInfo
	c.mu.Unlock()
	if cached != nil {
		return *cached
	}

	info, err := c.GetServerInfo()
	if err != nil {
		info = ServerInfo{}
	}
	c.mu.Lock()
	c.serverInfo = &info
	c.mu.Unlock()
	return info
}

// chunkerFor returns the parameters to chunk this library under, and whether
// the chunk lane can be used for it at all.
//
// Two separate reasons come back as one false, and both mean the same thing to
// every caller: send the file whole.
//
//   - The library is end-to-end encrypted. Its chunks are ciphertext this
//     client would have to produce, and the server cannot assemble a file out
//     of chunks it cannot open — the seed itself is derived from a content key
//     that is not on this path.
//   - The server did not report the parameters. Guessing them is the one
//     failure this surface cannot detect: every id would be well-formed, every
//     upload would succeed, and none of it would ever match anything.
//
// chunkerCache is one library's answer from chunkerFor, including the "send it
// whole" answer — a library that cannot use the chunk lane cannot start being
// able to, so that verdict is worth remembering too.
type chunkerCache struct {
	params store.Params
	ok     bool
}

func (c *APIClient) chunkerFor(libraryID string) (store.Params, bool) {
	c.mu.Lock()
	hit, cached := c.chunkers[libraryID]
	c.mu.Unlock()
	if cached {
		return hit.params, hit.ok
	}

	params, ok := c.fetchChunkerFor(libraryID)

	c.mu.Lock()
	if c.chunkers == nil {
		c.chunkers = map[string]chunkerCache{}
	}
	c.chunkers[libraryID] = chunkerCache{params: params, ok: ok}
	c.mu.Unlock()
	return params, ok
}

func (c *APIClient) fetchChunkerFor(libraryID string) (store.Params, bool) {
	library, err := c.Library(libraryID)
	if err != nil {
		return store.Params{}, false
	}
	if library.Encrypted || library.Chunker == nil {
		return store.Params{}, false
	}
	p := store.Params{
		Algorithm: library.Chunker.Algorithm,
		// The published constant, derived rather than transmitted. A plain
		// library chunks under it by definition, so a server sending one would
		// be sending a value this client would have to check anyway.
		Seed:          store.PlainSeed(),
		MinSize:       library.Chunker.MinSize,
		TargetSize:    library.Chunker.TargetSize,
		MaxSize:       library.Chunker.MaxSize,
		Normalization: library.Chunker.Normalization,
	}
	// Validated before use, because these arrived over the wire. A target below
	// the minimum is a server bug or a hostile server, and either way chunking
	// under it produces something no other client reproduces.
	if p.ValidateFor(false, nil) != nil {
		return store.Params{}, false
	}
	return p, true
}

// MissingChunks asks which of these chunks the library does not already hold.
// The answer comes back in the order asked, each id once.
func (c *APIClient) MissingChunks(libraryID string, chunks []string) ([]string, error) {
	var result struct {
		Missing []string `json:"missing"`
	}
	err := c.doRequest("POST", "/api/silo/v1/libraries/"+libraryID+"/chunks/missing",
		map[string][]string{"chunks": chunks}, &result)
	if err != nil {
		return nil, err
	}
	return result.Missing, nil
}

// PutChunk uploads one chunk. The server hashes what arrives and refuses it if
// it does not match chunkID, so a successful call is also proof the bytes
// crossed intact.
func (c *APIClient) PutChunk(libraryID, chunkID string, newBody func() (io.ReadCloser, int64, error)) error {
	resp, err := c.doStream("PUT", "/api/silo/v1/libraries/"+libraryID+"/chunks/"+chunkID,
		"application/octet-stream", newBody)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		msg, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("chunk %.8s: %s: %s", chunkID, resp.Status, string(msg))
	}
	return nil
}

// CommitChunks creates or replaces a file from chunks already uploaded. It
// transfers no content: the body is the ordered list of ids.
func (c *APIClient) CommitChunks(libraryID, remotePath string, chunks []string) error {
	return c.doRequest("PUT", entriesURL(libraryID, remotePath)+"?type=chunks",
		map[string][]string{"chunks": chunks}, nil)
}

// chunkSource is a place the bytes of a chunk can be read from. Any one
// occurrence will do — every copy of a chunk has the same content, which is
// what its id means.
//
// The length is here rather than derived because chunks are not a fixed size:
// where the previous cut fell is the only thing that says how long this one
// is, and by upload time that stream is gone.
type chunkSource struct {
	local string
	off   int64
	size  int64
	// data holds the bytes outright, for a chunk that is not a region of any
	// file on disk. An encrypted library's chunks are sealed frames: they
	// differ from the plaintext they were cut from, so there is nowhere to
	// re-read them and they have to be carried.
	data []byte
}

// fromBytes is a chunk that already exists in memory.
func fromBytes(b []byte) chunkSource { return chunkSource{data: b, size: int64(len(b))} }

// chunkFile computes the chunk ids of a local file, in order, along with a
// place each one can be read from. Nothing is held in memory but the chunker's
// window, so this is bounded however large the file is.
//
// A repeated chunk appears once in sources and as often as it occurs in ids:
// the sources are for uploading, where one copy is enough, and the ids are the
// file, where every occurrence counts.
func chunkFile(localPath string, p store.Params) (ids []string, sources map[string]chunkSource, err error) {
	file, err := os.Open(localPath)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = file.Close() }()

	chunker, err := store.NewChunker(p, file)
	if err != nil {
		return nil, nil, err
	}

	sources = make(map[string]chunkSource)
	for {
		ch, err := chunker.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, nil, err
		}
		// Hashed before the next call to Next, which is when ch.Data stops
		// being valid — it aliases the chunker's buffer rather than a copy.
		id := store.ChunkID(ch.Data).String()
		ids = append(ids, id)
		if _, seen := sources[id]; !seen {
			sources[id] = chunkSource{local: localPath, off: ch.Offset, size: int64(len(ch.Data))}
		}
	}
	return ids, sources, nil
}

// uploadChunks sends a file as chunks: name every part, ask which parts are
// new, send only those, then name the whole thing in one call.
//
// Nothing exists at the destination path until that last call, so an upload
// interrupted at any point leaves the library exactly as it was. Run it again
// and the chunks that landed are already there — the second attempt asks the
// same question and gets a shorter answer.
func (c *APIClient) uploadChunks(libraryID, parentDir, localPath string, p store.Params) error {
	ids, sources, err := chunkFile(localPath, p)
	if err != nil {
		return fmt.Errorf("failed to read %s: %v", localPath, err)
	}

	missing, err := c.MissingChunks(libraryID, ids)
	if err != nil {
		return err
	}

	for _, id := range missing {
		if _, ok := sources[id]; !ok {
			// The server answered with an id that was never offered.
			return fmt.Errorf("server asked for chunk %.8s, which is not part of this file", id)
		}
	}
	if err := c.sendChunks(libraryID, missing, sources, func(int, int64) {}); err != nil {
		return err
	}

	return c.CommitChunks(libraryID, path.Join("/", parentDir, filepath.Base(localPath)), ids)
}

// putChunkFrom uploads one chunk out of a local file, and reports how many
// bytes it sent.
func (c *APIClient) putChunkFrom(libraryID, id string, src chunkSource) (int64, error) {
	err := c.PutChunk(libraryID, id, func() (io.ReadCloser, int64, error) {
		if src.data != nil {
			return io.NopCloser(bytes.NewReader(src.data)), src.size, nil
		}
		// A factory, not a reader: doStream re-sends the body after a 401, and
		// a reader already drained cannot be sent twice.
		file, err := os.Open(src.local)
		if err != nil {
			return nil, 0, err
		}
		if _, err := file.Seek(src.off, io.SeekStart); err != nil {
			_ = file.Close()
			return nil, 0, err
		}
		return readCloser{io.LimitReader(file, src.size), file}, src.size, nil
	})
	return src.size, err
}

// readCloser pairs a limited reader with the file underneath it, so the whole
// chunk can be sent without handing the request the rest of the file.
type readCloser struct {
	io.Reader
	io.Closer
}
