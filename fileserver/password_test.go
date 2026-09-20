package silod

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/SiloDrive/silo/fileserver/credential"
	"github.com/SiloDrive/silo/store"
)

// Self-service password change. docs/auth.md fixes two things about it that
// are not obvious, and both are measured here: it requires the *current*
// password even though the request is already authenticated, and it signs
// every credential the account holds out.

const (
	wirePassword = "correct horse battery staple"
	newPassword  = "a fresh one, entirely unlike the last"
)

func changePassword(t *testing.T, base, token, body string) (int, string) {
	t.Helper()
	return call(t, "POST", base+"/api/silo/v1/auth/password", token, body)
}

// A password change leaves the account's other hosts signed in, unless it is
// asked not to.
//
// This is the third answer to a question that has now had all three. The
// original rule revoked sessions and spared devices, which is the worst of
// them: it spared precisely the long-lived, self-renewing credential worth
// worrying about while still unmounting things. It was replaced by revoking
// everything, on the argument that changing a password is what a person
// reaches for when they think something has been taken, and that the blanket
// revoke was the only thing pointed at a stolen device credential -- true at
// the time, and the reason the hygiene cost was accepted.
//
// It is not true any more. auth/logout/others says the same thing by name, so
// the password change no longer has to carry it as a side effect, and
// everybody rotating a password has stopped paying for the rare case. What the
// default costs is recorded in docs/plans/credential-management.md § Step 3:
// somebody who changes their password *because* they are worried, and stops
// there, is no longer covered by something they did not know was happening.
// That is answered by saying so -- the count comes back zero and the CLI names
// the next step -- rather than by revoking hosts nobody asked about.
func TestChangingThePasswordLeavesOtherHostsSignedIn(t *testing.T) {
	base, token := wire(t)
	device := issueCredential(t, credential.IssueOpts{Kind: credential.KindDevice, Perm: "rw"})
	session := issueCredential(t, credential.IssueOpts{Kind: credential.KindSession, Perm: "rw"})

	code, body := changePassword(t, base, token,
		`{"current_password":"`+wirePassword+`","new_password":"`+newPassword+`"}`)
	if code != http.StatusOK {
		t.Fatalf("changing the password: status %d, body %s", code, body)
	}
	if n := revokedCount(t, body); n != 0 {
		t.Errorf("revoked = %d, want 0 -- a password change revokes nothing unless asked", n)
	}

	if !alive(t, base, device) {
		t.Error("a device credential was signed out by a password change nobody asked to revoke")
	}
	if !alive(t, base, session) {
		t.Error("a session credential was signed out by a password change nobody asked to revoke")
	}
	if !alive(t, base, token) {
		t.Error("the session that changed the password was signed out by it")
	}
}

// Asked to, it signs the others out and keeps the one that asked.
//
// The exemption is the point: the caller demonstrably holds the password, it
// just sent it, so signing it out proves nothing and is the part of the old
// behaviour that most read as a bug.
func TestAPasswordChangeRevokesOtherHostsWhenAsked(t *testing.T) {
	base, token := wire(t)
	device := issueCredential(t, credential.IssueOpts{Kind: credential.KindDevice, Perm: "rw"})
	session := issueCredential(t, credential.IssueOpts{Kind: credential.KindSession, Perm: "rw"})

	code, body := changePassword(t, base, token,
		`{"current_password":"`+wirePassword+`","new_password":"`+newPassword+`","revoke_others":true}`)
	if code != http.StatusOK {
		t.Fatalf("changing the password: status %d, body %s", code, body)
	}
	if n := revokedCount(t, body); n != 2 {
		t.Errorf("revoked = %d, want 2 -- the device and the other session, not the caller", n)
	}

	if alive(t, base, device) {
		t.Error("a device credential survived a revoking password change, so a stolen one still works")
	}
	if alive(t, base, session) {
		t.Error("a session credential survived a revoking password change")
	}
	if !alive(t, base, token) {
		t.Error("the session that asked to revoke the others revoked itself as well")
	}
}

