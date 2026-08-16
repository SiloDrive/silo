package silod

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

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
	sqliteTestDB(t)

	const (
		repoA = "b1f2ad61-9164-418a-a47f-ab805dbd5694"
		repoB = "c2e3be72-a275-429b-b580-bc916ece6705"
		// Valid UUID, deliberately absent from Branch.
		repoMissing = "d3f4cf83-b386-43ac-c691-cda27fdf7816"
	)
	dbExec(t, "INSERT INTO Branch (name, repo_id, commit_id) VALUES (?, ?, ?)",
		"master", repoA, "0401fc662e3bc87a41f299a907c056aaf8322a27")
	dbExec(t, "INSERT INTO Branch (name, repo_id, commit_id) VALUES (?, ?, ?)",
		"master", repoB, "1512ad773f4cd98b52e3aab018d167bb09433b38")
	// A non-master branch on repoA must not be returned as its head.
	dbExec(t, "INSERT INTO Branch (name, repo_id, commit_id) VALUES (?, ?, ?)",
		"other", repoA, "2623be884e5dea9c63f4bbc129e278cc1a544c49")

	body, err := json.Marshal([]string{repoA, repoB, repoMissing})
	if err != nil {
		t.Fatalf("failed to marshal request: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/repo/head-commits-multi/", strings.NewReader(string(body)))
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
	want := map[string]string{
		repoA: "0401fc662e3bc87a41f299a907c056aaf8322a27",
		repoB: "1512ad773f4cd98b52e3aab018d167bb09433b38",
	}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for repoID, commitID := range want {
		if got[repoID] != commitID {
			t.Errorf("head of %s = %q, want %q", repoID, got[repoID], commitID)
		}
	}
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
