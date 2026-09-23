package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// loginServer is a server with no identity provider: sign-in is by password,
// and it records what the CLI asked of it.
type loginServer struct {
	mu          sync.Mutex
	enrolments  int      // auth/login asking for a named credential
	sessions    int      // auth/login asking for a plain session
	loggedOut   []string // the bearer each auth/logout presented
	issued      int
	clientNames []string
}

func (s *loginServer) start(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		switch r.URL.Path {
		case "/api/silo/v1/server-info":
			_ = json.NewEncoder(w).Encode(map[string]any{"version": "test", "features": []string{"libraries"}})
		case "/api/silo/v1/auth/login":
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["kind"] == "" {
				s.sessions++
				_ = json.NewEncoder(w).Encode(map[string]string{"token": "session-token"})
				return
			}
			s.enrolments++
			s.issued++
			s.clientNames = append(s.clientNames, body["client_name"])
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"credential": fmt.Sprintf("stored-%d", s.issued), "email": body["email"], "expires_at": 1893456000,
			})
		case "/api/silo/v1/auth/logout":
			s.loggedOut = append(s.loggedOut, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
			_ = json.NewEncoder(w).Encode(map[string]int{"revoked": 1})
		case "/api/silo/v1/libraries":
			auth := r.Header.Get("Authorization")
			if !strings.HasPrefix(auth, "Bearer stored-") && auth != "Bearer session-token" {
				http.Error(w, "Unauthorized", http.StatusUnauthorized)
				return
			}
			_ = json.NewEncoder(w).Encode([]map[string]any{})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// isolate points the credential store at a directory of the test's own and
// captures what login prints.
func isolate(t *testing.T) (configDir string, out *bytes.Buffer) {
	t.Helper()
	configDir = t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configDir)
	out = &bytes.Buffer{}
	was := stdout
	stdout = out
	t.Cleanup(func() { stdout = was })
	return configDir, out
}

func TestLoginStoresACredentialThatLaterCommandsUse(t *testing.T) {
	dir, out := isolate(t)
	s := &loginServer{}
	url := s.start(t)

	if err := Run(url, "dan@example.com", "pw", []string{"login"}); err != nil {
		t.Fatalf("login: %v", err)
	}
	if !strings.Contains(out.String(), "Signed in to "+url+" as dan@example.com") {
		t.Errorf("login said %q", out.String())
	}

	path := filepath.Join(dir, "silo", "credentials.json")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("no credential stored: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("the credential file is mode %o; it opens the account, so it must be 0600", mode)
	}

	// No address or password this time: the stored credential is presented,
	// and nothing signs in again.
	if err := Run(url+"/", "", "", []string{"libraries"}); err != nil {
		t.Fatalf("libraries with the stored credential: %v", err)
	}
	if s.enrolments != 1 || s.sessions != 0 {
		t.Errorf("enrolments=%d sessions=%d; want one enrolment and no session", s.enrolments, s.sessions)
	}
	if len(s.clientNames) != 1 || !strings.HasPrefix(s.clientNames[0], "silo CLI") {
		t.Errorf("the credential was labelled %v", s.clientNames)
	}
	// And a command using it leaves it alone.
	if len(s.loggedOut) != 0 {
		t.Errorf("a command signed the stored credential out: %v", s.loggedOut)
	}
}

func TestNotSignedInSaysHowToSignIn(t *testing.T) {
	isolate(t)
	url := (&loginServer{}).start(t)
	err := Run(url, "", "", []string{"libraries"})
	if err == nil || !strings.Contains(err.Error(), "silo login") {
		t.Fatalf("libraries with nothing = %v; want it to point at silo login", err)
	}
}

// Scripts that set a password keep working exactly as they did.
func TestTheEnvironmentWinsOverAStoredCredential(t *testing.T) {
	isolate(t)
	s := &loginServer{}
	url := s.start(t)
	if err := Run(url, "dan@example.com", "pw", []string{"login"}); err != nil {
		t.Fatal(err)
	}
	if err := Run(url, "dan@example.com", "pw", []string{"libraries"}); err != nil {
		t.Fatal(err)
	}
	if s.sessions != 1 {
		t.Errorf("sessions = %d; with SILO_EMAIL and SILO_PASSWORD set, the command should sign in with them", s.sessions)
	}
}

func TestLogoutRevokesAndForgets(t *testing.T) {
	isolate(t)
	s := &loginServer{}
	url := s.start(t)
	if err := Run(url, "dan@example.com", "pw", []string{"login"}); err != nil {
		t.Fatal(err)
	}
	if err := Run(url, "", "", []string{"logout"}); err != nil {
		t.Fatalf("logout: %v", err)
	}
	if len(s.loggedOut) != 1 || s.loggedOut[0] != "stored-1" {
		t.Errorf("logout presented %v; want the stored credential", s.loggedOut)
	}
	if err := Run(url, "", "", []string{"libraries"}); err == nil {
		t.Error("a command still ran after logout")
	}
}

// Signing in again replaces this host's credential and discards the old one,
// or every re-login would leave a ninety-day row in the list.
func TestLoggingInAgainDiscardsTheOldCredential(t *testing.T) {
	isolate(t)
	s := &loginServer{}
	url := s.start(t)
	for range 2 {
		if err := Run(url, "dan@example.com", "pw", []string{"login"}); err != nil {
			t.Fatal(err)
		}
	}
	if len(s.loggedOut) != 1 || s.loggedOut[0] != "stored-1" {
		t.Errorf("logged out %v; want the first credential discarded", s.loggedOut)
	}
	st, err := readStore()
	if err != nil {
		t.Fatal(err)
	}
	if got := st.Servers[serverKey(url)].Credential; got != "stored-2" {
		t.Errorf("stored %q, want the second credential", got)
	}
}

// The same host signs in with the same client id every time, so the account's
// list can tell a re-login from a new machine.
func TestAHostKeepsOneClientID(t *testing.T) {
	isolate(t)
	s := &loginServer{}
	url := s.start(t)
	var ids []string
	for range 2 {
		if err := Run(url, "dan@example.com", "pw", []string{"login"}); err != nil {
			t.Fatal(err)
		}
		st, _ := readStore()
		ids = append(ids, st.ClientID)
	}
	if ids[0] == "" || ids[0] != ids[1] {
		t.Errorf("client ids %v; want one, kept", ids)
	}
}

var _ io.Writer = (*bytes.Buffer)(nil)
