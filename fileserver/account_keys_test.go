package silod

import (
	"errors"

	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/dkam/silo/fileserver/account"
	"github.com/dkam/silo/fileserver/authmgr"
	"github.com/dkam/silo/fileserver/credential"
	"github.com/dkam/silo/fileserver/option"
	"github.com/dkam/silo/store"
)

// The account side of end-to-end encryption: the key material an account
// publishes, the blobs only its holder can open, and the one endpoint that
// answers before anybody has logged in.
//
// docs/storage.md's list of what is left calls this "the one item that
// unblocks a product decision already taken" -- the format in store/ is
// finished and tested, and had nowhere on the server to live. See
// docs/auth.md, "The client's KDF is not this one, and it needs four columns".

// keyMaterial is what a client builds locally and publishes: an identity
// keypair, the private half wrapped under a password-derived key, and a set of
// recovery wraps of the same private half.
//
// It is built with the real store/ functions rather than with random bytes,
// because two of the rules under test are about what the blobs *say* -- the
// parameters sealed inside the identity wrap have to agree with the column
// beside it, and a recovery wrap is not an identity wrap.
type keyMaterial struct {
	Params   store.KDFParams
	Public   []byte
	Wrapped  []byte
	Recovery [][]byte
	Codes    []string
}

func mintKeyMaterial(t *testing.T, holder, password string) keyMaterial {
	t.Helper()

	var salt [store.KDFSaltSize]byte
	for i := range salt {
		salt[i] = byte(i + 1)
	}
	params := store.DefaultKDFParams(salt)

	creds, err := store.DeriveCredentials(password, params)
	if err != nil {
		t.Fatalf("deriving credentials: %v", err)
	}
	id, err := store.GenerateIdentity()
	if err != nil {
		t.Fatalf("generating identity: %v", err)
	}
	priv := id.Private()
	wrapped, err := store.WrapIdentity(creds.WrapKey, holder, params, priv)
	if err != nil {
		t.Fatalf("wrapping identity: %v", err)
	}

	codes, err := store.GenerateRecoveryCodeSet()
	if err != nil {
		t.Fatalf("generating recovery codes: %v", err)
	}
	var wraps [][]byte
	for _, c := range codes {
		w, err := store.WrapForRecovery(c, holder, priv)
		if err != nil {
			t.Fatalf("wrapping for recovery: %v", err)
		}
		wraps = append(wraps, w)
	}

	pub := id.Public()
	return keyMaterial{
		Params:   params,
		Public:   pub[:],
		Wrapped:  wrapped,
		Recovery: wraps,
		Codes:    codes,
	}
}

func (k keyMaterial) keys() account.Keys {
	out := account.Keys{
		PublicKey:  k.Public,
		WrappedKey: k.Wrapped,
		KDFParams:  k.Params.String(),
	}
	for i, w := range k.Recovery {
		out.Recovery = append(out.Recovery, account.RecoveryWrap{Ordinal: i, WrappedKey: w})
	}
	return out
}

// mintPasswordAccount gives a test an account that can hold key material.
// mintAccount creates one with no password row, and the client KDF parameters
// live on that row -- deliberately, since they describe how a password becomes
// the key that opens the identity blob.
func mintPasswordAccount(t *testing.T, email, password string) *account.Account {
	t.Helper()
	authmgr.Init(siloPair.Read, siloPair.Write)
	if _, err := authmgr.CreateAccount(context.Background(), email, password, account.RoleUser); err != nil {
		t.Fatalf("create account %s: %v", email, err)
	}
	return acctFor(t, email)
}

func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := option.WithDBTimeout(context.Background())
	t.Cleanup(cancel)
	return ctx
}

// --- the store ---------------------------------------------------------

