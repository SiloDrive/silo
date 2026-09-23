package silod

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/SiloDrive/silo/fileserver/account"
)

// The invite surface over HTTP: minting, listing and withdrawing on the
// administrative lane, and redemption on no lane at all.
//
// Redemption is the half worth testing hardest. It is the only unauthenticated
// request that brings an account into being besides setup, it is where the
// address is proved rather than typed, and every one of its refusals is a
// message somebody outside the install has to act on.

// decodeInto reads a JSON body a handler answered with, failing the test with
// the body in hand rather than with a decoder's own words about a byte offset.
func decodeInto(t *testing.T, body string, v any) {
	t.Helper()
	if err := json.Unmarshal([]byte(body), v); err != nil {
		t.Fatalf("decoding %s: %v", body, err)
	}
}

// mintInvite asks the administrative route for one and returns the token and
// the credential id, which is what listing and withdrawal name it by.
func mintInvite(t *testing.T, base, adminToken, email string) (token, credentialID string) {
	t.Helper()
	code, body := call(t, "POST", base+"/api/silo/v1/admin/invites", adminToken,
		`{"email":"`+email+`"}`)
	if code != http.StatusCreated {
		t.Fatalf("minting an invite for %s: status %d, body %s", email, code, body)
	}
	var out struct {
		Token        string `json:"token"`
		CredentialID string `json:"credential_id"`
		Email        string `json:"email"`
	}
	decodeInto(t, body, &out)
	if out.Token == "" || out.CredentialID == "" {
		t.Fatalf("mint answered without a token or an id: %s", body)
	}
	if out.Email != email {
		t.Errorf("the invite is for %q, want %q", out.Email, email)
	}
	return out.Token, out.CredentialID
}

func redeem(t *testing.T, base, token, password string) (int, string) {
	t.Helper()
	return call(t, "POST", base+"/api/silo/v1/auth/redeem", "",
		`{"invite_token":"`+token+`","password":"`+password+`"}`)
}

// The gate. Minting an account by another door is still minting an account, so
// it asks for the same capability `silo user add` does -- and an ordinary
// account holding nothing is refused before it can invite anybody.
func TestTheInviteRoutesAskForTheUsersCapability(t *testing.T) {
	base, adminToken, userToken := adminWire(t)
	const path = "/api/silo/v1/admin/invites"

	if code, _ := call(t, "GET", base+path, "", ""); code != http.StatusUnauthorized {
		t.Errorf("no credential = %d, want 401", code)
	}
	if code, body := call(t, "POST", base+path, userToken, `{"email":"nobody@example.com"}`); code != http.StatusForbidden {
		t.Errorf("an ordinary account minting an invite = %d, want 403; body %s", code, body)
	}
	_, bareAdmin := makeAccount(t, base, "bare-inviter@example.com", "an admin with no rows",
		account.RoleAdmin)
	if code, body := call(t, "GET", base+path, bareAdmin, ""); code != http.StatusForbidden {
		t.Errorf("an admin holding no capabilities = %d, want 403; body %s", code, body)
	}
	if code, body := call(t, "GET", base+path, adminToken, ""); code != http.StatusOK {
		t.Errorf("a full administrator = %d, want 200; body %s", code, body)
	}
}

// The whole point, end to end: an address is invited, the person redeeming
// picks a password and gets a session, and the account they land in is the one
// the invite named.
func TestRedeemingAnInviteActivatesTheInvitedAddress(t *testing.T) {
	base, adminToken, _ := adminWire(t)
	const email, password = "invited@example.com", "correct horse battery staple"

	token, _ := mintInvite(t, base, adminToken, email)

	// Before redemption the account exists and opens nothing: a tombstone is a
	// placeholder for an address, not a way in.
	acct, err := account.ByEmail(adminCtx(t), email)
	if err != nil {
		t.Fatalf("the invite did not mint a tombstone for %s: %v", email, err)
	}
	if acct.IsActive {
		t.Error("the invited account is active before anybody redeemed it")
	}

	code, body := redeem(t, base, token, password)
	if code != http.StatusCreated {
		t.Fatalf("redeeming: status %d, body %s", code, body)
	}
	var out struct {
		Token string `json:"token"`
	}
	decodeInto(t, body, &out)
	if out.Token == "" {
		t.Fatalf("redemption answered with no session token: %s", body)
	}

	// The session works, and it is the invited account's.
	if code, body := call(t, "GET", base+"/api/silo/v1/libraries", out.Token, ""); code != http.StatusOK {
		t.Fatalf("the session redemption handed back: status %d, body %s", code, body)
	}
	// And the password it chose is the account's, so the person can log in
	// again tomorrow without another invite.
	if tok := loginToken(t, base, email, password); tok == "" {
		t.Error("logging in with the password chosen at redemption returned nothing")
	}
}

