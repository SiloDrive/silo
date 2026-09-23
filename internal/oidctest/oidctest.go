// Package oidctest is an identity provider for tests: discovery, JWKS, the
// RFC 8628 device authorization endpoint and the token endpoint, signing ID
// tokens with a key it generated.
//
// It exists so that every OIDC test in the server runs against something that
// speaks the protocol over HTTP rather than against a stub of Silo's own
// interfaces -- the bugs worth catching are in the conversation between Silo
// and the IdP, and a stub would agree with whatever Silo said.
//
// Its standard is that it refuses what a real IdP refuses. A fake that is
// lenient makes every test that uses it pass for the wrong reason, so it
// demands client authentication on both endpoints (the device endpoint
// included, where golang.org/x/oauth2 sends none by default and Keycloak
// refuses a confidential client that does not), answers slow_down to a client
// polling faster than the interval, and expires what it said would expire.
// Its own tests pin each refusal.
//
// The person at the device page is the test: a flow stays pending until the
// test calls Approve or Deny with the user code the client was shown.
package oidctest

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
)

// Identity is who the person approving a flow is, as the ID token will say.
type Identity struct {
	Subject string
	Email   string
	// EmailVerified is a pointer because absent is a case of its own: Entra
	// often omits the claim, and a relying party must read absent as false
	// rather than as "not asked".
	EmailVerified *bool
	Name          string

	// Claims overrides or adds claims after the ones above are set, which is
	// how a test produces a token with the wrong aud, iss or exp. A nil value
	// deletes the claim.
	Claims map[string]any

	// ForeignKey signs the token with a key the JWKS does not publish.
	ForeignKey bool
}

// Verified and Unverified are the two values EmailVerified takes when present.
var (
	Verified   = ptr(true)
	Unverified = ptr(false)
)

func ptr(b bool) *bool { return &b }

// IdP is a running fake identity provider.
type IdP struct {
	// Issuer is the issuer URL, which is also the server's base URL.
	Issuer       string
	ClientID     string
	ClientSecret string

	// Interval is the polling interval the device endpoint reports, in
	// seconds. ExpiresIn is how long a device code lives. Both are read when a
	// flow starts, so a test sets them before the client asks.
	Interval  int
	ExpiresIn time.Duration

	// RequirePKCE refuses a device authorization with no S256 code_challenge,
	// and a token request whose code_verifier does not match it. Clinch does
	// this by default for confidential clients, device grant included.
	RequirePKCE bool

	srv     *httptest.Server
	key     *rsa.PrivateKey
	foreign *rsa.PrivateKey
	kid     string

	mu         sync.Mutex
	down       bool
	flows      map[string]*flow // by device code
	byUserCode map[string]*flow
	started    int
	tokenCalls int
}

type flow struct {
	challenge  string
	deviceCode string
	userCode   string
	expires    time.Time
	interval   time.Duration
	lastPoll   time.Time
	denied     bool
	approved   *Identity
	redeemed   bool
}

