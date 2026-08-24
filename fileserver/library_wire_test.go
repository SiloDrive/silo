package silod

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dkam/silo/fileserver/api"
	"github.com/dkam/silo/fileserver/authmgr"
	"github.com/dkam/silo/fileserver/dbutil"
	"github.com/dkam/silo/fileserver/notif"
	"github.com/dkam/silo/fileserver/share"
	"github.com/dkam/silo/internal/lexicon"
)

// The wire says library.
//
// This is the tier with consumers. porter-fuse funnels every URL it builds
// through one package and reimplements this routing table as a strict fake, so
// what is written here is a contract with another codebase rather than an
// internal name. The lexicon guard in internal/lexicon would catch a route
// that still spelled it the old way, but only as a string in a source file —
// these tests ask the router, which is the only thing that answers for what a
// client will actually get.
//
// The old routes are asserted gone, not merely un-preferred. v1 has never
// shipped, so there is no shim and no second vocabulary: a client built for
// the old spelling gets 404, loudly, on its first call.

// wire stands the server up and returns its base URL and a bearer token.
func wire(t *testing.T) (base, token string) {
	t.Helper()
	sqliteTestDB(t)
	share.Init(siloPair.Read, "Group", false)
	api.Init(siloPair.Read, siloPair.Write)
	authmgr.Init(siloPair.Read, siloPair.Write)

	const email, password = "wire@example.com", "correct horse battery staple"
	if _, err := authmgr.CreateAccount(context.Background(), email, password, false); err != nil {
		t.Fatalf("create account: %v", err)
	}

	srv := httptest.NewServer(newHTTPRouter())
	t.Cleanup(srv.Close)

	body := strings.NewReader(`{"email":"` + email + `","password":"` + password + `"}`)
	resp, err := http.Post(srv.URL+"/api/silo/v1/auth/login", "application/json", body)
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	defer resp.Body.Close()
	var out struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode login: %v", err)
	}
	if out.Token == "" {
		t.Fatal("login returned no token")
	}
	return srv.URL, out.Token
}

func call(t *testing.T, method, url, token, body string) (int, string) {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, r)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(out)
}

