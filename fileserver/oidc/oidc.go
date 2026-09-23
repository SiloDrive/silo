// Package oidc is Silo's half of the RFC 8628 device grant: Silo is the OAuth
// client, and the device.
//
// docs/auth.md § OIDC holds the design and docs/plans/oidc.md the build order.
// The shape, in one breath: a client asks Silo to start a login, Silo asks the
// IdP, the IdP's code and URL go back through Silo for the client to show, the
// person approves at the IdP, and Silo verifies the ID token that results. The
// client never speaks to the IdP and the IdP is consulted once per enrolment,
// never per request -- what comes out of here is a verified set of claims, and
// turning those into an account and a Credential is somebody else's job.
//
// This package holds the conversation with the IdP and nothing about accounts,
// so that everything that can go wrong on the wire is tested against a fake
// IdP (internal/oidctest) with no database in sight.
package oidc

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/SiloDrive/silo/fileserver/option"
	gooidc "github.com/coreos/go-oidc/v3/oidc"
	log "github.com/sirupsen/logrus"
	"golang.org/x/oauth2"
)

// Policy is who an IdP login may become. See docs/plans/oidc.md § Who gets an
// account for the table; the binding that enforces it lives with the accounts.
type Policy string

const (
	// PolicyLink matches linked identities and verified addresses, and
	// creates nothing: invites stay the way in. The default.
	PolicyLink Policy = "link"
	// PolicyCreate is PolicyLink plus an account for anybody else the IdP
	// signs in.
	PolicyCreate Policy = "create"
	// PolicyIsolated creates an account per identity and never matches by
	// address, for an IdP whose address claims cannot be believed.
	PolicyIsolated Policy = "isolated"
)

// Config is a validated OIDC configuration.
type Config struct {
	Issuer         string
	ClientID       string
	ClientSecret   string
	Policy         Policy
	AllowedDomains []string
}

// Scopes are what Silo asks for, and offline_access is deliberately not one:
// once the ID token is verified Silo never acts for the person again, so a
// refresh token would be one more long-lived secret to hold and lose.
var Scopes = []string{gooidc.ScopeOpenID, "email", "profile"}

