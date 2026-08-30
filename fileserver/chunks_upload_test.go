package silod

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/dkam/silo/store"
)

// The write half of the batched chunk surface.
//
// `chunks/fetch` closed the read side: many chunks in one framed response.
// Going up was still one request per chunk, so a 1 GB file at the chunker's
// 1 MiB target was roughly a thousand round trips. This is the same framing
// read rather than written, which is what store/chunkstream.go's own comment
// said a batched upload would take.

// uploadBody frames chunks the way a client would, terminator included.
func uploadBody(t *testing.T, chunks ...[]byte) []byte {
	t.Helper()
	var body []byte
	var err error
	for _, c := range chunks {
		body, err = store.AppendChunkFrame(body, store.ChunkID(c), c)
		if err != nil {
			t.Fatal(err)
		}
	}
	return store.AppendChunkStreamEnd(body)
}

// streamHeader is what a client must send, and the media type is the same one
// the fetch side answers with — one framing, named once.
var streamHeader = map[string]string{"Content-Type": chunkStreamMediaType}

func TestManyChunksGoUpInOneRequest(t *testing.T) {
	libraryID, acct := testLibrary(t)
	vars := map[string]string{"libraryid": libraryID}

	first := bytes.Repeat([]byte("first. "), 1000)
	second := bytes.Repeat([]byte("second. "), 1000)
	third := bytes.Repeat([]byte("third. "), 1000)

	// One of the three is already there, which is the case a real client hits
	// whenever its chunks/missing answer went stale under it — another client
	// uploaded the same content in between. It must not be an error: the
	// content is what was asked for, and it is present.
	pre := idReq(t, putChunkHandler, acct, http.MethodPut, "/chunks/"+store.ChunkID(second).String(),
		merge(vars, "id", store.ChunkID(second).String()), second, nil)
	if pre.Code != http.StatusCreated {
		t.Fatalf("seeding PUT chunk = %d (%s), want 201", pre.Code, pre.Body.String())
	}

	w := idReq(t, chunksUploadHandler, acct, http.MethodPost, "/chunks",
		vars, uploadBody(t, first, second, third), streamHeader)
	if w.Code != http.StatusOK {
		t.Fatalf("POST chunks = %d (%s), want 200", w.Code, w.Body.String())
	}

	var got struct {
		Stored  int `json:"stored"`
		Present int `json:"present"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding the answer: %v (%s)", err, w.Body.String())
	}
	if got.Stored != 2 || got.Present != 1 {
		t.Errorf("stored=%d present=%d, want stored=2 present=1", got.Stored, got.Present)
	}

	// The point of the endpoint is that the bytes landed, so read them back
	// rather than trusting the count that just reported them.
	for _, c := range [][]byte{first, second, third} {
		id := store.ChunkID(c)
		r := idReq(t, getChunkHandler, acct, http.MethodGet, "/chunks/"+id.String(),
			merge(vars, "id", id.String()), nil, nil)
		if r.Code != http.StatusOK {
			t.Errorf("GET chunk %.8s = %d, want 200", id, r.Code)
			continue
		}
		if !bytes.Equal(r.Body.Bytes(), c) {
			t.Errorf("chunk %.8s came back with %d bytes, want %d", id, r.Body.Len(), len(c))
		}
	}
}

// A body that stops early must not read as a short but complete upload. The
// terminator is the only thing that distinguishes the two, which is the whole
// reason it exists.
func TestATruncatedUploadIsRefusedRatherThanBelieved(t *testing.T) {
	libraryID, acct := testLibrary(t)
	vars := map[string]string{"libraryid": libraryID}

	full := uploadBody(t, bytes.Repeat([]byte("cut me short. "), 1000))
	cut := full[:len(full)-store.ChunkFrameHeaderSize-10]

	w := idReq(t, chunksUploadHandler, acct, http.MethodPost, "/chunks", vars, cut, streamHeader)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("a truncated stream = %d (%s), want 400", w.Code, w.Body.String())
	}
}

// The id is the verification, and it is the only one available: a chunk is
// opaque bytes in a plain library and a sealed frame in an encrypted one.
func TestAChunkThatDoesNotHashToItsIDIsRefused(t *testing.T) {
	libraryID, acct := testLibrary(t)
	vars := map[string]string{"libraryid": libraryID}

	honest := []byte("the bytes that were named")
	var body []byte
	body, err := store.AppendChunkFrame(body, store.ChunkID(honest), honest)
	if err != nil {
		t.Fatal(err)
	}
	// A frame whose declared id belongs to different content.
	body, err = store.AppendChunkFrame(body, store.ChunkID([]byte("something else")), []byte("not that"))
	if err != nil {
		t.Fatal(err)
	}
	body = store.AppendChunkStreamEnd(body)

	w := idReq(t, chunksUploadHandler, acct, http.MethodPost, "/chunks", vars, body, streamHeader)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("a mis-named chunk = %d (%s), want 400", w.Code, w.Body.String())
	}
}

// The framing is not guessable from the bytes, so a body that does not declare
// it is refused rather than parsed hopefully.
func TestTheUploadStreamMustDeclareItsMediaType(t *testing.T) {
	libraryID, acct := testLibrary(t)
	vars := map[string]string{"libraryid": libraryID}

	body := uploadBody(t, []byte("well framed, badly labelled"))
	w := idReq(t, chunksUploadHandler, acct, http.MethodPost, "/chunks", vars, body,
		map[string]string{"Content-Type": "application/octet-stream"})
	if w.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("an unlabelled stream = %d (%s), want 415", w.Code, w.Body.String())
	}
}

// Over the cap is refused outright rather than accepted in part, for the
// reason the fetch side gives: a client cannot tell a short answer from a
// complete one, and here it would not know which of its chunks landed.
func TestAnOversizedBatchIsRefused(t *testing.T) {
	libraryID, acct := testLibrary(t)
	vars := map[string]string{"libraryid": libraryID}

	chunks := make([][]byte, maxUploadChunks+1)
	for i := range chunks {
		chunks[i] = []byte{byte(i), byte(i >> 8), 'x'}
	}
	w := idReq(t, chunksUploadHandler, acct, http.MethodPost, "/chunks", vars,
		uploadBody(t, chunks...), streamHeader)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("%d chunks = %d (%s), want 413", len(chunks), w.Code, w.Body.String())
	}
}
