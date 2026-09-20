package silod

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/SiloDrive/silo/fileserver/credential"
)

// Self-service credential management over the wire: what the account holds,
// and a revoke aimed at one row of it rather than at all of them.

type wireCredential struct {
	ID       string `json:"id"`
	Kind     string `json:"kind"`
	Label    string `json:"label"`
	Scope    string `json:"scope"`
	Perm     string `json:"perm"`
	Created  int64  `json:"created"`
	LastUsed *int64 `json:"last_used"`
	Current  bool   `json:"current"`
}

func listCredentials(t *testing.T, base, token string) []wireCredential {
	t.Helper()
	code, body := call(t, "GET", base+"/api/silo/v1/account/credentials", token, "")
	if code != http.StatusOK {
		t.Fatalf("list credentials: status %d, body %s", code, body)
	}
	var out struct {
		Credentials []wireCredential `json:"credentials"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("decoding %q: %v", body, err)
	}
	return out.Credentials
}

// The listing shows every credential the account holds and marks exactly the
// one that asked.
//
// The current flag is the half worth testing hardest: without it a client
// cannot render "this device", and a person deciding what to revoke is one
// misread row away from signing themselves out.
func TestListingCredentialsMarksTheOneThatAsked(t *testing.T) {
	base, token := wire(t)
	other := narrowed(t, "rw", credential.Scope{})

	list := listCredentials(t, base, token)
	if len(list) != 2 {
		t.Fatalf("listed %d credentials, want 2: %+v", len(list), list)
	}
	for _, c := range list {
		if c.Label == "" {
			t.Errorf("credential %s has no label; the list is unreadable without one", c.ID)
		}
	}

	// Which row is marked, not how many are. Asserting the count alone passes
	// just as happily when the flag is inverted, because two credentials with
	// the wrong one marked is still exactly one marked.
	mine := markedCurrent(t, base, token)
	theirs := markedCurrent(t, base, other)
	if mine == "" || theirs == "" {
		t.Fatalf("a listing marked nothing current: %q and %q", mine, theirs)
	}
	if mine == theirs {
		t.Errorf("both credentials marked the same row %s current", mine)
	}

	// And the row each one marks is really its own: revoke by the id the
	// listing gave and the credential that asked is the one that stops working.
	code, body := call(t, "DELETE", base+"/api/silo/v1/account/credentials/"+theirs, token, "")
	if code != http.StatusOK {
		t.Fatalf("revoking the row the other credential marked: status %d, body %s", code, body)
	}
	if alive(t, base, other) {
		t.Error("the id marked current for the other credential was not the other credential")
	}
	if !alive(t, base, token) {
		t.Error("revoking the other credential's row took the caller's own")
	}
}

// markedCurrent returns the id of the row a listing marks current when asked
// with this token, or "" if it marks none.
func markedCurrent(t *testing.T, base, token string) string {
	t.Helper()
	var found string
	var n int
	for _, c := range listCredentials(t, base, token) {
		if c.Current {
			found = c.ID
			n++
		}
	}
	if n > 1 {
		t.Errorf("%d rows marked current, want at most 1", n)
	}
	return found
}

// Revoking one credential leaves the rest alone. This is the whole point of
// the endpoint: before it, the only way to discard the laptop's credential was
// to discard the phone's as well.
func TestRevokingOneCredentialLeavesTheOthers(t *testing.T) {
	base, token := wire(t)
	doomed := narrowed(t, "rw", credential.Scope{})

	var id string
	for _, c := range listCredentials(t, base, token) {
		if !c.Current {
			id = c.ID
		}
	}
	if id == "" {
		t.Fatal("no other credential in the listing to revoke")
	}

	code, body := call(t, "DELETE", base+"/api/silo/v1/account/credentials/"+id, token, "")
	if code != http.StatusOK {
		t.Fatalf("revoke: status %d, body %s", code, body)
	}
	if n := revokedCount(t, body); n != 1 {
		t.Errorf("revoked = %d, want 1", n)
	}

	if alive(t, base, doomed) {
		t.Error("the revoked credential still works")
	}
	if !alive(t, base, token) {
		t.Error("revoking one credential took the caller's own with it")
	}
}

// Revoking your own id is allowed, and says so, so a client can tell it has
// just signed itself out rather than discovering it on the next request.
func TestRevokingYourOwnCredentialSaysSo(t *testing.T) {
	base, token := wire(t)

	var id string
	for _, c := range listCredentials(t, base, token) {
		if c.Current {
			id = c.ID
		}
	}
	if id == "" {
		t.Fatal("nothing in the listing was marked current")
	}

	code, body := call(t, "DELETE", base+"/api/silo/v1/account/credentials/"+id, token, "")
	if code != http.StatusOK {
		t.Fatalf("revoking own credential: status %d, body %s", code, body)
	}
	var out struct {
		Revoked int  `json:"revoked"`
		Current bool `json:"current"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("decoding %q: %v", body, err)
	}
	if out.Revoked != 1 || !out.Current {
		t.Errorf("body = %s, want revoked 1 and current true", body)
	}
	if alive(t, base, token) {
		t.Error("the credential still works after revoking itself")
	}
}

