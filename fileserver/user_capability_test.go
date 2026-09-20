package silod

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/SiloDrive/silo/fileserver/account"
	"github.com/SiloDrive/silo/fileserver/admin"
	"github.com/SiloDrive/silo/fileserver/option"
)

// The CLI is the only way authority is handed out until the admin routes land,
// and it is the install's own hand: it already holds the database, so there is
// no actor to check. What it does still honour is the invariant that is about
// the install rather than the caller.

func capUser(t *testing.T, email string, role account.Role) account.ID {
	t.Helper()
	ctx, cancel := option.WithDBTimeout(context.Background())
	defer cancel()
	id, _, err := account.Create(ctx, email, "", role)
	if err != nil {
		t.Fatalf("creating %s: %v", email, err)
	}
	return id
}

func heldBy(t *testing.T, id account.ID) []admin.Capability {
	t.Helper()
	ctx, cancel := option.WithDBTimeout(context.Background())
	defer cancel()
	caps, err := admin.Of(ctx, id)
	if err != nil {
		t.Fatalf("reading capabilities: %v", err)
	}
	return caps
}

func TestGrantAndRevokeFromTheCLI(t *testing.T) {
	userTestStore(t)
	id := capUser(t, "curator@example.com", account.RoleAdmin)

	if err := grantUser("curator@example.com", "users,quota"); err != nil {
		t.Fatalf("grant: %v", err)
	}
	if got := admin.Join(heldBy(t, id)); got != "users, quota" {
		t.Errorf("after the grant the account holds %q", got)
	}

	// Listing shows it, because an operator asking who administers this server
	// should not have to query the table to find out.
	out := captureStdout(t, func() {
		if err := listUsers(false); err != nil {
			t.Fatalf("list: %v", err)
		}
	})
	if !strings.Contains(out, "users, quota") {
		t.Errorf("the listing does not show the capabilities:\n%s", out)
	}

	if err := revokeUser("curator@example.com", "quota"); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if got := admin.Join(heldBy(t, id)); got != "users" {
		t.Errorf("after the revoke the account holds %q", got)
	}
}

// The invariant is about the install, so it holds on the CLI path too. It
// costs the operator nothing: handing grant to somebody else first is the
// thing they meant to do.
func TestTheCLIRefusesToDropTheLastGrant(t *testing.T) {
	userTestStore(t)
	capUser(t, "only@example.com", account.RoleAdmin)
	if err := grantUser("only@example.com", "grant"); err != nil {
		t.Fatalf("grant: %v", err)
	}

	err := revokeUser("only@example.com", "grant")
	if !errors.Is(err, admin.ErrLastGrant) {
		t.Fatalf("revoking the last grant = %v, want ErrLastGrant", err)
	}

	capUser(t, "second@example.com", account.RoleAdmin)
	if err := grantUser("second@example.com", "grant"); err != nil {
		t.Fatalf("granting a second holder: %v", err)
	}
	if err := revokeUser("only@example.com", "grant"); err != nil {
		t.Errorf("revoking with a second holder standing: %v", err)
	}
}

func TestGrantRefusesAnUnknownCapability(t *testing.T) {
	userTestStore(t)
	id := capUser(t, "curator@example.com", account.RoleAdmin)

	if err := grantUser("curator@example.com", "users,read_any_library"); err == nil {
		t.Fatal("grant accepted a capability outside the vocabulary")
	}
	// And it refused before writing anything, rather than granting the half it
	// recognised.
	if caps := heldBy(t, id); len(caps) != 0 {
		t.Errorf("a refused grant left %v behind", caps)
	}
}

func TestGrantRefusesAnUnknownAddress(t *testing.T) {
	userTestStore(t)
	if err := grantUser("nobody@example.com", "users"); err == nil {
		t.Fatal("grant accepted an address with no account")
	}
}
