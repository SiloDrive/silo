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
	"github.com/dkam/silo/fileserver/setup"
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
// emptyDB gives this package a database of its own and points account at it.
//
// It exists because account's handles are package-level and nil until Init
// runs, and a nil *sql.DB reaches QueryRowContext through a non-nil interface
// — so a handler that reads an account does not fail, it panics. Any test
// calling a handler needs this, including one that only wants the version
// string, and a test that leaves it out passes only for as long as some other
// test in the package happens to run first. That is the failure this helper
// was extracted for: two server-info tests were passing on setupPerms having
// gone before them, and panicked the moment either was run with -run.
func emptyDB(t *testing.T) *dbutil.DBPair {
	t.Helper()

	option.LoadFileServerOptions("") // defaults, incl. a non-zero DBOpTimeout

	siloPair, err := dbutil.OpenSQLite(filepath.Join(t.TempDir(), "silo.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = siloPair.Close() })
	if err := dbutil.CreateSiloTables(siloPair.Write); err != nil {
		t.Fatalf("create tables: %v", err)
	}
	account.Init(siloPair.Read, siloPair.Write)
	// setup keeps its own handles, and Required reads both: account first, then
	// the setup token. Seeding an account hides the second one, because Required
	// short-circuits and never reaches Peek — so a test with accounts survives
	// while the fresh-server path, the only one that answers setup_required at
	// all, panics. Wire both, the way boot does.
	setup.Init(siloPair.Read, siloPair.Write)
	return siloPair
}

func setupPerms(t *testing.T) {
	t.Helper()

	siloPair := emptyDB(t)
	ctx, cancel := option.WithDBTimeout(context.Background())
	defer cancel()
	for _, email := range []string{ownerUser, rwShareUser, roShareUser, strangerUser} {
		if _, _, err := account.Create(ctx, email, "", account.RoleUser); err != nil {
			t.Fatalf("create account %s: %v", email, err)
		}
	}

	if _, err := siloPair.Write.Exec(
		"INSERT INTO LibraryOwner (library_id, account_id) VALUES (?, ?)",
		testLibraryID, accountOf(t, ownerUser).ID); err != nil {
		t.Fatalf("seed LibraryOwner: %v", err)
	}
	libmgr.Init(siloPair.Read, siloPair.Write, t.TempDir())
	share.Init(siloPair.Read, siloPair.Write, "Group", false)

	// Seeded through the grant model, after share.Init, because that is what
	// CheckPerm reads. A seeder writing the table the checker no longer
	// consults would describe a world the server does not live in.
	for _, sh := range []struct{ user, perm string }{
		{rwShareUser, "rw"},
		{roShareUser, "r"},
	} {
		ctx, cancel := option.WithDBTimeout(context.Background())
		if err := share.Add(ctx, share.Grant{
			Principal: share.UserPrincipal(accountOf(t, sh.user).ID),
			LibraryID: testLibraryID,
			Perm:      sh.perm,
			CreatedBy: accountOf(t, ownerUser).ID,
		}); err != nil {
			cancel()
			t.Fatalf("grant %s on the test library: %v", sh.user, err)
		}
		cancel()
	}

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