// New starts an IdP for the life of the test.
func New(t testing.TB) *IdP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	idp := &IdP{
		ClientID:     "silo",
		ClientSecret: "the client secret",
		Interval:     1,
		ExpiresIn:    10 * time.Minute,
		key:          key,
		foreign:      foreign,
		kid:          "test-key",
		flows:        map[string]*flow{},
		byUserCode:   map[string]*flow{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", idp.discovery)
	mux.HandleFunc("GET /jwks", idp.jwks)
	mux.HandleFunc("POST /device", idp.deviceAuthorization)
	mux.HandleFunc("POST /token", idp.token)
	idp.srv = httptest.NewServer(mux)
	idp.Issuer = idp.srv.URL
	t.Cleanup(idp.srv.Close)
	return idp
}

// SetDown makes every endpoint answer 503, for the IdP outage a server must
// survive.
func (idp *IdP) SetDown(down bool) {
	idp.mu.Lock()
	defer idp.mu.Unlock()
	idp.down = down
}

// Approve is the person typing userCode at the device page and approving as
// id. It fails the test if no flow is showing that code.
func (idp *IdP) Approve(t testing.TB, userCode string, id Identity) {
	t.Helper()
	idp.mu.Lock()
	defer idp.mu.Unlock()
	f, ok := idp.byUserCode[userCode]
	if !ok {
		t.Fatalf("oidctest: no flow is showing user code %q", userCode)
	}
	f.approved = &id
}

// Deny is the person refusing at the device page.
func (idp *IdP) Deny(t testing.TB, userCode string) {
	t.Helper()
	idp.mu.Lock()
	defer idp.mu.Unlock()
	f, ok := idp.byUserCode[userCode]
	if !ok {
		t.Fatalf("oidctest: no flow is showing user code %q", userCode)
	}
	f.denied = true
}

// Pending returns the user codes of flows nobody has approved or denied yet,
// for a test that drives a client which shows the code rather than returning
// it.
func (idp *IdP) Pending() []string {
	idp.mu.Lock()
	defer idp.mu.Unlock()
	var out []string
	for code, f := range idp.byUserCode {
		if f.approved == nil && !f.denied {
			out = append(out, code)
		}
	}
	return out
}

// Started is how many device flows have been started, so a test can check a
// refusal happened before the IdP was asked anything.
func (idp *IdP) Started() int {
	idp.mu.Lock()
	defer idp.mu.Unlock()
	return idp.started
}

// TokenCalls is how many requests the token endpoint has answered, so a test
// can check a poller stopped.
func (idp *IdP) TokenCalls() int {
	idp.mu.Lock()
	defer idp.mu.Unlock()
	return idp.tokenCalls
}

func (idp *IdP) isDown(w http.ResponseWriter) bool {
	idp.mu.Lock()
	defer idp.mu.Unlock()
	if idp.down {
		http.Error(w, "down", http.StatusServiceUnavailable)
		return true
	}
	return false
}

func (idp *IdP) discovery(w http.ResponseWriter, _ *http.Request) {
	if idp.isDown(w) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"issuer":                                idp.Issuer,
		"authorization_endpoint":                idp.Issuer + "/authorize",
		"device_authorization_endpoint":         idp.Issuer + "/device",
		"token_endpoint":                        idp.Issuer + "/token",
		"jwks_uri":                              idp.Issuer + "/jwks",
		"response_types_supported":              []string{"code"},
		"subject_types_supported":               []string{"public"},
		"id_token_signing_alg_values_supported": []string{"RS256"},
		"token_endpoint_auth_methods_supported": []string{"client_secret_post"},
		"grant_types_supported": []string{
			"authorization_code", "urn:ietf:params:oauth:grant-type:device_code"},
	})
}

func (idp *IdP) jwks(w http.ResponseWriter, _ *http.Request) {
	if idp.isDown(w) {
		return
	}
	writeJSON(w, http.StatusOK, jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{
		Key: &idp.key.PublicKey, KeyID: idp.kid, Algorithm: string(jose.RS256), Use: "sig",
	}}})
}

// clientAuthenticated checks client_secret_post, the one method discovery
// advertises. HTTP Basic is refused rather than accepted as a courtesy: a
// server whose client sends the wrong one should find out here.
func (idp *IdP) clientAuthenticated(w http.ResponseWriter, r *http.Request) bool {
	if r.PostFormValue("client_id") != idp.ClientID || r.PostFormValue("client_secret") != idp.ClientSecret {
		oauthError(w, http.StatusUnauthorized, "invalid_client")
		return false
	}
	return true
}

