package silod

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/dkam/silo/fileserver/repomgr"
	"github.com/dkam/silo/store"
)

// A batch whose last operation fails must leave the library exactly as it was.
// The operations before it succeeded against a working tree, and committing
// that tree would half-apply the batch — the one outcome a client cannot
// recover from, because it has no way to find out which half.
func TestAFailedBatchCommitsNothing(t *testing.T) {
	repoID, acct := storeV2Library(t)

	before, err := repomgr.GetWithReason(repoID)
	if err != nil {
		t.Fatal(err)
	}

	w := do(t, batchHandler, acct, http.MethodPost, "/batch",
		map[string]string{"repoid": repoID},
		[]byte(`{"ops":[{"op":"mkdir","path":"/made"},{"op":"delete","path":"/absent"}]}`))
	if w.Code != http.StatusNotFound {
		t.Fatalf("batch = %d (%s), want 404 on the second op", w.Code, w.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if idx, _ := out["index"].(float64); int(idx) != 1 {
		t.Errorf("index = %v, want 1 — a client needs to know which op failed", out["index"])
	}

	after, err := repomgr.GetWithReason(repoID)
	if err != nil {
		t.Fatal(err)
	}
	if after.HeadCommitID != before.HeadCommitID {
		t.Error("a failed batch moved the head")
	}
	if after.RootID != before.RootID {
		t.Error("a failed batch changed the tree")
	}

	// The directory the first op made must not be reachable.
	rr := do(t, getEntry, acct, http.MethodGet, "/entries/made",
		map[string]string{"repoid": repoID, "path": "made"}, nil)
	if rr.Code != http.StatusNotFound {
		t.Errorf("the first op's directory survived a failed batch: %d", rr.Code)
	}
}

// The ordinary case, and the two properties that make a batch worth having:
// operations see each other's effects, and they land in one commit.
func TestABatchAppliesInOrderAndCommitsOnce(t *testing.T) {
	repoID, acct := storeV2Library(t)
	before, err := repomgr.GetWithReason(repoID)
	if err != nil {
		t.Fatal(err)
	}

	w := do(t, batchHandler, acct, http.MethodPost, "/batch",
		map[string]string{"repoid": repoID},
		[]byte(`{"ops":[{"op":"mkdir","path":"/a"},{"op":"mkdir","path":"/a/b"},{"op":"mkdir","path":"/a/b/c"}]}`))
	if w.Code != http.StatusOK {
		t.Fatalf("batch = %d (%s), want 200", w.Code, w.Body.String())
	}

	after, err := repomgr.GetWithReason(repoID)
	if err != nil {
		t.Fatal(err)
	}
	st, err := after.Store()
	if err != nil {
		t.Fatal(err)
	}
	id, err := store.ParseID(after.HeadCommitID)
	if err != nil {
		t.Fatal(err)
	}
	commit, err := st.GetCommit(id)
	if err != nil {
		t.Fatal(err)
	}
	// One commit for three operations: the parent is the head the batch
	// started from, with nothing in between.
	if len(commit.Parents) != 1 || commit.Parents[0].String() != before.HeadCommitID {
		t.Errorf("a batch minted more than one commit: parents %v", commit.Parents)
	}

	// The third mkdir could only succeed if it saw the first two.
	rr := do(t, getEntry, acct, http.MethodGet, "/entries/a/b/c",
		map[string]string{"repoid": repoID, "path": "a/b/c"}, nil)
	if rr.Code != http.StatusOK {
		t.Errorf("the nested directory is not there: %d (%s)", rr.Code, rr.Body.String())
	}
}

// mkdir of a directory that already exists is 409 on a store-v2 library, where
// the Seafile lane treats it as satisfied and carries on.
//
// Pinned because it is a divergence between the two lanes rather than an
// accident, and because it has a consequence worth knowing: a batch is not
// idempotent under a lost response. A client whose batch committed but whose
// reply never arrived cannot simply send it again — the mkdir that succeeded
// will now refuse. Retrying after a *failure* is safe, because a failed batch
// commits nothing; retrying after a timeout is not.
//
// Making Mkdir satisfied-by-existing would fix that, but it is a change to
// what the mutation layer means, and porter is a second implementation of the
// same rules. So it is recorded here rather than changed in passing.
func TestBatchMkdirOfAnExistingDirectoryIsRefusedOnStoreV2(t *testing.T) {
	repoID, acct := storeV2Library(t)

	w := do(t, batchHandler, acct, http.MethodPost, "/batch",
		map[string]string{"repoid": repoID}, []byte(`{"ops":[{"op":"mkdir","path":"/x"}]}`))
	if w.Code != http.StatusOK {
		t.Fatalf("first mkdir = %d (%s)", w.Code, w.Body.String())
	}
	before, err := repomgr.GetWithReason(repoID)
	if err != nil {
		t.Fatal(err)
	}

	w = do(t, batchHandler, acct, http.MethodPost, "/batch",
		map[string]string{"repoid": repoID}, []byte(`{"ops":[{"op":"mkdir","path":"/x"}]}`))
	if w.Code != http.StatusConflict {
		t.Fatalf("second mkdir = %d (%s), want 409", w.Code, w.Body.String())
	}

	after, err := repomgr.GetWithReason(repoID)
	if err != nil {
		t.Fatal(err)
	}
	if after.HeadCommitID != before.HeadCommitID {
		t.Error("the refused batch still moved the head")
	}
}

// Resumable upload: chunks go up separately, then one call names them in
// order. The file that comes back has to be the file that went up.
func TestChunksNamedInOrderBecomeTheFile(t *testing.T) {
	repoID, acct := storeV2Library(t)

	parts := [][]byte{
		bytes.Repeat([]byte("alpha-"), 40000),
		bytes.Repeat([]byte("beta-"), 40000),
		bytes.Repeat([]byte("gamma-"), 40000),
	}
	ids := make([]string, 0, len(parts))
	var whole []byte
	for _, p := range parts {
		id := store.ChunkID(p)
		ids = append(ids, id.String())
		whole = append(whole, p...)
		w := idReq(t, putChunkHandler, acct, http.MethodPut, "/blocks/"+id.String(),
			map[string]string{"repoid": repoID, "id": id.String()}, p, nil)
		if w.Code != http.StatusCreated {
			t.Fatalf("PUT chunk = %d (%s)", w.Code, w.Body.String())
		}
	}

	body, _ := json.Marshal(map[string]any{"blocks": ids})
	w := do(t, putEntry, acct, http.MethodPut, "/entries/joined.dat?type=blocks",
		map[string]string{"repoid": repoID, "path": "joined.dat"}, body)
	if w.Code != http.StatusCreated {
		t.Fatalf("PUT ?type=blocks = %d (%s), want 201", w.Code, w.Body.String())
	}

	rr := do(t, getEntry, acct, http.MethodGet, "/entries/joined.dat",
		map[string]string{"repoid": repoID, "path": "joined.dat"}, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET = %d (%s)", rr.Code, rr.Body.String())
	}
	if !bytes.Equal(rr.Body.Bytes(), whole) {
		t.Errorf("the reassembled file is %d bytes, want %d", rr.Body.Len(), len(whole))
	}
}

// Naming a chunk the server does not hold is 424 with the list, not a file
// with a hole in it.
func TestNamingAnAbsentChunkIsRefused(t *testing.T) {
	repoID, acct := storeV2Library(t)
	absent := store.ChunkID([]byte("never uploaded")).String()
	body, _ := json.Marshal(map[string]any{"blocks": []string{absent}})

	w := do(t, putEntry, acct, http.MethodPut, "/entries/holey.dat?type=blocks",
		map[string]string{"repoid": repoID, "path": "holey.dat"}, body)
	if w.Code != http.StatusFailedDependency {
		t.Fatalf("naming an absent chunk = %d (%s), want 424", w.Code, w.Body.String())
	}
}

// A name the rest of the server would refuse must not enter a library through
// a batch. objmgr.SplitPath rejects only "." and "..", so length, encoding and
// the ignore list are this package's rule — and the batch was the one write
// path that did not apply it.
func TestABatchRefusesANameTheSingleOpPathWouldRefuse(t *testing.T) {
	repoID, acct := storeV2Library(t)
	before, err := repomgr.GetWithReason(repoID)
	if err != nil {
		t.Fatal(err)
	}

	long := strings.Repeat("n", 300)
	w := do(t, batchHandler, acct, http.MethodPost, "/batch",
		map[string]string{"repoid": repoID},
		[]byte(`{"ops":[{"op":"mkdir","path":"/fine"},{"op":"mkdir","path":"/`+long+`"}]}`))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("batch with a 300-character name = %d (%s), want 400", w.Code, w.Body.String())
	}

	after, err := repomgr.GetWithReason(repoID)
	if err != nil {
		t.Fatal(err)
	}
	if after.HeadCommitID != before.HeadCommitID {
		t.Error("a refused batch still moved the head")
	}
}

// An unknown verb is refused before any of the batch is applied, so a typo in
// the last operation of a long batch costs nothing — no manifests built, no
// tree rewritten, nothing to throw away.
func TestABatchRefusesAnUnknownOpBeforeApplyingAnything(t *testing.T) {
	repoID, acct := storeV2Library(t)
	before, err := repomgr.GetWithReason(repoID)
	if err != nil {
		t.Fatal(err)
	}

	w := do(t, batchHandler, acct, http.MethodPost, "/batch",
		map[string]string{"repoid": repoID},
		[]byte(`{"ops":[{"op":"mkdir","path":"/a"},{"op":"frobnicate","path":"/b"}]}`))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("batch with an unknown op = %d (%s), want 400", w.Code, w.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if idx, _ := out["index"].(float64); int(idx) != 1 {
		t.Errorf("index = %v, want 1", out["index"])
	}

	after, err := repomgr.GetWithReason(repoID)
	if err != nil {
		t.Fatal(err)
	}
	if after.HeadCommitID != before.HeadCommitID {
		t.Error("the head moved for a batch that was never applied")
	}
}

// The commit_id a batch reports is the commit it just minted.
//
// It used to be read back out of the library row after the swap, which is a
// query for a value the server had just written — and one that returns a
// different writer's commit if theirs lands in between.
func TestABatchReportsTheCommitItMinted(t *testing.T) {
	repoID, acct := storeV2Library(t)

	w := do(t, batchHandler, acct, http.MethodPost, "/batch",
		map[string]string{"repoid": repoID}, []byte(`{"ops":[{"op":"mkdir","path":"/d"}]}`))
	if w.Code != http.StatusOK {
		t.Fatalf("batch = %d (%s)", w.Code, w.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if changed, _ := out["changed"].(bool); !changed {
		t.Error("changed = false for a batch that made a directory")
	}

	after, err := repomgr.GetWithReason(repoID)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := out["commit_id"].(string); got != after.HeadCommitID {
		t.Errorf("commit_id = %q, want the new head %q", got, after.HeadCommitID)
	}
}
