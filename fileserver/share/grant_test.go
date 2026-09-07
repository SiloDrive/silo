package share

import (
	"context"
	"testing"

	"github.com/dkam/silo/fileserver/account"
	"github.com/dkam/silo/fileserver/option"
)

func grantCtx(t *testing.T) context.Context {
	t.Helper()
	c, cancel := option.WithDBTimeout(context.Background())
	t.Cleanup(cancel)
	return c
}

// Sharing again with a different permission is a correction, not a second
// fact. Without the unique index and the upsert, both rows would stand and the
// answer would come from whichever the query happened to reach first -- which
// is a permission that changes when nothing changed.
func TestGrantingAgainReplacesRatherThanAccumulates(t *testing.T) {
	setupShareTest(t)
	owner := makeAccount(t, "owner@example.com")
	friend := makeAccount(t, "friend@example.com")
	libraryID := makeLibrary(t, owner)

	for _, perm := range []string{"rw", "r"} {
		if err := Add(grantCtx(t), Grant{
			Principal: UserPrincipal(friend.ID), LibraryID: libraryID, Perm: perm,
		}); err != nil {
			t.Fatalf("granting %s: %v", perm, err)
		}
	}
	grants, err := ForLibrary(grantCtx(t), libraryID)
	if err != nil {
		t.Fatal(err)
	}
	if len(grants) != 1 {
		t.Fatalf("the library holds %d grants, want the one correction", len(grants))
	}
	if grants[0].Perm != "r" {
		t.Errorf("perm = %q, want the second grant to have replaced the first", grants[0].Perm)
	}
	if got := CheckPerm(libraryID, friend.ID); got != "r" {
		t.Errorf("CheckPerm = %q, want r", got)
	}
}

func TestARevokedGrantStopsAnswering(t *testing.T) {
	setupShareTest(t)
	owner := makeAccount(t, "owner@example.com")
	friend := makeAccount(t, "friend@example.com")
	libraryID := makeLibrary(t, owner)

	if err := Add(grantCtx(t), Grant{
		Principal: UserPrincipal(friend.ID), LibraryID: libraryID, Perm: "rw",
	}); err != nil {
		t.Fatal(err)
	}
	if err := Remove(grantCtx(t), UserPrincipal(friend.ID), libraryID, "/"); err != nil {
		t.Fatal(err)
	}
	if got := CheckPerm(libraryID, friend.ID); got != "" {
		t.Errorf("CheckPerm after revoking = %q, want denied", got)
	}
	// Removing one that is not there is the state the caller asked for.
	if err := Remove(grantCtx(t), UserPrincipal(friend.ID), libraryID, "/"); err != nil {
		t.Errorf("removing an absent grant: %v", err)
	}
}

// A permission outside the model is refused at the write. A column holding
// something no rule recognises is a grant that matches nothing -- denied
// everything, or allowed it, depending on which way the reader is written.
func TestAGrantOutsideTheModelIsRefused(t *testing.T) {
	setupShareTest(t)
	owner := makeAccount(t, "owner@example.com")
	libraryID := makeLibrary(t, owner)

	for _, bad := range []Grant{
		{Principal: UserPrincipal(owner.ID), LibraryID: libraryID, Perm: "w"},
		{Principal: UserPrincipal(owner.ID), LibraryID: libraryID, Perm: ""},
		{Principal: UserPrincipal(owner.ID), LibraryID: libraryID, Perm: "admin"},
		{Principal: "", LibraryID: libraryID, Perm: "r"},
		{Principal: UserPrincipal(owner.ID), LibraryID: "", Perm: "r"},
	} {
		if err := Add(grantCtx(t), bad); err == nil {
			t.Errorf("Add accepted %+v", bad)
		}
	}
}

