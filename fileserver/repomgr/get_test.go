package repomgr

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dkam/silo/fileserver/account"
	"github.com/dkam/silo/fileserver/commitmgr"
	"github.com/dkam/silo/fileserver/dbutil"
	"github.com/dkam/silo/fileserver/objstore"
	"github.com/dkam/silo/fileserver/option"
	"github.com/dkam/silo/store"
)

const (
	testRepoID  = "328be500-1111-2222-3333-444455556666"
	absentRepo  = "99999999-8888-7777-6666-555555555555"
	testOwner   = "owner@example.com"
	missingHead = "ac9b78b5c6f7b1905277627bafef0fe99beb15ff"
)

// getTestStore wires a SQLite database and a real on-disk object store, and
// returns the data directory so a test can take an object back out of it.
func getTestStore(t *testing.T) string {
	t.Helper()

	option.LoadFileServerOptions("") // defaults, incl. a non-zero DBOpTimeout

	dir := t.TempDir()
	pair, err := dbutil.OpenSQLite(filepath.Join(dir, "silo.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = pair.Close() })
	if err := dbutil.CreateSiloTables(pair.Write); err != nil {
		t.Fatalf("create seafile tables: %v", err)
	}

	dataDir := filepath.Join(dir, "seafile-data")
	Init(pair.Read, pair.Write)
	account.Init(pair.Read, pair.Write)
	commitmgr.Init(dir, dataDir)

	// Faults are suppressed per (repo, kind) for five minutes, and the map is
	// package state that outlives one test.
	t.Cleanup(func() {
		clearFaults(testRepoID)
		clearFaults(absentRepo)
	})

	return dataDir
}

// testAccount mints the account a test library is owned by.
func testAccount(t *testing.T) *account.Account {
	t.Helper()
	ctx, cancel := option.WithDBTimeout(context.Background())
	defer cancel()
	if _, _, err := account.Create(ctx, testOwner, "", false); err != nil {
		t.Fatalf("create account: %v", err)
	}
	acct, err := account.ByEmail(ctx, testOwner)
	if err != nil {
		t.Fatalf("read account: %v", err)
	}
	return acct
}

// A library with no row is the one condition that is a statement about the
// request, and the only one a client may act on by forgetting the library.
func TestGetWithReasonMissingRowIsNotFound(t *testing.T) {
	getTestStore(t)

	repo, err := GetWithReason(absentRepo)
	if repo != nil {
		t.Fatal("got a repo for an id that was never created")
	}
	if !errors.Is(err, ErrRepoNotFound) {
		t.Fatalf("err = %v, want ErrRepoNotFound", err)
	}
	if code, _ := StatusFor(err); code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", code, http.StatusNotFound)
	}
}

// The bug: a `git filter-branch` removed the object store out from under a
// running server while the database survived, and every endpoint that resolved
// the library answered 404 "Repo not found" — which tells a sync client the
// library was deleted, and the copy it would then delete is the one that could
// have restored the object.
func TestGetWithReasonMissingCommitIsCorruptedNotNotFound(t *testing.T) {
	dataDir := getTestStore(t)

	repoID, err := CreateRepo("Porter Test", testAccount(t), DefaultFormat(false))
	if err != nil {
		t.Fatalf("CreateRepo: %v", err)
	}

	repo, err := GetWithReason(repoID)
	if err != nil {
		t.Fatalf("a freshly created repo did not load: %v", err)
	}
	head := repo.HeadCommitID

	// Exactly the damage the accident caused: the row stays, the object goes.
	if err := os.RemoveAll(objstore.RepoDir(dataDir, objstore.TypeCommits, repoID)); err != nil {
		t.Fatalf("remove commit store: %v", err)
	}
	t.Cleanup(func() { clearFaults(repoID) })

	repo, err = GetWithReason(repoID)
	if repo != nil {
		t.Fatal("got a repo whose head commit is gone")
	}
	if errors.Is(err, ErrRepoNotFound) {
		t.Fatalf("a missing object reported the library as absent: %v", err)
	}
	if !errors.Is(err, ErrRepoCorrupted) {
		t.Fatalf("err = %v, want ErrRepoCorrupted", err)
	}
	code, msg := StatusFor(err)
	if code != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d", code, http.StatusInternalServerError)
	}
	if msg == "" {
		t.Error("the 500 carries no explanation for the client")
	}
	// The detail has to survive the wrapping, or nothing in the log says which
	// object went missing.
	if !strings.Contains(err.Error(), head) {
		t.Errorf("err = %q, does not name the head commit %s", err, head)
	}

	// Get keeps its old signature for the callers that only need the repo.
	if got := Get(repoID); got != nil {
		t.Error("Get returned a repo whose head commit is gone")
	}
}

