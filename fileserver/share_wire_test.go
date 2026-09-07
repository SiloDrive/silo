package silod

import (
	"net/http"
	"strings"
	"testing"

	"github.com/dkam/silo/fileserver/account"
)

// The share surface: the grant model over HTTP.
//
// The model has been reachable only from CheckPerm, which reads grants nothing
// could write. These tests are about the writing, and about the one question
// the endpoints add to the model: whose decision a share is.

// shares is the path a library's grants live under.
func sharesPath(base, libraryID string) string {
	return base + "/api/silo/v1/libraries/" + libraryID + "/shares"
}

func shareWith(t *testing.T, base, token, libraryID, email, perm string) (int, string) {
	t.Helper()
	return call(t, "POST", sharesPath(base, libraryID), token,
		`{"email":"`+email+`","perm":"`+perm+`"}`)
}

// libraryIDs is what the caller's listing says they can reach.
func libraryIDs(t *testing.T, base, token string) []string {
	t.Helper()
	code, body := call(t, "GET", base+"/api/silo/v1/libraries", token, "")
	if code != http.StatusOK {
		t.Fatalf("listing libraries: status %d, body %s", code, body)
	}
	var out []struct {
		ID string `json:"id"`
	}
	decodeInto(t, body, &out)
	ids := make([]string, 0, len(out))
	for _, l := range out {
		ids = append(ids, l.ID)
	}
	return ids
}

func contains(ids []string, id string) bool {
	for _, got := range ids {
		if got == id {
			return true
		}
	}
	return false
}

// The point of the surface: a library shared read-only is one the other account
// can list and read and cannot write.
func TestSharingALibraryLetsTheOtherAccountReadIt(t *testing.T) {
	base, token := wire(t)
	id := makeLibrary(t, base, token)
	_, otherToken := makeAccount(t, base, "reader@example.com", "the reader's password", account.RoleUser)

	if contains(libraryIDs(t, base, otherToken), id) {
		t.Fatal("the library is in the other account's listing before it was shared")
	}
	if code, body := call(t, "GET", base+"/api/silo/v1/libraries/"+id+"/entries/", otherToken, ""); code != http.StatusForbidden {
		t.Fatalf("reading before the share = %d, want 403; body %s", code, body)
	}

	if code, body := shareWith(t, base, token, id, "reader@example.com", "r"); code != http.StatusCreated {
		t.Fatalf("sharing: status %d, body %s", code, body)
	}

	if !contains(libraryIDs(t, base, otherToken), id) {
		t.Error("the shared library is not in the other account's listing")
	}
	if code, body := call(t, "GET", base+"/api/silo/v1/libraries/"+id+"/entries/", otherToken, ""); code != http.StatusOK {
		t.Errorf("reading a library shared r = %d, want 200; body %s", code, body)
	}
	// r is a grant, not a suggestion.
	if code, body := call(t, "PUT", base+"/api/silo/v1/libraries/"+id+"/entries/note.txt?type=dir", otherToken, ""); code != http.StatusForbidden {
		t.Errorf("writing to a library shared r = %d, want 403; body %s", code, body)
	}
}

// rw is the other half, and it has to reach the write surface rather than only
// the listing.
func TestALibrarySharedForWritingCanBeWrittenTo(t *testing.T) {
	base, token := wire(t)
	id := makeLibrary(t, base, token)
	_, otherToken := makeAccount(t, base, "writer@example.com", "the writer's password", account.RoleUser)

	if code, body := shareWith(t, base, token, id, "writer@example.com", "rw"); code != http.StatusCreated {
		t.Fatalf("sharing: status %d, body %s", code, body)
	}
	if code, body := call(t, "PUT", base+"/api/silo/v1/libraries/"+id+"/entries/theirs?type=dir", otherToken, ""); code != http.StatusCreated {
		t.Errorf("writing to a library shared rw = %d, want 201; body %s", code, body)
	}
}

