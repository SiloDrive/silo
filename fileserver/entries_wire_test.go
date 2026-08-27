package silod

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dkam/silo/fileserver/account"
	"github.com/dkam/silo/fileserver/credential"
	"github.com/dkam/silo/fileserver/libmgr"
	"github.com/dkam/silo/fileserver/middleware"
	"github.com/dkam/silo/fileserver/option"
	"github.com/dkam/silo/fileserver/share"
	"github.com/dkam/silo/store"
	"github.com/gorilla/mux"
)

// testLibrary sets the package up with a throwaway database and one
// library, and returns its id and the account that owns it.
func testLibrary(t *testing.T) (string, *account.Account) {
	t.Helper()
	sqliteTestDB(t)
	share.Init(siloPair.Read, "Group", false)

	ctx := context.Background()
	if _, _, err := account.Create(ctx, "wire@example.com", "", false); err != nil {
		t.Fatalf("create account: %v", err)
	}
	acct, err := account.ByEmail(ctx, "wire@example.com")
	if err != nil {
		t.Fatalf("load account: %v", err)
	}

	libraryID, err := libmgr.CreateLibrary("Wire", acct, libmgr.DefaultFormat(false))
	if err != nil {
		t.Fatalf("CreateLibrary: %v", err)
	}
	return libraryID, acct
}

// do runs one request through a handler with the account and mux vars a real
// request would carry.
func do(t *testing.T, h http.HandlerFunc, acct *account.Account, method, target string, vars map[string]string, body []byte, opts ...func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	var rdr *bytes.Reader
	if body == nil {
		rdr = bytes.NewReader(nil)
	} else {
		rdr = bytes.NewReader(body)
	}
	r := httptest.NewRequest(method, target, rdr)
	r = mux.SetURLVars(r, vars)
	r = middleware.WithCredential(r, testCredential(acct), acct)
	for _, opt := range opts {
		opt(r)
	}
	w := httptest.NewRecorder()
	h(w, r)
	return w
}

// withHeader sets one header on a do request. Variadic rather than another
// positional parameter so the calls that need no header stay as they read now
// — and so the one that does need one stops rebuilding the request by hand,
// which is how it came to miss whatever do gains next.
func withHeader(k, v string) func(*http.Request) {
	return func(r *http.Request) { r.Header.Set(k, v) }
}

