package silod

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/dkam/silo/fileserver/account"
	"github.com/dkam/silo/fileserver/admin"
	"github.com/dkam/silo/fileserver/authmgr"
	"github.com/dkam/silo/fileserver/option"
)

// The administrative HTTP surface, and the gate in front of it.
//
// The gate is the part worth testing hardest. Every route here is reachable by
// an ordinary authenticated account until something stops it, and the thing
// that stops it is a conjunction whose two halves are stored in different
// tables -- so a test that only checked the happy path would pass just as well
// if either half had been dropped.

func adminCtx(t *testing.T) context.Context {
	t.Helper()
	c, cancel := option.WithDBTimeout(context.Background())
	t.Cleanup(cancel)
	return c
}

// makeAccount mints an account with a role and a capability set, and returns a
// token for it. The install's own hand, which is what setup and the CLI have.
func makeAccount(t *testing.T, base, email, password string, role account.Role, caps ...admin.Capability) (account.ID, string) {
	t.Helper()
	if _, err := authmgr.CreateAccount(adminCtx(t), email, password, role); err != nil {
		t.Fatalf("creating %s: %v", email, err)
	}
	acct, err := account.ByEmail(adminCtx(t), email)
	if err != nil {
		t.Fatalf("reading back %s: %v", email, err)
	}
	if len(caps) > 0 {
		if err := admin.Assign(adminCtx(t), acct.ID, caps...); err != nil {
			t.Fatalf("assigning capabilities: %v", err)
		}
	}
	return acct.ID, loginToken(t, base, email, password)
}

func loginToken(t *testing.T, base, email, password string) string {
	t.Helper()
	code, body := call(t, "POST", base+"/api/silo/v1/auth/login", "",
		`{"email":"`+email+`","password":"`+password+`"}`)
	if code != http.StatusOK {
		t.Fatalf("login for %s: status %d, body %s", email, code, body)
	}
	var out struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("decoding login: %v", err)
	}
	return out.Token
}

// adminWire stands the server up with one full administrator, and returns the
// base URL, that administrator's token, and the token of an ordinary account.
func adminWire(t *testing.T) (base, adminToken, userToken string) {
	t.Helper()
	base, userToken = wire(t)
	_, adminToken = makeAccount(t, base, "root@example.com", "an administrator's password",
		account.RoleAdmin, admin.All()...)
	return base, adminToken, userToken
}

// The conjunction, over HTTP. Neither half opens the door on its own, which is
// the whole reason capabilities are not a flag and the role is not a set.
func TestTheAdminGateNeedsBothHalves(t *testing.T) {
	base, adminToken, userToken := adminWire(t)
	const path = "/api/silo/v1/admin/accounts"

	// An ordinary account, whatever rows it holds.
	_, richUserToken := makeAccount(t, base, "rich@example.com", "a user with every row",
		account.RoleUser, admin.All()...)
	if code, body := call(t, "GET", base+path, richUserToken, ""); code != http.StatusForbidden {
		t.Errorf("a non-admin holding every capability = %d, want 403; body %s", code, body)
	}

	// An admin holding nothing.
	_, bareAdminToken := makeAccount(t, base, "bare@example.com", "an admin with no rows",
		account.RoleAdmin)
	code, body := call(t, "GET", base+path, bareAdminToken, "")
	if code != http.StatusForbidden {
		t.Errorf("an admin holding no capabilities = %d, want 403; body %s", code, body)
	}
	// The refusal names what is missing, so an administrator learns what to
	// ask for rather than only that they were refused.
	if !strings.Contains(body, string(admin.CapUsers)) {
		t.Errorf("the refusal does not name the capability: %q", strings.TrimSpace(body))
	}

	// Plain unauthenticated, and an ordinary account with no rows at all.
	if code, _ := call(t, "GET", base+path, "", ""); code != http.StatusUnauthorized {
		t.Errorf("no credential = %d, want 401", code)
	}
	if code, _ := call(t, "GET", base+path, userToken, ""); code != http.StatusForbidden {
		t.Errorf("an ordinary account = %d, want 403", code)
	}

	// And both halves together.
	if code, body := call(t, "GET", base+path, adminToken, ""); code != http.StatusOK {
		t.Errorf("a full administrator = %d, want 200; body %s", code, body)
	}
}

