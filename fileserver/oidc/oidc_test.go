package oidc

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/SiloDrive/silo/fileserver/option"
	"github.com/SiloDrive/silo/internal/oidctest"
)

func opts(idp *oidctest.IdP) option.OIDCOptions {
	return option.OIDCOptions{Issuer: idp.Issuer, ClientID: idp.ClientID, ClientSecret: idp.ClientSecret}
}

func full(issuer string) option.OIDCOptions {
	return option.OIDCOptions{Issuer: issuer, ClientID: "silo", ClientSecret: "secret"}
}

func TestNoOIDCConfigurationIsNotAnError(t *testing.T) {
	c, err := ParseConfig(option.OIDCOptions{})
	if c != nil || err != nil {
		t.Fatalf("ParseConfig(nothing) = %v, %v; want nil, nil", c, err)
	}
}

// Every configuration here is refused at startup, and each for a reason that
// would otherwise surface as a login failure somebody reports as an IdP
// outage -- or, for the last few, as accounts the operator did not mean to
// hand out.
func TestConfigurationsThatCannotWorkAreRefused(t *testing.T) {
	with := func(o option.OIDCOptions, edit func(*option.OIDCOptions)) option.OIDCOptions {
		edit(&o)
		return o
	}
	base := full("https://id.example.com")
	cases := map[string]struct {
		o    option.OIDCOptions
		want string
	}{
		"issuer alone":           {option.OIDCOptions{Issuer: "https://id.example.com"}, "client_id, client_secret missing"},
		"no secret":              {with(base, func(o *option.OIDCOptions) { o.ClientSecret = "" }), "client_secret missing"},
		"plain HTTP":             {full("http://id.example.com"), "plain HTTP"},
		"not a URL":              {full("id.example.com"), "not a URL"},
		"unknown policy":         {with(base, func(o *option.OIDCOptions) { o.Accounts = "everyone" }), "want link, create or isolated"},
		"an address as a domain": {with(base, func(o *option.OIDCOptions) { o.AllowedDomains = "alice@example.com" }), "not a domain"},
		"create against Google": {with(full("https://accounts.google.com"),
			func(o *option.OIDCOptions) { o.Accounts = "create" }), "needs allowed_domains"},
		"isolated against multi-tenant Entra": {with(full("https://login.microsoftonline.com/common/v2.0"),
			func(o *option.OIDCOptions) { o.Accounts = "isolated" }), "needs allowed_domains"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := ParseConfig(tc.o)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("ParseConfig = %v, want an error containing %q", err, tc.want)
			}
		})
	}
}

func TestConfigurationsThatCanWorkAreAccepted(t *testing.T) {
	cases := map[string]struct {
		o          option.OIDCOptions
		wantPolicy Policy
	}{
		"the default policy is link": {full("https://id.example.com"), PolicyLink},
		// Loopback is where a test IdP lives, and where a developer runs one.
		"plain HTTP to loopback": {full("http://127.0.0.1:9000"), PolicyLink},
		// Link creates nothing, so a public issuer is safe under it: Google's
		// verified addresses are proven, and nobody new gets in.
		"link against Google": {full("https://accounts.google.com"), PolicyLink},
		"create against Google with a domain list": {func() option.OIDCOptions {
			o := full("https://accounts.google.com")
			o.Accounts, o.AllowedDomains = "create", "example.com"
			return o
		}(), PolicyCreate},
		// A single-tenant issuer is the organisation's own directory.
		"create against single-tenant Entra": {func() option.OIDCOptions {
			o := full("https://login.microsoftonline.com/0d9b8a12-3456-7890-abcd-ef0123456789/v2.0")
			o.Accounts = "create"
			return o
		}(), PolicyCreate},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			c, err := ParseConfig(tc.o)
			if err != nil {
				t.Fatalf("ParseConfig: %v", err)
			}
			if c.Policy != tc.wantPolicy {
				t.Errorf("policy = %q, want %q", c.Policy, tc.wantPolicy)
			}
		})
	}
}

func TestDomainAllowed(t *testing.T) {
	c := &Config{AllowedDomains: []string{"example.com"}}
	for email, want := range map[string]bool{
		"a@example.com":      true,
		"A@Example.COM":      true,
		"a@sub.example.com":  false, // a subdomain is somebody else's mail server
		"a@example.com.evil": false,
		"a@notexample.com":   false,
		"example.com":        false,
		"":                   false,
	} {
		if got := c.DomainAllowed(email); got != want {
			t.Errorf("DomainAllowed(%q) = %v, want %v", email, got, want)
		}
	}
	if !(&Config{}).DomainAllowed("a@anywhere.org") {
		t.Error("no domain list restricted an address")
	}
}

func client(t *testing.T, idp *oidctest.IdP) *Client {
	t.Helper()
	c, err := ParseConfig(opts(idp))
	if err != nil {
		t.Fatal(err)
	}
	return NewClient(*c)
}

func ctx(t *testing.T) context.Context {
	t.Helper()
	c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return c
}

// login runs a flow to its end with the person approving as id.
func login(t *testing.T, idp *oidctest.IdP, c *Client, id oidctest.Identity) (*Claims, error) {
	t.Helper()
	da, err := c.Start(ctx(t))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	idp.Approve(t, da.UserCode, id)
	return c.Wait(ctx(t), da)
}

