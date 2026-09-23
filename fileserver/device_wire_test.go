package silod

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/SiloDrive/silo/fileserver/oidc"
	"github.com/SiloDrive/silo/fileserver/option"
	"github.com/SiloDrive/silo/internal/oidctest"
)

// Signing in through the identity provider, over the wire, against the fake
// IdP: the client's two endpoints, the person approving at the IdP in between,
// and the credential that comes out working like any other.

// withOIDC configures this server to sign in through idp under a policy.
func withOIDC(t *testing.T, idp *oidctest.IdP, accounts string) {
	t.Helper()
	t.Cleanup(func() { _ = oidc.Configure(option.OIDCOptions{}) })
	if err := oidc.Configure(option.OIDCOptions{
		Issuer: idp.Issuer, ClientID: idp.ClientID, ClientSecret: idp.ClientSecret, Accounts: accounts,
	}); err != nil {
		t.Fatalf("configuring OIDC: %v", err)
	}
}

type deviceStart struct {
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	PollToken               string `json:"poll_token"`
	Interval                int    `json:"interval"`
	ExpiresIn               int    `json:"expires_in"`
}

func startDevice(t *testing.T, base, body string) (int, deviceStart, string) {
	t.Helper()
	code, raw := post(t, base+"/api/silo/v1/auth/device", body)
	var out deviceStart
	if code == http.StatusOK {
		decodeInto(t, raw, &out)
	}
	return code, out, raw
}

func pollDevice(t *testing.T, base, pollToken string) (int, string) {
	t.Helper()
	b, _ := json.Marshal(map[string]string{"poll_token": pollToken})
	return post(t, base+"/api/silo/v1/auth/device/poll", string(b))
}

// pollUntilDone polls as a well-behaved client does, at the interval, until the
// answer is anything but "still waiting". A 429 is "still waiting, and slower":
// a caller that has just polled for itself meets one on the first iteration.
func pollUntilDone(t *testing.T, base string, s deviceStart) (int, string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		code, body := pollDevice(t, base, s.PollToken)
		if code != http.StatusAccepted && code != http.StatusTooManyRequests {
			return code, body
		}
		time.Sleep(time.Duration(s.Interval) * time.Second)
	}
	t.Fatal("the sign-in was still pending after 30 seconds")
	return 0, ""
}

type deviceEnrolled struct {
	Credential string `json:"credential"`
	ExpiresAt  int64  `json:"expires_at"`
	Email      string `json:"email"`
}

const laptop = `{"kind":"device","client_name":"SiloDrive (test laptop)"}`

// The whole thing: start, show the code, the person approves at the IdP, the
// client collects a credential, and the credential is an ordinary one -- it
// lists libraries, carries the name the client gave it, and is listed among
// the account's credentials.
func TestSigningInThroughTheIdentityProvider(t *testing.T) {
	base, _ := wire(t)
	idp := oidctest.New(t)
	withOIDC(t, idp, "create")

	code, s, raw := startDevice(t, base, laptop)
	if code != http.StatusOK {
		t.Fatalf("start = %d, %s", code, raw)
	}
	if s.UserCode == "" || s.PollToken == "" || !strings.HasPrefix(s.VerificationURI, idp.Issuer) {
		t.Fatalf("start answered %+v", s)
	}

	// Before anybody approves, the answer is "still waiting".
	if code, body := pollDevice(t, base, s.PollToken); code != http.StatusAccepted {
		t.Fatalf("poll before approval = %d, %s; want 202", code, body)
	}

	idp.Approve(t, s.UserCode, oidctest.Identity{
		Subject: "new-sub", Email: "newcomer@example.com", EmailVerified: oidctest.Verified,
	})
	code, body := pollUntilDone(t, base, s)
	if code != http.StatusOK {
		t.Fatalf("poll after approval = %d, %s", code, body)
	}
	var got deviceEnrolled
	decodeInto(t, body, &got)
	if got.Credential == "" || got.Email != "newcomer@example.com" || got.ExpiresAt == 0 {
		t.Fatalf("collected %+v", got)
	}

	if code, body := call(t, "GET", base+"/api/silo/v1/libraries", got.Credential, ""); code != http.StatusOK {
		t.Errorf("listing libraries with the collected credential = %d, %s", code, body)
	}
	code, body = call(t, "GET", base+"/api/silo/v1/account/credentials", got.Credential, "")
	if code != http.StatusOK || !strings.Contains(body, "SiloDrive (test laptop)") || !strings.Contains(body, `"device"`) {
		t.Errorf("the account's credentials = %d, %s; want the device credential labelled as the client asked", code, body)
	}
}

