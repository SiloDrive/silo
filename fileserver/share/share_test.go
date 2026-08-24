package share

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/dkam/silo/fileserver/account"
	"github.com/dkam/silo/fileserver/dbutil"
	"github.com/dkam/silo/fileserver/option"
	"github.com/dkam/silo/fileserver/repomgr"
	"github.com/google/uuid"
)

// setupShareTest opens a fresh database and wires every package CheckPerm
// reaches through — account, repomgr and share itself all read the same
// handle a real server would share. It returns the write handle, because
// share.go only ever holds a read one: seeding the relations it checks is
// the test's job, not something Init gives a caller a way to do.
func setupShareTest(t *testing.T, cloud bool) *sql.DB {
	t.Helper()
	if option.DBOpTimeout <= 0 {
		option.DBOpTimeout = 5 * time.Second
	}
	pair, err := dbutil.OpenSQLite(filepath.Join(t.TempDir(), "silo.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := dbutil.CreateSiloTables(pair.Write); err != nil {
		t.Fatalf("create tables: %v", err)
	}
	t.Cleanup(func() { _ = pair.Close() })

	account.Init(pair.Read, pair.Write)
	repomgr.Init(pair.Read, pair.Write, t.TempDir())
	Init(pair.Read, "Group", cloud)
	return pair.Write
}

func makeAccount(t *testing.T, email string) *account.Account {
	t.Helper()
	ctx, cancel := option.WithDBTimeout(context.Background())
	defer cancel()
	if _, _, err := account.Create(ctx, email, "", false); err != nil {
		t.Fatalf("create account %s: %v", email, err)
	}
	acct, err := account.ByEmail(ctx, email)
	if err != nil {
		t.Fatalf("read account %s: %v", email, err)
	}
	return acct
}

func makeRepo(t *testing.T, owner *account.Account) string {
	t.Helper()
	repoID, err := repomgr.CreateRepo("t", owner, repomgr.DefaultFormat(false))
	if err != nil {
		t.Fatalf("create repo: %v", err)
	}
	return repoID
}

// makeVirtualRepo records a subfolder of originRepoID as its own repo
// entity — the mechanism a subfolder share is built on: sharing that entity,
// not the origin, is what makes the share specific to path.
func makeVirtualRepo(t *testing.T, write *sql.DB, originRepoID, path string) string {
	t.Helper()
	vRepoID := uuid.New().String()
	if _, err := write.Exec("INSERT INTO VirtualRepo (repo_id, origin_repo, path, base_commit) VALUES (?, ?, ?, ?)",
		vRepoID, originRepoID, path, ""); err != nil {
		t.Fatalf("create virtual repo: %v", err)
	}
	return vRepoID
}

func shareRepo(t *testing.T, write *sql.DB, repoID string, from, to account.ID, perm string) {
	t.Helper()
	if _, err := write.Exec("INSERT INTO SharedRepo (repo_id, from_account_id, to_account_id, permission) VALUES (?, ?, ?, ?)",
		repoID, from, to, perm); err != nil {
		t.Fatalf("share repo: %v", err)
	}
}

// makeGroup creates a group with one member and returns its id.
func makeGroup(t *testing.T, write *sql.DB, name string, creator, member account.ID) int {
	t.Helper()
	res, err := write.Exec(`INSERT INTO "Group" (group_name, creator_account_id, timestamp, parent_group_id) VALUES (?, ?, ?, 0)`,
		name, creator, 1700000000)
	if err != nil {
		t.Fatalf("create group: %v", err)
	}
	id64, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("group id: %v", err)
	}
	if _, err := write.Exec("INSERT INTO GroupUser (group_id, account_id, is_staff) VALUES (?, ?, 0)", id64, member); err != nil {
		t.Fatalf("add group member: %v", err)
	}
	return int(id64)
}

func shareRepoToGroup(t *testing.T, write *sql.DB, repoID string, groupID int, sharer account.ID, perm string) {
	t.Helper()
	if _, err := write.Exec("INSERT INTO RepoGroup (repo_id, group_id, account_id, permission) VALUES (?, ?, ?, ?)",
		repoID, groupID, sharer, perm); err != nil {
		t.Fatalf("share repo to group: %v", err)
	}
}

