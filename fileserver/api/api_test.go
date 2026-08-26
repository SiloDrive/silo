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
	"github.com/dkam/silo/fileserver/credential"
	"github.com/dkam/silo/fileserver/dbutil"
	"github.com/dkam/silo/fileserver/libmgr"
	"github.com/dkam/silo/fileserver/middleware"
	"github.com/dkam/silo/fileserver/option"
	"github.com/dkam/silo/fileserver/share"
	"github.com/dkam/silo/fileserver/tokenstore"
)

const (
	ownerUser    = "owner@example.com"
	rwShareUser  = "rw@example.com"
	roShareUser  = "ro@example.com"
	strangerUser = "stranger@example.com"

	testLibraryID    = "11111111-2222-3333-4444-555555555555"
	missingLibraryID = "99999999-8888-7777-6666-555555555555"
)

// setupPerms wires up an in-package SQLite database and seeds one library owned by
// ownerUser, shared "rw" with rwShareUser and "r" with roShareUser.
// strangerUser is left with no relationship to the library at all.
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
	ctx, cancel := option.WithDBTimeout(context.Background())
	defer cancel()
	for _, email := range []string{ownerUser, rwShareUser, roShareUser, strangerUser} {
		if _, _, err := account.Create(ctx, email, "", false); err != nil {
			t.Fatalf("create account %s: %v", email, err)
		}
	}

	if _, err := siloPair.Write.Exec(
		"INSERT INTO LibraryOwner (library_id, account_id) VALUES (?, ?)",
		testLibraryID, accountOf(t, ownerUser).ID); err != nil {
		t.Fatalf("seed LibraryOwner: %v", err)
	}
	for _, s := range []struct{ user, perm string }{
		{rwShareUser, "rw"},
		{roShareUser, "r"},
	} {
		if _, err := siloPair.Write.Exec(
			"INSERT INTO SharedLibrary (library_id, from_account_id, to_account_id, permission) VALUES (?, ?, ?, ?)",
			testLibraryID, accountOf(t, ownerUser).ID, accountOf(t, s.user).ID, s.perm); err != nil {
			t.Fatalf("seed SharedLibrary for %s: %v", s.user, err)
		}
	}

	libmgr.Init(siloPair.Read, siloPair.Write, t.TempDir())
	share.Init(siloPair.Read, "Group", false)
	Init(siloPair.Read, siloPair.Write)
}

// accountOf resolves one of the test addresses to the account behind it. The
// tests still name people by address because that is what a reader recognises
// — but everything below the handler now works in ids.
func accountOf(t *testing.T, email string) *account.Account {
	t.Helper()
	ctx, cancel := option.WithDBTimeout(context.Background())
	defer cancel()
	acct, err := account.ByEmail(ctx, email)
	if err != nil {
		t.Fatalf("no account for %s: %v", email, err)
	}
	return acct
}