// ParseConfig validates what the options loader read. It returns nil and no
// error when OIDC is not configured at all.
//
// Everything it refuses, it refuses at startup rather than at the first login,
// because each is a configuration that cannot work or should not: a server
// that started and then failed every IdP login would look like an IdP outage.
func ParseConfig(o option.OIDCOptions) (*Config, error) {
	if !o.Configured() {
		return nil, nil
	}

	// All three or none. Any one of them is somebody configuring OIDC, and
	// half a configuration is a typo whose symptom would otherwise be a 401
	// from the IdP on somebody's first login.
	var missing []string
	for _, f := range []struct{ name, v string }{
		{"issuer", o.Issuer}, {"client_id", o.ClientID}, {"client_secret", o.ClientSecret},
	} {
		if f.v == "" {
			missing = append(missing, f.name)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("[oidc] is partly configured: %s missing", strings.Join(missing, ", "))
	}

	if err := checkIssuerURL(o.Issuer); err != nil {
		return nil, err
	}

	c := &Config{Issuer: o.Issuer, ClientID: o.ClientID, ClientSecret: o.ClientSecret}

	switch p := Policy(strings.ToLower(strings.TrimSpace(o.Accounts))); p {
	case "":
		c.Policy = PolicyLink
	case PolicyLink, PolicyCreate, PolicyIsolated:
		c.Policy = p
	default:
		return nil, fmt.Errorf("[oidc] accounts = %q: want link, create or isolated", o.Accounts)
	}

	domains, err := parseDomains(o.AllowedDomains)
	if err != nil {
		return nil, fmt.Errorf("[oidc] allowed_domains: %v", err)
	}
	c.AllowedDomains = domains

	// A public issuer signs in anybody who has an account there, which for
	// Google is anybody on earth. A policy that creates accounts, pointed at
	// one with no domain list, is a public sign-up server -- a thing somebody
	// might want, and a thing they should have to type rather than arrive at.
	if c.Policy != PolicyLink && len(c.AllowedDomains) == 0 && isPublicIssuer(c.Issuer) {
		return nil, fmt.Errorf("[oidc] accounts = %s against %s, which anyone can sign in to, "+
			"needs allowed_domains; without one, every account there gets a Silo account", c.Policy, c.Issuer)
	}
	return c, nil
}

// checkIssuerURL refuses plain HTTP except to loopback. The issuer URL is where
// the keys that sign every ID token come from, so fetching them in the clear
// lets anybody on the path mint an identity.
func checkIssuerURL(issuer string) error {
	u, err := url.Parse(issuer)
	if err != nil || u.Host == "" {
		return fmt.Errorf("[oidc] issuer %q is not a URL", issuer)
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		host := u.Hostname()
		if ip := net.ParseIP(host); host == "localhost" || (ip != nil && ip.IsLoopback()) {
			return nil
		}
		return fmt.Errorf("[oidc] issuer %q is plain HTTP; the signing keys would be fetched unauthenticated", issuer)
	default:
		return fmt.Errorf("[oidc] issuer %q is not an http(s) URL", issuer)
	}
}

// isPublicIssuer names the issuers anybody can hold an account at. It is not a
// complete list and cannot be; it is the two that a person configuring "Sign in
// with Google" or "with Microsoft" will actually type.
//
// A single-tenant Entra issuer (login.microsoftonline.com/<tenant>/v2.0) is the
// organisation's own directory and is not on it. The multi-tenant spellings
// are, because any tenant's administrator can put any address on any user.
func isPublicIssuer(issuer string) bool {
	u, err := url.Parse(issuer)
	if err != nil {
		return false
	}
	switch strings.ToLower(u.Hostname()) {
	case "accounts.google.com":
		return true
	case "login.microsoftonline.com":
		first, _, _ := strings.Cut(strings.TrimPrefix(u.Path, "/"), "/")
		switch strings.ToLower(first) {
		case "common", "organizations", "consumers":
			return true
		}
	}
	return false
}

func parseDomains(s string) ([]string, error) {
	var out []string
	for _, d := range strings.Split(s, ",") {
		// "@example.com" means example.com: that is how a person writes
		// "addresses at".
		d = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(d), "@"))
		if d == "" {
			continue
		}
		if strings.ContainsAny(d, "@ /") || !strings.Contains(d, ".") {
			return nil, fmt.Errorf("%q is not a domain", d)
		}
		out = append(out, d)
	}
	return out, nil
}

// DomainAllowed reports whether an address is inside the allowed domains. No
// list means no restriction.
func (c *Config) DomainAllowed(email string) bool {
	if len(c.AllowedDomains) == 0 {
		return true
	}
	_, domain, ok := strings.Cut(strings.ToLower(strings.TrimSpace(email)), "@")
	if !ok {
		return false
	}
	for _, d := range c.AllowedDomains {
		if domain == d {
			return true
		}
	}
	return false
}

// Errors a device flow can end in, apart from the IdP being unreachable.
var (
	// ErrNotConfigured is every entry point on a server with no [oidc].
	ErrNotConfigured = errors.New("OIDC is not configured")
	// ErrUnavailable is the IdP not answering discovery. It is the IdP's
	// outage rather than Silo's, and is reported as such.
	ErrUnavailable = errors.New("the identity provider is unavailable")
	// ErrDenied is the person refusing at the IdP's device page.
	ErrDenied = errors.New("the sign-in was denied at the identity provider")
	// ErrExpired is the device code running out before anybody approved.
	ErrExpired = errors.New("the sign-in expired before it was approved")
	// ErrBadToken is an ID token that did not verify. Nothing about it is
	// believed, so nothing about it is reported past this error.
	ErrBadToken = errors.New("the identity provider's token did not verify")
)

// RetryDiscoveryAfter is how long a failed discovery is remembered before the
// next login tries again. Long enough that a burst of logins during an outage
// is one request to the IdP rather than one each; short enough that the IdP
// coming back is noticed by the person retrying.
var RetryDiscoveryAfter = 10 * time.Second