// The precedence the old return-statement order encoded, now asked of the model
// directly: most specific kind wins, and within a kind the stronger permission.
func TestPrecedenceIsMostSpecificKindThenStrongestWithin(t *testing.T) {
	setupShareTest(t)
	owner := makeAccount(t, "owner@example.com")
	libraryID := makeLibrary(t, owner)
	friend := makeAccount(t, "friend@example.com")

	// An anonymous grant of rw and a user grant of r: the user grant wins,
	// because a grant naming you is a decision about you.
	if err := Add(grantCtx(t), Grant{Principal: Anon, LibraryID: libraryID, Perm: "rw"}); err != nil {
		t.Fatal(err)
	}
	if err := Add(grantCtx(t), Grant{Principal: UserPrincipal(friend.ID), LibraryID: libraryID, Perm: "r"}); err != nil {
		t.Fatal(err)
	}
	principals := []Principal{UserPrincipal(friend.ID), Anon}
	if got, err := permFor(grantCtx(t), libraryID, "/", principals); err != nil || got != "r" {
		t.Errorf("permFor = %q, %v; want r — the user grant is the more specific decision", got, err)
	}

	// Two principals of one kind disagreeing: the stronger wins.
	other := makeAccount(t, "other@example.com")
	if err := Add(grantCtx(t), Grant{Principal: UserPrincipal(other.ID), LibraryID: libraryID, Perm: "rw"}); err != nil {
		t.Fatal(err)
	}
	usersOnly := []Principal{UserPrincipal(friend.ID), UserPrincipal(other.ID)}
	if got, err := permFor(grantCtx(t), libraryID, "/", usersOnly); err != nil || got != "rw" {
		t.Errorf("permFor over two user grants = %q, %v; want rw", got, err)
	}

	// A link is the most specific of all: it authorises one request.
	if err := Add(grantCtx(t), Grant{Principal: LinkPrincipal("cred-1"), LibraryID: libraryID, Perm: "r"}); err != nil {
		t.Fatal(err)
	}
	withLink := append([]Principal{LinkPrincipal("cred-1")}, principals...)
	if got, err := permFor(grantCtx(t), libraryID, "/", withLink); err != nil || got != "r" {
		t.Errorf("permFor with a link = %q, %v; want r", got, err)
	}
}

// The shared-with-me question: every library any of the caller's principals
// holds a whole-library grant on, each with its permission.
func TestLibrariesForListsEveryGrantedLibrary(t *testing.T) {
	setupShareTest(t)
	owner := makeAccount(t, "owner@example.com")
	friend := makeAccount(t, "friend@example.com")
	readable := makeLibrary(t, owner)
	writable := makeLibrary(t, owner)

	if err := Add(grantCtx(t), Grant{Principal: UserPrincipal(friend.ID), LibraryID: readable, Perm: "r"}); err != nil {
		t.Fatal(err)
	}
	if err := Add(grantCtx(t), Grant{Principal: UserPrincipal(friend.ID), LibraryID: writable, Perm: "rw"}); err != nil {
		t.Fatal(err)
	}
	got, err := LibrariesFor(grantCtx(t), PrincipalsFor(friend.ID))
	if err != nil {
		t.Fatal(err)
	}
	if got[readable] != "r" || got[writable] != "rw" {
		t.Errorf("LibrariesFor = %v, want both libraries with their permissions", got)
	}
	if len(got) != 2 {
		t.Errorf("LibrariesFor returned %d libraries, want 2", len(got))
	}
}

// A grant is scoped to a library, and deleting the library takes its grants
// with it -- otherwise a row outlives its subject and would reappear if the id
// were ever reused.
func TestRemovingALibraryTakesItsGrants(t *testing.T) {
	setupShareTest(t)
	owner := makeAccount(t, "owner@example.com")
	friend := makeAccount(t, "friend@example.com")
	libraryID := makeLibrary(t, owner)

	if err := Add(grantCtx(t), Grant{Principal: UserPrincipal(friend.ID), LibraryID: libraryID, Perm: "rw"}); err != nil {
		t.Fatal(err)
	}
	if err := RemoveLibrary(grantCtx(t), libraryID); err != nil {
		t.Fatal(err)
	}
	grants, err := ForLibrary(grantCtx(t), libraryID)
	if err != nil {
		t.Fatal(err)
	}
	if len(grants) != 0 {
		t.Errorf("the library still holds %d grants", len(grants))
	}
}

func TestPrincipalKinds(t *testing.T) {
	id, err := account.NewID()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		p    Principal
		kind string
	}{
		{UserPrincipal(id), "user"},
		{LinkPrincipal("abc"), "link"},
		{Anon, "anon"},
	} {
		if got := c.p.Kind(); got != c.kind {
			t.Errorf("%q.Kind() = %q, want %q", c.p, got, c.kind)
		}
	}
}
