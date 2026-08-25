package silod

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/dkam/silo/fileserver/account"
	"github.com/dkam/silo/fileserver/libmgr"
	"github.com/dkam/silo/fileserver/middleware"
	"github.com/dkam/silo/store"
	"github.com/gorilla/mux"
)

// idReq runs one request against the id-addressed surface.
func idReq(t *testing.T, h http.HandlerFunc, acct *account.Account, method, target string,
	vars map[string]string, body []byte, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, target, bytes.NewReader(body))
	r = mux.SetURLVars(r, vars)
	r = middleware.WithAccount(r, acct)
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h(w, r)
	return w
}

// A client writes a whole change without the server ever chunking, naming or
// encoding anything: chunk, manifest, root directory, commit, then one
// compare-and-swap on the head. This is the only path an end-to-end encrypted
// library has, so it has to work end to end — and it is exercised here on a
// plain library precisely so the server can read the result back and prove the
// change actually landed, which on an encrypted one it could not.
func TestALibraryCanBeWrittenEntirelyByID(t *testing.T) {
	libraryID, acct := storeV2Library(t)
	vars := map[string]string{"libraryid": libraryID}
	library, err := libmgr.GetWithReason(libraryID)
	if err != nil {
		t.Fatal(err)
	}
	oldHead := library.HeadCommitID

	// One chunk, small enough that the manifest would inline it — so this
	// deliberately builds a chunked manifest by hand, the shape a real file
	// takes, rather than the shape the size would choose.
	content := bytes.Repeat([]byte("id-addressed. "), 8192)
	chunkID := store.ChunkID(content)

	w := idReq(t, putChunkHandler, acct, http.MethodPut, "/blocks/"+chunkID.String(),
		merge(vars, "id", chunkID.String()), content, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("PUT chunk = %d (%s), want 201", w.Code, w.Body.String())
	}

	m := &store.Manifest{
		FileSize: int64(len(content)),
		Chunks:   []store.ChunkRef{{ID: chunkID, Size: int64(len(content))}},
	}
	manifestBytes, err := m.Encode()
	if err != nil {
		t.Fatal(err)
	}
	manifestID := store.ObjectID(manifestBytes)
	w = idReq(t, putObjectHandler, acct, http.MethodPut, "/objects/"+manifestID.String(),
		merge(vars, "id", manifestID.String()), manifestBytes, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("PUT manifest = %d (%s), want 201", w.Code, w.Body.String())
	}

	// A new root naming that manifest, then a commit naming the root.
	dir := &store.Directory{Entries: []store.DirEntry{{
		ChildID: manifestID, Type: store.NodeFile, Name: []byte("byid.txt"), Mode: 0o644,
	}}}
	dirBytes, err := dir.Encode()
	if err != nil {
		t.Fatal(err)
	}
	rootID := store.ObjectID(dirBytes)
	w = idReq(t, putObjectHandler, acct, http.MethodPut, "/objects/"+rootID.String(),
		merge(vars, "id", rootID.String()), dirBytes, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("PUT directory = %d (%s), want 201", w.Code, w.Body.String())
	}

	parent, err := store.ParseID(oldHead)
	if err != nil {
		t.Fatal(err)
	}
	commitBytes, commitID := buildCommit(t, rootID, []store.ID{parent}, "someone@example.com")
	w = idReq(t, putObjectHandler, acct, http.MethodPut, "/objects/"+commitID.String(),
		merge(vars, "id", commitID.String()), commitBytes, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("PUT commit = %d (%s), want 201", w.Code, w.Body.String())
	}

	// Nothing has changed yet: objects are inert until the head names them.
	mid, err := libmgr.GetWithReason(libraryID)
	if err != nil {
		t.Fatal(err)
	}
	if mid.HeadCommitID != oldHead {
		t.Fatal("uploading objects moved the head; it must take a head swap")
	}

	w = idReq(t, putHeadHandler, acct, http.MethodPut, "/head", vars,
		[]byte(commitID.String()), map[string]string{"If-Match": `"` + oldHead + `"`})
	if w.Code != http.StatusOK {
		t.Fatalf("PUT head = %d (%s), want 200", w.Code, w.Body.String())
	}

	// The change is visible through the ordinary path surface, which is the
	// proof that the id lane and the entries lane are one library.
	after, err := libmgr.GetWithReason(libraryID)
	if err != nil {
		t.Fatal(err)
	}
	if after.HeadCommitID != commitID.String() {
		t.Fatalf("head = %s, want %s", after.HeadCommitID, commitID)
	}
	rr := do(t, getEntry, acct, http.MethodGet, "/entries/byid.txt",
		map[string]string{"libraryid": libraryID, "path": "byid.txt"}, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET the written file = %d (%s), want 200", rr.Code, rr.Body.String())
	}
	if !bytes.Equal(rr.Body.Bytes(), content) {
		t.Errorf("the file read back as %d bytes, want %d", rr.Body.Len(), len(content))
	}
}