func TestKeysRoundTripThroughTheDatabase(t *testing.T) {
	sqliteTestDB(t)
	acct := mintPasswordAccount(t, "keys@example.com", "correct horse battery staple")
	ctx := testCtx(t)

	km := mintKeyMaterial(t, acct.ID.String(), "correct horse battery staple")
	if err := account.SetKeys(ctx, acct.ID, km.keys()); err != nil {
		t.Fatalf("SetKeys: %v", err)
	}

	got, err := account.GetKeys(ctx, acct.ID)
	if err != nil {
		t.Fatalf("GetKeys: %v", err)
	}
	if string(got.PublicKey) != string(km.Public) {
		t.Error("the published public key came back different")
	}
	if string(got.WrappedKey) != string(km.Wrapped) {
		t.Error("the wrapped identity key came back different")
	}
	if got.KDFParams != km.Params.String() {
		t.Errorf("kdf params = %q, want %q", got.KDFParams, km.Params.String())
	}
	if len(got.Recovery) != store.RecoveryCodeSetSize {
		t.Fatalf("got %d recovery wraps, want %d", len(got.Recovery), store.RecoveryCodeSetSize)
	}

	// The blobs have to survive intact, not merely be counted: a recovery
	// wrap that came back under the wrong ordinal opens with the wrong code.
	for i, w := range got.Recovery {
		if w.Ordinal != i {
			t.Fatalf("recovery wrap %d has ordinal %d", i, w.Ordinal)
		}
		priv, err := store.UnwrapWithRecovery(km.Codes[i], acct.ID.String(), w.WrappedKey)
		if err != nil {
			t.Fatalf("recovery wrap %d did not open with its code: %v", i, err)
		}
		id, err := store.IdentityFromPrivate(priv)
		if err != nil {
			t.Fatalf("recovery wrap %d did not yield an identity: %v", i, err)
		}
		pub := id.Public()
		if string(pub[:]) != string(km.Public) {
			t.Errorf("recovery wrap %d opened to a different identity", i)
		}
	}
}

// The parameters ride inside the wrapped blob and sit in a column beside it,
// and store.WrapIdentity's own comment names the hazard: "a pair that can
// drift, after which the blob is unopenable and nothing says why". The column
// exists so the pre-login endpoint can answer without handing out the blob, so
// the two are not redundant -- they are one fact stored twice, which is
// exactly the shape that needs a check at the write.
func TestKeysAreRefusedWhenTheBlobAndTheColumnDisagree(t *testing.T) {
	sqliteTestDB(t)
	acct := mintPasswordAccount(t, "drift@example.com", "correct horse battery staple")
	ctx := testCtx(t)

	km := mintKeyMaterial(t, acct.ID.String(), "correct horse battery staple")
	k := km.keys()

	other := km.Params
	other.Salt[0] ^= 0xff
	k.KDFParams = other.String()

	if err := account.SetKeys(ctx, acct.ID, k); err == nil {
		t.Fatal("stored an identity whose sealed parameters differ from the column")
	}
}

func TestKeysAreRefusedForABadPublicKey(t *testing.T) {
	sqliteTestDB(t)
	acct := mintPasswordAccount(t, "shortkey@example.com", "correct horse battery staple")
	ctx := testCtx(t)

	km := mintKeyMaterial(t, acct.ID.String(), "correct horse battery staple")
	k := km.keys()
	k.PublicKey = k.PublicKey[:16]

	if err := account.SetKeys(ctx, acct.ID, k); err == nil {
		t.Fatal("stored a public key that is not an X25519 key")
	}
}

// Redemption deletes one blob and the rest of the set stands. auth.md is
// explicit that regenerating the set on redemption is the tidier-looking rule
// and the worse one, "because it invalidates the codes a person is still
// holding at the moment they have proved they lost their device".
func TestRedeemingARecoveryWrapLeavesTheRestStanding(t *testing.T) {
	sqliteTestDB(t)
	acct := mintPasswordAccount(t, "recover@example.com", "correct horse battery staple")
	ctx := testCtx(t)

	km := mintKeyMaterial(t, acct.ID.String(), "correct horse battery staple")
	if err := account.SetKeys(ctx, acct.ID, km.keys()); err != nil {
		t.Fatalf("SetKeys: %v", err)
	}

	gone, err := account.DeleteRecoveryWrap(ctx, acct.ID, 3)
	if err != nil {
		t.Fatalf("DeleteRecoveryWrap: %v", err)
	}
	if !gone {
		t.Fatal("redeeming an ordinal that exists reported nothing deleted")
	}

	got, err := account.GetKeys(ctx, acct.ID)
	if err != nil {
		t.Fatalf("GetKeys: %v", err)
	}
	if len(got.Recovery) != store.RecoveryCodeSetSize-1 {
		t.Errorf("got %d recovery wraps, want %d", len(got.Recovery), store.RecoveryCodeSetSize-1)
	}
	for _, w := range got.Recovery {
		if w.Ordinal == 3 {
			t.Error("the redeemed wrap is still there")
		}
	}

	// Redeeming the same one twice is not an error the caller can act on, but
	// it must not report a deletion that did not happen.
	gone, err = account.DeleteRecoveryWrap(ctx, acct.ID, 3)
	if err != nil {
		t.Fatalf("DeleteRecoveryWrap twice: %v", err)
	}
	if gone {
		t.Error("redeeming an ordinal that was already gone reported a deletion")
	}
}

