package silod

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dkam/silo/fileserver/account"
	"github.com/dkam/silo/fileserver/libmgr"
	"github.com/dkam/silo/store"
)

// QUERY entries/{path}: the manifest and the chunks covering a byte range, in
// one round trip.
//
// The two-request shape it replaces is GET objects/{id} then POST
// chunks/fetch, and those are serialised because the second cannot name its
// ids until the first lands. The first request fetches nothing — it asks where
// to look — and it is the round trip that happens before anything appears on
// screen. See Silo/silo#81.

// queryRanges runs one QUERY against a path and returns the recorder.
func queryRanges(t *testing.T, acct *account.Account, libraryID, path string, ranges [][2]int64) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(map[string]any{"ranges": ranges})
	if err != nil {
		t.Fatal(err)
	}
	return do(t, entriesHandler, acct, "QUERY", "/entries/"+path,
		map[string]string{"libraryid": libraryID, "path": path}, body)
}

// decodeRangeStream splits a response into the manifest frame and the chunk
// frames behind it, checking the framing on the way.
func decodeRangeStream(t *testing.T, w *httptest.ResponseRecorder) (*store.Manifest, []store.ChunkFrame) {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("QUERY = %d (%s), want 200", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Content-Type"); got != rangeStreamMediaType {
		t.Errorf("Content-Type = %q, want %q", got, rangeStreamMediaType)
	}
	frames, err := store.DecodeChunkFrames(w.Body.Bytes())
	if err != nil {
		t.Fatalf("DecodeChunkFrames: %v", err)
	}
	if len(frames) == 0 {
		t.Fatal("no frames at all; the manifest frame is always sent")
	}
	// The manifest leads, and it is an ordinary present frame: the decoder
	// above has already checked its bytes against the id it arrived under,
	// because a manifest id is the same SHA-256 over the same kind of thing a
	// chunk id is.
	if !frames[0].Present {
		t.Fatal("the manifest frame is marked absent")
	}
	m, err := store.DecodeManifest(frames[0].Bytes)
	if err != nil {
		t.Fatalf("the first frame does not decode as a manifest: %v", err)
	}
	return m, frames[1:]
}

