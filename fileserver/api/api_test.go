package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dkam/silo/fileserver/account"
	"github.com/dkam/silo/fileserver/dbutil"
	"github.com/dkam/silo/fileserver/middleware"
	"github.com/dkam/silo/fileserver/option"
	"github.com/dkam/silo/fileserver/repomgr"
	"github.com/dkam/silo/fileserver/share"
	"github.com/dkam/silo/fileserver/tokenstore"
)

const (
	ownerUser    = "owner@example.com"
	rwShareUser  = "rw@example.com"
	roShareUser  = "ro@example.com"
	strangerUser = "stranger@example.com"

	testRepoID    = "11111111-2222-3333-4444-555555555555"
	missingRepoID = "99999999-8888-7777-6666-555555555555"
)

// setupPerms wires up an in-package SQLite database and seeds one repo owned by
// ownerUser, shared "rw" with rwShareUser and "r" with roShareUser.
// strangerUser is left with no relationship to the repo at all.
func setupPerms(t *testing.T) {
	t.Helper()

	option.LoadFileServerOptions("") // defaults, incl. a non-zero DBOpTimeout

	dir := t.TempDir()

	siloPair, err := dbutil.OpenSQLite(filepath.Join(dir, "silo.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = siloPair.Close() })
	if err := dbutil.CreateSiloTables(siloPair.Write); err != nil {
		t.Fatalf("create tables: %v", err)
	}

	account.Init(siloPair.Read, siloPair.Write)
	ctx, cancel := account.WithTimeout()
	defer cancel()
	for _, email := range []string{ownerUser, rwShareUser, roShareUser, strangerUser} {
		if _, _, err := account.Create(ctx, email, "", false); err != nil {
			t.Fatalf("create account %s: %v", email, err)
		}
	}

	if _, err := siloPair.Write.Exec(
		"INSERT INTO RepoOwner (repo_id, account_id) VALUES (?, ?)",
		testRepoID, accountOf(t, ownerUser).ID); err != nil {
		t.Fatalf("seed RepoOwner: %v", err)
	}
	for _, s := range []struct{ user, perm string }{
		{rwShareUser, "rw"},
		{roShareUser, "r"},
	} {
		if _, err := siloPair.Write.Exec(
			"INSERT INTO SharedRepo (repo_id, from_account_id, to_account_id, permission) VALUES (?, ?, ?, ?)",
			testRepoID, accountOf(t, ownerUser).ID, accountOf(t, s.user).ID, s.perm); err != nil {
			t.Fatalf("seed SharedRepo for %s: %v", s.user, err)
		}
	}

	repomgr.Init(siloPair.Read, siloPair.Write)
	share.Init(siloPair.Read, "Group", false)
	Init(siloPair.Read, siloPair.Write)
}

// accountOf resolves one of the test addresses to the account behind it. The
// tests still name people by address because that is what a reader recognises
// — but everything below the handler now works in ids.
func accountOf(t *testing.T, email string) *account.Account {
	t.Helper()
	ctx, cancel := account.WithTimeout()
	defer cancel()
	acct, err := account.ByEmail(ctx, email)
	if err != nil {
		t.Fatalf("no account for %s: %v", email, err)
	}
	return acct
}

// postAccessToken invokes CreateAccessTokenHandler as `user` would, bypassing
// the auth middleware by seeding the context the same way RequireAuth does.
func postAccessToken(t *testing.T, user, repoID, op string) *httptest.ResponseRecorder {
	t.Helper()

	body, err := json.Marshal(accessTokenRequest{RepoID: repoID, ObjID: "{\"parent_dir\":\"/\"}", Op: op})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	req := httptest.NewRequest("POST", "/api/silo/v1/access-tokens", strings.NewReader(string(body)))
	req = req.WithContext(context.WithValue(req.Context(), middleware.AccountKey, accountOf(t, user)))

	rr := httptest.NewRecorder()
	CreateAccessTokenHandler(rr, req)
	return rr
}

// tokenFrom asserts a 200 and returns a usable token from the response.
func tokenFrom(t *testing.T, rr *httptest.ResponseRecorder) string {
	t.Helper()
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", rr.Code, rr.Body.String())
	}
	var resp accessTokenResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Token == "" {
		t.Fatal("expected a token, got empty string")
	}
	return resp.Token
}

func TestCreateAccessTokenPermissions(t *testing.T) {
	setupPerms(t)

	tests := []struct {
		name     string
		user     string
		repoID   string
		op       string
		wantCode int
	}{
		// The headline escalation: a user with no relationship to the repo
		// could mint an upload token and write into it, because the
		// /upload-api/ handler authorizes from the token alone.
		{"stranger cannot mint upload", strangerUser, testRepoID, "upload", http.StatusForbidden},
		{"stranger cannot mint download", strangerUser, testRepoID, "download", http.StatusForbidden},
		{"stranger cannot mint downloadblks", strangerUser, testRepoID, "downloadblks", http.StatusForbidden},
		{"stranger cannot mint download-dir", strangerUser, testRepoID, "download-dir", http.StatusForbidden},

		// A read-only share must not be upgradeable to write.
		{"read-only share cannot mint upload", roShareUser, testRepoID, "upload", http.StatusForbidden},
		{"read-only share cannot mint update", roShareUser, testRepoID, "update", http.StatusForbidden},
		{"read-only share cannot mint upload-link", roShareUser, testRepoID, "upload-link", http.StatusForbidden},

		// Legitimate access still works.
		{"owner can mint upload", ownerUser, testRepoID, "upload", http.StatusOK},
		{"owner can mint download", ownerUser, testRepoID, "download", http.StatusOK},
		{"rw share can mint upload", rwShareUser, testRepoID, "upload", http.StatusOK},
		{"read-only share can mint download", roShareUser, testRepoID, "download", http.StatusOK},
		{"read-only share can mint view", roShareUser, testRepoID, "view", http.StatusOK},

		// A repo that doesn't exist looks the same as one you can't see, so
		// the endpoint can't be used to probe for valid repo IDs.
		{"missing repo is forbidden not 404", ownerUser, missingRepoID, "download", http.StatusForbidden},

		// Unknown ops never yield a credential.
		{"unknown op rejected", ownerUser, testRepoID, "delete-everything", http.StatusBadRequest},
		{"empty-ish op rejected", ownerUser, testRepoID, "UPLOAD", http.StatusBadRequest},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rr := postAccessToken(t, tt.user, tt.repoID, tt.op)
			if rr.Code != tt.wantCode {
				t.Errorf("expected %d, got %d (%s)", tt.wantCode, rr.Code, strings.TrimSpace(rr.Body.String()))
			}
			if rr.Code != http.StatusOK && strings.Contains(rr.Body.String(), "token") {
				t.Errorf("denied request leaked a token: %s", rr.Body.String())
			}
		})
	}
}