// Publishing again replaces the whole set rather than merging into it. A
// password change re-wraps the identity key under a new wrapKey, and a stale
// blob left beside the new one is a blob that opens with the old password.
func TestPublishingKeysAgainReplacesTheSet(t *testing.T) {
	sqliteTestDB(t)
	acct := mintPasswordAccount(t, "replace@example.com", "correct horse battery staple")
	ctx := testCtx(t)

	first := mintKeyMaterial(t, acct.ID.String(), "correct horse battery staple")
	if err := account.SetKeys(ctx, acct.ID, first.keys()); err != nil {
		t.Fatalf("SetKeys: %v", err)
	}
	if _, err := account.DeleteRecoveryWrap(ctx, acct.ID, 9); err != nil {
		t.Fatalf("DeleteRecoveryWrap: %v", err)
	}

	second := mintKeyMaterial(t, acct.ID.String(), "a different password entirely")
	if err := account.SetKeys(ctx, acct.ID, second.keys()); err != nil {
		t.Fatalf("SetKeys again: %v", err)
	}

	got, err := account.GetKeys(ctx, acct.ID)
	if err != nil {
		t.Fatalf("GetKeys: %v", err)
	}
	if string(got.WrappedKey) != string(second.Wrapped) {
		t.Error("the second publish did not replace the wrapped identity key")
	}
	if len(got.Recovery) != store.RecoveryCodeSetSize {
		t.Errorf("got %d recovery wraps, want a full set of %d", len(got.Recovery), store.RecoveryCodeSetSize)
	}
	if _, err := store.UnwrapWithRecovery(first.Codes[0], acct.ID.String(), got.Recovery[0].WrappedKey); err == nil {
		t.Error("a code from the replaced set still opens a stored wrap")
	}
}

func TestAnAccountWithNoKeysHasNone(t *testing.T) {
	sqliteTestDB(t)
	acct := mintPasswordAccount(t, "bare@example.com", "correct horse battery staple")
	ctx := testCtx(t)

	if _, err := account.GetKeys(ctx, acct.ID); err == nil {
		t.Fatal("an account that has published nothing reported keys")
	}
	if _, err := account.ClientKDFParams(ctx, "bare@example.com"); err == nil {
		t.Fatal("an account that has published nothing reported KDF parameters")
	}
}

// --- the wire ----------------------------------------------------------

const keysPath = "/api/silo/v1/account/keys"

type wireKeys struct {
	PublicKey  []byte `json:"public_key"`
	WrappedKey []byte `json:"wrapped_key"`
	KDFParams  string `json:"kdf_params"`
	Recovery   []struct {
		Ordinal    int    `json:"ordinal"`
		WrappedKey []byte `json:"wrapped_key"`
	} `json:"recovery"`
}

func (k keyMaterial) body(t *testing.T) string {
	t.Helper()
	type wrap struct {
		Ordinal    int    `json:"ordinal"`
		WrappedKey []byte `json:"wrapped_key"`
	}
	req := struct {
		PublicKey  []byte `json:"public_key"`
		WrappedKey []byte `json:"wrapped_key"`
		KDFParams  string `json:"kdf_params"`
		Recovery   []wrap `json:"recovery"`
	}{PublicKey: k.Public, WrappedKey: k.Wrapped, KDFParams: k.Params.String()}
	for i, w := range k.Recovery {
		req.Recovery = append(req.Recovery, wrap{Ordinal: i, WrappedKey: w})
	}
	b, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("encoding key material: %v", err)
	}
	return string(b)
}

// wireAccountID reads the id of the account wire() logged in as, which is what
// every wrap in these tests is bound to.
func wireAccountID(t *testing.T) string {
	t.Helper()
	acct, err := account.ByEmail(testCtx(t), "wire@example.com")
	if err != nil {
		t.Fatalf("looking up the wire account: %v", err)
	}
	return acct.ID.String()
}