// A head swap that names a head somebody else has already replaced is refused.
// This is the case server-side merge used to absorb, and refusing it is the
// whole reason the surface exists in this shape.
func TestAStaleHeadSwapIsRefused(t *testing.T) {
	libraryID, acct := storeV2Library(t)
	vars := map[string]string{"libraryid": libraryID}
	library, err := libmgr.GetWithReason(libraryID)
	if err != nil {
		t.Fatal(err)
	}
	root, err := store.ParseID(library.RootID)
	if err != nil {
		t.Fatal(err)
	}
	parent, err := store.ParseID(library.HeadCommitID)
	if err != nil {
		t.Fatal(err)
	}

	commitBytes, commitID := buildCommit(t, root, []store.ID{parent}, "a@example.com")
	w := idReq(t, putObjectHandler, acct, http.MethodPut, "/objects/"+commitID.String(),
		merge(vars, "id", commitID.String()), commitBytes, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("PUT commit = %d, want 201", w.Code)
	}

	// A head that is not the current one: right shape, wrong value.
	stale := store.ObjectID([]byte("not the head")).String()
	w = idReq(t, putHeadHandler, acct, http.MethodPut, "/head", vars,
		[]byte(commitID.String()), map[string]string{"If-Match": `"` + stale + `"`})
	if w.Code != http.StatusConflict {
		t.Fatalf("swap against a stale head = %d (%s), want 409 — it does not descend from what was named",
			w.Code, w.Body.String())
	}
}

// A commit with no parent would replace a library's whole history in one call.
// It has to be refused however well formed it is.
func TestAHeadSwapToACommitThatDoesNotDescendIsRefused(t *testing.T) {
	libraryID, acct := storeV2Library(t)
	vars := map[string]string{"libraryid": libraryID}
	library, err := libmgr.GetWithReason(libraryID)
	if err != nil {
		t.Fatal(err)
	}
	root, err := store.ParseID(library.RootID)
	if err != nil {
		t.Fatal(err)
	}

	commitBytes, commitID := buildCommit(t, root, nil, "a@example.com")
	w := idReq(t, putObjectHandler, acct, http.MethodPut, "/objects/"+commitID.String(),
		merge(vars, "id", commitID.String()), commitBytes, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("PUT commit = %d, want 201", w.Code)
	}
	w = idReq(t, putHeadHandler, acct, http.MethodPut, "/head", vars,
		[]byte(commitID.String()), map[string]string{"If-Match": `"` + library.HeadCommitID + `"`})
	if w.Code != http.StatusConflict {
		t.Fatalf("swap to a parentless commit = %d (%s), want 409", w.Code, w.Body.String())
	}
}

// A head may only name a commit whose objects are already here. Otherwise a
// library can be pointed at a root that does not exist, and every reader after
// that gets damage rather than an answer.
func TestAHeadSwapToAnUnknownCommitIsRefused(t *testing.T) {
	libraryID, acct := storeV2Library(t)
	library, err := libmgr.GetWithReason(libraryID)
	if err != nil {
		t.Fatal(err)
	}
	absent := store.ObjectID([]byte("never uploaded")).String()
	w := idReq(t, putHeadHandler, acct, http.MethodPut, "/head", map[string]string{"libraryid": libraryID},
		[]byte(absent), map[string]string{"If-Match": `"` + library.HeadCommitID + `"`})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("swap to an absent commit = %d (%s), want 400", w.Code, w.Body.String())
	}
}

// buildInlineFileCommit uploads a one-file tree by id — an inline manifest,
// a root directory naming it, and a commit naming that root descending from
// the library's current head — and returns the swap the caller still has to
// make: the head it descends from, and the commit id PUT head would move to.
// Nothing is published; the tree exists but the head does not name it yet.
func buildInlineFileCommit(t *testing.T, acct *account.Account, libraryID, name string, content []byte) (oldHead string, commitID store.ID) {
	t.Helper()
	vars := map[string]string{"libraryid": libraryID}
	library, err := libmgr.GetWithReason(libraryID)
	if err != nil {
		t.Fatal(err)
	}
	oldHead = library.HeadCommitID

	m := &store.Manifest{FileSize: int64(len(content)), Inline: content}
	manifestBytes, err := m.Encode()
	if err != nil {
		t.Fatal(err)
	}
	manifestID := store.ObjectID(manifestBytes)
	w := idReq(t, putObjectHandler, acct, http.MethodPut, "/objects/"+manifestID.String(),
		merge(vars, "id", manifestID.String()), manifestBytes, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("PUT manifest = %d (%s), want 201", w.Code, w.Body.String())
	}

	dir := &store.Directory{Entries: []store.DirEntry{{
		ChildID: manifestID, Type: store.NodeFile, Name: []byte(name), Mode: 0o644,
	}}}
	dirBytes, err := dir.Encode()
	if err != nil {
		t.Fatal(err)
	}
	rootID := store.ObjectID(dirBytes)
	w = idReq(t, putObjectHandler, acct, http.MethodPut, "/objects/"+rootID.String(),
		merge(vars, "id", rootID.String()), dirBytes, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("PUT directory = %d (%s), want 201", w.Code, w.Body.String())
	}

	parent, err := store.ParseID(oldHead)
	if err != nil {
		t.Fatal(err)
	}
	commitBytes, commitID := buildCommit(t, rootID, []store.ID{parent}, acct.Email)
	w = idReq(t, putObjectHandler, acct, http.MethodPut, "/objects/"+commitID.String(),
		merge(vars, "id", commitID.String()), commitBytes, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("PUT commit = %d (%s), want 201", w.Code, w.Body.String())
	}
	return oldHead, commitID
}

