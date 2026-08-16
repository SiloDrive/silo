package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

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

// setupPerms wires up in-package SQLite databases and seeds one repo owned by
// ownerUser, shared "rw" with rwShareUser and "r" with roShareUser.
// strangerUser is left with no relationship to the repo at all.
func setupPerms(t *testing.T) {
	t.Helper()

	option.LoadFileServerOptions("") // defaults, incl. a non-zero DBOpTimeout
	dbutil.DBEngine = dbutil.EngineSQLite

	dir := t.TempDir()

	ccnetPair, err := dbutil.OpenSQLite(filepath.Join(dir, "ccnet.db"))
	if err != nil {
		t.Fatalf("open ccnet db: %v", err)
	}
	t.Cleanup(func() { _ = ccnetPair.Close() })
	if err := dbutil.CreateCcnetTables(ccnetPair.Write); err != nil {
		t.Fatalf("create ccnet tables: %v", err)
	}

	seafilePair, err := dbutil.OpenSQLite(filepath.Join(dir, "seafile.db"))
	if err != nil {
		t.Fatalf("open seafile db: %v", err)
	}
	t.Cleanup(func() { _ = seafilePair.Close() })
	if err := dbutil.CreateSeafileTables(seafilePair.Write); err != nil {
		t.Fatalf("create seafile tables: %v", err)
	}

	if _, err := seafilePair.Write.Exec(
		"INSERT INTO RepoOwner (repo_id, owner_id) VALUES (?, ?)", testRepoID, ownerUser); err != nil {
		t.Fatalf("seed RepoOwner: %v", err)
	}
	for _, s := range []struct{ user, perm string }{
		{rwShareUser, "rw"},
		{roShareUser, "r"},
	} {
		if _, err := seafilePair.Write.Exec(
			"INSERT INTO SharedRepo (repo_id, from_email, to_email, permission) VALUES (?, ?, ?, ?)",
			testRepoID, ownerUser, s.user, s.perm); err != nil {
			t.Fatalf("seed SharedRepo for %s: %v", s.user, err)
		}
	}

	repomgr.Init(seafilePair.Read, seafilePair.Write)
	share.Init(ccnetPair.Read, seafilePair.Read, "Group", false)
	Init(seafilePair.Read, seafilePair.Write)
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
	req = req.WithContext(context.WithValue(req.Context(), middleware.UserEmailKey, user))

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
