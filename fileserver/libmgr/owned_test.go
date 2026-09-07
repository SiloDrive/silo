package libmgr

import (
	"context"
	"slices"
	"testing"

	"github.com/dkam/silo/fileserver/account"
	"github.com/dkam/silo/fileserver/option"
)

// OwnedLibraryIDs answers with the account's own libraries and nobody else's.
//
// It is the owned half of the set an account-scoped notification socket rings
// for; a library that leaked into it from another owner would be a ring about
// something the account cannot see.
func TestOwnedLibraryIDsListsOnlyTheAccountsOwn(t *testing.T) {
	getTestStore(t)
	owner := testAccount(t)

	ctx, cancel := option.WithDBTimeout(context.Background())
	defer cancel()
	if _, _, err := account.Create(ctx, "other@example.com", "", account.RoleUser); err != nil {
		t.Fatalf("create account: %v", err)
	}
	other, err := account.ByEmail(ctx, "other@example.com")
	if err != nil {
		t.Fatalf("read account: %v", err)
	}

	var want []string
	for _, name := range []string{"Photos", "Papers"} {
		id, err := CreateLibrary(name, owner, DefaultFormat(false))
		if err != nil {
			t.Fatalf("CreateLibrary: %v", err)
		}
		want = append(want, id)
	}
	if _, err := CreateLibrary("Theirs", other, DefaultFormat(false)); err != nil {
		t.Fatalf("CreateLibrary: %v", err)
	}

	got, err := OwnedLibraryIDs(ctx, owner.ID)
	if err != nil {
		t.Fatalf("OwnedLibraryIDs: %v", err)
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}

	none, err := OwnedLibraryIDs(ctx, account.ID{})
	if err != nil {
		t.Fatalf("OwnedLibraryIDs for no account: %v", err)
	}
	if len(none) != 0 {
		t.Errorf("an account that exists nowhere owns %v", none)
	}
}
