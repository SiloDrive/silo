package silod

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dkam/silo/fileserver/account"
	"github.com/dkam/silo/fileserver/option"
	"github.com/dkam/silo/fileserver/share"
)

// syncAuthTestDB adds what the sync endpoints need to answer an authenticated
// request: repomgr and share wired up against the test database, and the auth
// caches cleared so one test cannot see another's decisions.
func syncAuthTestDB(t *testing.T) {
	t.Helper()

	sqliteTestDB(t)
	share.Init(siloPair.Read, "Group", false)

	origTTL := option.AuthCacheTTL
	option.AuthCacheTTL = 5 * time.Minute
	clearAuthCaches()

	t.Cleanup(func() {
		option.AuthCacheTTL = origTTL
		clearAuthCaches()
	})
}

const (
	repoA = "b1f2ad61-9164-418a-a47f-ab805dbd5694"
	repoB = "c2e3be72-a275-429b-b580-bc916ece6705"
	// Valid UUID, deliberately absent from Branch.
	repoMissing = "d3f4cf83-b386-43ac-c691-cda27fdf7816"
	// Owned by someone else entirely.
	repoTheirs = "e4f5df94-c497-44bd-d7a2-deb38fef8927"

	headA = "0401fc662e3bc87a41f299a907c056aaf8322a27"
	headB = "1512ad773f4cd98b52e3aab018d167bb09433b38"

	owner       = "owner@example.com"
	ownerToken  = "1111aaaa1111aaaa1111aaaa1111aaaa1111aaaa"
	outsider    = "outsider@example.com"
	outsiderTok = "2222bbbb2222bbbb2222bbbb2222bbbb2222bbbb"
)

// seedHeadCommitsMulti gives owner two libraries with heads, a third owned by
// someone else, and a sync token for each user.
func seedHeadCommitsMulti(t *testing.T) {
	t.Helper()
	syncAuthTestDB(t)

	dbExec(t, "INSERT INTO Branch (name, repo_id, commit_id) VALUES (?, ?, ?)", "master", repoA, headA)
	dbExec(t, "INSERT INTO Branch (name, repo_id, commit_id) VALUES (?, ?, ?)", "master", repoB, headB)
	// A non-master branch on repoA must not be returned as its head.
	dbExec(t, "INSERT INTO Branch (name, repo_id, commit_id) VALUES (?, ?, ?)",
		"other", repoA, "2623be884e5dea9c63f4bbc129e278cc1a544c49")
	dbExec(t, "INSERT INTO Branch (name, repo_id, commit_id) VALUES (?, ?, ?)",
		"master", repoTheirs, "3734cf995f6efbad74a5ccd23af389dd2b655d5a")

	ownerAcct := mintAccount(t, owner)
	outsiderAcct := mintAccount(t, outsider)

	for _, r := range []struct {
		repoID string
		acct   *account.Account
	}{
		{repoA, ownerAcct}, {repoB, ownerAcct}, {repoTheirs, outsiderAcct},
	} {
		dbExec(t, "INSERT INTO RepoOwner (repo_id, account_id) VALUES (?, ?)", r.repoID, r.acct.ID)
	}

	now := time.Now().Unix()
	dbExec(t, "INSERT INTO RepoUserToken (repo_id, account_id, token, ctime) VALUES (?, ?, ?, ?)",
		repoA, ownerAcct.ID, ownerToken, now)
	dbExec(t, "INSERT INTO RepoUserToken (repo_id, account_id, token, ctime) VALUES (?, ?, ?, ?)",
		repoTheirs, outsiderAcct.ID, outsiderTok, now)
}

func headCommitsMultiReq(body, token string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/repo/head-commits-multi/", strings.NewReader(body))
	if token != "" {
		req.Header.Set("Seafile-Repo-Token", token)
	}
	return req
}