func TestPublishingAndFetchingKeysOverTheWire(t *testing.T) {
	base, token := wire(t)
	km := mintKeyMaterial(t, wireAccountID(t), "correct horse battery staple")

	if code, body := call(t, "GET", base+keysPath, token, ""); code != http.StatusNotFound {
		t.Fatalf("GET before publishing: status %d, body %s", code, body)
	}

	code, body := call(t, "PUT", base+keysPath, token, km.body(t))
	if code != http.StatusOK {
		t.Fatalf("PUT keys: status %d, body %s", code, body)
	}

	code, body = call(t, "GET", base+keysPath, token, "")
	if code != http.StatusOK {
		t.Fatalf("GET keys: status %d, body %s", code, body)
	}
	var got wireKeys
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("decoding %s: %v", body, err)
	}
	if string(got.PublicKey) != string(km.Public) {
		t.Error("the public key did not survive the round trip")
	}
	if string(got.WrappedKey) != string(km.Wrapped) {
		t.Error("the wrapped identity key did not survive the round trip")
	}
	if got.KDFParams != km.Params.String() {
		t.Errorf("kdf_params = %q, want %q", got.KDFParams, km.Params.String())
	}
	if len(got.Recovery) != store.RecoveryCodeSetSize {
		t.Errorf("got %d recovery wraps, want %d", len(got.Recovery), store.RecoveryCodeSetSize)
	}
}

func TestRedeemingARecoveryWrapOverTheWire(t *testing.T) {
	base, token := wire(t)
	km := mintKeyMaterial(t, wireAccountID(t), "correct horse battery staple")
	if code, body := call(t, "PUT", base+keysPath, token, km.body(t)); code != http.StatusOK {
		t.Fatalf("PUT keys: status %d, body %s", code, body)
	}

	code, body := call(t, "DELETE", base+keysPath+"/recovery/2", token, "")
	if code != http.StatusOK {
		t.Fatalf("DELETE recovery/2: status %d, body %s", code, body)
	}
	var out struct {
		Remaining int `json:"remaining"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("decoding %s: %v", body, err)
	}
	if out.Remaining != store.RecoveryCodeSetSize-1 {
		t.Errorf("remaining = %d, want %d", out.Remaining, store.RecoveryCodeSetSize-1)
	}

	if code, body := call(t, "DELETE", base+keysPath+"/recovery/2", token, ""); code != http.StatusNotFound {
		t.Errorf("redeeming the same ordinal twice: status %d, body %s", code, body)
	}
}

// The key routes are about the account, not about one library, so a scoped
// credential is refused them by the same rule that refuses it the listing.
func TestAScopedCredentialCannotReachTheKeyRoutes(t *testing.T) {
	base, token := wire(t)
	km := mintKeyMaterial(t, wireAccountID(t), "correct horse battery staple")
	if code, body := call(t, "PUT", base+keysPath, token, km.body(t)); code != http.StatusOK {
		t.Fatalf("PUT keys: status %d, body %s", code, body)
	}

	id := makeLibrary(t, base, token)
	scoped := narrowed(t, "rw", credential.Scope{LibraryID: id})

	if code, _ := call(t, "GET", base+keysPath, scoped, ""); code != http.StatusForbidden {
		t.Errorf("GET with a scoped credential: status %d, want 403", code)
	}
	if code, _ := call(t, "PUT", base+keysPath, scoped, km.body(t)); code != http.StatusForbidden {
		t.Errorf("PUT with a scoped credential: status %d, want 403", code)
	}
}

// Publishing key material is a write. A read-only credential may read what the
// account holds and may not replace it -- replacing it is how a password
// change lands, and a credential that cannot write must not be able to make
// the account's own identity key unopenable.
func TestAReadOnlyCredentialCannotPublishKeys(t *testing.T) {
	base, token := wire(t)
	km := mintKeyMaterial(t, wireAccountID(t), "correct horse battery staple")
	if code, body := call(t, "PUT", base+keysPath, token, km.body(t)); code != http.StatusOK {
		t.Fatalf("PUT keys: status %d, body %s", code, body)
	}

	ro := narrowed(t, "r", credential.Scope{})
	if code, _ := call(t, "GET", base+keysPath, ro, ""); code != http.StatusOK {
		t.Errorf("GET with a read-only credential: status %d, want 200", code)
	}
	if code, _ := call(t, "PUT", base+keysPath, ro, km.body(t)); code != http.StatusForbidden {
		t.Errorf("PUT with a read-only credential: status %d, want 403", code)
	}
	if code, _ := call(t, "DELETE", base+keysPath+"/recovery/0", ro, ""); code != http.StatusForbidden {
		t.Errorf("DELETE with a read-only credential: status %d, want 403", code)
	}
}

func TestTheKeyRoutesNeedACredential(t *testing.T) {
	base, _ := wire(t)
	for _, c := range []struct{ method, path string }{
		{"GET", keysPath},
		{"PUT", keysPath},
		{"DELETE", keysPath + "/recovery/0"},
	} {
		if code, _ := call(t, c.method, base+c.path, "", ""); code != http.StatusUnauthorized {
			t.Errorf("%s %s with no credential: status %d, want 401", c.method, c.path, code)
		}
	}
}

// --- the pre-login parameters endpoint ---------------------------------

const kdfPath = "/api/silo/v1/auth/kdf"

func askKDF(t *testing.T, base, email string) (int, string) {
	t.Helper()
	body, err := json.Marshal(map[string]string{"email": email})
	if err != nil {
		t.Fatalf("encoding: %v", err)
	}
	return call(t, "POST", base+kdfPath, "", string(body))
}

func kdfParams(t *testing.T, body string) string {
	t.Helper()
	var out struct {
		KDFParams string `json:"kdf_params"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("decoding %s: %v", body, err)
	}
	return out.KDFParams
}

