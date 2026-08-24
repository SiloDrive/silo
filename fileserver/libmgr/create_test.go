package libmgr

import (
	"errors"
	"testing"

	"github.com/dkam/silo/store"
)

// A new library is store-v2. The ids are the visible half of that — sixty-four
// hex characters rather than forty — and the empty root is the other: the
// commit points at a directory object that decodes and holds nothing.
func TestACreatedLibraryIsStoreV2(t *testing.T) {
	getTestStore(t)

	libraryID, err := CreateLibrary("Fresh", testAccount(t), DefaultFormat(false))
	if err != nil {
		t.Fatalf("CreateLibrary: %v", err)
	}
	library, err := GetWithReason(libraryID)
	if err != nil {
		t.Fatalf("a freshly created library did not load: %v", err)
	}

	for _, f := range []struct{ what, id string }{
		{"head commit", library.HeadCommitID},
		{"root", library.RootID},
	} {
		if len(f.id) != 2*store.IDSize {
			t.Errorf("%s id %q is %d characters, want %d — this library is not store-v2",
				f.what, f.id, len(f.id), 2*store.IDSize)
		}
	}

	st, err := library.Store()
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	commitID, err := store.ParseID(library.HeadCommitID)
	if err != nil {
		t.Fatalf("parse head id: %v", err)
	}
	commit, err := st.GetCommit(commitID)
	if err != nil {
		t.Fatalf("the head commit did not decode: %v", err)
	}
	if commit.Root.String() != library.RootID {
		t.Errorf("commit root %s, Branch root_id %s — the catalog and the object disagree",
			commit.Root, library.RootID)
	}
	if len(commit.Parents) != 0 {
		t.Errorf("an initial commit has %d parents, want none", len(commit.Parents))
	}
	if commit.Author != testOwner {
		t.Errorf("author = %q, want %q", commit.Author, testOwner)
	}

	// The library name is not in the commit and must never be: the catalog is
	// the authority, and a name sealed in an E2EE commit could not be listed.
	if commit.Message == "Fresh" {
		t.Error("the commit carries the library name; the catalog is the authority for it")
	}

	entries, err := st.List(commit.Root)
	if err != nil {
		t.Fatalf("the root directory did not decode: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("a new library's root holds %d entries, want none", len(entries))
	}
}

// E2EE creation is refused, and the refusal names the content key rather than
// the commit. The server not being able to mint a sealed commit was never the
// blocker it looked like — a client mints it. What is missing is anywhere for
// the wrapped key to live, so this must keep failing until that exists.
func TestCreatingAnEncryptedLibraryIsRefusedForTheKeyNotTheCommit(t *testing.T) {
	getTestStore(t)

	_, err := CreateLibrary("Sealed", testAccount(t), DefaultFormat(true))
	if !errors.Is(err, ErrNoContentKey) {
		t.Fatalf("err = %v, want ErrNoContentKey", err)
	}
}
