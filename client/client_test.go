package client

import (
	"encoding/json"
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
		{"/", "/api/silo/v1/repos/r1/entries/"},
		{"", "/api/silo/v1/repos/r1/entries/"},
		{"/notes", "/api/silo/v1/repos/r1/entries/notes"},
		// Separators stay separators, or the route stops matching.
		{"/a/b/c.txt", "/api/silo/v1/repos/r1/entries/a/b/c.txt"},
		{"a/b", "/api/silo/v1/repos/r1/entries/a/b"},
		{"/trailing/", "/api/silo/v1/repos/r1/entries/trailing"},
		// A "?" in a name would otherwise start the query string and truncate
		// the path — this is the case QueryEscape gets wrong in the other
		// direction, by turning a space into "+".
		{"/awkward name?.txt", "/api/silo/v1/repos/r1/entries/awkward%20name%3F.txt"},
		{"/hash#tag.txt", "/api/silo/v1/repos/r1/entries/hash%23tag.txt"},
		{"/100%.txt", "/api/silo/v1/repos/r1/entries/100%25.txt"},
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
			path:   "/api/silo/v1/repos/r1/entries/notes",
		},
		{
			name:   "ListDir at the root",
			call:   func(c *APIClient) error { _, err := c.ListDir("r1", "/"); return err },
			method: "GET",
			path:   "/api/silo/v1/repos/r1/entries/",
		},
		{
			// Without ?type=dir this would be a file upload, which the server
			// does not accept — so the parameter is not optional.
			name:   "Mkdir asks for a directory explicitly",
			call:   func(c *APIClient) error { return c.Mkdir("r1", "/notes") },
			method: "PUT",
			path:   "/api/silo/v1/repos/r1/entries/notes",
			query:  "type=dir",
		},
		{
			name:   "DeleteFile",
			call:   func(c *APIClient) error { return c.DeleteFile("r1", "/notes/a.txt") },
			method: "DELETE",
			path:   "/api/silo/v1/repos/r1/entries/notes/a.txt",
		},
		{
			name:   "MoveFile",
			call:   func(c *APIClient) error { return c.MoveFile("r1", "/a.txt", "/sub/a.txt") },
			method: "POST",
			path:   "/api/silo/v1/repos/r1/entries/a.txt",
			body:   map[string]string{"op": "move", "to": "/sub/a.txt"},
		},
		{
			// A rename is a move whose destination shares a parent with its
			// source; the server has no separate operation for it.
			name:   "RenameFile becomes a move within the same parent",
			call:   func(c *APIClient) error { return c.RenameFile("r1", "/notes/old.txt", "new.txt") },
			method: "POST",
			path:   "/api/silo/v1/repos/r1/entries/notes/old.txt",
			body:   map[string]string{"op": "move", "to": "/notes/new.txt"},
		},
		{
			name:   "RenameFile at the root",
			call:   func(c *APIClient) error { return c.RenameFile("r1", "/old.txt", "new.txt") },
			method: "POST",
			path:   "/api/silo/v1/repos/r1/entries/old.txt",
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
	if want := "/api/silo/v1/repos/r1/changes"; got.path != want {
		t.Errorf("path = %s, want %s", got.path, want)
	}
	if want := "since=abc123"; got.query != want {
		t.Errorf("query = %q, want %q", got.query, want)
	}
}