// The whole of this package's job, against an IdP that refuses a client that
// does not authenticate at the device endpoint -- so this passing is also the
// check that Start sends the secret.
func TestAnApprovedFlowYieldsVerifiedClaims(t *testing.T) {
	idp := oidctest.New(t)
	claims, err := login(t, idp, client(t, idp), oidctest.Identity{
		Subject: "sub-1", Email: "a@example.com", EmailVerified: oidctest.Verified, Name: "A Person",
	})
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	want := Claims{Issuer: idp.Issuer, Subject: "sub-1", Email: "a@example.com", EmailVerified: true, Name: "A Person"}
	if *claims != want {
		t.Errorf("claims = %+v, want %+v", *claims, want)
	}
}

// Absent is false. An IdP that does not say an address is verified has not
// said it, and Entra often does not.
func TestAnAbsentEmailVerifiedIsFalse(t *testing.T) {
	idp := oidctest.New(t)
	claims, err := login(t, idp, client(t, idp), oidctest.Identity{Subject: "sub-1", Email: "a@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if claims.EmailVerified {
		t.Error("an address with no email_verified claim read as verified")
	}
}

// Some IdPs send the claim as a string.
func TestAStringEmailVerifiedIsRead(t *testing.T) {
	idp := oidctest.New(t)
	claims, err := login(t, idp, client(t, idp), oidctest.Identity{
		Subject: "sub-1", Email: "a@example.com", Claims: map[string]any{"email_verified": "true"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !claims.EmailVerified {
		t.Error(`email_verified "true" read as unverified`)
	}
}

func TestATokenThatDoesNotVerifyIsRefused(t *testing.T) {
	for name, id := range map[string]oidctest.Identity{
		"signed by a key the IdP does not publish": {ForeignKey: true},
		"issued for another client":                {Claims: map[string]any{"aud": "someone-else"}},
		"issued by another issuer":                 {Claims: map[string]any{"iss": "https://elsewhere.example.com"}},
		"expired":                                  {Claims: map[string]any{"exp": time.Now().Add(-time.Hour).Unix()}},
	} {
		t.Run(name, func(t *testing.T) {
			idp := oidctest.New(t)
			id.Subject, id.Email, id.EmailVerified = "sub-1", "a@example.com", oidctest.Verified
			claims, err := login(t, idp, client(t, idp), id)
			if !errors.Is(err, ErrBadToken) {
				t.Fatalf("Wait = %+v, %v; want ErrBadToken", claims, err)
			}
		})
	}
}

func TestADeniedFlowIsErrDenied(t *testing.T) {
	idp := oidctest.New(t)
	c := client(t, idp)
	da, err := c.Start(ctx(t))
	if err != nil {
		t.Fatal(err)
	}
	idp.Deny(t, da.UserCode)
	if _, err := c.Wait(ctx(t), da); !errors.Is(err, ErrDenied) {
		t.Fatalf("Wait = %v, want ErrDenied", err)
	}
}

func TestAnUnapprovedFlowExpires(t *testing.T) {
	idp := oidctest.New(t)
	idp.ExpiresIn = 2 * time.Second
	c := client(t, idp)
	da, err := c.Start(ctx(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Wait(ctx(t), da); !errors.Is(err, ErrExpired) {
		t.Fatalf("Wait = %v, want ErrExpired", err)
	}
}

// An IdP that is down when Silo starts must not stop it starting, and must be
// picked up when it comes back without a restart.
func TestAnIdPThatIsDownAtStartIsFoundWhenItReturns(t *testing.T) {
	was := RetryDiscoveryAfter
	RetryDiscoveryAfter = 50 * time.Millisecond
	t.Cleanup(func() { RetryDiscoveryAfter = was })

	idp := oidctest.New(t)
	idp.SetDown(true)

	t.Cleanup(func() { _ = Configure(option.OIDCOptions{}) })
	if err := Configure(opts(idp)); err != nil {
		t.Fatalf("Configure against a down IdP: %v", err)
	}
	if !Enabled() {
		t.Fatal("OIDC is not enabled while the IdP is down, so a client would not offer it")
	}

	if _, err := Current().Start(ctx(t)); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Start against a down IdP = %v, want ErrUnavailable", err)
	}

	idp.SetDown(false)
	time.Sleep(100 * time.Millisecond)
	if _, err := Current().Start(ctx(t)); err != nil {
		t.Fatalf("Start after the IdP came back: %v", err)
	}
}

// A failure is remembered for RetryDiscoveryAfter, so a burst of logins during
// an outage is one request to the IdP rather than one each.
func TestAFailedDiscoveryIsNotRetriedAtOnce(t *testing.T) {
	idp := oidctest.New(t)
	idp.SetDown(true)
	c := client(t, idp)
	if _, err := c.Start(ctx(t)); !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}
	idp.SetDown(false)
	if _, err := c.Start(ctx(t)); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Start straight after a failure = %v; want the remembered ErrUnavailable", err)
	}
}

func TestConfigureClearsAPreviousConfiguration(t *testing.T) {
	idp := oidctest.New(t)
	if err := Configure(opts(idp)); err != nil {
		t.Fatal(err)
	}
	if err := Configure(option.OIDCOptions{}); err != nil {
		t.Fatal(err)
	}
	if Enabled() {
		t.Error("OIDC is still enabled after a configuration with none")
	}
}
