package silod

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dkam/silo/fileserver/account"
	"github.com/dkam/silo/fileserver/middleware"
	"github.com/dkam/silo/fileserver/repomgr"
	"github.com/dkam/silo/fileserver/share"
	"github.com/dkam/silo/store"
	"github.com/gorilla/mux"
)

// storeV2Library sets the package up with a throwaway database and one
// store-v2 library, and returns its id and the account that owns it.
func storeV2Library(t *testing.T) (string, *account.Account) {
	t.Helper()
	sqliteTestDB(t)

	dir := t.TempDir()
	absDataDir = dir
	repomgr.Init(siloPair.Read, siloPair.Write, dir)
	share.Init(siloPair.Read, "Group", false)

	ctx := context.Background()
	if _, _, err := account.Create(ctx, "v2@example.com", "", false); err != nil {
		t.Fatalf("create account: %v", err)
	}
	acct, err := account.ByEmail(ctx, "v2@example.com")
	if err != nil {
		t.Fatalf("load account: %v", err)
	}

	repoID, err := repomgr.CreateRepo("v2", acct, repomgr.DefaultFormat(false))
	if err != nil {
		t.Fatalf("CreateRepo: %v", err)
	}
	return repoID, acct
}

// do runs one request through a handler with the account and mux vars a real
// request would carry.
func do(t *testing.T, h http.HandlerFunc, acct *account.Account, method, target string, vars map[string]string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	var rdr *bytes.Reader
	if body == nil {
		rdr = bytes.NewReader(nil)
	} else {
		rdr = bytes.NewReader(body)
	}
	r := httptest.NewRequest(method, target, rdr)
	r = mux.SetURLVars(r, vars)
	r = middleware.WithAccount(r, acct)
	w := httptest.NewRecorder()
	h(w, r)
	return w
}

// A store-v2 library takes a write and gives the same bytes back, at the whole
// file and at an offset. This is the round trip the HTTP surface exists for,
// and it is worth asserting here rather than only in objmgr: everything
// between — the format branch, the tree mutation, the commit and the head
// swap — is this package's, and objmgr's tests cannot see any of it.
func TestAStoreV2LibraryRoundTripsAFileOverHTTP(t *testing.T) {
	repoID, acct := storeV2Library(t)

	// Big enough to chunk rather than inline, so the manifest has a chunk list
	// and the ranged read has boundaries to get wrong.
	content := make([]byte, 3<<20)
	for i := range content {
		content[i] = byte(i * 7 % 251)
	}

	w := do(t, putEntry, acct, http.MethodPut, "/entries/big.bin",
		map[string]string{"repoid": repoID, "path": "big.bin"}, content)
	if w.Code != http.StatusCreated {
		t.Fatalf("PUT = %d (%s), want 201", w.Code, w.Body.String())
	}

	w = do(t, getEntry, acct, http.MethodGet, "/entries/big.bin",
		map[string]string{"repoid": repoID, "path": "big.bin"}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("GET = %d (%s), want 200", w.Code, w.Body.String())
	}
	if !bytes.Equal(w.Body.Bytes(), content) {
		t.Fatalf("GET returned %d bytes, want %d", w.Body.Len(), len(content))
	}

	// A range, across a chunk boundary wherever the chunker put one.
	const off, n = 1_000_000, 4096
	r := httptest.NewRequest(http.MethodGet, "/entries/big.bin", nil)
	r = mux.SetURLVars(r, map[string]string{"repoid": repoID, "path": "big.bin"})
	r = middleware.WithAccount(r, acct)
	r.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", off, off+n-1))
	rw := httptest.NewRecorder()
	getEntry(rw, r)
	if rw.Code != http.StatusPartialContent {
		t.Fatalf("ranged GET = %d, want 206", rw.Code)
	}
	if !bytes.Equal(rw.Body.Bytes(), content[off:off+n]) {
		t.Errorf("ranged GET returned the wrong bytes")
	}
	if got, want := rw.Header().Get("Content-Range"),
		fmt.Sprintf("bytes %d-%d/%d", off, off+n-1, len(content)); got != want {
		t.Errorf("Content-Range = %q, want %q", got, want)
	}
}

// The head must actually move, and the objects it names must be on disk. A
// round trip alone would pass if the read were served from anything cached.
func TestAStoreV2WriteMovesTheHeadToANewCommit(t *testing.T) {
	repoID, acct := storeV2Library(t)

	before, err := repomgr.GetWithReason(repoID)
	if err != nil {
		t.Fatal(err)
	}

	w := do(t, putEntry, acct, http.MethodPut, "/entries/a.txt",
		map[string]string{"repoid": repoID, "path": "a.txt"}, []byte("hello"))
	if w.Code != http.StatusCreated {
		t.Fatalf("PUT = %d (%s), want 201", w.Code, w.Body.String())
	}

	after, err := repomgr.GetWithReason(repoID)
	if err != nil {
		t.Fatal(err)
	}
	if after.HeadCommitID == before.HeadCommitID {
		t.Fatal("the head did not move")
	}
	if after.RootID == before.RootID {
		t.Fatal("the root did not change")
	}

	// GetWithReason stats the head object, so reaching here at all proves the
	// commit was written before the branch pointed at it. The parent link is
	// what this checks: history has to chain, not restart.
	st, err := repomgr.OpenStore(after.StoreID, after.Format)
	if err != nil {
		t.Fatal(err)
	}
	id, err := store.ParseID(after.HeadCommitID)
	if err != nil {
		t.Fatal(err)
	}
	commit, err := st.GetCommit(id)
	if err != nil {
		t.Fatalf("head commit did not decode: %v", err)
	}
	if len(commit.Parents) != 1 || commit.Parents[0].String() != before.HeadCommitID {
		t.Errorf("commit parents = %v, want the previous head %s", commit.Parents, before.HeadCommitID)
	}
	if commit.Author != acct.Email {
		t.Errorf("author = %q, want %q", commit.Author, acct.Email)
	}
}

// A write into a directory that is not there is a 404, not a silent mkdir -p.
func TestAStoreV2WriteRefusesAMissingParent(t *testing.T) {
	repoID, acct := storeV2Library(t)

	w := do(t, putEntry, acct, http.MethodPut, "/entries/nope/a.txt",
		map[string]string{"repoid": repoID, "path": "nope/a.txt"}, []byte("hello"))
	if w.Code != http.StatusNotFound {
		t.Fatalf("PUT into a missing directory = %d, want 404", w.Code)
	}
}
