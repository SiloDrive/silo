package silod

import (
	"testing"
	"time"

	"github.com/dkam/silo/fileserver/repomgr"
)

const (
	originRepo = "aaaa1111-2222-3333-4444-555555555555"
	childRepo  = "bbbb1111-2222-3333-4444-555555555555"
	otherRepo  = "cccc1111-2222-3333-4444-555555555555"
)

func seedRepo(t *testing.T, repoID, email, token string) {
	t.Helper()
	dbExec(t, "INSERT INTO Repo (repo_id) VALUES (?)", repoID)
	dbExec(t, "INSERT INTO Branch (name, repo_id, commit_id) VALUES (?, ?, ?)",
		"master", repoID, "0401fc662e3bc87a41f299a907c056aaf8322a27")
	dbExec(t, "INSERT INTO RepoHead (repo_id, branch_name) VALUES (?, ?)", repoID, "master")
	id := mintAccount(t, email).ID
	dbExec(t, "INSERT INTO RepoOwner (repo_id, account_id) VALUES (?, ?)", repoID, id)
	dbExec(t, "INSERT INTO RepoUserToken (repo_id, account_id, token, ctime) VALUES (?, ?, ?, ?)",
		repoID, id, token, time.Now().Unix())
}

// Deleting an origin removed its children's VirtualRepo rows but left their
// Repo, Branch and RepoUserToken rows behind. Each child survived as an
// apparently ordinary library whose StoreID still pointed at the origin's
// object store — which GC had just reclaimed — so a client kept syncing
// against an empty store and nothing ever cleaned the rows up.
func TestDeleteRepoCascadesToVirtualRepos(t *testing.T) {
	sqliteTestDB(t)
	repomgr.Init(siloPair.Read, siloPair.Write)

	seedRepo(t, originRepo, "owner@example.com", "1111aaaa1111aaaa1111aaaa1111aaaa1111aaaa")
	seedRepo(t, childRepo, "owner@example.com", "2222bbbb2222bbbb2222bbbb2222bbbb2222bbbb")
	seedRepo(t, otherRepo, "owner@example.com", "3333cccc3333cccc3333cccc3333cccc3333cccc")
	dbExec(t, "INSERT INTO VirtualRepo (repo_id, origin_repo, path, base_commit) VALUES (?, ?, ?, ?)",
		childRepo, originRepo, "/sub", "0401fc662e3bc87a41f299a907c056aaf8322a27")

	if err := repomgr.DeleteRepo(originRepo); err != nil {
		t.Fatalf("DeleteRepo returned %v", err)
	}

	for _, table := range []string{"Repo", "Branch", "RepoHead", "RepoOwner", "RepoUserToken"} {
		if n := countRows(t, "SELECT COUNT(*) FROM "+table+" WHERE repo_id = ?", childRepo); n != 0 {
			t.Errorf("%s still holds %d row(s) for the orphaned virtual repo", table, n)
		}
		if n := countRows(t, "SELECT COUNT(*) FROM "+table+" WHERE repo_id = ?", originRepo); n != 0 {
			t.Errorf("%s still holds %d row(s) for the deleted origin", table, n)
		}
		// An unrelated library must be untouched.
		if n := countRows(t, "SELECT COUNT(*) FROM "+table+" WHERE repo_id = ?", otherRepo); n != 1 {
			t.Errorf("%s holds %d row(s) for an unrelated library, want 1", table, n)
		}
	}

	if n := countRows(t, "SELECT COUNT(*) FROM VirtualRepo WHERE repo_id = ?", childRepo); n != 0 {
		t.Error("the VirtualRepo row survived")
	}

	// Both have to reach GC, or the child's storage directory leaks forever.
	for _, repoID := range []string{originRepo, childRepo} {
		if n := countRows(t, "SELECT COUNT(*) FROM GarbageRepos WHERE repo_id = ?", repoID); n != 1 {
			t.Errorf("%s was not recorded in GarbageRepos", repoID)
		}
	}
}

// A repo listed as its own origin is corrupt data, not a reason to recurse
// until the stack runs out.
func TestDeleteRepoSurvivesSelfReferencingVirtualRepo(t *testing.T) {
	sqliteTestDB(t)
	repomgr.Init(siloPair.Read, siloPair.Write)

	seedRepo(t, originRepo, "owner@example.com", "1111aaaa1111aaaa1111aaaa1111aaaa1111aaaa")
	dbExec(t, "INSERT INTO VirtualRepo (repo_id, origin_repo, path, base_commit) VALUES (?, ?, ?, ?)",
		originRepo, originRepo, "/", "0401fc662e3bc87a41f299a907c056aaf8322a27")

	done := make(chan error, 1)
	go func() { done <- repomgr.DeleteRepo(originRepo) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("DeleteRepo returned %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("DeleteRepo did not finish; probable infinite recursion")
	}

	if n := countRows(t, "SELECT COUNT(*) FROM Repo WHERE repo_id = ?", originRepo); n != 0 {
		t.Error("the repo was not deleted")
	}
}
