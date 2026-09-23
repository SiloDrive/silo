package client

import (
	"encoding/json"
	"github.com/SiloDrive/silo/store"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestEntriesURLEscapesPerSegment(t *testing.T) {
	cases := []struct {
		path string
		want string
	}{
		// The root is the empty match: "entries/" with nothing after it.
		{"/", "/api/silo/v1/libraries/r1/entries/"},
		{"", "/api/silo/v1/libraries/r1/entries/"},
		{"/notes", "/api/silo/v1/libraries/r1/entries/notes"},
		// Separators stay separators, or the route stops matching.
		{"/a/b/c.txt", "/api/silo/v1/libraries/r1/entries/a/b/c.txt"},
		{"a/b", "/api/silo/v1/libraries/r1/entries/a/b"},
		{"/trailing/", "/api/silo/v1/libraries/r1/entries/trailing"},
		// A "?" in a name would otherwise start the query string and truncate
		// the path — this is the case QueryEscape gets wrong in the other
		// direction, by turning a space into "+".
		{"/awkward name?.txt", "/api/silo/v1/libraries/r1/entries/awkward%20name%3F.txt"},
		{"/hash#tag.txt", "/api/silo/v1/libraries/r1/entries/hash%23tag.txt"},
		{"/100%.txt", "/api/silo/v1/libraries/r1/entries/100%25.txt"},
	}
	for _, c := range cases {
		if got := entriesURL("r1", c.path); got != c.want {
			t.Errorf("entriesURL(%q) = %q, want %q", c.path, got, c.want)
		}
	}
}

// recorded is what the fake server saw, so a test can assert on the request
// rather than on a response it made up itself.
type recorded struct {
	method string
	path   string
	query  string
	body   string
}

// newFakeServer answers every request with 200 and the given JSON, and records
// what it was asked. These tests are about the shape of what the client sends,
// so the response only has to be decodable into whatever the method returns.
func newFakeServer(t *testing.T, got *recorded, response string) *APIClient {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		*got = recorded{method: r.Method, path: r.URL.Path, query: r.URL.RawQuery, body: string(body)}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(response))
	}))
	t.Cleanup(srv.Close)
	return NewClient(srv.URL)
}

func TestFileOperationsUseTheEntriesEndpoint(t *testing.T) {
	cases := []struct {
		name   string
		call   func(*APIClient) error
		method string
		path   string
		query  string
		body   map[string]string
	}{
		{
			name:   "ListDir",
			call:   func(c *APIClient) error { _, err := c.ListDir("r1", "/notes"); return err },
			method: "GET",
			path:   "/api/silo/v1/libraries/r1/entries/notes",
		},
		{
			name:   "ListDir at the root",
			call:   func(c *APIClient) error { _, err := c.ListDir("r1", "/"); return err },
			method: "GET",
			path:   "/api/silo/v1/libraries/r1/entries/",
		},
		{
			// Without ?type=dir this would be a file upload, which the server
			// does not accept — so the parameter is not optional.
			name:   "Mkdir asks for a directory explicitly",
			call:   func(c *APIClient) error { return c.Mkdir("r1", "/notes") },
			method: "PUT",
			path:   "/api/silo/v1/libraries/r1/entries/notes",
			query:  "type=dir",
		},
		{
			name:   "DeleteFile",
			call:   func(c *APIClient) error { return c.DeleteFile("r1", "/notes/a.txt") },
			method: "DELETE",
			path:   "/api/silo/v1/libraries/r1/entries/notes/a.txt",
		},
		{
			name:   "MoveFile",
			call:   func(c *APIClient) error { return c.MoveFile("r1", "/a.txt", "/sub/a.txt") },
			method: "POST",
			path:   "/api/silo/v1/libraries/r1/entries/a.txt",
			body:   map[string]string{"op": "move", "to": "/sub/a.txt"},
		},
		{
			// A rename is a move whose destination shares a parent with its
			// source; the server has no separate operation for it.
			name:   "RenameFile becomes a move within the same parent",
			call:   func(c *APIClient) error { return c.RenameFile("r1", "/notes/old.txt", "new.txt") },
			method: "POST",
			path:   "/api/silo/v1/libraries/r1/entries/notes/old.txt",
			body:   map[string]string{"op": "move", "to": "/notes/new.txt"},
		},
		{
			name:   "RenameFile at the root",
			call:   func(c *APIClient) error { return c.RenameFile("r1", "/old.txt", "new.txt") },
			method: "POST",
			path:   "/api/silo/v1/libraries/r1/entries/old.txt",
			body:   map[string]string{"op": "move", "to": "/new.txt"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var got recorded
			client := newFakeServer(t, &got, "[]")
			if err := c.call(client); err != nil {
				t.Fatalf("call failed: %v", err)
			}
			if got.method != c.method {
				t.Errorf("method = %s, want %s", got.method, c.method)
			}
			if got.path != c.path {
				t.Errorf("path = %s, want %s", got.path, c.path)
			}
			if got.query != c.query {
				t.Errorf("query = %q, want %q", got.query, c.query)
			}
			if c.body == nil {
				if got.body != "" {
					t.Errorf("body = %q, want none", got.body)
				}
				return
			}
			var body map[string]string
			if err := json.Unmarshal([]byte(got.body), &body); err != nil {
				t.Fatalf("body %q is not JSON: %v", got.body, err)
			}
			for k, want := range c.body {
				if body[k] != want {
					t.Errorf("body[%q] = %q, want %q", k, body[k], want)
				}
			}
		})
	}
}

