package silod

import (
	"encoding/json"
	"net/http"
	"testing"
)

// An account has to be able to learn its own id before it has published
// anything, because that id is the holder every wrap binds as associated data:
// without it there is nothing to wrap an identity key to, and account/keys --
// the only other place the id appears -- answers 404 until an identity key
// exists. That is a bootstrap that cannot start.
func TestAnAccountCanLearnItsOwnIDBeforeItEnrols(t *testing.T) {
	base, token := wire(t)

	// Nothing is published yet, which is the state this endpoint exists for.
	if code, _ := call(t, "GET", base+"/api/silo/v1/account/keys", token, ""); code != http.StatusNotFound {
		t.Fatalf("account/keys before enrolment: status %d, want 404", code)
	}

	code, body := call(t, "GET", base+"/api/silo/v1/account", token, "")
	if code != http.StatusOK {
		t.Fatalf("GET account: status %d, body %s", code, body)
	}
	var got struct {
		AccountID string `json:"account_id"`
		Email     string `json:"email"`
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("decoding %s: %v", body, err)
	}
	if want := wireAccountID(t); got.AccountID != want {
		t.Errorf("account_id = %q, want %q", got.AccountID, want)
	}
	if got.Email != "wire@example.com" {
		t.Errorf("email = %q", got.Email)
	}
}

// It is behind a credential like everything else: the answer names an account,
// so an unauthenticated caller must not get one.
func TestWhoAmIRequiresACredential(t *testing.T) {
	base, _ := wire(t)
	if code, body := call(t, "GET", base+"/api/silo/v1/account", "", ""); code != http.StatusUnauthorized {
		t.Errorf("status %d, want 401; body %s", code, body)
	}
}