// Collected once. The flow is gone the moment the credential is handed out, so
// a poll token that leaked afterwards is worth nothing.
func TestTheCredentialIsCollectedOnce(t *testing.T) {
	base, _ := wire(t)
	idp := oidctest.New(t)
	withOIDC(t, idp, "create")

	_, s, _ := startDevice(t, base, laptop)
	idp.Approve(t, s.UserCode, oidctest.Identity{Subject: "sub", Email: "a@example.com", EmailVerified: oidctest.Verified})
	if code, body := pollUntilDone(t, base, s); code != http.StatusOK {
		t.Fatalf("first collection = %d, %s", code, body)
	}
	if code, body := pollDevice(t, base, s.PollToken); code != http.StatusGone {
		t.Fatalf("second collection = %d, %s; want 410", code, body)
	}
}

func TestPollingFasterThanTheIntervalIsRefusedWithRetryAfter(t *testing.T) {
	base, _ := wire(t)
	idp := oidctest.New(t)
	idp.Interval = 5
	withOIDC(t, idp, "create")

	_, s, _ := startDevice(t, base, laptop)
	if code, _ := pollDevice(t, base, s.PollToken); code != http.StatusAccepted {
		t.Fatalf("first poll = %d, want 202", code)
	}
	b, _ := json.Marshal(map[string]string{"poll_token": s.PollToken})
	resp, err := http.Post(base+"/api/silo/v1/auth/device/poll", "application/json", strings.NewReader(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusTooManyRequests || resp.Header.Get("Retry-After") != "5" {
		t.Fatalf("an immediate second poll = %d, Retry-After %q; want 429 and 5",
			resp.StatusCode, resp.Header.Get("Retry-After"))
	}
}

func TestADenialAtTheIdPIsRefused(t *testing.T) {
	base, _ := wire(t)
	idp := oidctest.New(t)
	withOIDC(t, idp, "create")

	_, s, _ := startDevice(t, base, laptop)
	idp.Deny(t, s.UserCode)
	code, body := pollUntilDone(t, base, s)
	if code != http.StatusForbidden || !strings.Contains(body, "denied") {
		t.Fatalf("poll after denial = %d, %s; want 403 saying it was denied", code, body)
	}
}

// The binding's refusal reaches the person in words they can act on, because
// the poll is the only place they will hear it.
func TestTheBindingsRefusalReachesThePerson(t *testing.T) {
	base, _ := wire(t)
	idp := oidctest.New(t)
	withOIDC(t, idp, "link")

	_, s, _ := startDevice(t, base, laptop)
	idp.Approve(t, s.UserCode, oidctest.Identity{Subject: "sub", Email: "stranger@example.com", EmailVerified: oidctest.Verified})
	code, body := pollUntilDone(t, base, s)
	if code != http.StatusForbidden || !strings.Contains(body, "ask an administrator for an invite") {
		t.Fatalf("poll = %d, %s; want 403 pointing at an invite", code, body)
	}
}

// Nobody approved, the code ran out: 410, and the goroutine that was polling
// the IdP for it has stopped -- checked, because leaking one per abandoned
// sign-in is the bug this would otherwise ship.
func TestAnAbandonedSignInExpiresAndStopsPollingTheIdP(t *testing.T) {
	base, _ := wire(t)
	idp := oidctest.New(t)
	idp.ExpiresIn = 2 * time.Second
	withOIDC(t, idp, "create")

	_, s, _ := startDevice(t, base, laptop)
	code, body := pollUntilDone(t, base, s)
	if code != http.StatusGone {
		t.Fatalf("poll after expiry = %d, %s; want 410", code, body)
	}

	settled := idp.TokenCalls()
	time.Sleep(3 * time.Second)
	if after := idp.TokenCalls(); after != settled {
		t.Errorf("the IdP was polled %d more times after the sign-in expired", after-settled)
	}
}

// A read-only request yields a read-only credential. The ceiling is the
// client's to ask for, exactly as it is on a password login.
func TestTheCredentialCarriesThePermItAskedFor(t *testing.T) {
	base, _ := wire(t)
	idp := oidctest.New(t)
	withOIDC(t, idp, "create")

	_, s, _ := startDevice(t, base, `{"kind":"device","client_name":"a reader","perm":"r"}`)
	idp.Approve(t, s.UserCode, oidctest.Identity{Subject: "sub", Email: "reader@example.com", EmailVerified: oidctest.Verified})
	code, body := pollUntilDone(t, base, s)
	if code != http.StatusOK {
		t.Fatalf("collect = %d, %s", code, body)
	}
	var got deviceEnrolled
	decodeInto(t, body, &got)
	if code, body := call(t, "POST", base+"/api/silo/v1/libraries", got.Credential, `{"name":"nope"}`); code != http.StatusForbidden {
		t.Errorf("creating a library with a read-only credential = %d, %s; want 403", code, body)
	}
}

// Refused before the IdP is asked anything, each of them.
func TestStartRefusals(t *testing.T) {
	t.Run("not configured", func(t *testing.T) {
		base, _ := wire(t)
		if code, _, raw := startDevice(t, base, laptop); code != http.StatusNotFound {
			t.Fatalf("start = %d, %s; want 404", code, raw)
		}
	})
	t.Run("no client name", func(t *testing.T) {
		base, _ := wire(t)
		idp := oidctest.New(t)
		withOIDC(t, idp, "create")
		if code, _, raw := startDevice(t, base, `{"kind":"device"}`); code != http.StatusBadRequest {
			t.Fatalf("start = %d, %s; want 400", code, raw)
		}
		if idp.Started() != 0 {
			t.Error("the IdP was asked to start a flow for a request that was refused")
		}
	})
	// The first administrator comes from the setup token. Otherwise whoever
	// reached the IdP's device page first would be it.
	t.Run("server not yet set up", func(t *testing.T) {
		base, _ := unclaimed(t)
		idp := oidctest.New(t)
		withOIDC(t, idp, "create")
		if code, _, raw := startDevice(t, base, laptop); code != http.StatusConflict {
			t.Fatalf("start = %d, %s; want 409", code, raw)
		}
		if idp.Started() != 0 {
			t.Error("the IdP was asked to start a flow on an unclaimed server")
		}
	})
	t.Run("IdP down", func(t *testing.T) {
		base, _ := wire(t)
		idp := oidctest.New(t)
		idp.SetDown(true)
		withOIDC(t, idp, "create")
		if code, _, raw := startDevice(t, base, laptop); code != http.StatusServiceUnavailable {
			t.Fatalf("start = %d, %s; want 503", code, raw)
		}
	})
	t.Run("too many from one address", func(t *testing.T) {
		base, _ := wire(t)
		idp := oidctest.New(t)
		withOIDC(t, idp, "create")
		var last int
		for range 11 {
			last, _, _ = startDevice(t, base, laptop)
		}
		if last != http.StatusTooManyRequests {
			t.Fatalf("the eleventh start in a minute = %d, want 429", last)
		}
	})
}

// An account that arrived through the IdP has no password, and GET /account
// says so, so that a client hides "change password" rather than offering a
// form that asks for a current password nobody has.
func TestAccountSaysWhetherThereIsAPassword(t *testing.T) {
	base, passwordToken := wire(t)
	idp := oidctest.New(t)
	withOIDC(t, idp, "create")

	hasPassword := func(token string) any {
		t.Helper()
		code, body := call(t, "GET", base+"/api/silo/v1/account", token, "")
		if code != http.StatusOK {
			t.Fatalf("GET /account = %d, %s", code, body)
		}
		var m map[string]any
		decodeInto(t, body, &m)
		return m["has_password"]
	}

	if got := hasPassword(passwordToken); got != true {
		t.Errorf("has_password for a password account = %v, want true", got)
	}

	_, s, _ := startDevice(t, base, laptop)
	idp.Approve(t, s.UserCode, oidctest.Identity{Subject: "sub", Email: "sso@example.com", EmailVerified: oidctest.Verified})
	code, body := pollUntilDone(t, base, s)
	if code != http.StatusOK {
		t.Fatalf("collect = %d, %s", code, body)
	}
	var got deviceEnrolled
	decodeInto(t, body, &got)
	if v := hasPassword(got.Credential); v != false {
		t.Errorf("has_password for an account made through the IdP = %v, want false", v)
	}
}