// chunkableContent is test data the content-defined chunker will actually cut.
//
// Written out because the obvious filler does not work and fails in a way that
// looks like a bug in the endpoint: a repeating byte pattern gives the gear
// hash nothing to find, so every chunk runs to the 4 MiB maximum and an 8 MiB
// file arrives as two chunks. These tests are about sending some chunks of a
// file and not others, which needs a file with chunks to choose between.
func chunkableContent(t *testing.T, n int, seed int64) []byte {
	t.Helper()
	b := make([]byte, n)
	// Deterministic, so a failure reproduces rather than depending on which
	// boundaries this run happened to find.
	rnd := rand.New(rand.NewSource(seed))
	if _, err := rnd.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

// A chunked file: the manifest arrives with the chunks covering the range and
// with nothing else, and the bytes assemble to what a ranged GET returns.
func TestQueryEntryAnswersAManifestAndTheCoveringChunks(t *testing.T) {
	libraryID, acct := testLibrary(t)
	vars := map[string]string{"libraryid": libraryID, "path": "big.bin"}

	content := chunkableContent(t, 8<<20, 31)
	w := do(t, putEntry, acct, http.MethodPut, "/entries/big.bin", vars, content)
	if w.Code != http.StatusCreated {
		t.Fatalf("PUT = %d (%s), want 201", w.Code, w.Body.String())
	}

	const off, n = 3_000_000, 4096
	m, chunks := decodeRangeStream(t, queryRanges(t, acct, libraryID, "big.bin", [][2]int64{{off, n}}))

	if m.FileSize != int64(len(content)) {
		t.Fatalf("the manifest says %d bytes, the file is %d", m.FileSize, len(content))
	}
	if len(m.Chunks) < 4 {
		t.Fatalf("the file chunked into %d chunks; this test needs several to prove it sent only some", len(m.Chunks))
	}
	// The point of the endpoint: a 4 KB read does not cost the file.
	if len(chunks) >= len(m.Chunks) {
		t.Errorf("a %d-byte range brought back %d of the file's %d chunks", n, len(chunks), len(m.Chunks))
	}

	// Assembling the chunks in manifest order and cutting the range out of
	// them must give the same bytes the ranged GET does, which is the read
	// this replaces.
	held := make(map[store.ID][]byte, len(chunks))
	for _, f := range chunks {
		if !f.Present {
			t.Fatalf("chunk %s came back absent", f.ID)
		}
		held[f.ID] = f.Bytes
	}
	var assembled bytes.Buffer
	var pos int64
	for _, ref := range m.Chunks {
		next := pos + ref.Size
		if next > off && pos < off+n {
			b, ok := held[ref.ID]
			if !ok {
				t.Fatalf("chunk %s covers the range and was not sent", ref.ID)
			}
			lo, hi := int64(0), ref.Size
			if off > pos {
				lo = off - pos
			}
			if off+n < next {
				hi = off + n - pos
			}
			assembled.Write(b[lo:hi])
		}
		pos = next
	}
	if !bytes.Equal(assembled.Bytes(), content[off:off+n]) {
		t.Errorf("the assembled range is %d bytes and does not match the file", assembled.Len())
	}
}

// Two reads at opposite ends — a header and a tail atom, which is what a
// preview of a video actually does — and the middle of the file does not come
// with them.
func TestQueryEntryAnswersTwoRangesWithoutTheMiddle(t *testing.T) {
	libraryID, acct := testLibrary(t)
	vars := map[string]string{"libraryid": libraryID, "path": "video.mp4"}

	content := chunkableContent(t, 8<<20, 17)
	if w := do(t, putEntry, acct, http.MethodPut, "/entries/video.mp4", vars, content); w.Code != http.StatusCreated {
		t.Fatalf("PUT = %d (%s), want 201", w.Code, w.Body.String())
	}

	size := int64(len(content))
	m, chunks := decodeRangeStream(t, queryRanges(t, acct, libraryID, "video.mp4",
		[][2]int64{{0, 4096}, {size - 4096, 4096}}))

	if len(m.Chunks) < 4 {
		t.Fatalf("the file chunked into %d chunks; this test needs several", len(m.Chunks))
	}
	if len(chunks) > 2 {
		t.Errorf("two 4 KB reads brought back %d chunks, want at most 2", len(chunks))
	}
	if chunks[0].ID != m.Chunks[0].ID {
		t.Errorf("the first frame is %s, want the file's first chunk %s", chunks[0].ID, m.Chunks[0].ID)
	}
	last := m.Chunks[len(m.Chunks)-1].ID
	if chunks[len(chunks)-1].ID != last {
		t.Errorf("the last frame is %s, want the file's last chunk %s", chunks[len(chunks)-1].ID, last)
	}
}

// A file small enough to inline has no chunks at all, so the manifest frame
// carries the bytes and the answer is complete in one frame. That is the whole
// file in one round trip, which is the best case the endpoint has.
func TestQueryEntryOfAnInlineFileIsTheManifestAlone(t *testing.T) {
	libraryID, acct := testLibrary(t)
	vars := map[string]string{"libraryid": libraryID, "path": "small.txt"}

	content := []byte("small enough that its bytes live in its manifest")
	if w := do(t, putEntry, acct, http.MethodPut, "/entries/small.txt", vars, content); w.Code != http.StatusCreated {
		t.Fatalf("PUT = %d (%s), want 201", w.Code, w.Body.String())
	}

	m, chunks := decodeRangeStream(t, queryRanges(t, acct, libraryID, "small.txt", [][2]int64{{0, 16}}))
	if len(chunks) != 0 {
		t.Errorf("an inline file answered %d chunk frames, want none", len(chunks))
	}
	if !bytes.Equal(m.Inline, content) {
		t.Errorf("the manifest's inline bytes are %q, want %q", m.Inline, content)
	}
}

// A range starting past the end is not an error: the manifest in the same
// response is what says how long the file actually is, so the client learns
// the answer rather than a status. Nothing else in the response could have
// told it, which is why this is not a 416.
func TestQueryEntryAnswersARangePastTheEndWithTheManifest(t *testing.T) {
	libraryID, acct := testLibrary(t)
	vars := map[string]string{"libraryid": libraryID, "path": "short.bin"}

	content := make([]byte, 1<<20)
	if w := do(t, putEntry, acct, http.MethodPut, "/entries/short.bin", vars, content); w.Code != http.StatusCreated {
		t.Fatalf("PUT = %d (%s), want 201", w.Code, w.Body.String())
	}

	m, chunks := decodeRangeStream(t, queryRanges(t, acct, libraryID, "short.bin", [][2]int64{{1 << 30, 4096}}))
	if len(chunks) != 0 {
		t.Errorf("a range past the end brought back %d chunks, want none", len(chunks))
	}
	if m.FileSize != int64(len(content)) {
		t.Errorf("the manifest says %d bytes, want %d", m.FileSize, len(content))
	}
}

// The refusals. Each one is a 400 rather than a short answer, for the reason
// chunks/fetch caps rather than truncates: a response that looks complete and
// is not is worse than one that did not happen.
func TestQueryEntryRefusesWhatItCannotAnswer(t *testing.T) {
	libraryID, acct := testLibrary(t)

	content := make([]byte, 1<<20)
	if w := do(t, putEntry, acct, http.MethodPut, "/entries/f.bin",
		map[string]string{"libraryid": libraryID, "path": "f.bin"}, content); w.Code != http.StatusCreated {
		t.Fatalf("PUT = %d (%s), want 201", w.Code, w.Body.String())
	}
	if w := do(t, putEntry, acct, http.MethodPut, "/entries/d?type=dir",
		map[string]string{"libraryid": libraryID, "path": "d"}, nil); w.Code != http.StatusCreated {
		t.Fatalf("mkdir = %d (%s), want 201", w.Code, w.Body.String())
	}

	for _, tc := range []struct {
		name   string
		path   string
		ranges [][2]int64
		want   int
	}{
		{"no ranges at all", "f.bin", [][2]int64{}, http.StatusBadRequest},
		{"a negative offset", "f.bin", [][2]int64{{-1, 10}}, http.StatusBadRequest},
		{"a zero length", "f.bin", [][2]int64{{0, 0}}, http.StatusBadRequest},
		{"a negative length", "f.bin", [][2]int64{{0, -1}}, http.StatusBadRequest},
		{"a directory", "d", [][2]int64{{0, 10}}, http.StatusBadRequest},
		{"a path that is not there", "nope.bin", [][2]int64{{0, 10}}, http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := queryRanges(t, acct, libraryID, tc.path, tc.ranges)
			if w.Code != tc.want {
				t.Errorf("QUERY = %d (%s), want %d", w.Code, w.Body.String(), tc.want)
			}
		})
	}

	// A pair that is not a pair. Sent as raw JSON because the typed helper
	// cannot spell it.
	w := do(t, entriesHandler, acct, "QUERY", "/entries/f.bin",
		map[string]string{"libraryid": libraryID, "path": "f.bin"}, []byte(`{"ranges":[[1,2,3]]}`))
	if w.Code != http.StatusBadRequest {
		t.Errorf("a three-element range = %d (%s), want 400", w.Code, w.Body.String())
	}
}

