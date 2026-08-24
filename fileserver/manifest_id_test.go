package silod

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"testing"

	"github.com/dkam/silo/store"
)

// The id a PUT returns is a manifest id, and computing it is not hashing the
// bytes.
//
// porter-brief.md told client authors to compute it by hashing what they
// uploaded, and to read a mismatch as "the bytes that landed are not the bytes
// you sent". Every comparison mismatches, because the id is the hash of the
// *encoded manifest*: for a small file one that carries the bytes inline
// behind a header, and for a large one a chunk list that contains no file
// bytes at all. A client following that paragraph reports corruption on every
// upload against a healthy server.
//
// These tests pin what a client actually has to do, because the rewritten
// paragraph tells them to do it — and a documented integrity check is only
// worth having if it passes.

// putFile uploads content and returns the id the server reports.
func putFile(t *testing.T, base, token, library, path string, content []byte) string {
	t.Helper()
	req, err := http.NewRequest("PUT",
		base+"/api/silo/v1/libraries/"+library+"/entries/"+path, bytes.NewReader(content))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT %s: %v", path, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT %s: status %d, body %s", path, resp.StatusCode, body)
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	return out.ID
}

// libraryChunker reads the chunker the server published for a library, the way
// a client has to: from the library's own row, never from a constant.
func libraryChunker(t *testing.T, base, token, library string) store.Params {
	t.Helper()
	code, body := call(t, "GET", base+"/api/silo/v1/libraries", token, "")
	if code != http.StatusOK {
		t.Fatalf("listing libraries: status %d, body %s", code, body)
	}
	var rows []struct {
		ID      string `json:"id"`
		Chunker *struct {
			Algorithm     string `json:"algorithm"`
			MinSize       int    `json:"min_size"`
			TargetSize    int    `json:"target_size"`
			MaxSize       int    `json:"max_size"`
			Normalization int    `json:"normalization"`
		} `json:"chunker"`
	}
	if err := json.Unmarshal([]byte(body), &rows); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	for _, row := range rows {
		if row.ID != library {
			continue
		}
		if row.Chunker == nil {
			t.Fatalf("library %s published no chunker, so a client cannot compute an id", library)
		}
		// The seed is deliberately not on the wire: a plain library chunks
		// under the published constant, which a client derives rather than
		// receives. See chunkerInfo in fileserver/api.
		return store.Params{
			Algorithm:     row.Chunker.Algorithm,
			Seed:          store.PlainSeed(),
			MinSize:       row.Chunker.MinSize,
			TargetSize:    row.Chunker.TargetSize,
			MaxSize:       row.Chunker.MaxSize,
			Normalization: row.Chunker.Normalization,
		}
	}
	t.Fatalf("library %s is not in the listing:\n%s", library, body)
	return store.Params{}
}

// clientManifestID computes the id a PUT will return, using only what a client
// has: the bytes, the library's published chunker, and the store package.
func clientManifestID(t *testing.T, p store.Params, content []byte) string {
	t.Helper()
	m := &store.Manifest{FileSize: int64(len(content))}
	if store.Inlined(m.FileSize) {
		m.Inline = content
	} else {
		chunker, err := store.NewChunker(p, bytes.NewReader(content))
		if err != nil {
			t.Fatalf("build chunker: %v", err)
		}
		var total int64
		for {
			ch, err := chunker.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				t.Fatalf("chunking: %v", err)
			}
			m.Chunks = append(m.Chunks, store.ChunkRef{
				ID:   store.ChunkID(ch.Data),
				Size: int64(len(ch.Data)),
			})
			total += int64(len(ch.Data))
		}
		m.FileSize = total
	}
	encoded, err := m.Encode()
	if err != nil {
		t.Fatalf("encode manifest: %v", err)
	}
	return store.ObjectID(encoded).String()
}

func TestAClientCanComputeTheIDAPUTWillReturn(t *testing.T) {
	base, token := wire(t)
	library := makeLibrary(t, base, token)
	params := libraryChunker(t, base, token, library)

	for _, c := range []struct {
		name    string
		path    string
		content []byte
	}{
		// The brief's own example file, and the case that shows why hashing
		// the bytes cannot work: an inline manifest wraps them in a header.
		{"inline", "greeting.txt", []byte("hello, porter\n")},
		// Over the inline threshold, so the manifest is a chunk list and holds
		// none of the file's bytes at all.
		{"chunked", "big.bin", pseudoRandomBytes(3<<20, "silo/porter-brief/check-the-id")},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := putFile(t, base, token, library, c.path, c.content)
			if want := clientManifestID(t, params, c.content); got != want {
				t.Errorf("client computed %s, server returned %s", want, got)
			}
			// The check the brief described, with its algorithm modernised
			// from SHA-1. It is here so that restoring that advice fails.
			if bytesHash := store.ObjectID(c.content).String(); got == bytesHash {
				t.Errorf("the id is the hash of the uploaded bytes (%s); "+
					"porter-brief's original advice would work and this test is wrong", bytesHash)
			}
		})
	}
}

// pseudoRandomBytes is content that actually chunks: a constant stream has no
// content-defined boundaries to find, and the boundaries are the point.
func pseudoRandomBytes(n int, seed string) []byte {
	out := make([]byte, 0, n+32)
	h := store.ObjectID([]byte(seed))
	for len(out) < n {
		out = append(out, h[:]...)
		h = store.ObjectID(h[:])
	}
	return out[:n]
}