// makeLibrary creates one and returns its id, so the id-scoped routes are
// asked about a library that exists. A 404 from a route that exists but was
// handed a stranger's id would be indistinguishable from a route that is not
// registered, which is the whole thing these tests are measuring.
func makeLibrary(t *testing.T, base, token string) string {
	t.Helper()
	code, body := call(t, "POST", base+"/api/silo/v1/libraries", token, `{"name":"wire"}`)
	if code != http.StatusCreated && code != http.StatusOK {
		t.Fatalf("creating a library: status %d, body %s", code, body)
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	return out.ID
}

func TestTheLibraryRoutesAnswer(t *testing.T) {
	base, token := wire(t)
	id := makeLibrary(t, base, token)

	// Not asserting a success code: several of these want a body this test has
	// no business constructing, and one of them would commit if it got one.
	// What is being measured is registration — a 404 here means the route is
	// not on the router, and every other status means it is.
	for _, c := range []struct{ method, path string }{
		{"GET", "/api/silo/v1/libraries"},
		{"GET", "/api/silo/v1/libraries/" + id + "/changes"},
		{"PATCH", "/api/silo/v1/libraries/" + id},
		{"POST", "/api/silo/v1/libraries/" + id + "/batch"},
		{"POST", "/api/silo/v1/libraries/" + id + "/blocks/missing"},
		{"GET", "/api/silo/v1/libraries/" + id + "/entries/"},
		{"PUT", "/api/silo/v1/libraries/" + id + "/head"},
	} {
		code, body := call(t, c.method, base+c.path, token, "")
		if code == http.StatusNotFound {
			t.Errorf("%s %s: 404, so the route is not registered (%s)",
				c.method, c.path, strings.TrimSpace(body))
		}
	}
}

func TestTheRetiredRoutesAreGone(t *testing.T) {
	base, token := wire(t)
	id := makeLibrary(t, base, token)

	// Built from lexicon.RetiredSegment rather than written out, because the
	// sweep that produced this file rewrote every literal of the old word —
	// including, in the first draft of this test, the seven that were the
	// point of it. It then asserted that the new routes 404, against a server
	// that serves them.
	gone := "/api/silo/v1/" + lexicon.RetiredSegment
	for _, c := range []struct{ method, path string }{
		{"GET", gone},
		{"POST", gone},
		{"GET", gone + "/" + id + "/changes"},
		{"POST", gone + "/" + id + "/batch"},
		{"POST", gone + "/" + id + "/blocks/missing"},
		{"GET", gone + "/" + id + "/entries/"},
		{"PUT", gone + "/" + id + "/head"},
	} {
		code, _ := call(t, c.method, base+c.path, token, "")
		if code != http.StatusNotFound {
			t.Errorf("%s %s: status %d, want 404 — the old spelling is meant to be gone, not aliased",
				c.method, c.path, code)
		}
	}
}

// TestServerInfoNamesTheVocabulary is the ask porter made when told this was
// coming, and it is worth more than it looks. A porter built for /libraries
// pointed at a server that still says /libraries gets 404 on the listing, and a
// 404 on the listing reaches a person as an account with nothing in it —
// working software that happens to be empty, which is the worst way for a
// version mismatch to present. A name in the features list lets the client say
// which of the two is out of date instead of showing an empty tree.
func TestServerInfoNamesTheVocabulary(t *testing.T) {
	base, _ := wire(t)
	code, body := call(t, "GET", base+"/api/silo/v1/server-info", "", "")
	if code != http.StatusOK {
		t.Fatalf("server-info: status %d", code)
	}
	var out struct {
		Features []string `json:"features"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	has := func(name string) bool {
		for _, f := range out.Features {
			if f == name {
				return true
			}
		}
		return false
	}
	if !has("libraries") {
		t.Errorf("features = %v, want it to include \"libraries\"", out.Features)
	}
	if !has("library-rename") {
		t.Errorf("features = %v, want \"library-rename\"", out.Features)
	}
	if has(lexicon.RetiredNoun + "-rename") {
		t.Errorf("features = %v, still advertises the old name", out.Features)
	}
}

// TestTheNotifyEventTypeSaysLibrary is here rather than in notif because of
// how it fails. Every other name in this sweep fails with a 404 the moment a
// client gets it wrong. This one is a string in a frame, and porter's frame
// switch ignores types it does not recognise by design — an unknown type is
// treated as latency, since polling still covers correctness. So a client that
// missed this rename does not break; it silently stops receiving push and
// nothing anywhere logs it. That is the one failure in this sweep worth a test
// of its own.
func TestTheNotifyEventTypeSaysLibrary(t *testing.T) {
	if notif.EventTypeLibraryUpdate != "library-update" {
		t.Errorf("notify event type = %q, want %q",
			notif.EventTypeLibraryUpdate, "library-update")
	}
}

// TestNoTableOrColumnSaysLibrary asks the database rather than the schema text.
// A CREATE TABLE that was renamed but left a library_id column behind would pass
// a grep over the schema constant and fail here, which is the right way round.
func TestNoTableOrColumnSaysLibrary(t *testing.T) {
	sqliteTestDB(t)

	rows, err := siloPair.Read.Query(
		`SELECT name FROM sqlite_master WHERE type IN ('table','index') ORDER BY name`)
	if err != nil {
		t.Fatalf("reading sqlite_master: %v", err)
	}
	defer rows.Close()

	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatalf("scan: %v", err)
		}
		names = append(names, n)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if len(names) == 0 {
		t.Fatal("no tables at all, so this test would pass for the wrong reason")
	}

	for _, n := range names {
		if lexicon.SaysOldWord(n) {
			t.Errorf("table or index %q still says library", n)
		}
		cols, err := siloPair.Read.Query(`SELECT name FROM pragma_table_info(?)`, n)
		if err != nil {
			continue // an index has no columns of its own
		}
		for cols.Next() {
			var c string
			if err := cols.Scan(&c); err != nil {
				t.Fatalf("scan column: %v", err)
			}
			if lexicon.SaysOldWord(c) {
				t.Errorf("column %s.%s still says library", n, c)
			}
		}
		cols.Close()
	}
	_ = dbutil.CreateSiloTables // the schema under test is the one this builds
}