// The capability is per route, not per subtree. An account holding users must
// not thereby be able to reset somebody's password -- that split is the reason
// passwords is its own word.
func TestEachRouteAsksForItsOwnCapability(t *testing.T) {
	base, adminToken, _ := adminWire(t)
	id, _ := makeAccount(t, base, "subject@example.com", "the subject's password", account.RoleUser)
	_, usersOnly := makeAccount(t, base, "onboarder@example.com", "an onboarder's password",
		account.RoleAdmin, admin.CapUsers)

	if code, _ := call(t, "GET", base+"/api/silo/v1/admin/accounts", usersOnly, ""); code != http.StatusOK {
		t.Errorf("the users capability does not open the accounts listing: %d", code)
	}
	for _, c := range []struct {
		method, path, body string
	}{
		{"POST", "/api/silo/v1/admin/accounts/" + id.String() + "/password", `{"password":"reset"}`},
		{"GET", "/api/silo/v1/admin/accounts/" + id.String() + "/quota", ""},
		{"PUT", "/api/silo/v1/admin/accounts/" + id.String() + "/role", `{"role":"guest"}`},
		{"PUT", "/api/silo/v1/admin/accounts/" + id.String() + "/caps", `{"capabilities":[]}`},
	} {
		if code, body := call(t, c.method, base+c.path, usersOnly, c.body); code != http.StatusForbidden {
			t.Errorf("%s %s with only the users capability = %d, want 403; body %s",
				c.method, c.path, code, body)
		}
	}
	// The full administrator reaches all of them.
	if code, body := call(t, "GET", base+"/api/silo/v1/admin/accounts/"+id.String()+"/quota", adminToken, ""); code != http.StatusOK {
		t.Errorf("quota as a full administrator = %d; body %s", code, body)
	}
}

// A read-only credential belonging to an administrator must not write. The
// ceiling is a property of the credential and the capability is a property of
// the account; holding one has never implied the other, and an administrator
// who mounted a laptop read-only should not find that it can create accounts.
func TestAReadOnlyCredentialCannotAdminister(t *testing.T) {
	base, _, _ := adminWire(t)

	// The login route mints the credential, and takes the ceiling with it.
	code, body := call(t, "POST", base+"/api/silo/v1/auth/login", "",
		`{"email":"root@example.com","password":"an administrator's password",`+
			`"kind":"device","client_name":"a read-only mount","perm":"r"}`)
	if code != http.StatusCreated && code != http.StatusOK {
		t.Fatalf("minting a read-only credential: %d, body %s", code, body)
	}
	var out struct {
		Credential string `json:"credential"`
		Token      string `json:"token"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("decoding the credential: %v; body %s", err, body)
	}
	readOnly := out.Credential
	if readOnly == "" {
		readOnly = out.Token
	}
	if readOnly == "" {
		t.Fatalf("no credential in %s", body)
	}

	// It reads, because reading is what its ceiling permits and the account
	// holds the capability.
	if code, body := call(t, "GET", base+"/api/silo/v1/admin/accounts", readOnly, ""); code != http.StatusOK {
		t.Errorf("a read-only credential cannot read the listing: %d, body %s", code, body)
	}
	// And it writes nothing, on any of the routes that write.
	for _, c := range []struct{ method, path, body string }{
		{"POST", "/api/silo/v1/admin/accounts", `{"email":"new@example.com","password":"x"}`},
	} {
		if code, body := call(t, c.method, base+c.path, readOnly, c.body); code != http.StatusForbidden {
			t.Errorf("%s %s with a read-only credential = %d, want 403; body %s",
				c.method, c.path, code, body)
		}
	}
}

func TestAdminCreatesAndDisablesAnAccount(t *testing.T) {
	base, adminToken, _ := adminWire(t)

	code, body := call(t, "POST", base+"/api/silo/v1/admin/accounts", adminToken,
		`{"email":"New@Example.COM","password":"a new account's password","role":"guest"}`)
	if code != http.StatusCreated {
		t.Fatalf("creating an account: %d, body %s", code, body)
	}
	var created struct {
		ID    string `json:"id"`
		Email string `json:"email"`
		Role  string `json:"role"`
	}
	if err := json.Unmarshal([]byte(body), &created); err != nil {
		t.Fatal(err)
	}
	if created.Email != "new@example.com" {
		t.Errorf("address is %q, want it normalised", created.Email)
	}
	if created.Role != "guest" {
		t.Errorf("role is %q, want guest", created.Role)
	}

	// The address is taken now, and saying so is not the same as succeeding.
	if code, _ := call(t, "POST", base+"/api/silo/v1/admin/accounts", adminToken,
		`{"email":"new@example.com","password":"another"}`); code != http.StatusConflict {
		t.Errorf("creating the same address twice = %d, want 409", code)
	}

	// The new account can log in, and stops being able to when disabled.
	token := loginToken(t, base, "new@example.com", "a new account's password")
	if code, _ := call(t, "GET", base+"/api/silo/v1/account", token, ""); code != http.StatusOK {
		t.Fatalf("the new account cannot use its token: %d", code)
	}
	if code, body := call(t, "POST", base+"/api/silo/v1/admin/accounts/"+created.ID+"/active",
		adminToken, `{"active":false}`); code != http.StatusOK {
		t.Fatalf("disabling: %d, body %s", code, body)
	}
	if code, _ := call(t, "GET", base+"/api/silo/v1/account", token, ""); code == http.StatusOK {
		t.Error("a disabled account's token still works")
	}
	// Enabling restores what they had, because disabling revoked nothing.
	if code, _ := call(t, "POST", base+"/api/silo/v1/admin/accounts/"+created.ID+"/active",
		adminToken, `{"active":true}`); code != http.StatusOK {
		t.Fatal("re-enabling failed")
	}
	if code, _ := call(t, "GET", base+"/api/silo/v1/account", token, ""); code != http.StatusOK {
		t.Error("re-enabling did not restore the credential the account held")
	}
}

// The route says what the reset cost, in the same words the CLI uses. That is
// the point of the endpoint rather than a nicety: the administrator doing this
// is the one person who cannot find out any other way.
func TestAnAdministrativeResetSaysWhatItCostTheKeyMaterial(t *testing.T) {
	base, adminToken, _ := adminWire(t)
	id, _ := makeAccount(t, base, "subject@example.com", "the subject's password", account.RoleUser)

	code, body := call(t, "POST", base+"/api/silo/v1/admin/accounts/"+id.String()+"/password",
		adminToken, `{"password":"a password the operator chose"}`)
	if code != http.StatusOK {
		t.Fatalf("resetting: %d, body %s", code, body)
	}
	var resp struct {
		IdentityStranded bool   `json:"identity_key_stranded"`
		Recoverable      bool   `json:"identity_recoverable"`
		Says             string `json:"says"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatal(err)
	}
	// This account published no identity key, so there was nothing to lose --
	// and the route says that rather than warning about a blob that is not
	// there.
	if resp.IdentityStranded {
		t.Errorf("the reset claims it stranded a key the account never published: %s", resp.Says)
	}
	if !strings.Contains(resp.Says, "no identity key") {
		t.Errorf("the response does not say what happened: %q", resp.Says)
	}
	loginToken(t, base, "subject@example.com", "a password the operator chose")
}