func (idp *IdP) deviceAuthorization(w http.ResponseWriter, r *http.Request) {
	if idp.isDown(w) || !idp.clientAuthenticated(w, r) {
		return
	}
	if !hasScope(r.PostFormValue("scope"), "openid") {
		oauthError(w, http.StatusBadRequest, "invalid_scope")
		return
	}

	challenge := r.PostFormValue("code_challenge")
	if challenge != "" && r.PostFormValue("code_challenge_method") != "S256" {
		oauthError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	idp.mu.Lock()
	defer idp.mu.Unlock()
	if idp.RequirePKCE && challenge == "" {
		oauthError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	idp.started++
	f := &flow{
		challenge:  challenge,
		deviceCode: randomHex(16),
		userCode:   strings.ToUpper(randomHex(2) + "-" + randomHex(2)),
		expires:    time.Now().Add(idp.ExpiresIn),
		interval:   time.Duration(idp.Interval) * time.Second,
	}
	idp.flows[f.deviceCode] = f
	idp.byUserCode[f.userCode] = f

	writeJSON(w, http.StatusOK, map[string]any{
		"device_code":               f.deviceCode,
		"user_code":                 f.userCode,
		"verification_uri":          idp.Issuer + "/activate",
		"verification_uri_complete": idp.Issuer + "/activate?user_code=" + f.userCode,
		"expires_in":                int(idp.ExpiresIn.Seconds()),
		"interval":                  idp.Interval,
	})
}

func (idp *IdP) token(w http.ResponseWriter, r *http.Request) {
	if idp.isDown(w) || !idp.clientAuthenticated(w, r) {
		return
	}
	if r.PostFormValue("grant_type") != "urn:ietf:params:oauth:grant-type:device_code" {
		oauthError(w, http.StatusBadRequest, "unsupported_grant_type")
		return
	}

	idp.mu.Lock()
	idp.tokenCalls++
	f, ok := idp.flows[r.PostFormValue("device_code")]
	if !ok {
		idp.mu.Unlock()
		oauthError(w, http.StatusBadRequest, "invalid_grant")
		return
	}
	if f.challenge != "" && s256(r.PostFormValue("code_verifier")) != f.challenge {
		idp.mu.Unlock()
		oauthError(w, http.StatusBadRequest, "invalid_grant")
		return
	}
	now := time.Now()
	// The order of these checks is RFC 8628 § 3.5's, and slow_down comes
	// before pending: a client that polls too fast learns so whether or not
	// the person has acted yet.
	switch {
	case now.After(f.expires):
		idp.mu.Unlock()
		oauthError(w, http.StatusBadRequest, "expired_token")
		return
	case f.redeemed:
		idp.mu.Unlock()
		oauthError(w, http.StatusBadRequest, "invalid_grant")
		return
	case f.denied:
		idp.mu.Unlock()
		oauthError(w, http.StatusBadRequest, "access_denied")
		return
	}
	// A little slack under the interval, so a ticker that fires a few
	// milliseconds early is not punished for scheduling noise.
	tooSoon := !f.lastPoll.IsZero() && now.Sub(f.lastPoll) < f.interval-100*time.Millisecond
	f.lastPoll = now
	if tooSoon {
		idp.mu.Unlock()
		oauthError(w, http.StatusBadRequest, "slow_down")
		return
	}
	if f.approved == nil {
		idp.mu.Unlock()
		oauthError(w, http.StatusBadRequest, "authorization_pending")
		return
	}
	f.redeemed = true
	id := *f.approved
	idp.mu.Unlock()

	idToken, err := idp.sign(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"access_token": randomHex(16),
		"token_type":   "Bearer",
		"expires_in":   300,
		"id_token":     idToken,
	})
}

func (idp *IdP) sign(id Identity) (string, error) {
	now := time.Now()
	claims := map[string]any{
		"iss": idp.Issuer,
		"sub": id.Subject,
		"aud": idp.ClientID,
		"iat": now.Unix(),
		"exp": now.Add(5 * time.Minute).Unix(),
	}
	if id.Email != "" {
		claims["email"] = id.Email
	}
	if id.EmailVerified != nil {
		claims["email_verified"] = *id.EmailVerified
	}
	if id.Name != "" {
		claims["name"] = id.Name
	}
	for k, v := range id.Claims {
		if v == nil {
			delete(claims, k)
			continue
		}
		claims[k] = v
	}

	key := idp.key
	if id.ForeignKey {
		key = idp.foreign
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", idp.kid))
	if err != nil {
		return "", err
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	jws, err := signer.Sign(payload)
	if err != nil {
		return "", err
	}
	return jws.CompactSerialize()
}

func s256(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func hasScope(scopes, want string) bool {
	for _, s := range strings.Fields(scopes) {
		if s == want {
			return true
		}
	}
	return false
}

func oauthError(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, map[string]string{"error": code})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("oidctest: reading randomness: %v", err))
	}
	return hex.EncodeToString(b)
}