// The redeemer does not choose their address: there is no field for one.
// Delivery of the invite to that inbox is the verification, so a body naming
// somebody else changes nothing.
func TestRedemptionIgnoresAnAddressInTheBody(t *testing.T) {
	base, adminToken, _ := adminWire(t)
	const invited = "meant-for-them@example.com"
	token, _ := mintInvite(t, base, adminToken, invited)

	code, body := call(t, "POST", base+"/api/silo/v1/auth/redeem", "",
		`{"invite_token":"`+token+`","password":"a password they chose","email":"someone-else@example.com"}`)
	if code != http.StatusCreated {
		t.Fatalf("redeeming: status %d, body %s", code, body)
	}
	if _, err := account.ByEmail(adminCtx(t), "someone-else@example.com"); err == nil {
		t.Fatal("the address in the body created an account")
	}
	acct, err := account.ByEmail(adminCtx(t), invited)
	if err != nil {
		t.Fatalf("reading back the invited account: %v", err)
	}
	if !acct.IsActive {
		t.Error("the invited account is still inactive after redemption")
	}
}

// Single-use, and the answer says which of the two refusals it is: an invite
// somebody already used is not one that ran out.
func TestASpentInviteIsRefusedAsSpent(t *testing.T) {
	base, adminToken, _ := adminWire(t)
	token, _ := mintInvite(t, base, adminToken, "once@example.com")

	if code, body := redeem(t, base, token, "the first password"); code != http.StatusCreated {
		t.Fatalf("the first redemption: status %d, body %s", code, body)
	}
	code, body := redeem(t, base, token, "the second password")
	if code != http.StatusConflict {
		t.Fatalf("the second redemption = %d, want 409; body %s", code, body)
	}
	if !strings.Contains(strings.ToLower(body), "redeem") {
		t.Errorf("the refusal does not say the invite was already redeemed: %q", strings.TrimSpace(body))
	}
}

// A withdrawn invite and a lapsed one give the holder the same answer, which is
// invite.Revoke's decision rather than this handler's: neither is something the
// holder can act on except by asking for another one.
func TestAWithdrawnInviteIsRefused(t *testing.T) {
	base, adminToken, _ := adminWire(t)
	token, id := mintInvite(t, base, adminToken, "withdrawn@example.com")

	if code, body := call(t, "DELETE", base+"/api/silo/v1/admin/invites/"+id, adminToken, ""); code != http.StatusNoContent {
		t.Fatalf("withdrawing: status %d, body %s", code, body)
	}
	if code, body := redeem(t, base, token, "too late"); code != http.StatusUnauthorized {
		t.Errorf("redeeming a withdrawn invite = %d, want 401; body %s", code, body)
	}
	// The row stays: a trail that forgets the invites nobody redeemed only
	// remembers the decisions that worked out.
	if code, body := call(t, "GET", base+"/api/silo/v1/admin/invites", adminToken, ""); code != http.StatusOK {
		t.Fatalf("listing: status %d, body %s", code, body)
	} else if !strings.Contains(body, "withdrawn@example.com") {
		t.Errorf("the withdrawn invite is not in the listing: %s", body)
	}
}

// A spent invite cannot be withdrawn, because there is nothing left to
// withdraw. The refusal is what sends an operator to the operation they
// actually want, which is taking the account back.
func TestAnInviteCannotBeWithdrawnAfterItIsRedeemed(t *testing.T) {
	base, adminToken, _ := adminWire(t)
	token, id := mintInvite(t, base, adminToken, "arrived@example.com")
	if code, body := redeem(t, base, token, "a password"); code != http.StatusCreated {
		t.Fatalf("redeeming: status %d, body %s", code, body)
	}
	if code, body := call(t, "DELETE", base+"/api/silo/v1/admin/invites/"+id, adminToken, ""); code != http.StatusConflict {
		t.Errorf("withdrawing a spent invite = %d, want 409; body %s", code, body)
	}
}

// Garbage gets the same answer a withdrawn invite does, so a guesser learns
// nothing about which half of the guess to keep.
func TestAnUnknownInviteTokenIsRefusedLikeALapsedOne(t *testing.T) {
	base, _, _ := adminWire(t)
	if code, _ := redeem(t, base, "silo_invite_notatoken", "a password"); code != http.StatusUnauthorized {
		t.Errorf("a malformed invite token = %d, want 401", code)
	}
	if code, _ := call(t, "POST", base+"/api/silo/v1/auth/redeem", "", `{"invite_token":"","password":"x"}`); code != http.StatusBadRequest {
		t.Errorf("an empty invite token = %d, want 400", code)
	}
	if code, _ := redeem(t, base, "silo_invite_notatoken", ""); code != http.StatusBadRequest {
		t.Errorf("an empty password = %d, want 400", code)
	}
}