// Whose decision a share is. Being able to write in somebody's library is not
// being able to hand it on, which is the difference between a grant and
// ownership -- and the listing is the same decision read the other way.
func TestOnlyTheOwnerManagesALibrarysShares(t *testing.T) {
	base, token := wire(t)
	id := makeLibrary(t, base, token)
	_, otherToken := makeAccount(t, base, "grantee@example.com", "the grantee's password", account.RoleUser)
	if code, body := shareWith(t, base, token, id, "grantee@example.com", "rw"); code != http.StatusCreated {
		t.Fatalf("sharing: status %d, body %s", code, body)
	}
	makeAccount(t, base, "third@example.com", "a third party's password", account.RoleUser)

	if code, body := shareWith(t, base, otherToken, id, "third@example.com", "r"); code != http.StatusForbidden {
		t.Errorf("a grantee re-sharing = %d, want 403; body %s", code, body)
	}
	if code, body := call(t, "GET", sharesPath(base, id), otherToken, ""); code != http.StatusForbidden {
		t.Errorf("a grantee reading the shares = %d, want 403; body %s", code, body)
	}
	if code, _ := call(t, "GET", sharesPath(base, id), "", ""); code != http.StatusUnauthorized {
		t.Error("no credential is not 401 on the share listing")
	}
	// And a library the caller cannot see at all answers as one that does not
	// exist to them, rather than confirming it exists.
	if code, body := shareWith(t, base, otherToken, "00000000-0000-0000-0000-000000000000", "third@example.com", "r"); code != http.StatusForbidden && code != http.StatusNotFound {
		t.Errorf("sharing a stranger's library = %d, want 403 or 404; body %s", code, body)
	}
}

// Unsharing, and what it takes away: the listing, and the read.
func TestUnsharingTakesTheLibraryBack(t *testing.T) {
	base, token := wire(t)
	id := makeLibrary(t, base, token)
	acctID, otherToken := makeAccount(t, base, "temporary@example.com", "a password", account.RoleUser)
	if code, body := shareWith(t, base, token, id, "temporary@example.com", "r"); code != http.StatusCreated {
		t.Fatalf("sharing: status %d, body %s", code, body)
	}

	code, body := call(t, "DELETE", sharesPath(base, id)+"/user:"+acctID.String(), token, "")
	if code != http.StatusNoContent {
		t.Fatalf("unsharing: status %d, body %s", code, body)
	}
	if contains(libraryIDs(t, base, otherToken), id) {
		t.Error("the library is still in the listing after it was unshared")
	}
	if code, _ := call(t, "GET", base+"/api/silo/v1/libraries/"+id+"/entries/", otherToken, ""); code != http.StatusForbidden {
		t.Error("the library is still readable after it was unshared")
	}
	// Idempotent: the caller asked for a state, and it is the state they asked
	// for. A second delete is somebody making sure.
	if code, _ := call(t, "DELETE", sharesPath(base, id)+"/user:"+acctID.String(), token, ""); code != http.StatusNoContent {
		t.Error("unsharing something already unshared is not 204")
	}
}

// The listing names who, as what, and when -- an operator's view of a library's
// blast radius, in the address they shared it at rather than in an account id
// they have never seen.
func TestTheShareListingNamesTheAddressItWasSharedTo(t *testing.T) {
	base, token := wire(t)
	id := makeLibrary(t, base, token)
	makeAccount(t, base, "named@example.com", "a password", account.RoleUser)
	if code, body := shareWith(t, base, token, id, "named@example.com", "rw"); code != http.StatusCreated {
		t.Fatalf("sharing: status %d, body %s", code, body)
	}

	code, body := call(t, "GET", sharesPath(base, id), token, "")
	if code != http.StatusOK {
		t.Fatalf("listing shares: status %d, body %s", code, body)
	}
	var out []struct {
		Principal string `json:"principal"`
		Email     string `json:"email"`
		Perm      string `json:"perm"`
		Path      string `json:"path"`
	}
	decodeInto(t, body, &out)
	if len(out) != 1 {
		t.Fatalf("the listing holds %d shares, want 1: %s", len(out), body)
	}
	if out[0].Email != "named@example.com" || out[0].Perm != "rw" || out[0].Path != "/" {
		t.Errorf("the share reads %+v, want named@example.com rw at /", out[0])
	}
	if !strings.HasPrefix(out[0].Principal, "user:") {
		t.Errorf("principal = %q, want a user principal", out[0].Principal)
	}
}