// Over the chunk cap is a 400, not a short stream. The cap is chunks/fetch's,
// applied after the ranges are resolved: the two endpoints answer in the same
// framing and a client sizes one buffer for both.
func TestQueryEntryRefusesRangesOverTheChunkCap(t *testing.T) {
	libraryID, acct := testLibrary(t)
	vars := map[string]string{"libraryid": libraryID}

	// Built by hand rather than written: getting a real file to chunk into
	// exactly maxFetchChunks+1 pieces means a quarter of a gigabyte of test
	// data to make a point about arithmetic. The chunks are small and real —
	// the head swap walks the tree and refuses a commit naming a chunk the
	// library does not hold, which is the invariant that makes a manifest of
	// invented ids unreachable by any path a client has.
	library, err := libmgr.GetWithReason(libraryID)
	if err != nil {
		t.Fatal(err)
	}
	oldHead := library.HeadCommitID

	const chunkSize = 1024
	m := &store.Manifest{FileSize: chunkSize * (maxFetchChunks + 1)}
	for i := 0; i <= maxFetchChunks; i++ {
		body := bytes.Repeat([]byte(fmt.Sprintf("%04d", i)), chunkSize/4)
		id := store.ChunkID(body)
		if w := idReq(t, putChunkHandler, acct, http.MethodPut, "/chunks/"+id.String(),
			merge(vars, "id", id.String()), body, nil); w.Code != http.StatusCreated {
			t.Fatalf("PUT chunk %d = %d (%s), want 201", i, w.Code, w.Body.String())
		}
		m.Chunks = append(m.Chunks, store.ChunkRef{ID: id, Size: chunkSize})
	}
	manifestBytes, err := m.Encode()
	if err != nil {
		t.Fatal(err)
	}
	manifestID := store.ObjectID(manifestBytes)
	if w := idReq(t, putObjectHandler, acct, http.MethodPut, "/objects/"+manifestID.String(),
		merge(vars, "id", manifestID.String()), manifestBytes, nil); w.Code != http.StatusCreated {
		t.Fatalf("PUT manifest = %d (%s), want 201", w.Code, w.Body.String())
	}

	dir := &store.Directory{Entries: []store.DirEntry{{
		ChildID: manifestID, Type: store.NodeFile, Name: []byte("wide.bin"), Mode: 0o644,
	}}}
	dirBytes, err := dir.Encode()
	if err != nil {
		t.Fatal(err)
	}
	rootID := store.ObjectID(dirBytes)
	if w := idReq(t, putObjectHandler, acct, http.MethodPut, "/objects/"+rootID.String(),
		merge(vars, "id", rootID.String()), dirBytes, nil); w.Code != http.StatusCreated {
		t.Fatalf("PUT directory = %d (%s), want 201", w.Code, w.Body.String())
	}
	parent, err := store.ParseID(oldHead)
	if err != nil {
		t.Fatal(err)
	}
	commitBytes, commitID := buildCommit(t, rootID, []store.ID{parent}, "wire@example.com")
	if w := idReq(t, putObjectHandler, acct, http.MethodPut, "/objects/"+commitID.String(),
		merge(vars, "id", commitID.String()), commitBytes, nil); w.Code != http.StatusCreated {
		t.Fatalf("PUT commit = %d (%s), want 201", w.Code, w.Body.String())
	}
	if w := idReq(t, putHeadHandler, acct, http.MethodPut, "/head", vars,
		[]byte(commitID.String()), map[string]string{"If-Match": `"` + oldHead + `"`}); w.Code != http.StatusOK {
		t.Fatalf("PUT head = %d (%s), want 200", w.Code, w.Body.String())
	}

	// The whole file is one chunk over the cap.
	w := queryRanges(t, acct, libraryID, "wide.bin", [][2]int64{{0, m.FileSize}})
	if w.Code != http.StatusBadRequest {
		t.Errorf("a range over the cap = %d (%s), want 400", w.Code, w.Body.String())
	}

	// One chunk under it is answered in full: the cap refuses, it does not
	// truncate, so the boundary is a clean line rather than a shrinking
	// answer.
	w = queryRanges(t, acct, libraryID, "wide.bin", [][2]int64{{0, chunkSize * maxFetchChunks}})
	_, chunks := decodeRangeStream(t, w)
	if len(chunks) != maxFetchChunks {
		t.Fatalf("got %d chunk frames, want %d", len(chunks), maxFetchChunks)
	}
	for i, f := range chunks {
		if f.ID != m.Chunks[i].ID {
			t.Fatalf("frame %d is %s, want %s", i, f.ID, m.Chunks[i].ID)
		}
	}
}

