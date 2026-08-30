package silod

import (
	"bytes"
	"net/http"
	"testing"

	"github.com/dkam/silo/client"
)

// Changing the password of an enrolled account.
//
// A new password produces a new wrapKey, and the identity key is wrapped under
// the old one. So a password change that only tells the server about the new
// password leaves the account holding a blob nothing it knows can open -- the
// libraries are still there, still encrypted, and the key that unwraps their
// content keys is gone. auth.md says PUT account/keys replaces the whole set
// for exactly this reason; this is the client that does it.
const nextPassword = "a second correct horse battery staple"

func TestChangingThePasswordKeepsTheIdentityKey(t *testing.T) {
	base, _ := wire(t)
	const email = "wire@example.com"

	c := client.NewClient(base)
	acct, _, err := c.Enrol(email, wirePassword)
	if err != nil {
		t.Fatalf("Enrol: %v", err)
	}
	before := acct.Identity.Public()

	// The library is the reason any of this matters: its content key is
	// wrapped to the identity, so an orphaned identity is an unreadable
	// library rather than an inconvenience.
	lib, kr, err := acct.CreateEncryptedLibrary("Survives a password change")
	if err != nil {
		t.Fatal(err)
	}
	fs := client.NewEncryptedLibrary(c, lib.ID, kr)
	if err := fs.WriteFile("notes.txt", []byte("written before the change"), 1000); err != nil {
		t.Fatal(err)
	}

	if _, err := c.ChangePassword(wirePassword, nextPassword); err != nil {
		t.Fatalf("ChangePassword: %v", err)
	}

	// A device that holds only the new password opens the same identity and
	// reads the library written under the old one.
	fresh := client.NewClient(base)
	reopened, err := fresh.OpenAccount(email, nextPassword)
	if err != nil {
		t.Fatalf("OpenAccount with the new password: %v", err)
	}
	after := reopened.Identity.Public()
	if !bytes.Equal(before[:], after[:]) {
		t.Error("the password change replaced the identity key")
	}
	reader, err := reopened.Open(lib.ID)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if got, err := reader.ReadFile("notes.txt"); err != nil {
		t.Fatalf("reading a file written before the password change: %v", err)
	} else if string(got) != "written before the change" {
		t.Errorf("read %q", got)
	}

	// The old password is gone in both senses: it does not log in, and it does
	// not open the account.
	if code := loginWith(t, base, email, wirePassword); code != http.StatusUnauthorized {
		t.Errorf("the old password still logs in: status %d", code)
	}
	if _, err := client.NewClient(base).OpenAccount(email, wirePassword); err == nil {
		t.Error("the old password still opens the account")
	}

	// And the account is still crossed over: the new password is not a login
	// secret either.
	if code := loginWith(t, base, email, nextPassword); code != http.StatusUnauthorized {
		t.Errorf("the new password logs in directly, so the change undid the crossover: status %d", code)
	}
}

// An account with no identity key is an ordinary password change, and must not
// acquire one as a side effect.
func TestChangingThePasswordOfAnUnenrolledAccount(t *testing.T) {
	base, _ := wire(t)
	c := client.NewClient(base)
	if err := c.Login("wire@example.com", wirePassword); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ChangePassword(wirePassword, nextPassword); err != nil {
		t.Fatalf("ChangePassword: %v", err)
	}
	if code := loginWith(t, base, "wire@example.com", nextPassword); code != http.StatusOK {
		t.Errorf("the new password does not log in: status %d", code)
	}
	// A fresh credential: the change revoked the session that made it.
	if _, err := c.AccountKeys(); err == nil {
		t.Error("a password change published an identity key")
	}
}
