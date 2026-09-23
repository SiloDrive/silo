package silod

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/SiloDrive/silo/fileserver/account"
	"github.com/SiloDrive/silo/fileserver/authmgr"
)

const testIssuer = "https://id.example.com"

func TestAnIdentityCanBeLinkedByHandAndListed(t *testing.T) {
	userTestStore(t)
	mintAccount(t, "linked@example.com")

	if err := linkIdentity("linked@example.com", testIssuer, "sub-1"); err != nil {
		t.Fatalf("link: %v", err)
	}
	// Linking again is not an error: the identity is where it was asked to be.
	if err := linkIdentity("Linked@Example.com", testIssuer, "sub-1"); err != nil {
		t.Fatalf("linking twice: %v", err)
	}
	out := captureStdout(t, func() {
		if err := listIdentities("linked@example.com"); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, testIssuer) || !strings.Contains(out, "sub-1") {
		t.Errorf("identity list = %q", out)
	}

	listing := captureStdout(t, func() {
		if err := listUsers(false); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(listing, "id.example.com") {
		t.Errorf("silo user list does not show the IdP:\n%s", listing)
	}
	asJSON := captureStdout(t, func() {
		if err := listUsers(true); err != nil {
			t.Fatal(err)
		}
	})
	var users []struct {
		Email      string `json:"email"`
		Identities []struct {
			Issuer, Subject string
		} `json:"identities"`
	}
	if err := json.Unmarshal([]byte(asJSON), &users); err != nil {
		t.Fatal(err)
	}
	for _, u := range users {
		if u.Email == "linked@example.com" && (len(u.Identities) != 1 || u.Identities[0].Subject != "sub-1") {
			t.Errorf("JSON identities = %+v", u.Identities)
		}
	}
}

// One identity is one person. Linking it to a second account would make the
// next sign-in land wherever the lookup happened to go.
func TestAnIdentityLinkedElsewhereIsNotMoved(t *testing.T) {
	userTestStore(t)
	mintAccount(t, "first@example.com")
	mintAccount(t, "second@example.com")
	if err := linkIdentity("first@example.com", testIssuer, "sub-1"); err != nil {
		t.Fatal(err)
	}
	if err := linkIdentity("second@example.com", testIssuer, "sub-1"); !errors.Is(err, account.ErrIdentityTaken) {
		t.Fatalf("linking an identity another account holds = %v, want ErrIdentityTaken", err)
	}
}

// An account with no password whose last identity is removed would read as a
// tombstone -- and a tombstone is what an invite or an IdP login at its
// address may claim, libraries and all.
func TestTheLastWayIntoAnAccountCannotBeUnlinked(t *testing.T) {
	userTestStore(t)
	mintAccount(t, "sso@example.com") // no password
	if err := linkIdentity("sso@example.com", testIssuer, "sub-1"); err != nil {
		t.Fatal(err)
	}

	err := unlinkIdentity("sso@example.com", testIssuer, "sub-1")
	if !errors.Is(err, account.ErrLastWayIn) {
		t.Fatalf("unlinking the only identity of a password-less account = %v, want ErrLastWayIn", err)
	}
	if arrived, _ := account.Arrived(adminCtx(t), "sso@example.com"); !arrived {
		t.Error("the refused unlink still left the account reading as a tombstone")
	}
}

func TestAnIdentityCanBeUnlinkedWhenAnotherWayInRemains(t *testing.T) {
	userTestStore(t)
	if _, err := authmgr.CreateAccount(adminCtx(t), "both@example.com", "a password", account.RoleUser); err != nil {
		t.Fatal(err)
	}
	if err := linkIdentity("both@example.com", testIssuer, "sub-1"); err != nil {
		t.Fatal(err)
	}
	if err := unlinkIdentity("both@example.com", testIssuer, "sub-1"); err != nil {
		t.Fatalf("unlink with a password left: %v", err)
	}
	if err := unlinkIdentity("both@example.com", testIssuer, "sub-1"); err == nil {
		t.Error("unlinking an identity that is not there reported success")
	}
}

func TestIdentitySubcommandsCheckTheirArguments(t *testing.T) {
	for _, args := range [][]string{
		{"identity"},
		{"identity", "list"},
		{"identity", "link", "a@example.com", testIssuer},
		{"identity", "unlink", "a@example.com"},
		{"identity", "frob", "a@example.com", testIssuer, "sub"},
	} {
		if err := RunUser(args); err == nil {
			t.Errorf("RunUser(%q) accepted a bad invocation", args)
		}
	}
}