// Too many ranges is refused before any of them is resolved: a body naming
// more spans than the cap can answer distinctly is a client bug, and the work
// of resolving them all to find that out is the work the cap exists to bound.
func TestQueryEntryRefusesTooManyRanges(t *testing.T) {
	libraryID, acct := testLibrary(t)
	vars := map[string]string{"libraryid": libraryID, "path": "f.bin"}
	if w := do(t, putEntry, acct, http.MethodPut, "/entries/f.bin", vars, make([]byte, 1<<20)); w.Code != http.StatusCreated {
		t.Fatalf("PUT = %d (%s), want 201", w.Code, w.Body.String())
	}

	ranges := make([][2]int64, maxQueryRanges+1)
	for i := range ranges {
		ranges[i] = [2]int64{int64(i), 1}
	}
	w := queryRanges(t, acct, libraryID, "f.bin", ranges)
	if w.Code != http.StatusBadRequest && w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("QUERY with %d ranges = %d (%s), want 400 or 413", len(ranges), w.Code, w.Body.String())
	}
}

// An unsupported method still names what is allowed, and QUERY is now among
// them. A 404 here would read as "wrong path" to a client whose only mistake
// was the verb.
func TestEntriesAllowHeaderNamesQuery(t *testing.T) {
	libraryID, acct := testLibrary(t)
	w := do(t, entriesHandler, acct, "PATCH", "/entries/f.bin",
		map[string]string{"libraryid": libraryID, "path": "f.bin"}, nil)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("PATCH = %d, want 405", w.Code)
	}
	if got := w.Header().Get("Allow"); !bytes.Contains([]byte(got), []byte("QUERY")) {
		t.Errorf("Allow = %q, want QUERY named in it", got)
	}
}

