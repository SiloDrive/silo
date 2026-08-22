package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/dkam/silo/fileserver/account"
	"github.com/dkam/silo/fileserver/authmgr"
	"github.com/dkam/silo/fileserver/dbutil"
	"github.com/dkam/silo/fileserver/option"
)

func init() {
	option.JWTPrivateKey = "test-secret-key-for-unit-tests"
}

// seedAccount gives the test a database and one account in it. RequireAuth
// reads the account behind a session token — that lookup is what makes a
// disabled account stop working mid-session — so a token alone is no longer
// enough to exercise the middleware.
func seedAccount(t *testing.T, email string) *account.Account {
	t.Helper()

	if option.DBOpTimeout <= 0 {
		option.DBOpTimeout = 5 * time.Second
	}
	pair, err := dbutil.OpenSQLite(filepath.Join(t.TempDir(), "silo.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = pair.Close() })
	if err := dbutil.CreateSiloTables(pair.Write); err != nil {
		t.Fatalf("create tables: %v", err)
	}
	account.Init(pair.Read, pair.Write)

	ctx, cancel := option.WithDBTimeout(context.Background())
	defer cancel()
	if _, _, err := account.Create(ctx, email, "PBKDF2SHA256$1$00$00", false); err != nil {
		t.Fatalf("create account: %v", err)
	}
	acct, err := account.ByEmail(ctx, email)
	if err != nil {
		t.Fatalf("read account: %v", err)
	}
	return acct
}

func TestRequireAuthSuccess(t *testing.T) {
	alice := seedAccount(t, "alice@example.com")
	token, err := authmgr.GenerateSessionToken(alice.ID)
	if err != nil {
		t.Fatalf("failed to generate token: %v", err)
	}

	var gotEmail string
	handler := RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotEmail = GetUserEmail(r)
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/api/silo/v1/repos", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rr.Code)
	}
	if gotEmail != "alice@example.com" {
		t.Errorf("expected alice@example.com, got %s", gotEmail)
	}
}

func TestRequireAuthMissingHeader(t *testing.T) {
	handler := RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("handler should not be called")
	}))

	req := httptest.NewRequest("GET", "/api/silo/v1/repos", nil)
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", rr.Code)
	}
}

func TestRequireAuthInvalidFormat(t *testing.T) {
	handler := RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("handler should not be called")
	}))

	tests := []struct {
		name   string
		header string
	}{
		{"no space", "Bearertoken"},
		{"wrong scheme", "Basic dXNlcjpwYXNz"},
		{"empty token", "Bearer "},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/api/silo/v1/repos", nil)
			req.Header.Set("Authorization", tt.header)
			rr := httptest.NewRecorder()

			handler.ServeHTTP(rr, req)

			if rr.Code != http.StatusUnauthorized {
				t.Errorf("expected 401, got %d", rr.Code)
			}
		})
	}
}

func TestRequireAuthExpiredToken(t *testing.T) {
	handler := RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("handler should not be called")
	}))

	req := httptest.NewRequest("GET", "/api/silo/v1/repos", nil)
	req.Header.Set("Authorization", "Bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJlbWFpbCI6InRlc3RAZXhhbXBsZS5jb20iLCJleHAiOjE1MDAwMDAwMDB9.invalid")
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", rr.Code)
	}
}

func TestRequireAuthCaseInsensitiveBearer(t *testing.T) {
	bob := seedAccount(t, "bob@example.com")
	token, err := authmgr.GenerateSessionToken(bob.ID)
	if err != nil {
		t.Fatalf("failed to generate token: %v", err)
	}

	handler := RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/api/silo/v1/repos", nil)
	req.Header.Set("Authorization", "bearer "+token)
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("expected 200 with lowercase 'bearer', got %d", rr.Code)
	}
}

func TestGetUserEmailNoContext(t *testing.T) {
	req := httptest.NewRequest("GET", "/", nil)
	email := GetUserEmail(req)
	if email != "" {
		t.Errorf("expected empty string, got %s", email)
	}
}

// A session outliving the account it names is the failure the id-carrying
// claim exists to prevent: the token still verifies, but the account behind it
// no longer may do anything.
func TestRequireAuthRefusesADisabledAccount(t *testing.T) {
	acct := seedAccount(t, "carol@example.com")
	token, err := authmgr.GenerateSessionToken(acct.ID)
	if err != nil {
		t.Fatalf("failed to generate token: %v", err)
	}

	handler := RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("handler should not be called")
	}))

	ctx, cancel := option.WithDBTimeout(context.Background())
	defer cancel()
	if err := account.SetActive(ctx, acct.ID, false); err != nil {
		t.Fatalf("disabling account: %v", err)
	}

	req := httptest.NewRequest("GET", "/api/silo/v1/repos", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("a disabled account kept its session: got %d, want 401", rr.Code)
	}
}
