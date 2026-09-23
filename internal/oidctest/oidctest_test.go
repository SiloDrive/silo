package oidctest

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// client is the relying party these tests play: the real libraries, configured
// the way Silo will configure them, so that what passes here is what Silo will
// meet.
func client(t *testing.T, idp *IdP) (*oidc.Provider, *oauth2.Config) {
	t.Helper()
	p, err := oidc.NewProvider(context.Background(), idp.Issuer)
	if err != nil {
		t.Fatalf("discovery: %v", err)
	}
	ep := p.Endpoint()
	ep.AuthStyle = oauth2.AuthStyleInParams
	return p, &oauth2.Config{
		ClientID: idp.ClientID, ClientSecret: idp.ClientSecret,
		Endpoint: ep, Scopes: []string{oidc.ScopeOpenID, "email", "profile"},
	}
}

func withSecret(idp *IdP) oauth2.AuthCodeOption {
	return oauth2.SetAuthURLParam("client_secret", idp.ClientSecret)
}

func ctx(t *testing.T) context.Context {
	t.Helper()
	c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return c
}

// idToken runs one device flow to completion, approving as id, and returns the
// raw ID token.
func idToken(t *testing.T, idp *IdP, cfg *oauth2.Config, id Identity) string {
	t.Helper()
	da, err := cfg.DeviceAuth(ctx(t), withSecret(idp))
	if err != nil {
		t.Fatalf("device authorization: %v", err)
	}
	idp.Approve(t, da.UserCode, id)
	tok, err := cfg.DeviceAccessToken(ctx(t), da)
	if err != nil {
		t.Fatalf("token: %v", err)
	}
	raw, _ := tok.Extra("id_token").(string)
	if raw == "" {
		t.Fatal("the token response carried no id_token")
	}
	return raw
}

// The whole conversation, through the libraries Silo uses: discovery names a
// device endpoint, the flow completes once the person approves, and the ID token
// verifies against the published keys and says who approved.
func TestADeviceFlowCompletesAndItsTokenVerifies(t *testing.T) {
	idp := New(t)
	p, cfg := client(t, idp)
	if cfg.Endpoint.DeviceAuthURL == "" {
		t.Fatal("discovery named no device authorization endpoint")
	}

	raw := idToken(t, idp, cfg, Identity{Subject: "sub-1", Email: "a@example.com", EmailVerified: Verified})
	tok, err := p.Verifier(&oidc.Config{ClientID: idp.ClientID}).Verify(ctx(t), raw)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	var claims struct {
		Email         string `json:"email"`
		EmailVerified *bool  `json:"email_verified"`
	}
	if err := tok.Claims(&claims); err != nil {
		t.Fatal(err)
	}
	if tok.Subject != "sub-1" || claims.Email != "a@example.com" || claims.EmailVerified == nil || !*claims.EmailVerified {
		t.Errorf("token says sub=%q email=%q verified=%v", tok.Subject, claims.Email, claims.EmailVerified)
	}
}

// golang.org/x/oauth2's DeviceAuth sends client_id and nothing else. A
// confidential client must authenticate there as at the token endpoint -- RFC
// 8628 § 3.1 -- and Keycloak refuses one that does not. The fake refusing it is
// what makes a Silo that forgot the secret fail a test instead of a deployment.
func TestTheDeviceEndpointRefusesAClientThatDoesNotAuthenticate(t *testing.T) {
	idp := New(t)
	_, cfg := client(t, idp)

	_, err := cfg.DeviceAuth(ctx(t))
	var re *oauth2.RetrieveError
	if !errors.As(err, &re) || re.ErrorCode != "invalid_client" {
		t.Fatalf("device authorization without the secret = %v, want invalid_client", err)
	}
	if idp.Started() != 0 {
		t.Error("a refused request still started a flow")
	}
}

func TestADeniedFlowEndsInAccessDenied(t *testing.T) {
	idp := New(t)
	_, cfg := client(t, idp)
	da, err := cfg.DeviceAuth(ctx(t), withSecret(idp))
	if err != nil {
		t.Fatal(err)
	}
	idp.Deny(t, da.UserCode)

	_, err = cfg.DeviceAccessToken(ctx(t), da)
	var re *oauth2.RetrieveError
	if !errors.As(err, &re) || re.ErrorCode != "access_denied" {
		t.Fatalf("a denied flow = %v, want access_denied", err)
	}
}

