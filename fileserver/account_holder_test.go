package silod

import (
	"encoding/json"
	"net/http"
	"testing"
)

// The holder is the string an identity wrap is bound to, and a client cannot
// open the blob it just fetched without it. store/wrap.go fixes it as the
// account's id in canonical UUID text, and until this landed nothing on the
// wire said what that id was: every test in this package reads it out of the
// database, which is a thing no client can do.
//
// It rides on the keys response rather than on a route of its own because it
// is only ever wanted with the blob it opens. A second round trip to learn one
// field is a second thing to get wrong at bootstrap, which is the moment with
// the least to fall back on.
func TestAccountKeysCarryTheHolderThatOpensThem(t *testing.T) {
	base, token, _ := enrolled(t)

	code, body := call(t, "GET", base+keysPath, token, "")
	if code != http.StatusOK {
		t.Fatalf("reading account keys: status %d, body %s", code, body)
	}
	var out struct {
		AccountID string `json:"account_id"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("decoding %s: %v", body, err)
	}
	if out.AccountID == "" {
		t.Fatal("account/keys served a wrapped identity key and no holder to open it with")
	}
	if want := wireAccountID(t); out.AccountID != want {
		t.Errorf("account_id = %q, want the account's own id %q", out.AccountID, want)
	}
}
