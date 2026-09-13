package silod

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/dkam/silo/fileserver/account"
	"github.com/dkam/silo/fileserver/credential"
)

// A credential row answers two different questions and docs/auth.md keeps them
// apart: the label says *which device holds this*, and it is frozen at
// enrolment because renewal inherits it. What is running right now is a
// separate fact with a separate lifetime, so it gets a separate column.
//
// client_id is the device's own identity -- minted once by the client, not
// derived from anything a person can rename -- and it is what makes a Mac that
// re-enrolls after a lapse visibly the same Mac rather than a new row.

// deviceCred finds the one device credential the account holds. The listing is
// what an operator reads, so the test reads it through the same call.
func deviceCred(t *testing.T) *credential.Credential {
	t.Helper()
	creds, err := credential.ListByAccount(testCtx(t), wireAccount(t).ID)
	if err != nil {
		t.Fatalf("listing credentials: %v", err)
	}
	for _, c := range creds {
		if c.Kind == credential.KindDevice {
			return c
		}
	}
	t.Fatal("no device credential was issued")
	return nil
}

func TestEnrolmentRecordsTheClientID(t *testing.T) {
	base, _ := wire(t)

	const domainID = "com.nmilne.SiloDrive.7C9A1E4F-0B2D-4A11-9E6C-5F3D2A8B1C40"
	code, out := enrol(t, base,
		`{`+wireLogin+`,"kind":"device","client_name":"dan's macbook","client_id":"`+domainID+`"}`)
	if code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %v", code, out)
	}

	if got := deviceCred(t).ClientID; got != domainID {
		t.Errorf("client_id = %q, want %q", got, domainID)
	}
}

// The property this test exists to hold is the one
// docs/bugs/fixed/adding-a-number-to-a-token-response-breaks-clients.md is
// about: a client that sends what it always sent gets what it always got.
// client_id modifies an enrolment, it does not trigger one, so a body carrying
// only client_id is still a plain login and still answers with "token".
func TestClientIDAloneIsNotAnEnrolment(t *testing.T) {
	base, _ := wire(t)

	code, out := enrol(t, base, `{`+wireLogin+`,"client_id":"com.nmilne.SiloDrive.abc"}`)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %v", code, out)
	}
	if tok, _ := out["token"].(string); !strings.HasPrefix(tok, "silo_session_") {
		t.Errorf("token = %v, want a session credential", out["token"])
	}
	if _, ok := out["credential"]; ok {
		t.Error("client_id alone turned a plain login into an enrolment")
	}
}

// Renewal takes no body and copies every field, so the successor carries the
// device identity its parent carried. That is what makes a 90-day chain
// groupable by device rather than by a label nobody can join on.
func TestRenewalCarriesTheClientID(t *testing.T) {
	base, _ := wire(t)

	const domainID = "com.nmilne.SiloDrive.11112222-3333-4444-5555-666677778888"
	code, out := enrol(t, base,
		`{`+wireLogin+`,"kind":"device","client_name":"dan's macbook","client_id":"`+domainID+`"}`)
	if code != http.StatusCreated {
		t.Fatalf("enrol: status %d: %v", code, out)
	}
	cred, _ := out["credential"].(string)

	code, body := call(t, "POST", base+"/api/silo/v1/auth/renew", cred, "")
	if code != http.StatusOK && code != http.StatusCreated {
		t.Fatalf("renew: status %d, body %s", code, body)
	}
	var renewed struct {
		Credential string `json:"credential"`
	}
	if err := json.Unmarshal([]byte(body), &renewed); err != nil {
		t.Fatalf("decoding %s: %v", body, err)
	}

	creds, err := credential.ListByAccount(testCtx(t), wireAccount(t).ID)
	if err != nil {
		t.Fatalf("listing credentials: %v", err)
	}
	var seen int
	for _, c := range creds {
		if c.Kind != credential.KindDevice {
			continue
		}
		seen++
		if c.ClientID != domainID {
			t.Errorf("credential %s has client_id %q, want %q", c.ID, c.ClientID, domainID)
		}
	}
	if seen < 2 {
		t.Fatalf("saw %d device credentials, want the parent and its successor", seen)
	}
}

// The label froze at enrolment; this is the half it cannot answer. It rides
// the last_used write rather than adding one of its own, so an always-on
// mount's polling is not serialised behind a bookkeeping UPDATE -- which is
// the whole reason last_used is coarse in the first place.
func TestTheLastUserAgentIsRecorded(t *testing.T) {
	base, _ := wire(t)

	code, out := enrol(t, base, `{`+wireLogin+`,"kind":"device","client_name":"dan's macbook"}`)
	if code != http.StatusCreated {
		t.Fatalf("enrol: status %d: %v", code, out)
	}
	cred, _ := out["credential"].(string)

	const ua = "SiloDrive/0.1.0 (macOS 26.5; extension; build 126; 500d40c)"
	req, err := http.NewRequest("GET", base+"/api/silo/v1/libraries", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+cred)
	req.Header.Set("User-Agent", ua)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET libraries: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET libraries: status %d", resp.StatusCode)
	}

	// The stamp is detached onto its own goroutine, so the response arriving
	// does not mean the row has been written. Polling is the honest wait.
	deadline := time.Now().Add(3 * time.Second)
	for {
		if got := deviceCred(t).LastUA; got == ua {
			return
		} else if time.Now().After(deadline) {
			t.Fatalf("last_ua = %q, want %q", got, ua)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// wireAccount is the account wire() created. The credential listing is keyed
// by account id, and wireAccountID answers in the string form the HTTP surface
// uses rather than the one the store takes.
func wireAccount(t *testing.T) *account.Account {
	t.Helper()
	acct, err := account.ByEmail(testCtx(t), "wire@example.com")
	if err != nil {
		t.Fatalf("looking up the wire account: %v", err)
	}
	return acct
}