func TestChangesPassesSinceAsAQueryParameter(t *testing.T) {
	var got recorded
	client := newFakeServer(t, &got, `{"anchor":"x","changes":[]}`)
	if _, err := client.Changes("r1", "abc123"); err != nil {
		t.Fatalf("Changes failed: %v", err)
	}
	if got.method != "GET" {
		t.Errorf("method = %s, want GET", got.method)
	}
	if want := "/api/silo/v1/libraries/r1/changes"; got.path != want {
		t.Errorf("path = %s, want %s", got.path, want)
	}
	if want := "since=abc123"; got.query != want {
		t.Errorf("query = %q, want %q", got.query, want)
	}
}

// A password change signs out session credentials, and the TUI's own token is
// one of them -- credential.KindSession is "the TUI, the CLI". So the request
// that succeeds is also the request that invalidates the caller, and the
// client has to come back from that on its own: it caches the password it
// logged in with and replays it on any 401, which after this call is the old
// one. Left alone, changing a password signs you out of the client you changed
// it from, and the message you get is a 401 about a password you just proved
// you knew.
func TestChangingThePasswordLeavesTheClientSignedIn(t *testing.T) {
	current := "old-secret"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req map[string]string
		_ = json.Unmarshal(body, &req)

		switch r.URL.Path {
		case "/api/silo/v1/auth/kdf":
			// A change now asks what this address derives under, because on an
			// enrolled account it has an identity key to re-wrap. This account
			// has none, so the answer only has to parse.
			_, _ = w.Write([]byte(`{"kdf_params":"` +
				store.DefaultKDFParams([store.KDFSaltSize]byte{1, 2, 3}).String() + `"}`))
		case "/api/silo/v1/account/keys":
			// No identity key, which is what makes this an ordinary password
			// change rather than a re-wrap.
			http.Error(w, "This account has published no identity key", http.StatusNotFound)
		case "/api/silo/v1/auth/login":
			if req["password"] != current {
				http.Error(w, "Invalid password", http.StatusUnauthorized)
				return
			}
			// A new token each time, so a stale one is refused below rather
			// than accidentally still working.
			_, _ = w.Write([]byte(`{"token":"token-for-` + req["password"] + `"}`))
		case "/api/silo/v1/auth/password":
			if req["current_password"] != current {
				http.Error(w, "Invalid password", http.StatusUnauthorized)
				return
			}
			current = req["new_password"]
			// Every session credential is revoked, this one included.
			_, _ = w.Write([]byte(`{"revoked":3}`))
		default:
			if r.Header.Get("Authorization") != "Bearer token-for-"+current {
				http.Error(w, "Unauthorized", http.StatusUnauthorized)
				return
			}
			_, _ = w.Write([]byte(`[]`))
		}
	}))
	t.Cleanup(srv.Close)

	c := NewClient(srv.URL)
	if err := c.Login("someone@example.com", "old-secret"); err != nil {
		t.Fatalf("login: %v", err)
	}

	revoked, err := c.ChangePassword("old-secret", "new-secret")
	if err != nil {
		t.Fatalf("ChangePassword: %v", err)
	}
	if revoked != 3 {
		t.Errorf("revoked = %d, want 3", revoked)
	}

	// The point of the test. The old token is dead, so this drives the
	// automatic re-login, which has to present the new password.
	if _, err := c.ListLibraries(); err != nil {
		t.Errorf("a call after the change: %v -- the client re-authenticated with the old password", err)
	}
}

// SILO_URL typed with a trailing slash is the same server. Joined naively it
// put "//api/..." on the wire, which the router answers with 404 -- so every
// command failed with a message that pointed at the server rather than at a
// slash.
func TestABaseURLWithATrailingSlashReachesTheSameRoutes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/silo/v1/server-info" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"version":"test","features":[]}`))
	}))
	defer srv.Close()

	if _, err := NewClient(srv.URL + "/").GetServerInfo(); err != nil {
		t.Fatalf("server-info through %s/: %v", srv.URL, err)
	}
}