// head-commits-multi is the batched head poll the desktop client uses to check
// every repo in one request. It shipped with a bare "LOCK IN SHARE MODE"
// appended to the SELECT, which MySQL accepts and SQLite — the default backend
// — cannot parse, so the handler answered 500 for every request on the
// backend most deployments actually run. Clients survived on the per-repo
// GET /commit/HEAD fallback, which is why it went unnoticed.
//
// The test drives the handler against a real SQLite database rather than
// asserting on the generated SQL string, because the failure was a prepare
// error inside the driver and only the driver can tell us it is gone.
func TestHeadCommitsMultiUnderSQLite(t *testing.T) {
	seedHeadCommitsMulti(t)

	body, err := json.Marshal([]string{repoA, repoB, repoMissing})
	if err != nil {
		t.Fatalf("failed to marshal request: %v", err)
	}
	req := headCommitsMultiReq(string(body), ownerToken)
	rsp := httptest.NewRecorder()

	if appErr := headCommitsMultiCB(rsp, req); appErr != nil {
		t.Fatalf("headCommitsMultiCB returned %d: %v", appErr.Code, appErr.Error)
	}
	if rsp.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rsp.Code, http.StatusOK)
	}

	var got map[string]string
	if err := json.Unmarshal(rsp.Body.Bytes(), &got); err != nil {
		t.Fatalf("failed to decode response %q: %v", rsp.Body.String(), err)
	}
	want := map[string]string{repoA: headA, repoB: headB}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for repoID, commitID := range want {
		if got[repoID] != commitID {
			t.Errorf("head of %s = %q, want %q", repoID, got[repoID], commitID)
		}
	}
}

// Unauthenticated, the endpoint is an oracle: anyone who can reach the port
// and knows a repo id learns that the library exists and watches its head
// move, which is its activity, holding no credential at all.
func TestHeadCommitsMultiRequiresAToken(t *testing.T) {
	seedHeadCommitsMulti(t)

	body, err := json.Marshal([]string{repoA, repoB})
	if err != nil {
		t.Fatalf("failed to marshal request: %v", err)
	}

	for _, c := range []struct {
		name, token string
		wantCode    int
	}{
		{"no token", "", http.StatusBadRequest},
		{"unknown token", "9999cccc9999cccc9999cccc9999cccc9999cccc", http.StatusForbidden},
	} {
		appErr := headCommitsMultiCB(httptest.NewRecorder(), headCommitsMultiReq(string(body), c.token))
		if appErr == nil {
			t.Errorf("%s: the request was answered, want %d", c.name, c.wantCode)
			continue
		}
		if appErr.Code != c.wantCode {
			t.Errorf("%s: status = %d, want %d", c.name, appErr.Code, c.wantCode)
		}
	}
}

// Holding a token for one library must not reveal anything about another, and
// the response must not distinguish "does not exist" from "not yours".
func TestHeadCommitsMultiAnswersOnlyForReadableRepos(t *testing.T) {
	seedHeadCommitsMulti(t)

	body, err := json.Marshal([]string{repoA, repoTheirs, repoMissing})
	if err != nil {
		t.Fatalf("failed to marshal request: %v", err)
	}
	rsp := httptest.NewRecorder()
	if appErr := headCommitsMultiCB(rsp, headCommitsMultiReq(string(body), ownerToken)); appErr != nil {
		t.Fatalf("headCommitsMultiCB returned %d: %v", appErr.Code, appErr.Error)
	}

	var got map[string]string
	if err := json.Unmarshal(rsp.Body.Bytes(), &got); err != nil {
		t.Fatalf("failed to decode response %q: %v", rsp.Body.String(), err)
	}
	if _, leaked := got[repoTheirs]; leaked {
		t.Error("the head of a library the caller cannot read was disclosed")
	}
	if _, leaked := got[repoMissing]; leaked {
		t.Error("a library that does not exist appeared in the response")
	}
	if got[repoA] != headA {
		t.Errorf("head of the caller's own repo = %q, want %q", got[repoA], headA)
	}
}