// A library takes a write and gives the same bytes back, at the whole
// file and at an offset. This is the round trip the HTTP surface exists for,
// and it is worth asserting here rather than only in objmgr: everything
// between — the format branch, the tree mutation, the commit and the head
// swap — is this package's, and objmgr's tests cannot see any of it.
func TestALibraryRoundTripsAFileOverHTTP(t *testing.T) {
	libraryID, acct := testLibrary(t)

	// Big enough to chunk rather than inline, so the manifest has a chunk list
	// and the ranged read has boundaries to get wrong.
	content := make([]byte, 3<<20)
	for i := range content {
		content[i] = byte(i * 7 % 251)
	}

	w := do(t, putEntry, acct, http.MethodPut, "/entries/big.bin",
		map[string]string{"libraryid": libraryID, "path": "big.bin"}, content)
	if w.Code != http.StatusCreated {
		t.Fatalf("PUT = %d (%s), want 201", w.Code, w.Body.String())
	}

	w = do(t, getEntry, acct, http.MethodGet, "/entries/big.bin",
		map[string]string{"libraryid": libraryID, "path": "big.bin"}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("GET = %d (%s), want 200", w.Code, w.Body.String())
	}
	if !bytes.Equal(w.Body.Bytes(), content) {
		t.Fatalf("GET returned %d bytes, want %d", w.Body.Len(), len(content))
	}

	// A range, across a chunk boundary wherever the chunker put one.
	const off, n = 1_000_000, 4096
	rw := do(t, getEntry, acct, http.MethodGet, "/entries/big.bin",
		map[string]string{"libraryid": libraryID, "path": "big.bin"}, nil,
		withHeader("Range", fmt.Sprintf("bytes=%d-%d", off, off+n-1)))
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
func TestAWriteMovesTheHeadToANewCommit(t *testing.T) {
	libraryID, acct := testLibrary(t)

	before, err := libmgr.GetWithReason(libraryID)
	if err != nil {
		t.Fatal(err)
	}

	w := do(t, putEntry, acct, http.MethodPut, "/entries/a.txt",
		map[string]string{"libraryid": libraryID, "path": "a.txt"}, []byte("hello"))
	if w.Code != http.StatusCreated {
		t.Fatalf("PUT = %d (%s), want 201", w.Code, w.Body.String())
	}

	after, err := libmgr.GetWithReason(libraryID)
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
func TestAWriteRefusesAMissingParent(t *testing.T) {
	libraryID, acct := testLibrary(t)

	w := do(t, putEntry, acct, http.MethodPut, "/entries/nope/a.txt",
		map[string]string{"libraryid": libraryID, "path": "nope/a.txt"}, []byte("hello"))
	if w.Code != http.StatusNotFound {
		t.Fatalf("PUT into a missing directory = %d, want 404", w.Code)
	}
}

// An upload over the limit is refused rather than truncated, whether or not
// the client declared its length.
//
// The chunked case is the one worth pinning. This lane took the LimitReader
// out of spoolBody and left both size checks behind, so an over-long body was
// cut at the limit, chunked, committed and answered 201 with an ETag — the
// client had every reason to believe the whole file was stored, and nothing
// anywhere said otherwise. A truncation that reports success is worse than a
// refusal, and there is no request this can be confused with: a client that
// declares no length is exactly the one that cannot be caught up front.
func TestAnUploadOverTheLimitIsRefusedNotTruncated(t *testing.T) {
	libraryID, acct := testLibrary(t)

	oldMax := option.MaxUploadSize
	option.MaxUploadSize = 64
	t.Cleanup(func() { option.MaxUploadSize = oldMax })

	content := bytes.Repeat([]byte("x"), 4096)
	vars := map[string]string{"libraryid": libraryID, "path": "big.bin"}

	for _, tc := range []struct {
		name string
		opts []func(*http.Request)
	}{
		{"declared length", nil},
		{"chunked", []func(*http.Request){func(r *http.Request) { r.ContentLength = -1 }}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := do(t, putEntry, acct, http.MethodPut, "/entries/big.bin", vars, content, tc.opts...)
			if w.Code != http.StatusRequestEntityTooLarge {
				t.Fatalf("PUT = %d (%s), want 413", w.Code, w.Body.String())
			}
			// And no path was created: a refused upload must not appear as a
			// short file, which is the failure mode a 413 alone would hide.
			g := do(t, getEntry, acct, http.MethodGet, "/entries/big.bin", vars, nil)
			if g.Code != http.StatusNotFound {
				t.Errorf("GET after refusal = %d (%d bytes), want 404", g.Code, g.Body.Len())
			}
		})
	}
}

// Writing a file over an existing directory is refused as a conflict, not
// reported as a server fault.
//
// writeTreeErr's whole reason for existing is that the set of objmgr sentinels
// deserving an answer other than 500 is fixed, and every handler was deciding
// it again. It had three arms; the batch had six; so this request answered 409
// inside a batch and 500 outside it, on the same library. One table now, and
// this is an arm that was missing from it.
func TestWritingAFileOverADirectoryIsAConflictNotAServerError(t *testing.T) {
	libraryID, acct := testLibrary(t)

	w := do(t, batchHandler, acct, http.MethodPost, "/batch",
		map[string]string{"libraryid": libraryID}, []byte(`{"ops":[{"op":"mkdir","path":"/d"}]}`))
	if w.Code != http.StatusOK {
		t.Fatalf("mkdir = %d (%s)", w.Code, w.Body.String())
	}

	vars := map[string]string{"libraryid": libraryID, "path": "d"}
	w = do(t, putEntry, acct, http.MethodPut, "/entries/d", vars, []byte("nope"))
	if w.Code != http.StatusConflict {
		t.Errorf("PUT over a directory = %d (%s), want 409", w.Code, w.Body.String())
	}
}

// testCredential is the unnarrowed credential a handler now reads its
// permission from. These tests authenticate directly rather than through the
// router, so they have to supply what RequireCredential would have: unscoped
// and rw, so what they measure is the account's own permission and not the
// ceiling, which has its own tests in ceiling_test.go.
func testCredential(acct *account.Account) *credential.Credential {
	return &credential.Credential{
		Kind: credential.KindSession, AccountID: acct.ID, Label: "test", Perm: "rw",
	}
}
