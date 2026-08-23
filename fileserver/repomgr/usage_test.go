package repomgr

import (
	"bytes"
	"context"
	"testing"

	"github.com/dkam/silo/fileserver/objmgr"
	"github.com/dkam/silo/fileserver/option"
	storefmt "github.com/dkam/silo/store"
)

// newLibrary creates a store-v2 library owned by the test account and returns
// it loaded.
func newLibrary(t *testing.T, name string) *Repo {
	t.Helper()
	repoID, err := CreateRepo(name, testAccount(t), DefaultFormat(false))
	if err != nil {
		t.Fatalf("CreateRepo: %v", err)
	}
	repo, err := GetWithReason(repoID)
	if err != nil {
		t.Fatalf("load %s: %v", name, err)
	}
	return repo
}

// commitFile writes a file into a library and moves its head, the way a
// mutation on the wire would. The test does the head move itself because
// accounting deliberately has no hook there — the point being tested is that
// it does not need one.
func commitFile(t *testing.T, repo *Repo, path string, content []byte) {
	t.Helper()
	st, err := repo.Store()
	if err != nil {
		t.Fatal(err)
	}
	root, err := storefmt.ParseID(repo.RootID)
	if err != nil {
		t.Fatal(err)
	}
	m, err := st.WriteFile(bytes.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	id, err := st.PutManifest(m)
	if err != nil {
		t.Fatal(err)
	}
	newRoot, err := st.PutNode(root, path, objmgr.Node{ID: id, Type: storefmt.NodeFile, Mode: 0o644}, 1)
	if err != nil {
		t.Fatalf("PutNode %s: %v", path, err)
	}
	moveHead(t, repo, st, newRoot)
}

func removePath(t *testing.T, repo *Repo, path string) {
	t.Helper()
	st, err := repo.Store()
	if err != nil {
		t.Fatal(err)
	}
	root, err := storefmt.ParseID(repo.RootID)
	if err != nil {
		t.Fatal(err)
	}
	newRoot, err := st.Remove(root, path, 2)
	if err != nil {
		t.Fatalf("Remove %s: %v", path, err)
	}
	moveHead(t, repo, st, newRoot)
}

func moveHead(t *testing.T, repo *Repo, st *objmgr.Store, newRoot storefmt.ID) {
	t.Helper()
	parent, err := storefmt.ParseID(repo.HeadCommitID)
	if err != nil {
		t.Fatal(err)
	}
	commitID, err := st.PutCommit(&storefmt.Commit{Root: newRoot, Parents: []storefmt.ID{parent}, CreatedAt: 2, Author: testOwner})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := option.WithDBTimeout(context.Background())
	defer cancel()
	if _, err := writeDB.ExecContext(ctx,
		"UPDATE Branch SET commit_id = ?, root_id = ? WHERE repo_id = ? AND name = 'master'",
		commitID.String(), newRoot.String(), repo.ID); err != nil {
		t.Fatal(err)
	}
	repo.HeadCommitID = commitID.String()
	repo.RootID = newRoot.String()
}

func recordedRoot(t *testing.T, repoID string) string {
	t.Helper()
	_, at, err := readUsage(repoID)
	if err != nil {
		t.Fatal(err)
	}
	return at
}

// A library nobody has ever asked about has no row, and the first question
// measures it outright. After that the row is the answer.
func TestUsageMeasuresALibraryWithNoRowYet(t *testing.T) {
	getTestStore(t)
	repo := newLibrary(t, "Fresh")

	u, err := Usage(repo)
	if err != nil {
		t.Fatalf("Usage: %v", err)
	}
	if u != (objmgr.Usage{}) {
		t.Fatalf("a new library uses %+v, want nothing", u)
	}

	commitFile(t, repo, "/a.txt", bytes.Repeat([]byte("a"), 300))
	u, err = Usage(repo)
	if err != nil {
		t.Fatalf("Usage: %v", err)
	}
	if u != (objmgr.Usage{Size: 300, FileCount: 1}) {
		t.Fatalf("usage = %+v, want 300 bytes in 1 file", u)
	}
	if at := recordedRoot(t, repo.ID); at != repo.RootID {
		t.Errorf("row records root %q, library is on %q — the answer was not stored", at, repo.RootID)
	}
}

// The whole point of recording the root: a library that has not moved is
// answered from the row, and a library that has moved is brought forward by a
// delta. Neither needs anything on the write path to have remembered.
func TestUsageFollowsALibraryWithoutAWriteHook(t *testing.T) {
	getTestStore(t)
	repo := newLibrary(t, "Followed")

	commitFile(t, repo, "/big.txt", bytes.Repeat([]byte("b"), 5000))
	if u, _ := Usage(repo); u.Size != 5000 {
		t.Fatalf("usage = %+v after the first write", u)
	}

	// Three head moves with nobody looking, then one question.
	commitFile(t, repo, "/small.txt", bytes.Repeat([]byte("s"), 10))
	commitFile(t, repo, "/big.txt", bytes.Repeat([]byte("B"), 1000))
	removePath(t, repo, "/small.txt")

	u, err := Usage(repo)
	if err != nil {
		t.Fatalf("Usage: %v", err)
	}
	if u != (objmgr.Usage{Size: 1000, FileCount: 1}) {
		t.Fatalf("usage = %+v, want the 1000-byte file alone", u)
	}
}

// A stale row whose recorded root is gone must not become a permanent error.
// This is the retention case: a library written once, left alone past the
// history cutoff, and asked about afterwards, when the tree the row names has
// been collected. It falls back to measuring what is there now.
func TestUsageFallsBackToAFullWalkWhenTheOldTreeIsGone(t *testing.T) {
	getTestStore(t)
	repo := newLibrary(t, "Cut")
	commitFile(t, repo, "/a.txt", bytes.Repeat([]byte("a"), 100))
	if _, err := Usage(repo); err != nil {
		t.Fatal(err)
	}

	// Point the row at a root that was never stored, which is what a collected
	// one looks like from here.
	gone := storefmt.ID{1, 2, 3}
	ctx, cancel := option.WithDBTimeout(context.Background())
	defer cancel()
	if _, err := writeDB.ExecContext(ctx,
		"UPDATE RepoUsage SET root_id = ?, size = 999999, file_count = 42 WHERE repo_id = ?",
		gone.String(), repo.ID); err != nil {
		t.Fatal(err)
	}
	commitFile(t, repo, "/b.txt", bytes.Repeat([]byte("b"), 50))

	u, err := Usage(repo)
	if err != nil {
		t.Fatalf("a lost baseline made usage unanswerable: %v", err)
	}
	if u != (objmgr.Usage{Size: 150, FileCount: 2}) {
		t.Fatalf("usage = %+v, want the current tree measured outright", u)
	}
}

// Two readers finding the same stale row must not both add their delta to it.
// The conditional update is what stops that, so a second publication computed
// from a root that is no longer recorded has to be dropped on the floor.
func TestALateWriteDoesNotAddItselfTwice(t *testing.T) {
	getTestStore(t)
	repo := newLibrary(t, "Raced")
	commitFile(t, repo, "/a.txt", bytes.Repeat([]byte("a"), 100))

	u, err := Usage(repo)
	if err != nil {
		t.Fatal(err)
	}
	stale := repo.RootID
	commitFile(t, repo, "/b.txt", bytes.Repeat([]byte("b"), 100))
	if _, err := Usage(repo); err != nil {
		t.Fatal(err)
	}

	// The loser of the race finishes late and tries to publish a total it
	// computed from the root that has since been superseded.
	if err := writeUsage(repo.ID, u.Add(objmgr.Usage{Size: 100, FileCount: 1}), stale, repo.RootID); err != nil {
		t.Fatal(err)
	}
	got, err := Usage(repo)
	if err != nil {
		t.Fatal(err)
	}
	if got != (objmgr.Usage{Size: 200, FileCount: 2}) {
		t.Fatalf("usage = %+v after a late write, want 200 bytes in 2 files", got)
	}
}

// An account is charged for what it owns, and the total is the sum of the
// libraries' own numbers.
func TestAccountUsageSumsTheLibrariesAnAccountOwns(t *testing.T) {
	getTestStore(t)
	acct := testAccount(t)

	one := newLibrary(t, "One")
	two := newLibrary(t, "Two")
	commitFile(t, one, "/x", bytes.Repeat([]byte("x"), 700))
	commitFile(t, two, "/y", bytes.Repeat([]byte("y"), 300))
	commitFile(t, two, "/z", bytes.Repeat([]byte("z"), 25))

	total, err := AccountUsage(acct.ID)
	if err != nil {
		t.Fatalf("AccountUsage: %v", err)
	}
	if total != (objmgr.Usage{Size: 1025, FileCount: 3}) {
		t.Fatalf("account usage = %+v, want 1025 bytes in 3 files", total)
	}

	// And it stays right when a library moves under it, since the sweep brings
	// stale rows forward the same way a single library's read does.
	removePath(t, one, "/x")
	total, err = AccountUsage(acct.ID)
	if err != nil {
		t.Fatal(err)
	}
	if total != (objmgr.Usage{Size: 325, FileCount: 2}) {
		t.Fatalf("account usage = %+v after a delete, want 325 bytes in 2 files", total)
	}
}
