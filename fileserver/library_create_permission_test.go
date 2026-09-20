package silod

import (
	"net/http"
	"strings"
	"testing"

	"github.com/SiloDrive/silo/fileserver/account"
	"github.com/SiloDrive/silo/fileserver/option"
)

// Creating a library is the one write with no library to ask about, so it is
// the one write share.CheckPerm cannot gate: the account is making the thing
// permissions would be read from. account.Role.MayCreateLibrary is the rule
// that answers instead, and these tests are what put it in front of the route
// -- it existed, and was tested, and nothing called it, so a guest with any
// write credential could create libraries the role is defined to forbid.
//
// The route is the whole surface. silo-drive's mkdir in the mount root is a
// POST to it and nothing more, so a gate here covers the TUI, the CLI and the
// filesystem at once, and there is no second place for the rule to disagree
// with itself.

// installAllows sets the config key for one test and puts it back afterwards.
func installAllows(t *testing.T, allow bool) {
	t.Helper()
	orig := option.AllowUserCreateLibrary
	t.Cleanup(func() { option.AllowUserCreateLibrary = orig })
	option.AllowUserCreateLibrary = allow
}

func createLibrary(t *testing.T, base, token, body string) (int, string) {
	t.Helper()
	return call(t, "POST", base+"/api/silo/v1/libraries", token, body)
}

func TestAUserCreatesALibraryWhereTheInstallAllowsIt(t *testing.T) {
	base, token := wire(t)
	installAllows(t, true)

	if code, body := createLibrary(t, base, token, `{"name":"Documents"}`); code != http.StatusCreated {
		t.Fatalf("creating a library as a user on a permissive install: status %d, body %s", code, body)
	}
}

// The curated install: an admin owns the libraries and everybody else syncs
// what they are given.
func TestAUserIsRefusedALibraryWhereTheInstallDoesNot(t *testing.T) {
	base, token := wire(t)
	installAllows(t, false)

	code, body := createLibrary(t, base, token, `{"name":"Documents"}`)
	if code != http.StatusForbidden {
		t.Fatalf("creating a library as a user on a curated install: status %d, want 403, body %s", code, body)
	}

	// Refused all the way down, not merely reported as refused. A 403 over a
	// library that was written anyway is the failure this half catches.
	if _, listing := call(t, "GET", base+"/api/silo/v1/libraries", token, ""); strings.Contains(listing, "Documents") {
		t.Errorf("the refused library is in the listing: %s", listing)
	}
}

// A guest creates nothing, whatever the install says about users. The setting
// curates what a user may do; it does not promote a guest.
func TestAGuestIsRefusedALibraryEvenWhereUsersMayCreate(t *testing.T) {
	base, _ := wire(t)
	installAllows(t, true)
	_, guest := makeAccount(t, base, "guest@example.com", "a guest's password", account.RoleGuest)

	if code, body := createLibrary(t, base, guest, `{"name":"Documents"}`); code != http.StatusForbidden {
		t.Fatalf("creating a library as a guest: status %d, want 403, body %s", code, body)
	}
}

// No install setting takes anything away from an admin: one that could would
// lock the administrator out of curating the thing the setting exists for.
func TestAnAdminCreatesALibraryWhereUsersMayNot(t *testing.T) {
	base, _ := wire(t)
	installAllows(t, false)
	_, adminToken := makeAccount(t, base, "root@example.com", "an administrator's password", account.RoleAdmin)

	if code, body := createLibrary(t, base, adminToken, `{"name":"Documents"}`); code != http.StatusCreated {
		t.Fatalf("creating a library as an admin on a curated install: status %d, body %s", code, body)
	}
}

// The gate is asked before the request is read, so both library formats are
// behind it. Measured by the status: an ungated guest reaches the encrypted
// branch and is turned away for having published no identity key, which is 409
// -- a refusal that says the wrong thing and would let the plain branch through
// the moment a key existed.
func TestTheGateIsAskedBeforeTheEncryptedBranch(t *testing.T) {
	base, _ := wire(t)
	installAllows(t, true)
	_, guest := makeAccount(t, base, "guest@example.com", "a guest's password", account.RoleGuest)

	if code, body := createLibrary(t, base, guest, `{"name":"Documents","e2ee":true}`); code != http.StatusForbidden {
		t.Fatalf("creating an encrypted library as a guest: status %d, want 403, body %s", code, body)
	}
}
