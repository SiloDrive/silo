package silod

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/SiloDrive/silo/fileserver/setup"
)

// unclaimed stands up a server that has never had an account, and returns its
// base URL and the setup token it minted.
//
// It cannot use wire(t): that helper creates an account, which is the one thing
// a server under test here must not have.
func unclaimed(t *testing.T) (base string, tok setup.Token) {
	t.Helper()
	base = serveTestAPI(t)

	tok, err := setup.Ensure(t.Context())
	if err != nil {
		t.Fatalf("minting the setup token: %v", err)
	}
	if tok.IsZero() {
		t.Fatal("a fresh database says it does not need setting up")
	}
	return base, tok
}

// post sends the unauthenticated JSON request that setup always is. The empty
// bearer token is what makes it unauthenticated: setup and login are registered
// on the bare router, outside RequireCredential, so the header is ignored.
func post(t *testing.T, url, body string) (int, string) {
	t.Helper()
	return call(t, "POST", url, "", body)
}

func setupBody(email, password, token string) string {
	b, _ := json.Marshal(map[string]string{
		"email": email, "password": password, "setup_token": token,
	})
	return string(b)
}

// server-info is how a client finds out it is looking at a server nobody has
// claimed. It says so while that is true, and stops the moment it is not.
//
// The absence of the key afterwards is the assertion, not its value: a claimed
// server has to send the body it sent before this feature existed, so the
// response is decoded as a map rather than into a struct that would paper over
// a key that had appeared.
func TestServerInfoSaysSetupIsRequiredOnlyWhileItIs(t *testing.T) {
	base, tok := unclaimed(t)

	info := func() map[string]any {
		t.Helper()
		resp, err := http.Get(base + "/api/silo/v1/server-info")
		if err != nil {
			t.Fatalf("server-info: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		var m map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
			t.Fatalf("decoding server-info: %v", err)
		}
		return m
	}

	before := info()
	if before["setup_required"] != true {
		t.Errorf("setup_required is %v on an unclaimed server, want true", before["setup_required"])
	}
	var named bool
	for _, f := range before["features"].([]any) {
		if f == "setup" {
			named = true
		}
	}
	if !named {
		t.Error(`features does not name "setup"`)
	}

	if code, body := post(t, base+"/api/silo/v1/auth/setup",
		setupBody("owner@example.com", "correct horse battery staple", tok.String())); code != http.StatusCreated {
		t.Fatalf("setup answered %d: %s", code, body)
	}

	after := info()
	if _, present := after["setup_required"]; present {
		t.Errorf("setup_required is still in the body after setup: %v", after["setup_required"])
	}
	named = false
	for _, f := range after["features"].([]any) {
		if f == "setup" {
			named = true
		}
	}
	if !named {
		t.Error(`features stopped naming "setup" after setup; the name is the build, not the state`)
	}
}

// The point of the whole feature: the operator picks the address and password,
// and what comes back is an ordinary session token.
func TestSetupCreatesTheFirstAccountAndReturnsAUsableToken(t *testing.T) {
	base, tok := unclaimed(t)

	code, body := post(t, base+"/api/silo/v1/auth/setup",
		setupBody("chosen@example.com", "correct horse battery staple", tok.String()))
	if code != http.StatusCreated {
		t.Fatalf("setup answered %d: %s", code, body)
	}

	var out struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("decoding the setup response: %v", err)
	}
	if out.Token == "" {
		t.Fatal("setup returned no token")
	}

	if code, body := call(t, "GET", base+"/api/silo/v1/libraries", out.Token, ""); code != http.StatusOK {
		t.Errorf("the token setup returned does not work: %d %s", code, body)
	}

	// And the credentials chosen in that request are the ones that log in.
	if code, body := post(t, base+"/api/silo/v1/auth/login",
		`{"email":"chosen@example.com","password":"correct horse battery staple"}`); code != http.StatusOK {
		t.Errorf("logging in with the chosen credentials: %d %s", code, body)
	}
}

// A wrong token and a malformed one are the same answer, so a guesser cannot
// learn which half of the guess to keep. docs/auth.md's rule about
// indistinguishable 401s.
func TestAWrongSetupTokenIsIndistinguishableFromAMalformedOne(t *testing.T) {
	base, _ := unclaimed(t)

	other, err := setup.Generate()
	if err != nil {
		t.Fatalf("generating a wrong token: %v", err)
	}

	wrongCode, wrongBody := post(t, base+"/api/silo/v1/auth/setup",
		setupBody("me@example.com", "a password", other.String()))
	malformedCode, malformedBody := post(t, base+"/api/silo/v1/auth/setup",
		setupBody("me@example.com", "a password", "not-a-token"))

	if wrongCode != http.StatusUnauthorized {
		t.Errorf("a wrong token answered %d, want 401", wrongCode)
	}
	if malformedCode != wrongCode || malformedBody != wrongBody {
		t.Errorf("malformed answered %d %q; wrong answered %d %q -- they must be identical",
			malformedCode, malformedBody, wrongCode, wrongBody)
	}
}