// putHeadHandler publishes a tree without ever asking whether the owner
// could still afford it — the per-object checks a client passed on the way
// up are only an estimate, and nothing charged the exact number at the
// moment that estimate is supposed to be replaced. This is that charge.
func TestPutHeadRefusesAHeadMoveThatWouldExceedQuota(t *testing.T) {
	libraryID, acct := storeV2Library(t)
	setQuota(t, acct, 1000)

	oldHead, commitID := buildInlineFileCommit(t, acct, libraryID, "big.bin", bytes.Repeat([]byte("a"), 1500))

	vars := map[string]string{"libraryid": libraryID}
	w := idReq(t, putHeadHandler, acct, http.MethodPut, "/head", vars,
		[]byte(commitID.String()), map[string]string{"If-Match": `"` + oldHead + `"`})
	if w.Code != http.StatusInsufficientStorage {
		t.Fatalf("head move over quota = %d (%s), want %d", w.Code, w.Body.String(), http.StatusInsufficientStorage)
	}

	after, err := libmgr.GetWithReason(libraryID)
	if err != nil {
		t.Fatal(err)
	}
	if after.HeadCommitID != oldHead {
		t.Error("the refused head move published the tree anyway")
	}
}

// Two libraries under one owner, each individually well under quota, but
// together over it. checkQuotaV2's usage read used to race a concurrent one
// with nothing serializing the sequence, so two head moves — the point
// where usage is actually charged — could each read the account's usage
// before either had committed and both be admitted. lockOwner (quota_v2.go)
// closes it by holding the owner's admission lock across the commit, not
// only the read.
func TestConcurrentHeadMovesCannotJointlyExceedQuota(t *testing.T) {
	libraryA, acct := storeV2Library(t)
	libraryB, err := libmgr.CreateLibrary("v2b", acct, libmgr.DefaultFormat(false))
	if err != nil {
		t.Fatal(err)
	}
	setQuota(t, acct, 1000)

	oldHeadA, commitA := buildInlineFileCommit(t, acct, libraryA, "a.bin", bytes.Repeat([]byte("a"), 600))
	oldHeadB, commitB := buildInlineFileCommit(t, acct, libraryB, "b.bin", bytes.Repeat([]byte("b"), 600))

	type swap struct {
		libraryID, oldHead string
		commitID           store.ID
	}
	swaps := []swap{{libraryA, oldHeadA, commitA}, {libraryB, oldHeadB, commitB}}

	var wg sync.WaitGroup
	codes := make([]int, len(swaps))
	for i, sw := range swaps {
		wg.Add(1)
		go func(i int, sw swap) {
			defer wg.Done()
			w := idReq(t, putHeadHandler, acct, http.MethodPut, "/head",
				map[string]string{"libraryid": sw.libraryID}, []byte(sw.commitID.String()),
				map[string]string{"If-Match": `"` + sw.oldHead + `"`})
			codes[i] = w.Code
		}(i, sw)
	}
	wg.Wait()

	admitted := 0
	for _, code := range codes {
		switch code {
		case http.StatusOK:
			admitted++
		case http.StatusInsufficientStorage:
		default:
			t.Fatalf("head move answered %d, want 200 or %d", code, http.StatusInsufficientStorage)
		}
	}
	if admitted > 1 {
		t.Fatal("both concurrent head moves were admitted; 600+600 exceeds the 1000 quota")
	}

	u, err := libmgr.AccountUsage(acct.ID)
	if err != nil {
		t.Fatal(err)
	}
	if u.Size > 1000 {
		t.Fatalf("account usage after the race is %d, over the 1000 quota", u.Size)
	}
}

func buildCommit(t *testing.T, root store.ID, parents []store.ID, author string) ([]byte, store.ID) {
	t.Helper()
	c := &store.Commit{Root: root, Parents: parents, CreatedAt: 1700000000, Author: author}
	b, err := c.Encode()
	if err != nil {
		t.Fatal(err)
	}
	return b, store.ObjectID(b)
}

func merge(base map[string]string, k, v string) map[string]string {
	out := make(map[string]string, len(base)+1)
	for bk, bv := range base {
		out[bk] = bv
	}
	out[k] = v
	return out
}