// httpClient is every request Silo makes to the IdP. The timeout is the point:
// a hung IdP must cost a login a 503 rather than a goroutine for ever.
var httpClient = &http.Client{Timeout: 15 * time.Second}

// Client is a configured IdP, discovered lazily.
//
// Discovery is not done at startup, deliberately. An IdP that is down when
// Silo boots must not take the file server down with it: password login and
// every credential already issued work without the IdP, and they are what
// people are using.
type Client struct {
	Config Config

	mu       sync.Mutex
	provider *gooidc.Provider
	oauth    *oauth2.Config
	verifier *gooidc.IDTokenVerifier
	lastErr  error
	lastTry  time.Time
}

// NewClient returns a client for a validated configuration. It makes no
// request.
func NewClient(c Config) *Client { return &Client{Config: c} }

// ready discovers the provider if it has not been, and reports ErrUnavailable
// while it cannot be.
func (c *Client) ready() (*oauth2.Config, *gooidc.IDTokenVerifier, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.provider != nil {
		return c.oauth, c.verifier, nil
	}
	if c.lastErr != nil && time.Since(c.lastTry) < RetryDiscoveryAfter {
		return nil, nil, c.lastErr
	}

	// context.Background and not the request's: go-oidc keeps this context
	// for the key set's later refreshes, and a request context would be
	// cancelled the moment the first login returned, leaving a provider that
	// can no longer fetch a rotated key. The client's timeout bounds it.
	ctx := gooidc.ClientContext(context.Background(), httpClient)
	c.lastTry = time.Now()
	p, err := gooidc.NewProvider(ctx, c.Config.Issuer)
	if err != nil {
		log.Warnf("OIDC discovery against %s failed: %v", c.Config.Issuer, err)
		c.lastErr = fmt.Errorf("%w: %v", ErrUnavailable, err)
		return nil, nil, c.lastErr
	}
	ep := p.Endpoint()
	if ep.DeviceAuthURL == "" {
		// Configured, reachable, and unable to do the one thing Silo needs.
		// Reported as unavailable rather than cached as fatal, because the
		// fix is on the IdP side and should not need a Silo restart.
		c.lastErr = fmt.Errorf("%w: %s advertises no device_authorization_endpoint", ErrUnavailable, c.Config.Issuer)
		log.Warn(c.lastErr)
		return nil, nil, c.lastErr
	}
	// client_secret_post, which is what auth.md settled on and what every
	// IdP that implements RFC 8628 accepts.
	ep.AuthStyle = oauth2.AuthStyleInParams

	c.provider = p
	c.oauth = &oauth2.Config{
		ClientID: c.Config.ClientID, ClientSecret: c.Config.ClientSecret,
		Endpoint: ep, Scopes: Scopes,
	}
	c.verifier = p.Verifier(&gooidc.Config{ClientID: c.Config.ClientID})
	c.lastErr = nil
	return c.oauth, c.verifier, nil
}

func (c *Client) ctx(ctx context.Context) context.Context {
	return context.WithValue(ctx, oauth2.HTTPClient, httpClient)
}

// Pending is a device flow the IdP has started: what to show the person, and
// the PKCE verifier Wait has to send back.
type Pending struct {
	*oauth2.DeviceAuthResponse
	verifier string
}

// Start asks the IdP for a device code.
//
// The client secret is sent here explicitly. golang.org/x/oauth2's DeviceAuth
// sends client_id alone, but a confidential client authenticates at the device
// endpoint as at the token endpoint (RFC 8628 § 3.1), and an IdP enforcing that
// refuses the request without it.
//
// So is a PKCE challenge, and not because it protects anything. PKCE guards an
// authorization code on its way back through a redirect; the device grant has
// no redirect, and the device code never leaves Silo. But Clinch requires it
// of a confidential client by default, device grant included, and an IdP that
// does not want it ignores it -- so sending it costs a SHA-256 and spares every
// such operator a setting to find.
func (c *Client) Start(ctx context.Context) (*Pending, error) {
	cfg, _, err := c.ready()
	if err != nil {
		return nil, err
	}
	verifier := oauth2.GenerateVerifier()
	da, err := cfg.DeviceAuth(c.ctx(ctx),
		oauth2.SetAuthURLParam("client_secret", c.Config.ClientSecret),
		oauth2.S256ChallengeOption(verifier))
	if err != nil {
		return nil, fmt.Errorf("%w: device authorization: %v", ErrUnavailable, err)
	}
	return &Pending{DeviceAuthResponse: da, verifier: verifier}, nil
}