// postAccessToken invokes CreateAccessTokenHandler as `user` would, bypassing
// the auth middleware by seeding the context the same way RequireAuth does.
func postAccessToken(t *testing.T, user, libraryID, op string) *httptest.ResponseRecorder {
	t.Helper()

	body, err := json.Marshal(accessTokenRequest{LibraryID: libraryID, ObjID: "{\"parent_dir\":\"/\"}", Op: op})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	req := httptest.NewRequest("POST", "/api/silo/v1/access-tokens", strings.NewReader(string(body)))
	req = middleware.WithCredential(req, testCredential(accountOf(t, user)), accountOf(t, user))

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
		name      string
		user      string
		libraryID string
		op        string
		wantCode  int
	}{
		// The headline escalation: a user with no relationship to the library
		// could mint an upload token and write into it, because the
		// /upload-api/ handler authorizes from the token alone.
		{"stranger cannot mint upload", strangerUser, testLibraryID, "upload", http.StatusForbidden},
		{"stranger cannot mint download", strangerUser, testLibraryID, "download", http.StatusForbidden},
		{"stranger cannot mint downloadblks", strangerUser, testLibraryID, "downloadblks", http.StatusForbidden},
		{"stranger cannot mint download-dir", strangerUser, testLibraryID, "download-dir", http.StatusForbidden},

		// A read-only share must not be upgradeable to write.
		{"read-only share cannot mint upload", roShareUser, testLibraryID, "upload", http.StatusForbidden},
		{"read-only share cannot mint update", roShareUser, testLibraryID, "update", http.StatusForbidden},
		{"read-only share cannot mint upload-link", roShareUser, testLibraryID, "upload-link", http.StatusForbidden},

		// Legitimate access still works.
		{"owner can mint upload", ownerUser, testLibraryID, "upload", http.StatusOK},
		{"owner can mint download", ownerUser, testLibraryID, "download", http.StatusOK},
		{"rw share can mint upload", rwShareUser, testLibraryID, "upload", http.StatusOK},
		{"read-only share can mint download", roShareUser, testLibraryID, "download", http.StatusOK},
		{"read-only share can mint view", roShareUser, testLibraryID, "view", http.StatusOK},

		// A library that doesn't exist looks the same as one you can't see, so
		// the endpoint can't be used to probe for valid library IDs.
		{"missing library is forbidden not 404", ownerUser, missingLibraryID, "download", http.StatusForbidden},

		// Unknown ops never yield a credential.
		{"unknown op rejected", ownerUser, testLibraryID, "delete-everything", http.StatusBadRequest},
		{"empty-ish op rejected", ownerUser, testLibraryID, "UPLOAD", http.StatusBadRequest},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rr := postAccessToken(t, tt.user, tt.libraryID, tt.op)
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

	token := tokenFrom(t, postAccessToken(t, ownerUser, testLibraryID, "upload"))

	info := tokenstore.QueryToken(token)
	if info == nil {
		t.Fatal("minted token is not in the token store")
	}
	if info.LibraryID != testLibraryID {
		t.Errorf("expected library %s, got %s", testLibraryID, info.LibraryID)
	}
	if info.Op != "upload" {
		t.Errorf("expected op upload, got %s", info.Op)
	}
	if info.User != ownerUser {
		t.Errorf("expected user %s, got %s", ownerUser, info.User)
	}
}

func TestCreateAccessTokenRequiresLibraryAndOp(t *testing.T) {
	setupPerms(t)

	for _, tt := range []struct{ name, libraryID, op string }{
		{"no library", "", "download"},
		{"no op", testLibraryID, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rr := postAccessToken(t, ownerUser, tt.libraryID, tt.op)
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
func TestListLibrariesAnswersEmptyArrayNotNull(t *testing.T) {
	setupPerms(t)

	req := httptest.NewRequest("GET", "/api/silo/v1/libraries", nil)
	req = middleware.WithCredential(req, testCredential(accountOf(t, strangerUser)), accountOf(t, strangerUser))

	rr := httptest.NewRecorder()
	ListLibrariesHandler(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", rr.Code, rr.Body.String())
	}
	if got := strings.TrimSpace(rr.Body.String()); got != "[]" {
		t.Errorf("empty account listed as %q, want []", got)
	}

	// And the populated case still lists, so the fix did not empty the endpoint.
	req = httptest.NewRequest("GET", "/api/silo/v1/libraries", nil)
	req = middleware.WithCredential(req, testCredential(accountOf(t, ownerUser)), accountOf(t, ownerUser))
	rr = httptest.NewRecorder()
	ListLibrariesHandler(rr, req)

	var libraries []map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &libraries); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(libraries) != 1 || libraries[0]["id"] != testLibraryID {
		t.Errorf("owner's listing = %v, want the one seeded library", libraries)
	}
}

// testCredential is the unnarrowed credential a handler reads its permission
// from. These tests call handlers directly, so they supply what
// RequireCredential would have: unscoped and rw, so the ceiling is not what
// they are measuring.
func testCredential(acct *account.Account) *credential.Credential {
	return &credential.Credential{
		Kind: credential.KindSession, AccountID: acct.ID, Label: "test", Perm: "rw",
	}
}
