package silod

import (
	"net/http"
	"testing"

	"github.com/dkam/silo/fileserver/credential"
)

// Self-service password change. docs/auth.md fixes two things about it that
// are not obvious, and both are measured here: it requires the *current*
// password even though the request is already authenticated, and it revokes
// session credentials while leaving device credentials mounted.

const (
	wirePassword = "correct horse battery staple"
	newPassword  = "a fresh one, entirely unlike the last"
)

func changePassword(t *testing.T, base, token, body string) (int, string) {
	t.Helper()
	return call(t, "POST", base+"/api/silo/v1/auth/password", token, body)
}

// The rule docs/auth.md argues for: unmounting somebody's laptop as a side
// effect of routine hygiene teaches them to stop doing hygiene, so devices
// stay and sessions go.
func TestChangingThePasswordRevokesSessionsAndKeepsDevices(t *testing.T) {
	base, token := wire(t)
	device := issueCredential(t, credential.IssueOpts{Kind: credential.KindDevice, Perm: "rw"})
	session := issueCredential(t, credential.IssueOpts{Kind: credential.KindSession, Perm: "rw"})

	code, body := changePassword(t, base, token,
		`{"current_password":"`+wirePassword+`","new_password":"`+newPassword+`"}`)
	if code != http.StatusOK {
		t.Fatalf("changing the password: status %d, body %s", code, body)
	}
	if n := revokedCount(t, body); n != 2 {
		t.Errorf("revoked = %d, want 2 -- the two session credentials", n)
	}

	if !alive(t, base, device) {
		t.Error("a device credential was signed out by a password change")
	}
	if alive(t, base, session) {
		t.Error("a session credential survived a password change")
	}
	// Including the one that asked, which is a session and gets no exemption.
	if alive(t, base, token) {
		t.Error("the session that changed the password survived it")
	}
}

func TestTheNewPasswordWorksAndTheOldOneDoesNot(t *testing.T) {
	base, token := wire(t)

	if code, body := changePassword(t, base, token,
		`{"current_password":"`+wirePassword+`","new_password":"`+newPassword+`"}`); code != http.StatusOK {
		t.Fatalf("changing the password: status %d, body %s", code, body)
	}

	if code, _ := enrol(t, base, `{"email":"wire@example.com","password":"`+newPassword+`"}`); code != http.StatusOK {
		t.Errorf("logging in with the new password: status %d, want 200", code)
	}
	if code, _ := enrol(t, base, `{"email":"wire@example.com","password":"`+wirePassword+`"}`); code != http.StatusUnauthorized {
		t.Errorf("logging in with the old password: status %d, want 401", code)
	}
}

// A stolen credential must not upgrade itself into account takeover. That is
// the whole reason the current password is required on a request that has
// already authenticated.
func TestChangingThePasswordRequiresTheCurrentOne(t *testing.T) {
	base, token := wire(t)

	code, body := changePassword(t, base, token,
		`{"current_password":"not it","new_password":"`+newPassword+`"}`)
	if code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401: %s", code, body)
	}

	// Nothing moved: the old password still logs in, and the credential that
	// made the failed attempt is still good.
	if code, _ := enrol(t, base, `{"email":"wire@example.com","password":"`+wirePassword+`"}`); code != http.StatusOK {
		t.Errorf("the old password stopped working after a refused change: status %d", code)
	}
	if !alive(t, base, token) {
		t.Error("a refused password change revoked the credential that asked")
	}
}

func TestAnIncompletePasswordChangeIsRefused(t *testing.T) {
	base, token := wire(t)

	for _, tc := range []struct {
		name string
		body string
	}{
		{"no current password", `{"new_password":"` + newPassword + `"}`},
		{"no new password", `{"current_password":"` + wirePassword + `"}`},
		{"an empty new password", `{"current_password":"` + wirePassword + `","new_password":""}`},
		{"not JSON", `{`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if code, body := changePassword(t, base, token, tc.body); code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400: %s", code, body)
			}
		})
	}

	// The shape is checked before the password is, so none of the above told
	// the caller anything about whether their password was right -- and the
	// account is untouched.
	if code, _ := enrol(t, base, `{"email":"wire@example.com","password":"`+wirePassword+`"}`); code != http.StatusOK {
		t.Errorf("a refused password change altered the account: status %d", code)
	}
}

// The ceiling applies. A credential handed out as read-only is the one an
// operator gave to something they did not fully trust, and setting the
// account's password is the most consequential write there is.
func TestAReadOnlyCredentialCannotChangeThePassword(t *testing.T) {
	base, _ := wire(t)
	readonly := narrowed(t, "r", credential.Scope{})

	code, body := changePassword(t, base, readonly,
		`{"current_password":"`+wirePassword+`","new_password":"`+newPassword+`"}`)
	if code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: %s", code, body)
	}
	if code, _ := enrol(t, base, `{"email":"wire@example.com","password":"`+wirePassword+`"}`); code != http.StatusOK {
		t.Errorf("a refused password change altered the account: status %d", code)
	}
}

// And so does the narrowing: the password is the account's, which is strictly
// wider than a credential cut to one library.
func TestAScopedCredentialCannotChangeThePassword(t *testing.T) {
	base, token := wire(t)
	id := makeLibrary(t, base, token)
	scoped := narrowed(t, "rw", credential.Scope{LibraryID: id})

	code, body := changePassword(t, base, scoped,
		`{"current_password":"`+wirePassword+`","new_password":"`+newPassword+`"}`)
	if code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: %s", code, body)
	}
}
