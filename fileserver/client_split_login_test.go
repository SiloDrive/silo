package silod

import (
	"bytes"
	"net/http"
	"testing"

	"github.com/dkam/silo/client"
	"github.com/dkam/silo/store"
)

// Split-derivation login, from the client side.
//
// The server has accepted a derived key since 895c159 and nothing has ever
// sent one, so every login until now has handed over the secret that opens the
// identity blob. That is the difference between encryption that is real
// against a stolen disk and encryption that is real against the server, and it
// is the gate on offering any of this to a person.
//
// What a crossed-over account looks like from outside is the assertion worth
// writing: the password still opens everything for its owner, and no longer
// logs in.

// loginWith posts a raw credential to the login endpoint, which is what a
// client that had not crossed over would send.
func loginWith(t *testing.T, base, email, secret string) int {
	t.Helper()
	code, _ := call(t, "POST", base+"/api/silo/v1/auth/login", "",
		`{"email":"`+email+`","password":"`+secret+`"}`)
	return code
}

func TestEnrollingCrossesTheAccountOverAndStopsSendingThePassword(t *testing.T) {
	base, _ := wire(t)
	const email = "wire@example.com"

	// Before: the password is the login secret, which is also the secret that
	// would open an identity blob if there were one.
	if code := loginWith(t, base, email, wirePassword); code != http.StatusOK {
		t.Fatalf("logging in with the password before enrolment: status %d", code)
	}

	c := client.NewClient(base)
	acct, err := c.Enrol(email, wirePassword)
	if err != nil {
		t.Fatalf("Enrol: %v", err)
	}
	if acct.ID == "" || acct.Identity == nil {
		t.Fatal("Enrol returned no identity")
	}

	// After: the password no longer logs in. Nothing else in this test matters
	// as much -- this is the statement that the server has stopped seeing the
	// secret that opens the blob.
	if code := loginWith(t, base, email, wirePassword); code != http.StatusUnauthorized {
		t.Errorf("the password still logs in after crossover: status %d", code)
	}

	// And the account still opens from that same password on a device that
	// holds nothing else, which is the whole point of deriving rather than
	// replacing.
	second := client.NewClient(base)
	reopened, err := second.OpenAccount(email, wirePassword)
	if err != nil {
		t.Fatalf("OpenAccount after crossover: %v", err)
	}
	if reopened.ID != acct.ID {
		t.Errorf("account id = %s, want %s", reopened.ID, acct.ID)
	}
	a, b := acct.Identity.Public(), reopened.Identity.Public()
	if !bytes.Equal(a[:], b[:]) {
		t.Error("the identity that came back is not the one enrolment published")
	}

	// The wrong password is still wrong, and fails at the login rather than
	// somewhere confusing later.
	if _, err := second.OpenAccount(email, "not the password"); err == nil {
		t.Error("OpenAccount succeeded with the wrong password")
	}
}

// An enrolled account can create and read an encrypted library without the
// password ever reaching the server -- which is the combination the gate on
// silo#13 is actually about.
func TestACrossedOverAccountStillDrivesAnEncryptedLibrary(t *testing.T) {
	base, _ := wire(t)
	c := client.NewClient(base)
	acct, err := c.Enrol("wire@example.com", wirePassword)
	if err != nil {
		t.Fatalf("Enrol: %v", err)
	}

	lib, kr, err := acct.CreateEncryptedLibrary("Sealed after crossover")
	if err != nil {
		t.Fatalf("CreateEncryptedLibrary: %v", err)
	}
	fs := client.NewEncryptedLibrary(c, lib.ID, kr)
	if err := fs.WriteFile("notes.txt", []byte("still works"), 1000); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	second := client.NewClient(base)
	sacct, err := second.OpenAccount("wire@example.com", wirePassword)
	if err != nil {
		t.Fatalf("OpenAccount: %v", err)
	}
	reader, err := sacct.Open(lib.ID)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if got, err := reader.ReadFile("notes.txt"); err != nil {
		t.Fatalf("ReadFile: %v", err)
	} else if string(got) != "still works" {
		t.Errorf("read %q", got)
	}
}

// An account that has not crossed over still logs in with its password: the
// server tells nobody which kind an address is, so the client tries the derived
// key and falls back. A client that only sent the derived key would lock every
// existing account out on the day it shipped.
func TestAnAccountThatHasNotCrossedOverStillLogsIn(t *testing.T) {
	base, _ := wire(t)
	c := client.NewClient(base)
	if err := c.Login("wire@example.com", wirePassword); err != nil {
		t.Fatalf("Login on an account that has not crossed over: %v", err)
	}
	if _, err := c.ListLibraries(); err != nil {
		t.Fatalf("the session does not work: %v", err)
	}
	// And OpenAccount reports the absent key material rather than a login
	// failure, because the login is not what went wrong.
	if _, err := c.OpenAccount("wire@example.com", wirePassword); err == nil {
		t.Error("OpenAccount succeeded for an account with no published keys")
	}
	_ = store.CKSize
}