// getDirPerm has no database behind it: a permission map and a path in, the
// nearest ancestor's permission out. This is the rule a subfolder share
// applies through, pinned without anything else in play.
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
		if got := getDirPerm(perms, c.path); got != c.want {
			t.Errorf("getDirPerm(%q) = %q, want %q", c.path, got, c.want)
		}
	}
}

// CheckPerm is the authorization gate entries.go, api/changes.go and
// api/api.go all call before touching a library. A wrong answer here is
// either a library leaking to someone it was never shared with, or an
// owner locked out of their own data — which is why it is tested directly
// rather than only through the handlers that happen to call it.
func TestCheckPermOwnerAlwaysHasReadWrite(t *testing.T) {
	setupShareTest(t, false)
	owner := makeAccount(t, "owner@example.com")
	repoID := makeRepo(t, owner)

	if got := CheckPerm(repoID, owner.ID); got != "rw" {
		t.Errorf("owner permission = %q, want rw", got)
	}
}

func TestCheckPermWithNoRelationIsDenied(t *testing.T) {
	setupShareTest(t, false)
	owner := makeAccount(t, "owner2@example.com")
	stranger := makeAccount(t, "stranger2@example.com")
	repoID := makeRepo(t, owner)

	if got := CheckPerm(repoID, stranger.ID); got != "" {
		t.Errorf("a user with no relation to the library got %q, want denied", got)
	}
}

func TestCheckPermIndividualShareGrantsTheRecordedPermission(t *testing.T) {
	for _, perm := range []string{"r", "rw"} {
		t.Run(perm, func(t *testing.T) {
			write := setupShareTest(t, false)
			owner := makeAccount(t, "owner-"+perm+"@example.com")
			friend := makeAccount(t, "friend-"+perm+"@example.com")
			repoID := makeRepo(t, owner)
			shareRepo(t, write, repoID, owner.ID, friend.ID, perm)

			if got := CheckPerm(repoID, friend.ID); got != perm {
				t.Errorf("shared permission = %q, want %q", got, perm)
			}
		})
	}
}

// A surprising enough rule to pin: checkRepoSharePerm returns as soon as an
// individual share answers, so a wider group grant on the same repo never
// even gets asked about. A user shared "r" individually reads "r", even if
// a group they are also in was shared "rw".
func TestCheckPermIndividualShareTakesPrecedenceOverAGroupShare(t *testing.T) {
	write := setupShareTest(t, false)
	owner := makeAccount(t, "owner3@example.com")
	member := makeAccount(t, "member3@example.com")
	repoID := makeRepo(t, owner)

	shareRepo(t, write, repoID, owner.ID, member.ID, "r")
	groupID := makeGroup(t, write, "team3", owner.ID, member.ID)
	shareRepoToGroup(t, write, repoID, groupID, owner.ID, "rw")

	if got := CheckPerm(repoID, member.ID); got != "r" {
		t.Errorf("permission = %q, want r — the individual share must win", got)
	}
}

func TestCheckPermGroupShareGrantsPermission(t *testing.T) {
	write := setupShareTest(t, false)
	owner := makeAccount(t, "owner4@example.com")
	member := makeAccount(t, "member4@example.com")
	repoID := makeRepo(t, owner)

	groupID := makeGroup(t, write, "team4", owner.ID, member.ID)
	shareRepoToGroup(t, write, repoID, groupID, owner.ID, "rw")

	if got := CheckPerm(repoID, member.ID); got != "rw" {
		t.Errorf("group-shared permission = %q, want rw", got)
	}
}

// checkGroupPermByUser's own precedence rule: rw from any group wins over r
// from another, and the query carries no ORDER BY to make that trivial — the
// loop has to get the right answer whichever order the rows arrive in.
func TestCheckPermPrefersReadWriteWhenTwoGroupsDisagree(t *testing.T) {
	write := setupShareTest(t, false)
	owner := makeAccount(t, "owner5@example.com")
	member := makeAccount(t, "member5@example.com")
	repoID := makeRepo(t, owner)

	readers := makeGroup(t, write, "readers", owner.ID, member.ID)
	shareRepoToGroup(t, write, repoID, readers, owner.ID, "r")
	writers := makeGroup(t, write, "writers", owner.ID, member.ID)
	shareRepoToGroup(t, write, repoID, writers, owner.ID, "rw")

	if got := CheckPerm(repoID, member.ID); got != "rw" {
		t.Errorf("permission with a read and a read-write group = %q, want rw", got)
	}
}

