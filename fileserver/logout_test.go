package silod

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/dkam/silo/fileserver/credential"
)

// Logout. docs/auth.md's list of what is left calls this "the smallest gap":
// a client could mint a credential over HTTP and had no way to discard one,
// so a laptop that wanted to sign out had to ask an operator with shell
// access to run `silo token revoke`.

// revokedCount reads the {"revoked": n} body both logout routes answer with.
func revokedCount(t *testing.T, body string) int {
	t.Helper()
	var out struct {
		Revoked int `json:"revoked"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("decoding %q: %v", body, err)
	}
	return out.Revoked
}

// alive reports whether a credential still resolves, by asking a route that
// every credential in these tests may reach.
func alive(t *testing.T, base, token string) bool {
	t.Helper()
	code, _ := call(t, "GET", base+"/api/silo/v1/libraries", token, "")
	return code == http.StatusOK
}

func TestLogoutDiscardsThePresentingCredential(t *testing.T) {
	base, token := wire(t)
	if !alive(t, base, token) {
		t.Fatal("the credential did not work before logging out")
	}

	code, body := call(t, "POST", base+"/api/silo/v1/auth/logout", token, "")
	if code != http.StatusOK {
		t.Fatalf("logout: status %d, body %s", code, body)
	}
	if n := revokedCount(t, body); n != 1 {
		t.Errorf("revoked = %d, want 1", n)
	}

	if alive(t, base, token) {
		t.Error("the credential still works after logging out")
	}
}

// A credential cut to one library is refused every account-wide route, which
// is the right answer for a route that reads the account and the wrong one
// here: this route's subject is the credential presenting it, which is not
// wider than the narrowing but exactly it. A mount that cannot sign itself
// out has to be revoked by somebody else.
func TestALibraryScopedCredentialCanLogItselfOut(t *testing.T) {
	base, token := wire(t)
	id := makeLibrary(t, base, token)
	scoped := narrowed(t, "r", credential.Scope{LibraryID: id})

	code, body := call(t, "POST", base+"/api/silo/v1/auth/logout", scoped, "")
	if code != http.StatusOK {
		t.Fatalf("logout with a scoped credential: status %d, body %s", code, body)
	}

	// And it is really gone, rather than answered politely.
	if code, _ := call(t, "GET", base+"/api/silo/v1/libraries/"+id+"/entries/", scoped, ""); code != http.StatusUnauthorized {
		t.Errorf("the scoped credential still resolves: status %d", code)
	}
}

func TestLogoutRequiresACredential(t *testing.T) {
	base, _ := wire(t)

	resp, err := http.Post(base+"/api/silo/v1/auth/logout", "application/json", nil)
	if err != nil {
		t.Fatalf("logout: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

// Signing out everywhere is the account's own panic button: one request, and
// every device and session it holds stops working on its next.
func TestLoggingOutEverywhereRevokesEveryCredential(t *testing.T) {
	base, token := wire(t)
	other := narrowed(t, "rw", credential.Scope{})

	code, body := call(t, "POST", base+"/api/silo/v1/auth/logout/everywhere", token, "")
	if code != http.StatusOK {
		t.Fatalf("logout everywhere: status %d, body %s", code, body)
	}
	if n := revokedCount(t, body); n != 2 {
		t.Errorf("revoked = %d, want 2", n)
	}

	if alive(t, base, token) {
		t.Error("the presenting credential survived a sign-out-everywhere")
	}
	if alive(t, base, other) {
		t.Error("another credential on the account survived a sign-out-everywhere")
	}
}

// The account-wide form is account-wide, so the existing narrowing rule
// applies to it unchanged: a credential cut to one library may not reach a
// route that answers about every credential the account holds.
func TestAScopedCredentialCannotLogTheAccountOutEverywhere(t *testing.T) {
	base, token := wire(t)
	id := makeLibrary(t, base, token)
	scoped := narrowed(t, "rw", credential.Scope{LibraryID: id})

	code, body := call(t, "POST", base+"/api/silo/v1/auth/logout/everywhere", scoped, "")
	if code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: %s", code, body)
	}
	if !alive(t, base, token) {
		t.Error("a refused sign-out-everywhere revoked something anyway")
	}
}

// Revocation only ever takes access away, so a read-only credential may do
// it. Refusing would mean a client that has been narrowed to the point of
// harmlessness is also the one that cannot slam the door.
func TestAReadOnlyCredentialMayLogOut(t *testing.T) {
	base, _ := wire(t)
	readonly := narrowed(t, "r", credential.Scope{})

	if code, body := call(t, "POST", base+"/api/silo/v1/auth/logout", readonly, ""); code != http.StatusOK {
		t.Errorf("logout with a read-only credential: status %d, body %s", code, body)
	}
}