// A Branch row with an empty commit id is a corrupt row, not a missing one.
func TestGetWithReasonEmptyHeadIsCorrupted(t *testing.T) {
	getTestStore(t)

	f := DefaultFormat(false)
	if _, err := seafileWriteDB.Exec(
		"INSERT INTO Repo (repo_id, chunker, chunk_min, chunk_target, chunk_max, chunk_norm, e2ee) VALUES (?, ?, ?, ?, ?, ?, ?)",
		testRepoID, f.Chunker, f.MinSize, f.TargetSize, f.MaxSize, f.Normalization, f.E2EE); err != nil {
		t.Fatalf("seed Repo: %v", err)
	}
	if _, err := seafileWriteDB.Exec(
		"INSERT INTO Branch (name, repo_id, commit_id) VALUES ('master', ?, '')", testRepoID); err != nil {
		t.Fatalf("seed Branch: %v", err)
	}

	_, err := GetWithReason(testRepoID)
	if !errors.Is(err, ErrRepoCorrupted) {
		t.Fatalf("err = %v, want ErrRepoCorrupted", err)
	}
	if code, _ := StatusFor(err); code != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d", code, http.StatusInternalServerError)
	}
}

// A database that cannot be read says nothing about whether the library
// exists, so it must not answer for it.
func TestGetWithReasonDeadDatabaseIsUnavailable(t *testing.T) {
	getTestStore(t)

	// Closing the read handle is the closest stand-in for the database being
	// away that does not need one to be running.
	if err := seafileDB.Close(); err != nil {
		t.Fatalf("close read handle: %v", err)
	}

	_, err := GetWithReason(testRepoID)
	if !errors.Is(err, ErrRepoUnavailable) {
		t.Fatalf("err = %v, want ErrRepoUnavailable", err)
	}
	code, _ := StatusFor(err)
	if code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", code, http.StatusServiceUnavailable)
	}
}

// A persistent fault is rediscovered by every retrying client. Reporting it
// once per interval is what keeps one server fault from becoming twelve log
// lines and twelve error reports — and a repair has to re-arm it, or a
// recurrence would be silent.
func TestFaultIsReportedOnceUntilRepaired(t *testing.T) {
	getTestStore(t)

	if !firstReport(testRepoID, ErrRepoCorrupted) {
		t.Fatal("the first occurrence was suppressed")
	}
	if firstReport(testRepoID, ErrRepoCorrupted) {
		t.Error("the same fault was reported twice")
	}
	// A different fault on the same library is a different thing to say.
	if !firstReport(testRepoID, ErrRepoUnavailable) {
		t.Error("an unrelated fault was suppressed as a repeat")
	}
	// As is the same fault on a different library.
	if !firstReport(absentRepo, ErrRepoCorrupted) {
		t.Error("a fault on another library was suppressed as a repeat")
	}

	clearFaults(testRepoID)
	if !firstReport(testRepoID, ErrRepoCorrupted) {
		t.Error("a recurrence after a repair was suppressed")
	}
}

