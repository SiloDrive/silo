package silod

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/dkam/silo/fileserver/account"
	"github.com/dkam/silo/fileserver/credential"
	"github.com/dkam/silo/fileserver/option"
)

// Renewal. A device credential lives 90 days and had no way to produce a
// replacement, so the only route to credential number two was the password --
// which a headless client can only supply by keeping the password at rest
// forever. These tests pin the two halves that make renewal different from the
// sliding expiry docs/auth.md refuses: the successor is a new row, and the old
// row's expiry does not move.

// renewCred posts the route and returns the status and the decoded body.
func renewCred(t *testing.T, base, token string) (int, map[string]any) {
	t.Helper()
	code, body := call(t, "POST", base+"/api/silo/v1/auth/renew", token, "")
	var out map[string]any
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		// A refusal is text, not JSON; the status is what the caller checks.
		return code, nil
	}
	return code, out
}

// credentialID pulls the public half out of a token, which is what a row is
// found by: silo_<kind>_<id>_<secret><check>.
func credentialID(t *testing.T, token string) string {
	t.Helper()
	parts := strings.Split(token, "_")
	if len(parts) != 4 {
		t.Fatalf("token %q is not silo_<kind>_<id>_<secret><check>", token)
	}
	return parts[2]
}

// credentialRows is what `silo token list` shows an operator: every row the
// wire account holds.
func credentialRows(t *testing.T) []*credential.Credential {
	t.Helper()
	ctx, cancel := option.WithDBTimeout(context.Background())
	defer cancel()

	acct, err := account.ByEmail(ctx, "wire@example.com")
	if err != nil {
		t.Fatalf("looking up the wire account: %v", err)
	}
	rows, err := credential.ListByAccount(ctx, acct.ID)
	if err != nil {
		t.Fatalf("listing credentials: %v", err)
	}
	return rows
}

// device issues a device credential with a life and a name of its own, which is
// the shape a client holds after enrolment.
func device(t *testing.T, label string, life time.Duration) string {
	t.Helper()
	return issueCredential(t, credential.IssueOpts{
		Kind:     credential.KindDevice,
		Perm:     "rw",
		Label:    label,
		Lifetime: life,
	})
}

func TestADeviceCredentialMintsItsSuccessor(t *testing.T) {
	base, _ := wire(t)
	held := device(t, "dan's macbook", time.Hour)

	code, out := renewCred(t, base, held)
	if code != http.StatusCreated {
		t.Fatalf("renew: status %d, want 201: %v", code, out)
	}

	next, _ := out["credential"].(string)
	if !strings.HasPrefix(next, "silo_device_") {
		t.Errorf("credential = %q, want a device credential", next)
	}
	if !alive(t, base, next) {
		t.Error("the successor does not work")
	}
	if out["email"] != "wire@example.com" {
		t.Errorf("email = %v", out["email"])
	}

	// A full lifetime, not the remainder of the one it replaces.
	exp, ok := out["expires_at"].(float64)
	if !ok || int64(exp) < time.Now().Add(89*24*time.Hour).Unix() {
		t.Errorf("expires_at = %v, want roughly 90 days out", out["expires_at"])
	}
}

// The credential that asked keeps working, and keeps its own expiry.
//
// Both halves matter. Revoking it here would be one-in-one-out and would mean
// a response lost in transit costs a headless client the credential it holds
// and the one it never received; extending it here would be the sliding expiry
// the absolute lifetime exists to refuse.
func TestRenewingLeavesTheOldRowExactlyAsItWas(t *testing.T) {
	base, _ := wire(t)
	held := device(t, "a daemon", time.Hour)

	before := rowByID(t, credentialID(t, held))
	if code, out := renewCred(t, base, held); code != http.StatusCreated {
		t.Fatalf("renew: status %d: %v", code, out)
	}
	after := rowByID(t, credentialID(t, held))

	if !alive(t, base, held) {
		t.Error("renewing revoked the credential that asked for it")
	}
	if after.ExpiresAt != before.ExpiresAt {
		t.Errorf("expires_at moved from %d to %d; the lifetime is absolute",
			before.ExpiresAt, after.ExpiresAt)
	}
}

// rowByID finds one credential in the account's listing.
func rowByID(t *testing.T, id string) *credential.Credential {
	t.Helper()
	for _, c := range credentialRows(t) {
		if c.ID == id {
			return c
		}
	}
	t.Fatalf("no credential %s in the account's listing", id)
	return nil
}

// The successor inherits the ceiling rather than restating it, so there is no
// request a client could widen one with -- and it inherits the label, so the
// chain is visible to an operator under the name they went looking for.
func TestTheSuccessorInheritsTheCeilingAndTheName(t *testing.T) {
	base, token := wire(t)
	mine := makeLibrary(t, base, token)
	other := makeLibrary(t, base, token)

	scoped := issueCredential(t, credential.IssueOpts{
		Kind:     credential.KindDevice,
		Perm:     "r",
		Scope:    credential.Scope{LibraryID: mine},
		Label:    "a read-only mount",
		Lifetime: time.Hour,
	})

	code, out := renewCred(t, base, scoped)
	if code != http.StatusCreated {
		t.Fatalf("renew: status %d, want 201: %v", code, out)
	}
	next, _ := out["credential"].(string)

	if code, body := call(t, "GET", base+"/api/silo/v1/libraries/"+mine+"/entries/", next, ""); code != http.StatusOK {
		t.Errorf("the successor cannot read its own library: status %d, body %s", code, body)
	}
	if code, body := call(t, "PUT", base+"/api/silo/v1/libraries/"+mine+"/entries/x.txt", next, "x"); code != http.StatusForbidden {
		t.Errorf("the successor may write: status %d, want 403, body %s", code, body)
	}
	if code, body := call(t, "GET", base+"/api/silo/v1/libraries/"+other+"/entries/", next, ""); code != http.StatusForbidden {
		t.Errorf("the successor reaches out of scope: status %d, want 403, body %s", code, body)
	}

	row := rowByID(t, credentialID(t, next))
	if row.Label != "a read-only mount" {
		t.Errorf("label = %q, want the name its parent carried", row.Label)
	}
}

func TestRenewalRefusals(t *testing.T) {
	base, session := wire(t)

	// A session lasts a day because a person walked away from a terminal, and
	// one that can renew itself never ends -- which is the sliding expiry this
	// route is at pains not to be.
	if code, out := renewCred(t, base, session); code != http.StatusForbidden {
		t.Errorf("renewing a session: status %d, want 403: %v", code, out)
	}

	resp, err := http.Post(base+"/api/silo/v1/auth/renew", "application/json", nil)
	if err != nil {
		t.Fatalf("renew with no credential: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("renew with no credential: status %d, want 401", resp.StatusCode)
	}
}

// A client cannot learn this route from a 404 in time for it to help: the
// fallback for not having it is keeping the password at rest, which is a
// decision made at enrolment rather than on the morning the credential stops
// working. So it has a name.
func TestServerInfoNamesCredentialRenew(t *testing.T) {
	base := serveTestAPI(t)

	resp, err := http.Get(base + "/api/silo/v1/server-info")
	if err != nil {
		t.Fatalf("server-info: %v", err)
	}
	defer resp.Body.Close()

	var out struct {
		Features []string `json:"features"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode server-info: %v", err)
	}
	if !slices.Contains(out.Features, "credential-renew") {
		t.Errorf("features = %v, want it to include \"credential-renew\"", out.Features)
	}
}