// An id that is not this account's is 404 rather than 403, and the two reasons
// it can be missing are deliberately indistinguishable: answering 403 for
// somebody else's row would tell a caller which ids exist on other accounts.
func TestRevokingAnIdThatIsNotYoursIsNotFound(t *testing.T) {
	base, token := wire(t)

	for _, id := range []string{"no-such-credential", "00000000-0000-0000-0000-000000000000"} {
		code, body := call(t, "DELETE", base+"/api/silo/v1/account/credentials/"+id, token, "")
		if code != http.StatusNotFound {
			t.Errorf("revoking %q: status %d, body %s; want 404", id, code, body)
		}
	}
	if !alive(t, base, token) {
		t.Error("a failed revocation disturbed the caller's own credential")
	}
}

// A read-only credential may revoke, which is the rule the logout routes
// already hold: revocation only ever takes access away. Requiring write would
// make the targeted revoke stricter than logout/everywhere, which a read-only
// credential may already fire.
func TestAReadOnlyCredentialMayRevoke(t *testing.T) {
	base, _ := wire(t)
	readOnly := narrowed(t, "r", credential.Scope{})
	doomed := narrowed(t, "rw", credential.Scope{})

	// Ask with the doomed credential itself, so the row it marks current is
	// unambiguously the one about to be revoked.
	var id string
	for _, c := range listCredentials(t, base, doomed) {
		if c.Current {
			id = c.ID
		}
	}
	if id == "" {
		t.Fatal("the doomed credential did not appear in its own listing")
	}

	code, body := call(t, "DELETE", base+"/api/silo/v1/account/credentials/"+id, readOnly, "")
	if code != http.StatusOK {
		t.Fatalf("read-only revoke: status %d, body %s", code, body)
	}
	if alive(t, base, doomed) {
		t.Error("the revoked credential still works")
	}
	if !alive(t, base, readOnly) {
		t.Error("the read-only credential revoked itself instead of its target")
	}
}

// A credential narrowed to one library is refused both routes. Reading or
// revoking everything the account holds is strictly wider than the scope, the
// way logout/everywhere is and the way plain logout is not.
func TestALibraryScopedCredentialCannotManageTheAccountsCredentials(t *testing.T) {
	base, token := wire(t)
	id := makeLibrary(t, base, token)
	scoped := narrowed(t, "rw", credential.Scope{LibraryID: id})

	code, body := call(t, "GET", base+"/api/silo/v1/account/credentials", scoped, "")
	if code != http.StatusForbidden {
		t.Errorf("listing with a scoped credential: status %d, body %s; want 403", code, body)
	}
	code, body = call(t, "DELETE", base+"/api/silo/v1/account/credentials/anything", scoped, "")
	if code != http.StatusForbidden {
		t.Errorf("revoking with a scoped credential: status %d, body %s; want 403", code, body)
	}
}

// An invited account's listing does not offer the invite it arrived on.
//
// That row is kept -- the Invite table references it as the record of an
// arrival, which is why credential.Revoke excludes the kind -- but it is not
// something the person can act on: it opens one route, which refuses it as
// spent, and a DELETE aimed at it answers 404. Listing it would be a menu item
// that does nothing. `silo token list` still shows it, because an operator is
// reading the record rather than choosing what to sign out.
func TestAnInvitedAccountIsNotOfferedItsOwnInvite(t *testing.T) {
	base, adminToken, _ := adminWire(t)
	const email, password = "invited@example.com", "correct horse battery staple"

	inviteToken, inviteCredID := mintInvite(t, base, adminToken, email)
	code, body := redeem(t, base, inviteToken, password)
	if code != http.StatusCreated {
		t.Fatalf("redeeming: status %d, body %s", code, body)
	}
	var out struct {
		Token string `json:"token"`
	}
	decodeInto(t, body, &out)

	for _, c := range listCredentials(t, base, out.Token) {
		if c.ID == inviteCredID {
			t.Errorf("the listing offers the spent invite %s", c.ID)
		}
		if c.Kind == "invite" {
			t.Errorf("the listing offers a credential of kind invite: %+v", c)
		}
	}

	// And aiming at it directly is a 404 rather than a 500 from the foreign
	// key -- the half of step 0 the endpoint stands on.
	code, body = call(t, "DELETE", base+"/api/silo/v1/account/credentials/"+inviteCredID, out.Token, "")
	if code != http.StatusNotFound {
		t.Errorf("revoking the spent invite: status %d, body %s; want 404", code, body)
	}

	// Signing out of everything still works for this account, which is what
	// was broken before.
	code, body = call(t, "POST", base+"/api/silo/v1/auth/logout/everywhere", out.Token, "")
	if code != http.StatusOK {
		t.Errorf("logout/everywhere for an invited account: status %d, body %s", code, body)
	}
}