func TestAdminSetsAQuotaAndClearsIt(t *testing.T) {
	base, adminToken, _ := adminWire(t)
	id, _ := makeAccount(t, base, "subject@example.com", "the subject's password", account.RoleUser)
	path := "/api/silo/v1/admin/accounts/" + id.String() + "/quota"

	code, body := call(t, "PUT", base+path, adminToken, `{"quota":1000000}`)
	if code != http.StatusOK {
		t.Fatalf("setting a quota: %d, body %s", code, body)
	}
	var q struct {
		Quota int64 `json:"quota"`
	}
	if err := json.Unmarshal([]byte(body), &q); err != nil {
		t.Fatal(err)
	}
	if q.Quota != 1000000 {
		t.Errorf("quota reads back as %d", q.Quota)
	}

	// Null is "no ceiling", and it needs to be a third state: a stored zero
	// would mean no bytes allowed.
	if code, _ := call(t, "PUT", base+path, adminToken, `{"quota":0}`); code != http.StatusBadRequest {
		t.Errorf("a zero quota = %d, want 400", code)
	}
	code, body = call(t, "PUT", base+path, adminToken, `{"quota":null}`)
	if code != http.StatusOK {
		t.Fatalf("clearing a quota: %d, body %s", code, body)
	}
	if err := json.Unmarshal([]byte(body), &q); err != nil {
		t.Fatal(err)
	}
	if q.Quota == 1000000 {
		t.Error("clearing the quota left the old ceiling in force")
	}
}

