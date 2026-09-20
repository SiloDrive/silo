package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/SiloDrive/silo/client"
)

// credentialServer stands in for the account/credentials surface and records
// what the command did to it, which is the only way to observe the half of
// this command that produces no output: whether it cleaned up after itself.
type credentialServer struct {
	mu        sync.Mutex
	loggedOut int
	revoked   []string
	userAgent string
	// revokeCurrent is what DELETE reports back: the id given was the caller's
	// own session.
	revokeCurrent bool
	creds         []client.Credential
}

func newCredentialServer(t *testing.T, s *credentialServer) *client.APIClient {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if ua := r.UserAgent(); ua != "" {
			s.userAgent = ua
		}
		// Enforced, because one test turns on whether a request made after a
		// logout is refused -- and a fake server that waves everything
		// through would report that it succeeded no matter what the client
		// did.
		if r.URL.Path != "/api/silo/v1/auth/login" && r.Header.Get("Authorization") != "Bearer session-token" {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		switch {
		case r.URL.Path == "/api/silo/v1/auth/login":
			_ = json.NewEncoder(w).Encode(map[string]string{"token": "session-token"})
		case r.URL.Path == "/api/silo/v1/auth/logout":
			s.loggedOut++
			_ = json.NewEncoder(w).Encode(map[string]int{"revoked": 1})
		case r.URL.Path == "/api/silo/v1/account/credentials" && r.Method == "GET":
			_ = json.NewEncoder(w).Encode(map[string][]client.Credential{"credentials": s.creds})
		case r.Method == "DELETE":
			s.revoked = append(s.revoked, r.URL.Path)
			_ = json.NewEncoder(w).Encode(map[string]any{"revoked": 1, "current": s.revokeCurrent})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	c := client.NewClient(srv.URL)
	if err := c.Login("dan@example.com", "correct horse battery staple"); err != nil {
		t.Fatalf("login: %v", err)
	}
	return c
}

// Listing credentials does not add one.
//
// This is the behaviour the command exists to have and the one nothing else
// would catch: the CLI's HTTP lane logs in and receives a session credential,
// so without the sign-out at the end, every `silo credential list` leaves a row
// behind -- in the very table it was printing.
func TestListingCredentialsSignsItsOwnSessionOut(t *testing.T) {
	s := &credentialServer{creds: []client.Credential{
		{ID: "a", Kind: "device", Label: "laptop", Perm: "rw"},
		{ID: "b", Kind: "session", Label: "silo credential", Perm: "rw", Current: true},
	}}
	c := newCredentialServer(t, s)

	if err := cmdCredential(c, []string{"list"}); err != nil {
		t.Fatalf("credential list: %v", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loggedOut != 1 {
		t.Errorf("logged out %d times, want 1; a list that does not clean up adds a row every time it prints one", s.loggedOut)
	}
	if s.userAgent != "silo credential" {
		t.Errorf("User-Agent = %q, want %q: the server labels the row from it", s.userAgent, "silo credential")
	}
}

// Revoking somebody else's row still signs this command out afterwards.
func TestRevokingAnotherCredentialStillSignsOut(t *testing.T) {
	s := &credentialServer{}
	c := newCredentialServer(t, s)

	if err := cmdCredential(c, []string{"revoke", "a"}); err != nil {
		t.Fatalf("credential revoke: %v", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.revoked) != 1 {
		t.Fatalf("revoked %v, want one id", s.revoked)
	}
	if s.loggedOut != 1 {
		t.Errorf("logged out %d times, want 1", s.loggedOut)
	}
}

// Revoking this command's own id is the logout, and is not followed by a
// second one.
//
// The double call would not fail loudly -- the second logout answers
// {"revoked": 0} against a credential that is already gone -- which is exactly
// why it is worth a test: nothing about the output would reveal it.
func TestRevokingItsOwnSessionDoesNotLogOutTwice(t *testing.T) {
	s := &credentialServer{revokeCurrent: true}
	c := newCredentialServer(t, s)

	if err := cmdCredential(c, []string{"revoke", "b"}); err != nil {
		t.Fatalf("credential revoke: %v", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.revoked) != 1 {
		t.Fatalf("revoked %v, want one id", s.revoked)
	}
	if s.loggedOut != 0 {
		t.Errorf("logged out %d times after revoking its own session; the revoke was the logout", s.loggedOut)
	}
}

// A failed revocation still signs the command's session out. The error the
// person asked about is what comes back, but the row does not survive it.
func TestAFailedRevokeStillSignsOut(t *testing.T) {
	s := &credentialServer{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		switch r.URL.Path {
		case "/api/silo/v1/auth/login":
			_ = json.NewEncoder(w).Encode(map[string]string{"token": "session-token"})
		case "/api/silo/v1/auth/logout":
			s.loggedOut++
			_ = json.NewEncoder(w).Encode(map[string]int{"revoked": 1})
		default:
			http.Error(w, "No such credential", http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	c := client.NewClient(srv.URL)
	if err := c.Login("dan@example.com", "pw"); err != nil {
		t.Fatalf("login: %v", err)
	}
	c.UserAgent = "silo credential"

	if err := cmdCredential(c, []string{"revoke", "nope"}); err == nil {
		t.Error("revoking an unknown id returned no error")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loggedOut != 1 {
		t.Errorf("logged out %d times after a failed revoke, want 1", s.loggedOut)
	}
}

// A spent client does not quietly sign back in.
//
// doRequest re-logs in and retries once on a 401, which is right for a long
// session and wrong immediately after a logout: a client used again would mint
// a second credential and leave it behind, which is the litter the logout was
// for.
func TestAClientDoesNotSignBackInAfterLoggingOut(t *testing.T) {
	s := &credentialServer{}
	c := newCredentialServer(t, s)

	if err := c.Logout(); err != nil {
		t.Fatalf("logout: %v", err)
	}
	if _, err := c.ListCredentials(); err == nil {
		t.Error("a request after logout succeeded; the client signed itself back in")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loggedOut != 1 {
		t.Errorf("logged out %d times, want 1", s.loggedOut)
	}
}
