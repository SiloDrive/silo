package silod

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/dkam/silo/store"
)

// The download half of the chunk surface: many chunks in one response, framed
// so each is named and verifiable on arrival.
//
// This is the round trip silo-drive needs and could not have. Its upload has asked
// "which of these do you hold?" and sent only the answer since 0.4.5; its
// download had no equivalent, so an edit to one byte of a 1 GiB file uploaded
// one chunk and downloaded the whole file to build it.
func TestChunksFetchReturnsFramedChunks(t *testing.T) {
	libraryID, acct := testLibrary(t)
	vars := map[string]string{"libraryid": libraryID}

	first := bytes.Repeat([]byte("first. "), 1000)
	second := bytes.Repeat([]byte("second. "), 1000)
	firstID, secondID := store.ChunkID(first), store.ChunkID(second)
	for _, c := range [][]byte{first, second} {
		id := store.ChunkID(c)
		w := idReq(t, putChunkHandler, acct, http.MethodPut, "/chunks/"+id.String(),
			merge(vars, "id", id.String()), c, nil)
		if w.Code != http.StatusCreated {
			t.Fatalf("PUT chunk = %d (%s), want 201", w.Code, w.Body.String())
		}
	}
	// One id the store has never seen, so the absent case is exercised in the
	// same response as the present one — a client must be able to tell them
	// apart without a second request.
	absentID := store.ChunkID([]byte("never uploaded"))

	body, err := json.Marshal(map[string]any{
		"chunks": []string{firstID.String(), secondID.String(), absentID.String()},
	})
	if err != nil {
		t.Fatal(err)
	}
	w := idReq(t, chunksFetchHandler, acct, http.MethodPost, "/chunks/fetch", vars, body, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("POST chunks/fetch = %d (%s), want 200", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Content-Type"); got != chunkStreamMediaType {
		t.Errorf("Content-Type = %q, want %q", got, chunkStreamMediaType)
	}

	frames, err := store.DecodeChunkFrames(w.Body.Bytes())
	if err != nil {
		t.Fatalf("DecodeChunkFrames: %v", err)
	}
	if len(frames) != 3 {
		t.Fatalf("got %d frames, want 3", len(frames))
	}
	// Order is the order asked in, so a client streaming the response can lay
	// chunks down without buffering the whole thing.
	if frames[0].ID != firstID || !bytes.Equal(frames[0].Bytes, first) {
		t.Errorf("frame 0 is not the first chunk")
	}
	if frames[1].ID != secondID || !bytes.Equal(frames[1].Bytes, second) {
		t.Errorf("frame 1 is not the second chunk")
	}
	if frames[2].ID != absentID || frames[2].Present {
		t.Errorf("frame 2 = %+v, want the absent marker for %s", frames[2], absentID)
	}
}

// A repeated id costs one frame, not two. A file with a run of zeroes names
// the same chunk many times, and sending those bytes once per mention is the
// bandwidth this endpoint exists to save.
func TestChunksFetchSendsARepeatedChunkOnce(t *testing.T) {
	libraryID, acct := testLibrary(t)
	vars := map[string]string{"libraryid": libraryID}

	content := bytes.Repeat([]byte("repeated. "), 500)
	id := store.ChunkID(content)
	w := idReq(t, putChunkHandler, acct, http.MethodPut, "/chunks/"+id.String(),
		merge(vars, "id", id.String()), content, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("PUT chunk = %d, want 201", w.Code)
	}

	body, _ := json.Marshal(map[string]any{
		"chunks": []string{id.String(), id.String(), id.String()},
	})
	w = idReq(t, chunksFetchHandler, acct, http.MethodPost, "/chunks/fetch", vars, body, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("POST chunks/fetch = %d (%s), want 200", w.Code, w.Body.String())
	}
	frames, err := store.DecodeChunkFrames(w.Body.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != 1 {
		t.Fatalf("got %d frames for three mentions of one id, want 1", len(frames))
	}
}

// Over the cap is a refusal, not a truncation. A short answer that looks
// complete is the failure pagination was built to avoid, and this endpoint
// has no cursor to continue with.
func TestChunksFetchRefusesTooManyChunks(t *testing.T) {
	libraryID, acct := testLibrary(t)
	ids := make([]string, maxFetchChunks+1)
	for i := range ids {
		ids[i] = store.ChunkID([]byte{byte(i), byte(i >> 8)}).String()
	}
	body, _ := json.Marshal(map[string]any{"chunks": ids})
	w := idReq(t, chunksFetchHandler, acct, http.MethodPost, "/chunks/fetch",
		map[string]string{"libraryid": libraryID}, body, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("asking for %d chunks = %d (%s), want 400", len(ids), w.Code, w.Body.String())
	}
}