func TestThePreLoginEndpointServesTheAccountsOwnParameters(t *testing.T) {
	base, token := wire(t)
	km := mintKeyMaterial(t, wireAccountID(t), "correct horse battery staple")
	if code, body := call(t, "PUT", base+keysPath, token, km.body(t)); code != http.StatusOK {
		t.Fatalf("PUT keys: status %d, body %s", code, body)
	}

	code, body := askKDF(t, base, "wire@example.com")
	if code != http.StatusOK {
		t.Fatalf("auth/kdf: status %d, body %s", code, body)
	}
	if got := kdfParams(t, body); got != km.Params.String() {
		t.Errorf("kdf_params = %q, want %q", got, km.Params.String())
	}
}

// The endpoint is an account-enumeration oracle by construction: it is
// unauthenticated and its answer differs per account. An address nobody holds
// must receive plausible parameters rather than a 404, and the same ones every
// time -- the difference between two requests would answer the question a
// single request must not.
func TestAnUnknownAddressGetsPlausibleParametersAndTheSameOnesTwice(t *testing.T) {
	base, _ := wire(t)

	code, body := askKDF(t, base, "nobody@example.com")
	if code != http.StatusOK {
		t.Fatalf("auth/kdf for an unknown address: status %d, body %s", code, body)
	}
	first := kdfParams(t, body)

	p, err := store.ParseKDFParams(first)
	if err != nil {
		t.Fatalf("the fake parameters do not parse: %v", err)
	}
	if err := p.Validate(); err != nil {
		t.Errorf("the fake parameters are outside the bounds a client will derive under: %v", err)
	}

	_, body = askKDF(t, base, "nobody@example.com")
	if second := kdfParams(t, body); second != first {
		t.Errorf("two requests for one unknown address answered differently:\n%s\n%s", first, second)
	}

	_, body = askKDF(t, base, "somebodyelse@example.com")
	if other := kdfParams(t, body); other == first {
		t.Error("two different unknown addresses got the same salt; the fake is not per-address")
	}
}

// An account that exists but has published no key material is in the same
// position as an address nobody holds, and must be answered the same way. It
// is the case a naive handler gets wrong -- the row is there, so it is
// tempting to answer "no parameters", which says the account exists.
func TestAnAccountWithNoPublishedKeysLooksLikeAnUnknownAddress(t *testing.T) {
	base, _ := wire(t)

	_, body := askKDF(t, base, "wire@example.com")
	real := kdfParams(t, body)
	if _, err := store.ParseKDFParams(real); err != nil {
		t.Fatalf("an account with no keys did not get plausible parameters: %q", real)
	}

	_, body = askKDF(t, base, "nobody@example.com")
	if unknown := kdfParams(t, body); unknown == real {
		t.Error("an account with no keys and an unknown address got identical answers by accident")
	}
}

func TestThePreLoginEndpointRefusesAnEmptyAddress(t *testing.T) {
	base, _ := wire(t)
	if code, body := askKDF(t, base, ""); code != http.StatusBadRequest {
		t.Errorf("auth/kdf with no address: status %d, body %s", code, body)
	}
}