// The invite kind opens exactly one route. It is not a session token for the
// address it names, and the header path must not accept it at all.
func TestAnInviteTokenIsNotASessionToken(t *testing.T) {
	base, adminToken, _ := adminWire(t)
	token, _ := mintInvite(t, base, adminToken, "notasession@example.com")
	if code, _ := call(t, "GET", base+"/api/silo/v1/libraries", token, ""); code != http.StatusUnauthorized {
		t.Errorf("an invite presented as a bearer token = %d, want 401", code)
	}
}

// Redemption can carry the split-derivation crossover, which is the shape the
// E2EE bootstrap needs: the client derives twice, sends the authKey, and the
// wrapKey never leaves. The account is crossed over from its first moment
// rather than by a later password change.
func TestRedemptionCanCrossTheAccountOverToADerivedKey(t *testing.T) {
	base, adminToken, _ := adminWire(t)
	const email = "derived@example.com"
	token, _ := mintInvite(t, base, adminToken, email)

	params := testKDFParams()
	code, body := call(t, "POST", base+"/api/silo/v1/auth/redeem", "",
		`{"invite_token":"`+token+`","password":"an auth key",`+
			`"client_kdf_params":"`+params.String()+`"}`)
	if code != http.StatusCreated {
		t.Fatalf("redeeming with kdf params: status %d, body %s", code, body)
	}
	got, err := account.ClientKDFParams(adminCtx(t), email)
	if err != nil {
		t.Fatalf("reading back the parameters for %s: %v", email, err)
	}
	if got != params.String() {
		t.Errorf("client_kdf_params = %q, want %q", got, params.String())
	}
}

// The model's own guards still hold through the handler: an address somebody is
// already using cannot be invited, because activating it would hand a stranger
// whatever was shared to it.
func TestAnActiveAddressCannotBeInvited(t *testing.T) {
	base, adminToken, _ := adminWire(t)
	code, body := call(t, "POST", base+"/api/silo/v1/admin/invites", adminToken,
		`{"email":"root@example.com"}`)
	if code != http.StatusConflict {
		t.Errorf("inviting an address in use = %d, want 409; body %s", code, body)
	}
}

// The listing is the operator's view of who is outstanding and who arrived, and
// it never carries a token: mint is the one moment the secret exists.
func TestTheInviteListingCarriesNoToken(t *testing.T) {
	base, adminToken, _ := adminWire(t)
	tok, _ := mintInvite(t, base, adminToken, "listed@example.com")

	code, body := call(t, "GET", base+"/api/silo/v1/admin/invites", adminToken, "")
	if code != http.StatusOK {
		t.Fatalf("listing: status %d, body %s", code, body)
	}
	if strings.Contains(body, tok) {
		t.Error("the listing hands the invite token back out")
	}
	if !strings.Contains(body, "listed@example.com") {
		t.Errorf("the listing does not name the invited address: %s", body)
	}
}

// An invite may name a role, and an admin invite is not an escalation: the
// account arrives with the role and no capability rows, and admin.Can is a
// conjunction.
func TestAnInviteCarriesItsRole(t *testing.T) {
	base, adminToken, _ := adminWire(t)
	const email = "curator@example.com"
	code, body := call(t, "POST", base+"/api/silo/v1/admin/invites", adminToken,
		`{"email":"`+email+`","role":"guest"}`)
	if code != http.StatusCreated {
		t.Fatalf("minting: status %d, body %s", code, body)
	}
	var out struct {
		Token string `json:"token"`
	}
	decodeInto(t, body, &out)
	if code, body := redeem(t, base, out.Token, "a guest's password"); code != http.StatusCreated {
		t.Fatalf("redeeming: status %d, body %s", code, body)
	}
	acct, err := account.ByEmail(adminCtx(t), email)
	if err != nil {
		t.Fatalf("reading back %s: %v", email, err)
	}
	if acct.Role != account.RoleGuest {
		t.Errorf("the redeemed account is %q, want guest", acct.Role)
	}
	if code, body := call(t, "POST", base+"/api/silo/v1/admin/invites", adminToken,
		`{"email":"typo@example.com","role":"wizard"}`); code != http.StatusBadRequest {
		t.Errorf("an unknown role = %d, want 400; body %s", code, body)
	}
}