// The same request over a real socket and a real router, because everything
// above this line calls the handler directly and so proves nothing about the
// verb. QUERY has no constant in net/http and no route of its own here: it
// reaches entriesHandler because the entries route is registered for every
// method, and it reaches the process at all because Go's server takes any
// valid method token. Both of those are assumptions, and this is where they
// are checked.
func TestQueryEntryAnswersOverTheWire(t *testing.T) {
	base, token := wire(t)
	libraryID := makeLibrary(t, base, token)

	content := chunkableContent(t, 4<<20, 7)
	entry := base + "/api/silo/v1/libraries/" + libraryID + "/entries/wire.bin"
	req, err := http.NewRequest(http.MethodPut, entry, bytes.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("PUT = %d, want 201", resp.StatusCode)
	}

	req, err = http.NewRequest("QUERY", entry, strings.NewReader(`{"ranges":[[0,4096]]}`))
	if err != nil {
		t.Fatalf("net/http refused to build a QUERY request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("QUERY: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("QUERY = %d (%s), want 200", resp.StatusCode, body)
	}
	if got := resp.Header.Get("Content-Type"); got != rangeStreamMediaType {
		t.Errorf("Content-Type = %q, want %q", got, rangeStreamMediaType)
	}
	frames, err := store.DecodeChunkFrames(body)
	if err != nil {
		t.Fatalf("DecodeChunkFrames: %v", err)
	}
	if len(frames) < 2 {
		t.Fatalf("got %d frames, want a manifest and at least one chunk", len(frames))
	}
	m, err := store.DecodeManifest(frames[0].Bytes)
	if err != nil {
		t.Fatalf("the first frame does not decode as a manifest: %v", err)
	}
	if !bytes.Equal(frames[1].Bytes[:4096], content[:4096]) {
		t.Error("the first chunk does not open the file")
	}
	if m.FileSize != int64(len(content)) {
		t.Errorf("the manifest says %d bytes, want %d", m.FileSize, len(content))
	}
}

// An E2EE library refuses the QUERY exactly as it refuses the GET beside it,
// and for the reason resolve cannot be talked out of: the server holds no
// content key, so it cannot match a plaintext path segment against the sealed
// names a directory object carries. It does not know whether the file is
// there.
//
// The refusal has to be 403 and not 404 for the reason
// TestReadingAnEncryptedLibraryByPathIsNotALie records — a client told the
// file does not exist is being lied to about the library's contents — and it
// has to be checked here rather than assumed from the GET, because this is a
// second entry point into the same walk and a second chance to flatten the
// error on the way out.
func TestQueryOnAnEncryptedLibraryIsRefusedNotDenied(t *testing.T) {
	base, token, km := enrolled(t)
	seed := mintSeed(t, km.Public)
	if code, body := call(t, "POST", base+"/api/silo/v1/libraries", token, seed.body(t, "Sealed")); code != http.StatusOK && code != http.StatusCreated {
		t.Fatalf("creating the encrypted library: status %d, body %s", code, body)
	}

	code, body := call(t, "QUERY",
		base+"/api/silo/v1/libraries/"+seed.LibraryID+"/entries/notes.txt",
		token, `{"ranges":[[0,4096]]}`)

	if code == http.StatusNotFound {
		t.Fatalf("a QUERY on an E2EE library answered 404 %q; the server cannot know whether the file exists",
			strings.TrimSpace(body))
	}
	if code != http.StatusForbidden {
		t.Fatalf("status %d, want 403; body %s", code, body)
	}
	if !strings.Contains(strings.ToLower(body), "end-to-end encrypted") {
		t.Errorf("the refusal does not say why: %q", strings.TrimSpace(body))
	}
}

// ?at= resolves against an old tree here exactly as it does on the GET.
//
// It is worth pinning rather than assuming, because it is not written down in
// queryEntry as a feature so much as inherited: the handler calls the same
// rootFor the GET does, and a refactor that resolved from library.RootID
// directly would silently start answering the present for every request that
// asked about the past. A client reading a range of an old version would get
// bytes from the current one, under a manifest that decodes perfectly.
func TestQueryEntryReadsAPastCommit(t *testing.T) {
	libraryID, acct := testLibrary(t)
	vars := map[string]string{"libraryid": libraryID, "path": "f.bin"}

	first := chunkableContent(t, 2<<20, 3)
	if w := do(t, putEntry, acct, http.MethodPut, "/entries/f.bin", vars, first); w.Code != http.StatusCreated {
		t.Fatalf("first PUT = %d (%s), want 201", w.Code, w.Body.String())
	}
	library, err := libmgr.GetWithReason(libraryID)
	if err != nil {
		t.Fatal(err)
	}
	was := library.HeadCommitID

	second := chunkableContent(t, 5<<20, 4)
	if w := do(t, putEntry, acct, http.MethodPut, "/entries/f.bin", vars, second); w.Code != http.StatusOK && w.Code != http.StatusCreated {
		t.Fatalf("second PUT = %d (%s), want 200 or 201", w.Code, w.Body.String())
	}

	body, err := json.Marshal(map[string]any{"ranges": [][2]int64{{0, 4096}}})
	if err != nil {
		t.Fatal(err)
	}
	w := do(t, entriesHandler, acct, "QUERY", "/entries/f.bin?at="+was, vars, body)
	m, chunks := decodeRangeStream(t, w)
	if m.FileSize != int64(len(first)) {
		t.Errorf("at=%s answered a manifest of %d bytes, want the first version's %d", was, m.FileSize, len(first))
	}
	if len(chunks) == 0 || !bytes.Equal(chunks[0].Bytes[:4096], first[:4096]) {
		t.Error("the chunks are not the first version's")
	}
}