// Claims is what a verified ID token says about the person. Nothing else of the
// IdP's is kept: its access and refresh tokens are discarded here.
type Claims struct {
	Issuer        string
	Subject       string
	Email         string
	EmailVerified bool
	Name          string
}

// Wait polls the IdP until the person approves, refuses, or the code expires,
// and returns the verified claims. It blocks for as long as that takes -- up to
// the code's expiry -- and is meant to run on a goroutine of its own.
//
// The polling itself is golang.org/x/oauth2's: interval, slow_down and
// authorization_pending are the subtle parts of RFC 8628, and getting them
// wrong is how a relying party gets rate-limited by its IdP.
func (c *Client) Wait(ctx context.Context, p *Pending) (*Claims, error) {
	cfg, verifier, err := c.ready()
	if err != nil {
		return nil, err
	}
	tok, err := cfg.DeviceAccessToken(c.ctx(ctx), p.DeviceAuthResponse, oauth2.VerifierOption(p.verifier))
	if err != nil {
		var re *oauth2.RetrieveError
		switch {
		case errors.As(err, &re) && re.ErrorCode == "access_denied":
			return nil, ErrDenied
		case errors.As(err, &re) && re.ErrorCode == "expired_token",
			errors.Is(err, context.DeadlineExceeded):
			return nil, ErrExpired
		}
		return nil, fmt.Errorf("%w: token: %v", ErrUnavailable, err)
	}
	return verify(ctx, verifier, tok)
}

// verify checks the ID token -- signature against the published keys, iss, aud
// equal to Silo's client id, exp and iat, which is what go-oidc's verifier does
// configured this way -- and, when the IdP sent one, at_hash against the access
// token it came with.
func verify(ctx context.Context, verifier *gooidc.IDTokenVerifier, tok *oauth2.Token) (*Claims, error) {
	raw, _ := tok.Extra("id_token").(string)
	if raw == "" {
		return nil, fmt.Errorf("%w: the token response carried no id_token", ErrBadToken)
	}
	idt, err := verifier.Verify(gooidc.ClientContext(ctx, httpClient), raw)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadToken, err)
	}
	if idt.AccessTokenHash != "" {
		if err := idt.VerifyAccessToken(tok.AccessToken); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrBadToken, err)
		}
	}

	var body struct {
		Email string `json:"email"`
		// any, because some IdPs send the string "true". Absent is false:
		// an IdP that does not say an address is verified has not said it.
		EmailVerified any    `json:"email_verified"`
		Name          string `json:"name"`
	}
	if err := idt.Claims(&body); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadToken, err)
	}
	return &Claims{
		Issuer:        idt.Issuer,
		Subject:       idt.Subject,
		Email:         body.Email,
		EmailVerified: body.EmailVerified == true || body.EmailVerified == "true",
		Name:          body.Name,
	}, nil
}

var (
	currentMu sync.RWMutex
	current   *Client
)

// Configure installs the server's OIDC configuration, or clears it when there
// is none. It validates and makes no request; see Client for why.
func Configure(o option.OIDCOptions) error {
	c, err := ParseConfig(o)
	if err != nil {
		return err
	}
	currentMu.Lock()
	defer currentMu.Unlock()
	if c == nil {
		current = nil
		return nil
	}
	current = NewClient(*c)
	log.Infof("OIDC login through %s (accounts: %s)", c.Issuer, c.Policy)
	return nil
}

// Current is the configured client, or nil.
func Current() *Client {
	currentMu.RLock()
	defer currentMu.RUnlock()
	return current
}

// Enabled reports whether OIDC is configured -- not whether the IdP is
// answering. server-info advertises it on this, so a client shows the right
// first screen during an outage and reports the outage when it arrives.
func Enabled() bool { return Current() != nil }