func TestARevokingPasswordChangeEndsARenewedDeviceCredentialToo(t *testing.T) {
	base, token := wire(t)
	device := issueCredential(t, credential.IssueOpts{Kind: credential.KindDevice, Perm: "rw"})

	code, body := call(t, "POST", base+"/api/silo/v1/auth/renew", device, "")
	if code != http.StatusCreated {
		t.Fatalf("renewing: status %d, body %s", code, body)
	}
	var out struct {
		Credential string `json:"credential"`
	}
	decodeInto(t, body, &out)
	if out.Credential == "" {
		t.Fatalf("renewal returned no credential: %s", body)
	}
	if !alive(t, base, out.Credential) {
		t.Fatal("the renewed credential did not work before the password changed")
	}

	// revoke_others, because that is the mode this test is about: the question
	// is whether a renewal escapes a revocation, not whether a password change
	// revokes by default -- it does not.
	if code, body := changePassword(t, base, token,
		`{"current_password":"`+wirePassword+`","new_password":"`+newPassword+`","revoke_others":true}`); code != http.StatusOK {
		t.Fatalf("changing the password: status %d, body %s", code, body)
	}

	if alive(t, base, out.Credential) {
		t.Error("a credential renewed before the password change outlived it")
	}
	if alive(t, base, device) {
		t.Error("the credential it was renewed from outlived the password change")
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

// The crossover, end to end over the wire: after it, the authKey logs in and
// the password does not.
//
// This is the moment the password stops being a wire credential, which is what
// split-derivation login is for. `curl -u wire@example.com:<password>` against
// this account is 401 afterwards, and there is no header or flag that brings
// it back — the server no longer holds anything the password hashes to.
func TestCrossingOverMakesTheAuthKeyTheCredential(t *testing.T) {
	base, token := wire(t)
	const authKey = "b7e2f409a1c8d35062e9b4f7a0d2c85319f6e0b4d7a2c9f350e8b1d6a4f2c9730"
	params := testKDFParams()

	if code, body := changePassword(t, base, token,
		`{"current_password":"`+wirePassword+`","new_password":"`+authKey+`",`+
			`"client_kdf_params":"`+params.String()+`"}`); code != http.StatusOK {
		t.Fatalf("crossing over: status %d, body %s", code, body)
	}

	if code, _ := enrol(t, base, `{"email":"wire@example.com","password":"`+authKey+`"}`); code != http.StatusOK {
		t.Errorf("logging in with the authKey: status %d, want 200", code)
	}
	if code, _ := enrol(t, base, `{"email":"wire@example.com","password":"`+wirePassword+`"}`); code != http.StatusUnauthorized {
		t.Errorf("logging in with the old password: status %d, want 401", code)
	}

	// And the parameters landed with the hash, so a new device can ask what to
	// stretch under before it can log in at all. A crossover that wrote one
	// and not the other is an account nobody can reach.
	code, body := askKDF(t, base, "wire@example.com")
	if code != http.StatusOK {
		t.Fatalf("auth/kdf: status %d, body %s", code, body)
	}
	if got := kdfParams(t, body); got != params.String() {
		t.Errorf("kdf_params = %q, want %q", got, params.String())
	}
}

// A change that would silently undo a crossover is refused rather than taken.
//
// Without the parameters the server would store a hash of a raw password and
// clear the column, putting the account back on password login — and every
// device that had been sending an authKey would start failing, with the server
// looking wrong rather than the request that did it. This is not a state to
// arrive at by omission.
func TestAPasswordChangeWithoutParametersIsRefusedOnACrossedOverAccount(t *testing.T) {
	base, token := wire(t)
	const authKey = "0c5a8e2f7b1d4936a0e8c2f5b7d1a439e6c0b8d2f4a7159c3e0b6d2a8f4c17390"
	params := testKDFParams()

	if code, body := changePassword(t, base, token,
		`{"current_password":"`+wirePassword+`","new_password":"`+authKey+`",`+
			`"client_kdf_params":"`+params.String()+`"}`); code != http.StatusOK {
		t.Fatalf("crossing over: status %d, body %s", code, body)
	}

	// The crossover revoked that session along with the rest, so log back in
	// the way a client now has to: with the authKey.
	_, out := enrol(t, base, `{"email":"wire@example.com","password":"`+authKey+`"}`)
	token, _ = out["token"].(string)
	if token == "" {
		t.Fatalf("logging back in with the authKey returned no token: %v", out)
	}

	code, body := changePassword(t, base, token,
		`{"current_password":"`+authKey+`","new_password":"back to a password"}`)
	if code != http.StatusConflict {
		t.Fatalf("a paramless change on a crossed-over account: status %d, body %s, want 409", code, body)
	}

	// And nothing moved: the authKey still logs in.
	if code, _ := enrol(t, base, `{"email":"wire@example.com","password":"`+authKey+`"}`); code != http.StatusOK {
		t.Errorf("the refused change disturbed the account anyway: status %d, want 200", code)
	}
}

// Parameters the server cannot read are the caller's mistake, and are refused
// before anything is written -- a crossover half-described is the failure the
// one-statement write exists to prevent.
func TestACrossoverWithUnreadableParametersIsRefused(t *testing.T) {
	base, token := wire(t)

	code, body := changePassword(t, base, token,
		`{"current_password":"`+wirePassword+`","new_password":"an authKey",`+
			`"client_kdf_params":"not a parameter string"}`)
	if code != http.StatusBadRequest {
		t.Fatalf("unreadable client_kdf_params: status %d, body %s, want 400", code, body)
	}
	if code, _ := enrol(t, base, `{"email":"wire@example.com","password":"`+wirePassword+`"}`); code != http.StatusOK {
		t.Errorf("the refused crossover changed the password anyway: status %d, want 200", code)
	}
}

func TestServerInfoAdvertisesSplitLogin(t *testing.T) {
	base, token := wire(t)
	code, body := call(t, "GET", base+"/api/silo/v1/server-info", token, "")
	if code != http.StatusOK {
		t.Fatalf("server-info: status %d, body %s", code, body)
	}
	var out struct {
		Features []string `json:"features"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("decoding %s: %v", body, err)
	}
	for _, f := range out.Features {
		if f == "split-login" {
			return
		}
	}
	t.Errorf("server-info does not advertise split-login: %v", out.Features)
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

// testKDFParams is a parameter set a client would plausibly choose, with a
// fixed salt so a test can compare what came back against what went up.
func testKDFParams() store.KDFParams {
	var salt [store.KDFSaltSize]byte
	for i := range salt {
		salt[i] = byte(i + 7)
	}
	return store.DefaultKDFParams(salt)
}
