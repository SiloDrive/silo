package share

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/dkam/silo/fileserver/account"
	"github.com/dkam/silo/fileserver/dbutil"
	"github.com/dkam/silo/fileserver/libmgr"
	"github.com/dkam/silo/fileserver/option"
	"github.com/google/uuid"
)

// setupShareTest opens a fresh database and wires every package CheckPerm
// reaches through — account, libmgr and share itself all read the same
// handle a real server would share. It returns the write handle, because
// share.go only ever holds a read one: seeding the relations it checks is
// the test's job, not something Init gives a caller a way to do.
func setupShareTest(t *testing.T) *sql.DB {
	t.Helper()
	if option.DBOpTimeout <= 0 {
		option.DBOpTimeout = 5 * time.Second
	}
	pair, err := dbutil.OpenSQLite(filepath.Join(t.TempDir(), "silo.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := dbutil.Prepare(pair.Write); err != nil {
		t.Fatalf("create tables: %v", err)
	}
	t.Cleanup(func() { _ = pair.Close() })

	account.Init(pair.Read, pair.Write)
	libmgr.Init(pair.Read, pair.Write, t.TempDir())
	Init(pair.Read, pair.Write)
	return pair.Write
}

func makeAccount(t *testing.T, email string) *account.Account {
	t.Helper()
	ctx, cancel := option.WithDBTimeout(context.Background())
	defer cancel()
	if _, _, err := account.Create(ctx, email, "", account.RoleUser); err != nil {
		t.Fatalf("create account %s: %v", email, err)
	}
	acct, err := account.ByEmail(ctx, email)
	if err != nil {
		t.Fatalf("read account %s: %v", email, err)
	}
	return acct
}

func makeLibrary(t *testing.T, owner *account.Account) string {
	t.Helper()
	libraryID, err := libmgr.CreateLibrary("t", owner, libmgr.DefaultFormat(false))
	if err != nil {
		t.Fatalf("create library: %v", err)
	}
	return libraryID
}

// makeVirtualLibrary records a subfolder of originLibraryID as its own library
// entity — the mechanism a subfolder share is built on: sharing that entity,
// not the origin, is what makes the share specific to path.
func makeVirtualLibrary(t *testing.T, write *sql.DB, originLibraryID, path string) string {
	t.Helper()
	vLibraryID := uuid.New().String()
	if _, err := write.Exec("INSERT INTO VirtualLibrary (library_id, origin_library, path, base_commit) VALUES (?, ?, ?, ?)",
		vLibraryID, originLibraryID, path, ""); err != nil {
		t.Fatalf("create virtual library: %v", err)
	}
	return vLibraryID
}

// shareLibrary grants one account read or write on a whole library. It goes
// through the package's own Add rather than an INSERT, because the seeding
// path and the checking path reading the same model is most of what the
// unification bought -- a helper that wrote rows CheckPerm no longer consults
// would pass by describing a world the server does not live in.
func shareLibrary(t *testing.T, write *sql.DB, libraryID string, from, to account.ID, perm string) {
	t.Helper()
	ctx, cancel := option.WithDBTimeout(context.Background())
	defer cancel()
	if err := Add(ctx, Grant{
		Principal: UserPrincipal(to), LibraryID: libraryID, Perm: perm, CreatedBy: from,
	}); err != nil {
		t.Fatalf("share library: %v", err)
	}
}

func TestGetDirPermWalksUpToTheNearestAncestor(t *testing.T) {
	perms := map[string]string{
		"/docs":      "r",
		"/docs/2024": "rw",
	}
	cases := []struct{ path, want string }{
		{"/docs/2024/q1/report.txt", "rw"}, // nearest ancestor is /docs/2024
		{"/docs/other.txt", "r"},           // falls back to /docs
		{"/docs", "r"},                     // the shared path itself
		{"/elsewhere/x", ""},               // no shared ancestor at all
	}
	for _, c := range cases {
		if got := nearestGrant(perms, c.path); got != c.want {
			t.Errorf("nearestGrant(%q) = %q, want %q", c.path, got, c.want)
		}
	}
}

// CheckPerm is the authorization gate entries.go, api/changes.go and
// api/api.go all call before touching a library. A wrong answer here is
// either a library leaking to someone it was never shared with, or an
// owner locked out of their own data — which is why it is tested directly
// rather than only through the handlers that happen to call it.
func TestCheckPermOwnerAlwaysHasReadWrite(t *testing.T) {
	setupShareTest(t)
	owner := makeAccount(t, "owner@example.com")
	libraryID := makeLibrary(t, owner)

	if got := CheckPerm(libraryID, owner.ID); got != "rw" {
		t.Errorf("owner permission = %q, want rw", got)
	}
}

func TestCheckPermWithNoRelationIsDenied(t *testing.T) {
	setupShareTest(t)
	owner := makeAccount(t, "owner2@example.com")
	stranger := makeAccount(t, "stranger2@example.com")
	libraryID := makeLibrary(t, owner)

	if got := CheckPerm(libraryID, stranger.ID); got != "" {
		t.Errorf("a user with no relation to the library got %q, want denied", got)
	}
}

func TestCheckPermIndividualShareGrantsTheRecordedPermission(t *testing.T) {
	for _, perm := range []string{"r", "rw"} {
		t.Run(perm, func(t *testing.T) {
			write := setupShareTest(t)
			owner := makeAccount(t, "owner-"+perm+"@example.com")
			friend := makeAccount(t, "friend-"+perm+"@example.com")
			libraryID := makeLibrary(t, owner)
			shareLibrary(t, write, libraryID, owner.ID, friend.ID, perm)

			if got := CheckPerm(libraryID, friend.ID); got != perm {
				t.Errorf("shared permission = %q, want %q", got, perm)
			}
		})
	}
}

// A grant to the anonymous principal reaches a signed-in account that holds
// no grant of its own. PrincipalsFor leaves Anon out on purpose, so this pins
// that CheckPerm asks the second question itself rather than stopping at the
// first.
func TestCheckPermAnAnonymousGrantReachesASignedInStranger(t *testing.T) {
	setupShareTest(t)
	owner := makeAccount(t, "iowner@example.com")
	stranger := makeAccount(t, "istranger@example.com")
	libraryID := makeLibrary(t, owner)
	ctx, cancel := option.WithDBTimeout(context.Background())
	defer cancel()
	if err := Add(ctx, Grant{
		Principal: Anon, LibraryID: libraryID, Perm: "r", Listed: true, CreatedBy: owner.ID,
	}); err != nil {
		t.Fatalf("grant the anonymous principal: %v", err)
	}

	if got := CheckPerm(libraryID, stranger.ID); got != "r" {
		t.Errorf("permission = %q, want r", got)
	}
}

func TestCheckPermVirtualLibraryOwnerOfOriginHasReadWrite(t *testing.T) {
	write := setupShareTest(t)
	owner := makeAccount(t, "owner7@example.com")
	origin := makeLibrary(t, owner)
	vLibraryID := makeVirtualLibrary(t, write, origin, "/sub")

	if got := CheckPerm(vLibraryID, owner.ID); got != "rw" {
		t.Errorf("origin owner on a virtual library = %q, want rw", got)
	}
}

// A subfolder share is recorded against the virtual library's own id, not the
// origin's, and must not be visible through the origin library itself — sharing
// "/sub" is not sharing the whole library.
func TestCheckPermVirtualLibraryGrantsTheSubfolderShare(t *testing.T) {
	write := setupShareTest(t)
	owner := makeAccount(t, "owner9@example.com")
	friend := makeAccount(t, "friend9@example.com")
	origin := makeLibrary(t, owner)
	vLibraryID := makeVirtualLibrary(t, write, origin, "/sub")

	shareLibrary(t, write, vLibraryID, owner.ID, friend.ID, "r")

	if got := CheckPerm(vLibraryID, friend.ID); got != "r" {
		t.Errorf("subfolder share permission = %q, want r", got)
	}
	if got := CheckPerm(origin, friend.ID); got != "" {
		t.Errorf("the subfolder share leaked onto the origin library: %q", got)
	}
}

// With no subfolder-specific share, checkVirtualLibraryPerm falls all the way
// back to a blanket share of the whole origin library — the last of the three
// checks it runs in order.
func TestCheckPermVirtualLibraryFallsBackToABlanketOriginShare(t *testing.T) {
	write := setupShareTest(t)
	owner := makeAccount(t, "owner10@example.com")
	friend := makeAccount(t, "friend10@example.com")
	origin := makeLibrary(t, owner)
	vLibraryID := makeVirtualLibrary(t, write, origin, "/sub")

	shareLibrary(t, write, origin, owner.ID, friend.ID, "rw")

	if got := CheckPerm(vLibraryID, friend.ID); got != "rw" {
		t.Errorf("permission via a blanket origin share = %q, want rw", got)
	}
}