// Each of these is a token a verifier must refuse, and the fake has to be able
// to produce every one or the verifier's refusals go untested.
func TestTheFakeCanProduceEachTokenAVerifierMustRefuse(t *testing.T) {
	cases := map[string]Identity{
		"signed by a key the JWKS does not publish": {ForeignKey: true},
		"issued for another client":                 {Claims: map[string]any{"aud": "someone-else"}},
		"issued by another issuer":                  {Claims: map[string]any{"iss": "https://elsewhere.example.com"}},
		"expired":                                   {Claims: map[string]any{"exp": time.Now().Add(-time.Hour).Unix()}},
	}
	for name, id := range cases {
		t.Run(name, func(t *testing.T) {
			idp := New(t)
			p, cfg := client(t, idp)
			id.Subject = "sub-1"
			raw := idToken(t, idp, cfg, id)
			if _, err := p.Verifier(&oidc.Config{ClientID: idp.ClientID}).Verify(ctx(t), raw); err == nil {
				t.Fatal("the verifier accepted it")
			}
		})
	}
}

// Absent and false are different claims, and a relying party must see the
// difference to treat them the same on purpose.
func TestEmailVerifiedCanBeAbsent(t *testing.T) {
	idp := New(t)
	p, cfg := client(t, idp)
	raw := idToken(t, idp, cfg, Identity{Subject: "sub-1", Email: "a@example.com"})
	tok, err := p.Verifier(&oidc.Config{ClientID: idp.ClientID}).Verify(ctx(t), raw)
	if err != nil {
		t.Fatal(err)
	}
	var claims map[string]any
	if err := tok.Claims(&claims); err != nil {
		t.Fatal(err)
	}
	if _, present := claims["email_verified"]; present {
		t.Error("email_verified is present on a token whose identity did not set it")
	}
}

// pollRaw asks the token endpoint once, by hand, so a test can poll faster
// than any well-behaved library would.
func pollRaw(t *testing.T, idp *IdP, deviceCode string) string {
	t.Helper()
	resp, err := http.PostForm(idp.Issuer+"/token", url.Values{
		"grant_type":    {"urn:ietf:params:oauth:grant-type:device_code"},
		"device_code":   {deviceCode},
		"client_id":     {idp.ClientID},
		"client_secret": {idp.ClientSecret},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var body struct {
		Error string `json:"error"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&body)
	return body.Error
}

func TestPollingFasterThanTheIntervalIsToldToSlowDown(t *testing.T) {
	idp := New(t)
	idp.Interval = 5
	_, cfg := client(t, idp)
	da, err := cfg.DeviceAuth(ctx(t), withSecret(idp))
	if err != nil {
		t.Fatal(err)
	}
	if got := pollRaw(t, idp, da.DeviceCode); got != "authorization_pending" {
		t.Fatalf("first poll = %q, want authorization_pending", got)
	}
	if got := pollRaw(t, idp, da.DeviceCode); got != "slow_down" {
		t.Fatalf("an immediate second poll = %q, want slow_down", got)
	}
}

func TestADeviceCodeExpires(t *testing.T) {
	idp := New(t)
	idp.ExpiresIn = 50 * time.Millisecond
	_, cfg := client(t, idp)
	da, err := cfg.DeviceAuth(ctx(t), withSecret(idp))
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if got := pollRaw(t, idp, da.DeviceCode); got != "expired_token" {
		t.Fatalf("polling an expired code = %q, want expired_token", got)
	}
}

// One ID token per approval: a device code already exchanged is spent.
func TestADeviceCodeIsExchangedOnce(t *testing.T) {
	idp := New(t)
	_, cfg := client(t, idp)
	da, err := cfg.DeviceAuth(ctx(t), withSecret(idp))
	if err != nil {
		t.Fatal(err)
	}
	idp.Approve(t, da.UserCode, Identity{Subject: "sub-1"})
	if _, err := cfg.DeviceAccessToken(ctx(t), da); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Duration(idp.Interval) * time.Second)
	if got := pollRaw(t, idp, da.DeviceCode); got != "invalid_grant" {
		t.Fatalf("exchanging a spent code = %q, want invalid_grant", got)
	}
}

func TestADownIdPFailsDiscovery(t *testing.T) {
	idp := New(t)
	idp.SetDown(true)
	if _, err := oidc.NewProvider(context.Background(), idp.Issuer); err == nil {
		t.Fatal("discovery succeeded against an IdP that is down")
	} else if !strings.Contains(err.Error(), "503") {
		t.Logf("discovery failed as wanted, with: %v", err)
	}
}
