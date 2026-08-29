package silod

import (
	"bytes"
	"math/rand"
	"net/http"
	"testing"

	"github.com/dkam/silo/store"
)

// A file's manifest, addressed by the path the client already has.
//
// The bytes are the same object `GET objects/{id}` serves, so a client that
// already decodes one decodes both; the reason to have the path spelling as
// well is scope. The id-addressed surface is library-level — a credential
// narrowed to a subtree is refused it, because a chunk id says nothing about
// where the chunk is linked — so without this a path-scoped credential can
// read a file's bytes and cannot read its chunk list.
func TestAManifestCanBeReadByPath(t *testing.T) {
	libraryID, acct := testLibrary(t)
	vars := map[string]string{"libraryid": libraryID, "path": "big.bin"}

	// Over the inline threshold, so there is a chunk list rather than the
	// bytes themselves — and pseudorandom rather than patterned, because a
	// periodic sequence gives a content-defined chunker nothing to cut on and
	// the whole 3 MiB comes back as one chunk.
	content := make([]byte, 3<<20)
	if _, err := rand.New(rand.NewSource(1)).Read(content); err != nil {
		t.Fatal(err)
	}
	w := do(t, putEntry, acct, http.MethodPut, "/entries/big.bin", vars, content)
	if w.Code != http.StatusCreated {
		t.Fatalf("PUT = %d (%s), want 201", w.Code, w.Body.String())
	}
	manifestID := bytes.Trim([]byte(w.Header().Get("ETag")), `"`)

	w = do(t, getEntry, acct, http.MethodGet, "/entries/big.bin?type=manifest", vars, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("GET ?type=manifest = %d (%s), want 200", w.Code, w.Body.String())
	}

	m, err := store.DecodeManifest(w.Body.Bytes())
	if err != nil {
		t.Fatalf("the body did not decode as a manifest: %v", err)
	}
	if m.FileSize != int64(len(content)) {
		t.Errorf("manifest file_size = %d, want %d", m.FileSize, len(content))
	}
	if len(m.Chunks) < 2 {
		t.Fatalf("manifest has %d chunks; a 3 MiB file should have several", len(m.Chunks))
	}
	// The chunk sizes are plaintext lengths and they have to add up, because
	// that is the arithmetic a client maps a read offset through.
	var total int64
	for _, c := range m.Chunks {
		total += c.Size
	}
	if total != m.FileSize {
		t.Errorf("chunk sizes sum to %d, want file_size %d", total, m.FileSize)
	}

	// The tag is the bare object id, not the "v1-" representation tag the
	// listing carries: this body IS the object, so it cannot change under a
	// fixed id, and the same bytes at objects/{id} must validate the same way.
	// The ETag on the entries surface is v1-prefixed, so the two must differ.
	wantTag := `"` + string(bytes.TrimPrefix(manifestID, []byte(etagPrefix))) + `"`
	if got := w.Header().Get("ETag"); got != wantTag {
		t.Errorf("ETag = %q, want %q", got, wantTag)
	}
	if got := w.Header().Get("Cache-Control"); got == "" {
		t.Error("no Cache-Control; the manifest is immutable and should say so")
	}
}

// The same bytes, both ways in. Two addresses for one representation is only
// safe if they really are one representation.
func TestTheManifestByPathIsTheObjectByID(t *testing.T) {
	libraryID, acct := testLibrary(t)
	vars := map[string]string{"libraryid": libraryID, "path": "big.bin"}

	content := make([]byte, 2<<20)
	if _, err := rand.New(rand.NewSource(2)).Read(content); err != nil {
		t.Fatal(err)
	}
	w := do(t, putEntry, acct, http.MethodPut, "/entries/big.bin", vars, content)
	if w.Code != http.StatusCreated {
		t.Fatalf("PUT = %d, want 201", w.Code)
	}
	id := string(bytes.TrimPrefix(bytes.Trim([]byte(w.Header().Get("ETag")), `"`), []byte(etagPrefix)))

	byPath := do(t, getEntry, acct, http.MethodGet, "/entries/big.bin?type=manifest", vars, nil)
	byID := idReq(t, getObjectHandler, acct, http.MethodGet, "/objects/"+id,
		map[string]string{"libraryid": libraryID, "id": id}, nil, nil)
	if byPath.Code != http.StatusOK || byID.Code != http.StatusOK {
		t.Fatalf("by path = %d, by id = %d; want 200 from both", byPath.Code, byID.Code)
	}
	if !bytes.Equal(byPath.Body.Bytes(), byID.Body.Bytes()) {
		t.Error("the manifest read by path differs from the object read by id")
	}
	if byPath.Header().Get("ETag") != byID.Header().Get("ETag") {
		t.Errorf("tags differ: by path %q, by id %q",
			byPath.Header().Get("ETag"), byID.Header().Get("ETag"))
	}
}

// A directory has no manifest, and saying so is not the same as saying the
// path is absent — the client asked the wrong question about a real entry.
func TestAskingADirectoryForAManifestIsRefused(t *testing.T) {
	libraryID, acct := testLibrary(t)
	vars := map[string]string{"libraryid": libraryID, "path": "sub"}

	w := do(t, putEntry, acct, http.MethodPut, "/entries/sub?type=dir", vars, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("mkdir = %d (%s), want 201", w.Code, w.Body.String())
	}
	w = do(t, getEntry, acct, http.MethodGet, "/entries/sub?type=manifest", vars, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("GET a directory ?type=manifest = %d (%s), want 400", w.Code, w.Body.String())
	}
}
