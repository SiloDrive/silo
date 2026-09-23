package middleware

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/SiloDrive/silo/fileserver/credential"
	"github.com/SiloDrive/silo/fileserver/option"
	log "github.com/sirupsen/logrus"
)

// A refusal is the one line an operator has to work from. The 401 body says
// nothing on purpose -- see credentialRefused -- so everything that identifies
// who knocked has to be in the log, or a socket retrying every thirty seconds
// is an unattributable heartbeat in the journal.
//
// What it may say is bounded by the same rule the body obeys: the client's
// address and what it *presented*. The credential id is the public half, which
// an operator can match against `silo token list`; the secret never appears.

// captureDebug collects what the package logs while fn runs.
func captureDebug(t *testing.T, fn func()) string {
	t.Helper()

	var buf bytes.Buffer
	prevOut, prevLevel := log.StandardLogger().Out, log.GetLevel()
	log.SetOutput(&buf)
	log.SetLevel(log.DebugLevel)
	t.Cleanup(func() {
		log.SetOutput(prevOut)
		log.SetLevel(prevLevel)
	})

	fn()
	return buf.String()
}

// knock sends one request that will be refused, and returns what was logged.
func knock(t *testing.T, req *http.Request) string {
	t.Helper()

	h := RequireCredential(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("the handler was reached by a request that should have been refused")
	}))
	return captureDebug(t, func() { h.ServeHTTP(httptest.NewRecorder(), req) })
}

// notification is the route the socket knocks on, and the one that showed the
// problem: a device polling with a credential the server no longer honours.
func notificationReq(header string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/notification", nil)
	if header != "" {
		req.Header.Set("Authorization", header)
	}
	req.Header.Set("User-Agent", "silo-drive/0.5.2")
	req.RemoteAddr = "198.51.100.4:52344"
	return req
}

func TestRefusalNamesTheCaller(t *testing.T) {
	testDB(t)

	// Well-formed and unknown: the shape a revoked or reissued credential
	// leaves behind once its row is gone.
	tok, secret, err := credential.NewToken(credential.KindDevice)
	if err != nil {
		t.Fatalf("minting a token: %v", err)
	}

	line := knock(t, notificationReq("Bearer "+secret))

	for _, want := range []string{"/notification", "198.51.100.4", tok.ID, "device", "silo-drive/0.5.2"} {
		if !strings.Contains(line, want) {
			t.Errorf("the refusal does not name %q: %s", want, line)
		}
	}
	if strings.Contains(line, secret) {
		t.Errorf("the refusal logged the credential secret: %s", line)
	}
}

// A credential that does not parse has no id to name, and saying so is still
// worth a line: the address and the agent are what is left to go on.
func TestRefusalNamesTheCallerWithoutAnID(t *testing.T) {
	testDB(t)

	line := knock(t, notificationReq("Bearer not-a-credential"))
	for _, want := range []string{"198.51.100.4", "silo-drive/0.5.2"} {
		if !strings.Contains(line, want) {
			t.Errorf("the refusal does not name %q: %s", want, line)
		}
	}
}

// No Authorization header at all is the other thing that knocks repeatedly,
// and it logged nothing whatsoever before.
func TestRefusalNamesTheCallerWithNoCredential(t *testing.T) {
	testDB(t)

	line := knock(t, notificationReq(""))
	if !strings.Contains(line, "198.51.100.4") {
		t.Errorf("a request with no credential logged no address: %s", line)
	}
}

// Behind a proxy the peer address is the proxy's. The refusal reads the
// client's the same way the rate limiter does, so the two agree about who a
// request came from -- and disbelieves the header on the same terms.
func TestRefusalReadsTheForwardedAddress(t *testing.T) {
	testDB(t)

	orig := option.TrustProxyHeaders
	t.Cleanup(func() { option.TrustProxyHeaders = orig })

	req := notificationReq("Bearer not-a-credential")
	req.Header.Set("X-Forwarded-For", "203.0.113.9")

	option.TrustProxyHeaders = true
	if line := knock(t, req); !strings.Contains(line, "203.0.113.9") {
		t.Errorf("the refusal ignored a trusted X-Forwarded-For: %s", line)
	}

	option.TrustProxyHeaders = false
	line := knock(t, req)
	if !strings.Contains(line, "198.51.100.4") || strings.Contains(line, "203.0.113.9") {
		t.Errorf("the refusal believed an untrusted X-Forwarded-For: %s", line)
	}
}

// TestRefusalLineShape is the line itself, so a change to the wording is a
// change a reviewer sees rather than one an operator's grep finds later.
func TestRefusalLineShape(t *testing.T) {
	testDB(t)

	tok, secret, err := credential.NewToken(credential.KindDevice)
	if err != nil {
		t.Fatalf("minting a token: %v", err)
	}

	// The user agent is quoted in the line and logrus escapes those quotes on
	// its way out, so the expectation carries them the way the journal does.
	want := `Credential refused on /notification, from 198.51.100.4 (device credential ` +
		tok.ID + `, \"silo-drive/0.5.2\"): invalid credential`
	if line := knock(t, notificationReq("Bearer "+secret)); !strings.Contains(line, want) {
		t.Errorf("refusal line:\n got %s\nwant %s", line, want)
	}
}