// The write and the read have to agree, and they are in different functions
// with the column list spelled out twice. A transposed pair here — target and
// max, say — produces a library that loads, chunks, and cuts in places no
// other client will reproduce, with nothing anywhere reporting a problem.
func TestALibrarysFormatSurvivesTheCatalog(t *testing.T) {
	getTestStore(t)

	want := Format{
		Chunker:       store.ChunkerAlgorithm,
		MinSize:       128 << 10,
		TargetSize:    512 << 10,
		MaxSize:       2 << 20,
		Normalization: 1,
		E2EE:          false,
	}
	if err := want.Validate(); err != nil {
		t.Fatalf("the test's own format is invalid: %v", err)
	}

	// Deliberately not the defaults: defaults would survive a loader that
	// dropped the columns entirely and filled them in from store's constants.
	repoID, err := CreateRepo("Formatted", testAccount(t), want)
	if err != nil {
		t.Fatalf("CreateRepo: %v", err)
	}

	repo, err := GetWithReason(repoID)
	if err != nil {
		t.Fatalf("a freshly created repo did not load: %v", err)
	}
	if repo.Format != want {
		t.Fatalf("loaded %+v, created %+v", repo.Format, want)
	}

	// GetEx reads the same row through its own copy of the query.
	if ex := GetEx(repoID); ex == nil || ex.Format != want {
		t.Fatalf("GetEx loaded %+v, created %+v", ex, want)
	}
}

// An E2EE library's initial commit is sealed under a key the server does not
// have, so the server must refuse to mint one rather than create a library
// whose history it silently wrote in the clear.
func TestTheServerWillNotCreateAnEncryptedLibrary(t *testing.T) {
	getTestStore(t)

	if _, err := CreateRepo("Secret", testAccount(t), DefaultFormat(true)); !errors.Is(err, ErrNoContentKey) {
		t.Fatalf("the server created an E2EE library: %v", err)
	}
}

// The library's display metadata and its root come out of the catalog now, not
// out of the head commit. Everything this asserts used to be read back from a
// commit object on every load.
func TestALibrarysMetadataComesFromTheCatalog(t *testing.T) {
	getTestStore(t)
	owner := testAccount(t)

	repoID, err := CreateRepo("Holiday photos", owner, DefaultFormat(false))
	if err != nil {
		t.Fatalf("CreateRepo: %v", err)
	}
	repo, err := GetWithReason(repoID)
	if err != nil {
		t.Fatal(err)
	}
	if repo.Name != "Holiday photos" {
		t.Errorf("name = %q, want %q", repo.Name, "Holiday photos")
	}
	if repo.LastModifier != owner.Email {
		t.Errorf("last modifier = %q, want %q", repo.LastModifier, owner.Email)
	}
	if repo.LastModificationTime == 0 {
		t.Error("no modification time")
	}
	if repo.RootID == "" {
		t.Error("no root id — the head's root has to come from the same row as the head")
	}
	if repo.HeadCommitID == "" {
		t.Error("no head commit id")
	}
}

// A rename is a catalog UPDATE and moves no head. The old spelling wrote a
// commit whose only change was the name it carried, so every client saw a new
// head, fetched it, and diffed two identical roots.
func TestRenamingALibraryDoesNotMoveItsHead(t *testing.T) {
	getTestStore(t)

	repoID, err := CreateRepo("Before", testAccount(t), DefaultFormat(false))
	if err != nil {
		t.Fatalf("CreateRepo: %v", err)
	}
	before, err := GetWithReason(repoID)
	if err != nil {
		t.Fatal(err)
	}

	if err := SetRepoName(repoID, "After"); err != nil {
		t.Fatalf("SetRepoName: %v", err)
	}

	after, err := GetWithReason(repoID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Name != "After" {
		t.Errorf("name = %q, want %q", after.Name, "After")
	}
	if after.HeadCommitID != before.HeadCommitID {
		t.Errorf("the head moved on a rename: %s -> %s", before.HeadCommitID, after.HeadCommitID)
	}
	if after.RootID != before.RootID {
		t.Errorf("the root changed on a rename: %s -> %s", before.RootID, after.RootID)
	}
}

// A store-v2 library has no Seafile key ceremony to read and no commit the old
// decoder could parse, so nothing must try. The head id's width is what says
// which format a library is in.
func TestAStoreV2HeadIsNotHandedToTheSeafileCommitDecoder(t *testing.T) {
	repo := &Repo{ID: "does-not-matter", HeadCommitID: strings.Repeat("a", 64)}
	if err := loadSeafileCrypto(repo); err != nil {
		t.Fatalf("a 64-character head went looking for a Seafile commit: %v", err)
	}
	if repo.IsEncrypted || repo.Version != 0 {
		t.Error("a store-v2 head filled in the vestigial fields")
	}
}