// The caps route takes a set, and both of the admin package's invariants reach
// it unchanged -- which is the point of the route being a thin layer over
// Grant and Revoke rather than its own copy of the rules.
func TestTheCapabilitiesRouteIsASetAndKeepsTheInvariants(t *testing.T) {
	base, adminToken, _ := adminWire(t)
	id, _ := makeAccount(t, base, "deputy@example.com", "a deputy's password", account.RoleAdmin)
	path := "/api/silo/v1/admin/accounts/" + id.String() + "/caps"

	code, body := call(t, "PUT", base+path, adminToken, `{"capabilities":["users","quota"]}`)
	if code != http.StatusOK {
		t.Fatalf("setting capabilities: %d, body %s", code, body)
	}
	if !strings.Contains(body, "users") || !strings.Contains(body, "quota") {
		t.Errorf("the response does not hold the set: %s", body)
	}

	// A set, not an addition: sending a shorter one takes the difference away.
	code, body = call(t, "PUT", base+path, adminToken, `{"capabilities":["users"]}`)
	if code != http.StatusOK {
		t.Fatalf("shrinking the set: %d, body %s", code, body)
	}
	if strings.Contains(body, "quota") {
		t.Errorf("the set was treated as an addition: %s", body)
	}

	// Outside the vocabulary is refused at the door, and nothing is written.
	if code, _ := call(t, "PUT", base+path, adminToken,
		`{"capabilities":["users","read_any_library"]}`); code != http.StatusBadRequest {
		t.Errorf("an unknown capability = %d, want 400", code)
	}

	// Nobody grants what they do not hold. The deputy holds users and now the
	// grant capability, and still cannot hand on retention.
	if code, _ := call(t, "PUT", base+path, adminToken,
		`{"capabilities":["users","grant"]}`); code != http.StatusOK {
		t.Fatal("granting the deputy the grant capability failed")
	}
	deputy := loginToken(t, base, "deputy@example.com", "a deputy's password")
	other, _ := makeAccount(t, base, "other@example.com", "another password", account.RoleAdmin)
	otherPath := "/api/silo/v1/admin/accounts/" + other.String() + "/caps"
	code, body = call(t, "PUT", base+otherPath, deputy, `{"capabilities":["retention"]}`)
	if code != http.StatusForbidden {
		t.Errorf("handing on an unheld capability = %d, want 403; body %s", code, body)
	}
	if code, _ := call(t, "PUT", base+otherPath, deputy, `{"capabilities":["users"]}`); code != http.StatusOK {
		t.Error("handing on a held capability was refused")
	}
}

// A server nobody can administer is a data-loss event with extra steps, and
// there are two doors to it: taking the row away, and taking the role away.
func TestTheRouteRefusesToLeaveNobodyAbleToAdminister(t *testing.T) {
	base, adminToken, _ := adminWire(t)
	root, err := account.ByEmail(adminCtx(t), "root@example.com")
	if err != nil {
		t.Fatal(err)
	}
	id := root.ID.String()

	// The row.
	code, body := call(t, "PUT", base+"/api/silo/v1/admin/accounts/"+id+"/caps", adminToken,
		`{"capabilities":["users","passwords","quota","tokens","retention"]}`)
	if code != http.StatusForbidden {
		t.Errorf("dropping the last grant = %d, want 403; body %s", code, body)
	}

	// The role. Same outcome, different door, and it is the one the
	// last-holder rule alone would have missed.
	code, body = call(t, "PUT", base+"/api/silo/v1/admin/accounts/"+id+"/role", adminToken,
		`{"role":"user"}`)
	if code != http.StatusForbidden {
		t.Errorf("demoting the last administrator = %d, want 403; body %s", code, body)
	}

	// With a second full administrator standing, both are ordinary changes.
	second, _ := makeAccount(t, base, "second@example.com", "a second administrator", account.RoleAdmin)
	if code, _ := call(t, "PUT", base+"/api/silo/v1/admin/accounts/"+second.String()+"/caps",
		adminToken, `{"capabilities":["grant"]}`); code != http.StatusOK {
		t.Fatal("granting a second holder failed")
	}
	if code, body := call(t, "PUT", base+"/api/silo/v1/admin/accounts/"+id+"/role", adminToken,
		`{"role":"user"}`); code != http.StatusOK {
		t.Errorf("demoting with a second administrator standing = %d; body %s", code, body)
	}
}

func TestAdminRoutesAnswerForAnAccountThatIsNotThere(t *testing.T) {
	base, adminToken, _ := adminWire(t)
	if code, _ := call(t, "GET", base+"/api/silo/v1/admin/accounts/not-a-uuid/quota", adminToken, ""); code != http.StatusBadRequest {
		t.Errorf("a malformed id = %d, want 400", code)
	}
	missing := "00000000-0000-7000-8000-000000000001"
	if code, _ := call(t, "GET", base+"/api/silo/v1/admin/accounts/"+missing+"/quota", adminToken, ""); code != http.StatusNotFound {
		t.Errorf("an id naming nobody = %d, want 404", code)
	}
}
