package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dkam/silo/fileserver/account"
	"github.com/dkam/silo/fileserver/authmgr"
)

// A request with no Authorization header reaches the handler, with no account.
func TestOptionalAuthLetsAnAnonymousRequestThrough(t *testing.T) {
	called := false
	handler := OptionalAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		if acct := GetAccount(r); acct != nil {
			t.Errorf("an unauthenticated request carried account %v", acct.Email)
		}
		w.WriteHeader(http.StatusOK)
	}))

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest("GET", "/notification", nil))

	if !called {
		t.Error("the handler was not reached")
	}
	if rr.Code != http.StatusOK {
		t.Errorf("got %d, want 200", rr.Code)
	}
}

// A good token is resolved, exactly as RequireAuth would.
func TestOptionalAuthResolvesAGoodToken(t *testing.T) {
	alice := seedAccount(t, "alice@example.com")
	token, err := authmgr.GenerateSessionToken(alice.ID)
	if err != nil {
		t.Fatalf("generate token: %v", err)
	}

	var got string
	handler := OptionalAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = GetUserEmail(r)
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/notification", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", rr.Code)
	}
	if got != "alice@example.com" {
		t.Errorf("got %q, want alice@example.com", got)
	}
}

// A credential that is offered and bad is refused, not quietly downgraded.
//
// This is the whole difference between optional and lax. A caller sending a
// token believes it is authenticated; letting it through as anonymous would
// tell it so only by way of the permissions it silently stopped having.
func TestOptionalAuthRefusesABadToken(t *testing.T) {
	seedAccount(t, "alice@example.com")

	for _, tc := range []struct{ name, header string }{
		{"garbage", "Bearer not-a-jwt"},
		{"wrong scheme", "Token abcdef"},
		{"no scheme", "abcdef"},
		{"empty bearer", "Bearer "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handler := OptionalAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				t.Fatalf("a request with %q reached the handler", tc.header)
			}))
			req := httptest.NewRequest("GET", "/notification", nil)
			req.Header.Set("Authorization", tc.header)
			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, req)
			if rr.Code != http.StatusUnauthorized {
				t.Errorf("got %d, want 401", rr.Code)
			}
		})
	}
}

// A deactivated account is refused here too.
//
// The one-body rule in requireCredential exists so that deactivating an account
// stops every lane at once. A lane that skipped the check would be a way for a
// disabled user to keep a live socket until their token expired.
func TestOptionalAuthRefusesADeactivatedAccount(t *testing.T) {
	alice := seedAccount(t, "alice@example.com")
	token, err := authmgr.GenerateSessionToken(alice.ID)
	if err != nil {
		t.Fatalf("generate token: %v", err)
	}
	if err := account.SetActive(t.Context(), alice.ID, false); err != nil {
		t.Fatalf("deactivate: %v", err)
	}

	handler := OptionalAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("a deactivated account reached the handler")
	}))
	req := httptest.NewRequest("GET", "/notification", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("got %d, want 401", rr.Code)
	}
}