// A refused attempt leaves the server exactly as it was: no account, and the
// real token still good. A wrong guess must not lock the operator out.
func TestARefusedSetupCreatesNothingAndBurnsNothing(t *testing.T) {
	base, tok := unclaimed(t)

	if code, _ := post(t, base+"/api/silo/v1/auth/setup",
		setupBody("attacker@example.com", "a password", "SILO-ZZZZ-ZZZZ-ZZZZ-ZZZZ")); code != http.StatusUnauthorized {
		t.Fatalf("a wrong token answered %d, want 401", code)
	}

	var accounts int
	if err := siloPair.Read.QueryRow("SELECT COUNT(*) FROM Account").Scan(&accounts); err != nil {
		t.Fatalf("counting accounts: %v", err)
	}
	if accounts != 0 {
		t.Errorf("a refused setup created %d accounts", accounts)
	}

	if code, body := post(t, base+"/api/silo/v1/auth/setup",
		setupBody("owner@example.com", "correct horse battery staple", tok.String())); code != http.StatusCreated {
		t.Errorf("the real token stopped working after a wrong guess: %d %s", code, body)
	}
}

// Single use, over the wire. The second attempt is 409 rather than 401: the
// token is not wrong, the server is finished with setup, and those want
// different handling -- log in rather than find a better token.
func TestSetupIsRefusedOnceTheServerHasAnAccount(t *testing.T) {
	base, tok := unclaimed(t)

	if code, body := post(t, base+"/api/silo/v1/auth/setup",
		setupBody("first@example.com", "correct horse battery staple", tok.String())); code != http.StatusCreated {
		t.Fatalf("first setup answered %d: %s", code, body)
	}

	code, body := post(t, base+"/api/silo/v1/auth/setup",
		setupBody("second@example.com", "another password", tok.String()))
	if code != http.StatusConflict {
		t.Errorf("a replayed setup answered %d, want 409: %s", code, body)
	}
	if !strings.Contains(body, "already been set up") {
		t.Errorf("the 409 body does not say why: %q", body)
	}
}

// Every field is required, and a missing one is a 400 rather than a 401 --
// nothing about a missing field is a failed authentication.
func TestSetupRequiresEveryField(t *testing.T) {
	base, tok := unclaimed(t)

	for _, body := range []string{
		setupBody("", "a password", tok.String()),
		setupBody("me@example.com", "", tok.String()),
		setupBody("me@example.com", "a password", ""),
	} {
		if code, out := post(t, base+"/api/silo/v1/auth/setup", body); code != http.StatusBadRequest {
			t.Errorf("%s answered %d, want 400: %s", body, code, out)
		}
	}
}

// Guessing is throttled, and the refusal carries the wait rather than leaving a
// client to invent one.
func TestSetupIsRateLimited(t *testing.T) {
	base, _ := unclaimed(t)

	body := setupBody("me@example.com", "a password", "SILO-ZZZZ-ZZZZ-ZZZZ-ZZZZ")
	var limited bool
	for i := 0; i < 12; i++ {
		resp, err := http.Post(base+"/api/silo/v1/auth/setup", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatalf("attempt %d: %v", i, err)
		}
		code := resp.StatusCode
		retry := resp.Header.Get("Retry-After")
		_ = resp.Body.Close()

		if code == http.StatusTooManyRequests {
			limited = true
			if retry == "" {
				t.Error("the 429 carries no Retry-After")
			} else if n, err := strconv.Atoi(retry); err != nil || n < 1 {
				t.Errorf("Retry-After is %q, want whole seconds >= 1", retry)
			}
			break
		}
	}
	if !limited {
		t.Error("twelve wrong tokens from one address were never throttled")
	}
}

// A test that exhausts the setup bucket must not leave the next one throttled.
//
// The limiter is a package-level var in fileserver/api and every test here
// reaches it from 127.0.0.1, so it is shared state with a lifetime longer than
// any single test. TestSetupIsRateLimited spends the bucket on purpose and used
// to leave it spent; in source order it happened to run late enough not to
// matter, and under -shuffle it failed four of its neighbours with a 429 that
// looked like a product bug. This asserts the property directly rather than
// leaving it to the order tests happen to run in.
func TestAnExhaustedSetupBucketDoesNotLeakIntoTheNextTest(t *testing.T) {
	base, _ := unclaimed(t)
	body := setupBody("nobody@example.com", "a password", "SILO-ZZZZ-ZZZZ-ZZZZ-ZZZZ")
	for i := 0; i < 12; i++ {
		resp, err := http.Post(base+"/api/silo/v1/auth/setup", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatalf("attempt %d: %v", i, err)
		}
		_ = resp.Body.Close()
	}

	// A second server, as a second test would stand up. Its setup must be
	// judged on its own merits, not on what the bucket above spent.
	base2, tok := unclaimed(t)
	code, out := post(t, base2+"/api/silo/v1/auth/setup",
		setupBody("first@example.com", "correct horse battery staple", tok.String()))
	if code != http.StatusCreated {
		t.Fatalf("setup on a fresh server answered %d (%s); the previous test's "+
			"exhausted bucket leaked into this one", code, out)
	}
}