// A caller who can read nothing in the list gets an empty map rather than an
// error: which of the ids were rejected is itself the thing not to disclose.
func TestHeadCommitsMultiReturnsEmptyMapWhenNothingIsReadable(t *testing.T) {
	seedHeadCommitsMulti(t)

	body, err := json.Marshal([]string{repoA, repoB})
	if err != nil {
		t.Fatalf("failed to marshal request: %v", err)
	}
	rsp := httptest.NewRecorder()
	if appErr := headCommitsMultiCB(rsp, headCommitsMultiReq(string(body), outsiderTok)); appErr != nil {
		t.Fatalf("headCommitsMultiCB returned %d: %v", appErr.Code, appErr.Error)
	}
	if rsp.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rsp.Code, http.StatusOK)
	}

	var got map[string]string
	if err := json.Unmarshal(rsp.Body.Bytes(), &got); err != nil {
		t.Fatalf("failed to decode response %q: %v", rsp.Body.String(), err)
	}
	if len(got) != 0 {
		t.Errorf("got %v, want an empty map", got)
	}
}

// The /seafhttp sync routes had no body limit at all: server.go caps only
// MaxHeaderBytes, and the 1MB MaxBytesReader it installs covers the
// /api/silo/v1 JSON routes only. One request from any client with write access
// to one library could make the server allocate until it was OOM-killed.
func TestBodyLimits(t *testing.T) {
	const limit = 1024

	t.Run("readLimitedBody accepts a body at the limit", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(strings.Repeat("a", limit)))
		data, appErr := readLimitedBody(httptest.NewRecorder(), req, limit)
		if appErr != nil {
			t.Fatalf("readLimitedBody returned %d: %v", appErr.Code, appErr.Message)
		}
		if len(data) != limit {
			t.Errorf("read %d bytes, want %d", len(data), limit)
		}
	})

	t.Run("readLimitedBody refuses one byte past the limit", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(strings.Repeat("a", limit+1)))
		_, appErr := readLimitedBody(httptest.NewRecorder(), req, limit)
		if appErr == nil {
			t.Fatal("readLimitedBody accepted an oversized body, want an error")
		}
		if appErr.Code != http.StatusRequestEntityTooLarge {
			t.Errorf("status = %d, want %d", appErr.Code, http.StatusRequestEntityTooLarge)
		}
	})

	t.Run("decodeLimitedJSON refuses an oversized body", func(t *testing.T) {
		// Valid JSON, so nothing but the limit can reject it.
		body := "[" + strings.Repeat(`"x",`, limit) + `"x"]`
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
		var got []string
		appErr := decodeLimitedJSON(httptest.NewRecorder(), req, limit, &got)
		if appErr == nil {
			t.Fatal("decodeLimitedJSON accepted an oversized body, want an error")
		}
		if appErr.Code != http.StatusRequestEntityTooLarge {
			t.Errorf("status = %d, want %d", appErr.Code, http.StatusRequestEntityTooLarge)
		}
	})

	t.Run("decodeLimitedJSON reports malformed JSON as a bad request", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("{not json"))
		var got []string
		appErr := decodeLimitedJSON(httptest.NewRecorder(), req, limit, &got)
		if appErr == nil {
			t.Fatal("decodeLimitedJSON accepted malformed JSON, want an error")
		}
		if appErr.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want %d", appErr.Code, http.StatusBadRequest)
		}
	})
}

// A repo ID that is not a UUID reaches the SELECT by string interpolation, so
// the handler has to reject it before building the statement.
func TestHeadCommitsMultiRejectsNonUUID(t *testing.T) {
	sqliteTestDB(t)

	for _, bad := range []string{
		"not-a-uuid",
		"' OR '1'='1",
		"",
	} {
		body, err := json.Marshal([]string{bad})
		if err != nil {
			t.Fatalf("failed to marshal request: %v", err)
		}
		req := httptest.NewRequest(http.MethodPost, "/repo/head-commits-multi/", strings.NewReader(string(body)))
		appErr := headCommitsMultiCB(httptest.NewRecorder(), req)
		if appErr == nil || appErr.Code != http.StatusBadRequest {
			t.Errorf("headCommitsMultiCB(%q) = %v, want 400", bad, appErr)
		}
	}
}