// The address must not travel in a URL. It is the one identifier this endpoint
// takes and the request is unauthenticated, so a GET would put every address
// anyone asked about into the access log, the proxy log and any Referer a
// browser sent onward.
func TestThePreLoginEndpointIsNotAGET(t *testing.T) {
	base, _ := wire(t)
	if code, _ := call(t, "GET", base+kdfPath+"?email=wire@example.com", "", ""); code != http.StatusMethodNotAllowed {
		t.Errorf("GET auth/kdf: status %d, want 405", code)
	}
}

func TestServerInfoAdvertisesTheAccountKeySurface(t *testing.T) {
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
		if f == "account-keys" {
			return
		}
	}
	t.Errorf("server-info does not advertise account-keys: %v", out.Features)
}

// An operator reset drops the account back to password login, and takes
// nothing else with it.
//
// The parameters have to go. After crossover they govern how a login password
// becomes the authKey the hash column is made of, so leaving them beside a
// hash of a raw password is the exact failure this is all about: the client
// stretches the new password under the old salt and sends an authKey the
// stored hash was never made from, and the account cannot log in at all.
//
// The identity key and the recovery wraps must stay. They are not wrapped
// under the password — each recovery blob opens with a recovery code — so the
// user redeems one, recovers the identity key, re-wraps it under the new
// password and republishes. Deleting them here would turn a password reset
// into permanent loss of every library the account can read, which is not
// what an operator asked for and not something they could undo.
func TestAnOperatorResetDropsTheAccountBackToPasswordLogin(t *testing.T) {
	sqliteTestDB(t)
	acct := mintPasswordAccount(t, "reset@example.com", "correct horse battery staple")
	ctx := testCtx(t)

	km := mintKeyMaterial(t, acct.ID.String(), "correct horse battery staple")
	if err := account.SetKeys(ctx, acct.ID, km.keys()); err != nil {
		t.Fatalf("SetKeys: %v", err)
	}
	if _, err := account.ClientKDFParams(ctx, "reset@example.com"); err != nil {
		t.Fatalf("ClientKDFParams before the reset: %v", err)
	}

	if err := authmgr.SetAccountPassword(ctx, acct.ID, "an operator's choice"); err != nil {
		t.Fatalf("SetAccountPassword: %v", err)
	}

	if _, err := account.ClientKDFParams(ctx, "reset@example.com"); !errors.Is(err, account.ErrNotFound) {
		t.Errorf("client_kdf_params after an operator reset: err = %v, want ErrNotFound", err)
	}

	// And the material the user can still recover from is untouched.
	got, err := account.GetKeys(ctx, acct.ID)
	if err != nil {
		t.Fatalf("the reset took the identity key with it: %v", err)
	}
	if string(got.PublicKey) != string(km.Public) {
		t.Error("the reset replaced the published public key")
	}
	if len(got.Recovery) != len(km.keys().Recovery) {
		t.Errorf("the reset left %d recovery wraps, want %d", len(got.Recovery), len(km.keys().Recovery))
	}
}

// The crossover write puts the hash and the parameters down together.
//
// Separately is what makes an account unopenable: a hash written without its
// parameters describes a secret the parameters no longer produce, and the
// window between two statements is a window a crash can land in. One
// transaction, or neither.
func TestTheCrossoverWritesTheHashAndParametersTogether(t *testing.T) {
	sqliteTestDB(t)
	acct := mintPasswordAccount(t, "cross@example.com", "correct horse battery staple")
	ctx := testCtx(t)

	km := mintKeyMaterial(t, acct.ID.String(), "a new password entirely")
	hash, err := authmgr.HashAuthKey("an authKey standing in for 32 random bytes")
	if err != nil {
		t.Fatalf("HashAuthKey: %v", err)
	}
	if err := account.SetPasswordAndKDFParams(ctx, acct.ID, hash, km.Params.String()); err != nil {
		t.Fatalf("SetPasswordAndKDFParams: %v", err)
	}

	got, err := account.ClientKDFParams(ctx, "cross@example.com")
	if err != nil {
		t.Fatalf("ClientKDFParams after the crossover: %v", err)
	}
	if got != km.Params.String() {
		t.Errorf("client_kdf_params = %q, want %q", got, km.Params.String())
	}

	// And the hash that landed beside them is the one that was handed over,
	// not a re-hash of something else.
	_, stored, err := account.PasswordHash(ctx, "cross@example.com")
	if err != nil {
		t.Fatalf("PasswordHash: %v", err)
	}
	if stored != hash {
		t.Errorf("stored hash = %q, want %q", stored, hash)
	}
}