// TestCreateAccessTokenGrantsMatchRequest checks that an allowed request still
// produces a token the downstream handlers will actually accept — i.e. the
// permission gate didn't change the token's contents.
func TestCreateAccessTokenGrantsMatchRequest(t *testing.T) {
	setupPerms(t)

	token := tokenFrom(t, postAccessToken(t, ownerUser, testRepoID, "upload"))

	info := tokenstore.QueryToken(token)
	if info == nil {
		t.Fatal("minted token is not in the token store")
	}
	if info.RepoID != testRepoID {
		t.Errorf("expected repo %s, got %s", testRepoID, info.RepoID)
	}
	if info.Op != "upload" {
		t.Errorf("expected op upload, got %s", info.Op)
	}
	if info.User != ownerUser {
		t.Errorf("expected user %s, got %s", ownerUser, info.User)
	}
}

func TestCreateAccessTokenRequiresRepoAndOp(t *testing.T) {
	setupPerms(t)

	for _, tt := range []struct{ name, repoID, op string }{
		{"no repo", "", "download"},
		{"no op", testRepoID, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rr := postAccessToken(t, ownerUser, tt.repoID, tt.op)
			if rr.Code != http.StatusBadRequest {
				t.Errorf("expected 400, got %d", rr.Code)
			}
		})
	}
}

// An account with no libraries is the state every new account is in, so this is
// the first response a fresh client sees. It has to be an array: /changes
// already promises "always an array, never null", and two list endpoints on one
// lane spelling "nothing" differently is something a client can only learn by
// emptying an account and looking. Go hides it — a nil slice ranges zero times
// — which is why this asserts on the bytes rather than on the decoded value.
func TestListReposAnswersEmptyArrayNotNull(t *testing.T) {
	setupPerms(t)

	req := httptest.NewRequest("GET", "/api/silo/v1/repos", nil)
	req = req.WithContext(context.WithValue(req.Context(), middleware.AccountKey, accountOf(t, strangerUser)))

	rr := httptest.NewRecorder()
	ListReposHandler(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", rr.Code, rr.Body.String())
	}
	if got := strings.TrimSpace(rr.Body.String()); got != "[]" {
		t.Errorf("empty account listed as %q, want []", got)
	}

	// And the populated case still lists, so the fix did not empty the endpoint.
	req = httptest.NewRequest("GET", "/api/silo/v1/repos", nil)
	req = req.WithContext(context.WithValue(req.Context(), middleware.AccountKey, accountOf(t, ownerUser)))
	rr = httptest.NewRecorder()
	ListReposHandler(rr, req)

	var repos []map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &repos); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(repos) != 1 || repos[0]["id"] != testRepoID {
		t.Errorf("owner's listing = %v, want the one seeded repo", repos)
	}
}
