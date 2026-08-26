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
