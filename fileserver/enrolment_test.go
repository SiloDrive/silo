package silod

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// Enrolment: a client presents the password once and receives a credential it
// can narrow itself. docs/auth.md calls the password an enrolment credential
// rather than a request credential -- presented once, exchanged, forgotten --
// and this is the exchange.

// enrol posts a login body and returns the status and the decoded response.
func enrol(t *testing.T, base, body string) (int, map[string]any) {
	t.Helper()
	resp, err := http.Post(base+"/api/silo/v1/auth/login", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	defer resp.Body.Close()

	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		// A refusal is text, not JSON; the status is what the caller checks.
		return resp.StatusCode, nil
	}
	return resp.StatusCode, out
}

const wireLogin = `"email":"wire@example.com","password":"correct horse battery staple"`

// The shape every existing client sends must answer exactly what it always
// answered. A field added to a token response is a client-visible change --
// see docs/bugs/fixed/adding-a-number-to-a-token-response-breaks-clients.md --
// so the enrolment response is reached by asking for it, not by upgrading
// everyone who logs in.
func TestAPlainLoginIsUnchanged(t *testing.T) {
	base, _ := wire(t)

	code, out := enrol(t, base, `{`+wireLogin+`}`)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	tok, _ := out["token"].(string)
	if !strings.HasPrefix(tok, "silo_session_") {
		t.Errorf("token = %q, want a session credential", tok)
	}
	for _, field := range []string{"credential", "expires_at", "email"} {
		if _, ok := out[field]; ok {
			t.Errorf("a plain login response carries %q; it did not before", field)
		}
	}
}

// Asking for a device credential is what silo-drive does once, at enrolment.
func TestEnrollingADeviceReturnsTheDocumentedShape(t *testing.T) {
	base, _ := wire(t)

	code, out := enrol(t, base, `{`+wireLogin+`,"kind":"device","client_name":"SiloDrive 1.2 (macOS)"}`)
	if code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %v", code, out)
	}
	cred, _ := out["credential"].(string)
	if !strings.HasPrefix(cred, "silo_device_") {
		t.Errorf("credential = %q, want a device credential", cred)
	}
	if _, ok := out["token"].(string); ok {
		t.Error("the enrolment response carries the old \"token\" field too")
	}
	if out["email"] != "wire@example.com" {
		t.Errorf("email = %v", out["email"])
	}
	exp, ok := out["expires_at"].(float64)
	if !ok || exp == 0 {
		t.Errorf("expires_at = %v, want a unix time", out["expires_at"])
	}

	// The client_name becomes the label, which is what an operator revokes by.
	if code, body := call(t, "GET", base+"/api/silo/v1/libraries", cred, ""); code != http.StatusOK {
		t.Errorf("the enrolled credential does not work: status %d, body %s", code, body)
	}
}

// perm and scope can only narrow, so they need no validation branch: asking
// for more than the account has yields what the account has, because the
// ceiling is applied per request.
func TestEnrollingCanNarrowItself(t *testing.T) {
	base, token := wire(t)
	mine := makeLibrary(t, base, token)
	other := makeLibrary(t, base, token)

	code, out := enrol(t, base,
		`{`+wireLogin+`,"kind":"device","client_name":"backup tool","perm":"r","scope":"`+mine+`"}`)
	if code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %v", code, out)
	}
	cred, _ := out["credential"].(string)

	if code, body := call(t, "GET", base+"/api/silo/v1/libraries/"+mine+"/entries/", cred, ""); code != http.StatusOK {
		t.Errorf("reading its own library: status %d, body %s", code, body)
	}
	if code, body := call(t, "PUT", base+"/api/silo/v1/libraries/"+mine+"/entries/x.txt", cred, "x"); code != http.StatusForbidden {
		t.Errorf("writing with perm=r: status %d, want 403, body %s", code, body)
	}
	if code, body := call(t, "GET", base+"/api/silo/v1/libraries/"+other+"/entries/", cred, ""); code != http.StatusForbidden {
		t.Errorf("reading out of scope: status %d, want 403, body %s", code, body)
	}
}

func TestEnrolmentRefusals(t *testing.T) {
	base, _ := wire(t)

	cases := []struct {
		name string
		body string
		want int
	}{
		// access and s3 credentials are not minted by presenting a password:
		// one is a capability URL's, the other derives from the master key.
		{"a kind this lane does not mint", `{` + wireLogin + `,"kind":"access","client_name":"x"}`, http.StatusBadRequest},
		{"a kind that is not a kind", `{` + wireLogin + `,"kind":"wat","client_name":"x"}`, http.StatusBadRequest},
		// A misspelled perm must not become "no access", which is how minPerm
		// reads anything it does not recognise.
		{"a misspelled perm", `{` + wireLogin + `,"kind":"device","client_name":"x","perm":"read"}`, http.StatusBadRequest},
		{"an unparseable scope", `{` + wireLogin + `,"kind":"device","client_name":"x","scope":"lib:"}`, http.StatusBadRequest},
		{"enrolment with no client_name", `{` + wireLogin + `,"kind":"device"}`, http.StatusBadRequest},
		// Proof of possession is designed and not built. Minting a credential
		// that Resolve refuses would hand a client something that can never work.
		{"a public key", `{` + wireLogin + `,"kind":"device","client_name":"x","public_key":"AAAA"}`, http.StatusNotImplemented},
		// The password is still the password, whatever else is asked for.
		{"a wrong password", `{"email":"wire@example.com","password":"nope","kind":"device","client_name":"x"}`, http.StatusUnauthorized},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if code, out := enrol(t, base, tc.body); code != tc.want {
				t.Errorf("status = %d, want %d: %v", code, tc.want, out)
			}
		})
	}
}