// InnerPubRepo is the self-hosted "anyone signed in may read this" switch,
// and it must not leak into cloud mode: a multi-tenant deployment has no
// business granting access on the strength of a row meant for a single
// self-hosted instance's whole user base.
func TestCheckPermInnerPubRepoOnlyAppliesOutsideCloudMode(t *testing.T) {
	for _, tc := range []struct {
		name  string
		cloud bool
		want  string
	}{
		{"self-hosted grants it", false, "r"},
		{"cloud mode ignores it", true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			write := setupShareTest(t, tc.cloud)
			owner := makeAccount(t, "iowner-"+tc.name+"@example.com")
			stranger := makeAccount(t, "istranger-"+tc.name+"@example.com")
			repoID := makeRepo(t, owner)
			if _, err := write.Exec("INSERT INTO InnerPubRepo (repo_id, permission) VALUES (?, ?)", repoID, "r"); err != nil {
				t.Fatalf("insert inner pub repo: %v", err)
			}

			if got := CheckPerm(repoID, stranger.ID); got != tc.want {
				t.Errorf("permission = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestCheckPermVirtualRepoOwnerOfOriginHasReadWrite(t *testing.T) {
	write := setupShareTest(t, false)
	owner := makeAccount(t, "owner7@example.com")
	origin := makeRepo(t, owner)
	vRepoID := makeVirtualRepo(t, write, origin, "/sub")

	if got := CheckPerm(vRepoID, owner.ID); got != "rw" {
		t.Errorf("origin owner on a virtual repo = %q, want rw", got)
	}
}

// A subfolder share is recorded against the virtual repo's own id, not the
// origin's, and must not be visible through the origin repo itself — sharing
// "/sub" is not sharing the whole library.
func TestCheckPermVirtualRepoGrantsTheSubfolderShare(t *testing.T) {
	write := setupShareTest(t, false)
	owner := makeAccount(t, "owner9@example.com")
	friend := makeAccount(t, "friend9@example.com")
	origin := makeRepo(t, owner)
	vRepoID := makeVirtualRepo(t, write, origin, "/sub")

	shareRepo(t, write, vRepoID, owner.ID, friend.ID, "r")

	if got := CheckPerm(vRepoID, friend.ID); got != "r" {
		t.Errorf("subfolder share permission = %q, want r", got)
	}
	if got := CheckPerm(origin, friend.ID); got != "" {
		t.Errorf("the subfolder share leaked onto the origin repo: %q", got)
	}
}

// With no subfolder-specific share, checkVirtualRepoPerm falls all the way
// back to a blanket share of the whole origin repo — the last of the three
// checks it runs in order.
func TestCheckPermVirtualRepoFallsBackToABlanketOriginShare(t *testing.T) {
	write := setupShareTest(t, false)
	owner := makeAccount(t, "owner10@example.com")
	friend := makeAccount(t, "friend10@example.com")
	origin := makeRepo(t, owner)
	vRepoID := makeVirtualRepo(t, write, origin, "/sub")

	shareRepo(t, write, origin, owner.ID, friend.ID, "rw")

	if got := CheckPerm(vRepoID, friend.ID); got != "rw" {
		t.Errorf("permission via a blanket origin share = %q, want rw", got)
	}
}

// GetReposByOwner is the account's own-library listing, read by every
// "your libraries" response; empty must mean exactly that, not an error.
func TestGetReposByOwnerListsOwnedLibrariesOnly(t *testing.T) {
	setupShareTest(t, false)
	owner := makeAccount(t, "lister@example.com")
	other := makeAccount(t, "other@example.com")
	repoID := makeRepo(t, owner)
	_ = makeRepo(t, other)

	repos, err := GetReposByOwner(owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != 1 || repos[0].ID != repoID {
		t.Fatalf("GetReposByOwner = %+v, want just %s", repos, repoID)
	}

	stranger := makeAccount(t, "nothing-owned@example.com")
	none, err := GetReposByOwner(stranger.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(none) != 0 {
		t.Errorf("an owner with no libraries got %d back, want 0", len(none))
	}
}