// Sharing again with a different permission is a correction rather than a
// second fact, which is share.Add's rule reaching the wire.
func TestSharingAgainReplacesThePermission(t *testing.T) {
	base, token := wire(t)
	id := makeLibrary(t, base, token)
	_, otherToken := makeAccount(t, base, "corrected@example.com", "a password", account.RoleUser)

	if code, _ := shareWith(t, base, token, id, "corrected@example.com", "rw"); code != http.StatusCreated {
		t.Fatal("the first share was refused")
	}
	if code, body := shareWith(t, base, token, id, "corrected@example.com", "r"); code != http.StatusCreated {
		t.Fatalf("correcting the share: status %d, body %s", code, body)
	}

	code, body := call(t, "GET", sharesPath(base, id), token, "")
	if code != http.StatusOK {
		t.Fatalf("listing shares: status %d, body %s", code, body)
	}
	var out []struct {
		Perm string `json:"perm"`
	}
	decodeInto(t, body, &out)
	if len(out) != 1 {
		t.Fatalf("the listing holds %d shares, want 1: %s", len(out), body)
	}
	if out[0].Perm != "r" {
		t.Errorf("perm = %q after the correction, want r", out[0].Perm)
	}
	if code, _ := call(t, "PUT", base+"/api/silo/v1/libraries/"+id+"/entries/theirs?type=dir", otherToken, ""); code != http.StatusForbidden {
		t.Error("the narrowed share still permits writing")
	}
}

// An address nobody has enrolled is held for them: the share mints the
// inactive account the address will belong to, and redemption of an invite to
// that address inherits it. This is the join between the two halves of
// docs/plans/sharing.md step 1, and the reason a tombstone is a placeholder for
// an address rather than a way in.
func TestSharingToAnAddressNobodyHasEnrolledHoldsItForThem(t *testing.T) {
	base, adminToken, _ := adminWire(t)
	id := makeLibrary(t, base, adminToken)
	const email = "not-here-yet@example.com"

	if code, body := shareWith(t, base, adminToken, id, email, "r"); code != http.StatusCreated {
		t.Fatalf("sharing to an unknown address: status %d, body %s", code, body)
	}
	acct, err := account.ByEmail(adminCtx(t), email)
	if err != nil {
		t.Fatalf("the share did not mint an account for %s: %v", email, err)
	}
	if acct.IsActive {
		t.Error("the account the share minted is active; a tombstone is not a way in")
	}

	// And when their person arrives, the library is waiting.
	token, _ := mintInvite(t, base, adminToken, email)
	code, body := redeem(t, base, token, "the password they chose")
	if code != http.StatusCreated {
		t.Fatalf("redeeming: status %d, body %s", code, body)
	}
	var out struct {
		Token string `json:"token"`
	}
	decodeInto(t, body, &out)
	if !contains(libraryIDs(t, base, out.Token), id) {
		t.Error("the library shared before they arrived is not in their listing")
	}
}

// The refusals that are about the request rather than about permission.
func TestTheShareRequestIsCheckedBeforeAnythingIsWritten(t *testing.T) {
	base, token := wire(t)
	id := makeLibrary(t, base, token)
	makeAccount(t, base, "someone@example.com", "a password", account.RoleUser)

	for _, tc := range []struct{ name, body string }{
		{"no address", `{"perm":"r"}`},
		{"an unspellable permission", `{"email":"someone@example.com","perm":"x"}`},
		{"no permission", `{"email":"someone@example.com"}`},
		{"sharing with yourself", `{"email":"wire@example.com","perm":"r"}`},
	} {
		if code, body := call(t, "POST", sharesPath(base, id), token, tc.body); code != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400; body %s", tc.name, code, body)
		}
	}
	if code, body := call(t, "GET", sharesPath(base, id), token, ""); code != http.StatusOK {
		t.Fatalf("listing shares: status %d, body %s", code, body)
	} else if strings.Contains(body, "someone@example.com") {
		t.Error("a refused share was written anyway")
	}
}

// An encrypted library cannot be shared yet, and the refusal says why rather
// than handing somebody a library they will never be able to read. The wrap row
// this waits on is docs/plans/e2ee-completion.md step 3.
func TestSharingAnEncryptedLibraryIsRefusedUntilItsKeyCanTravel(t *testing.T) {
	base, token, km := enrolled(t)
	seed := mintSeed(t, km.Public)
	if code, body := call(t, "POST", base+"/api/silo/v1/libraries", token, seed.body(t, "Sealed")); code != http.StatusCreated {
		t.Fatalf("creating an encrypted library: status %d, body %s", code, body)
	}
	makeAccount(t, base, "recipient@example.com", "a password", account.RoleUser)

	code, body := shareWith(t, base, token, seed.LibraryID, "recipient@example.com", "r")
	if code != http.StatusNotImplemented {
		t.Fatalf("sharing an encrypted library = %d, want 501; body %s", code, body)
	}
	if !strings.Contains(strings.ToLower(body), "key") {
		t.Errorf("the refusal does not say what is missing: %q", strings.TrimSpace(body))
	}
}
