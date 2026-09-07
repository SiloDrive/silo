package silod

import (
	"context"
	"encoding/json"
	"github.com/dkam/silo/fileserver/account"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/dkam/silo/fileserver/admin"
	"github.com/dkam/silo/fileserver/api"
	"github.com/dkam/silo/fileserver/authmgr"
	"github.com/dkam/silo/fileserver/dbutil"
	"github.com/dkam/silo/fileserver/invite"
	"github.com/dkam/silo/fileserver/notif"
	"github.com/dkam/silo/fileserver/option"
	"github.com/dkam/silo/fileserver/setup"
	"github.com/dkam/silo/fileserver/share"
	"github.com/dkam/silo/internal/lexicon"
)

// The wire says library.
//
// This is the tier with consumers. silo-drive funnels every URL it builds
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

// serveTestAPI points every store the router reaches at a fresh database and
// runs the real router over it, returning the base URL.
//
// Shared with unclaimed (setup_wire_test.go), which needs the same server
// without the account. A store left out here is a nil *sql.DB that panics on
// first use rather than failing to compile, so the list is worth having in one
// place: a new dependency of the router is then one edit, not two.
func serveTestAPI(t *testing.T) string {
	t.Helper()
	sqliteTestDB(t)
	share.Init(siloPair.Read, siloPair.Write)
	api.Init(siloPair.Read, siloPair.Write)
	authmgr.Init(siloPair.Read, siloPair.Write)
	admin.Init(siloPair.Read, siloPair.Write)
	setup.Init(siloPair.Read, siloPair.Write)
	invite.Init(siloPair.Read, siloPair.Write)

	// serveHandler rather than the bare router, so a wire test exercises the
	// stack the server actually runs.
	srv := httptest.NewServer(serveHandler(newHTTPRouter(), false))
	t.Cleanup(srv.Close)
	return srv.URL
}

// wire stands the server up and returns its base URL and a bearer token.
func wire(t *testing.T) (base, token string) {
	t.Helper()
	base = serveTestAPI(t)

	const email, password = "wire@example.com", "correct horse battery staple"
	if _, err := authmgr.CreateAccount(context.Background(), email, password, account.RoleUser); err != nil {
		t.Fatalf("create account: %v", err)
	}

	body := strings.NewReader(`{"email":"` + email + `","password":"` + password + `"}`)
	resp, err := http.Post(base+"/api/silo/v1/auth/login", "application/json", body)
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
	return base, out.Token
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
		{"POST", "/api/silo/v1/libraries/" + id + "/chunks/missing"},

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

// TestTheIDAddressedRoutesAnswer covers /chunks/{id} and /objects/{id}, which
// every other test in this package reaches by calling the handler with a vars
// map — so nothing asserted they were on the router at all. silo-drive does not
// exercise either, so its strict fake cannot catch it either.
//
// They cannot be measured the way the routes above are. Those answer 404 only
// when unregistered; these answer 404 for an object nobody stored, which is
// the same status for the opposite reason — the first draft of this test
// "passed" by finding a route it had just deleted, because the surviving route
// beside it 404'd for a missing chunk. So the probe is a method the route does
// not serve: mux answers 405 when the path matched and the method did not, and
// 404 only when nothing matched at all. 405 is the proof of registration.
func TestTheIDAddressedRoutesAnswer(t *testing.T) {
	base, token := wire(t)
	id := makeLibrary(t, base, token)

	// Sixty-four hex characters: the routes pin their variable to that width,
	// so a shorter placeholder would miss the route and report a registered
	// route as absent.
	const hexID = "0000000000000000000000000000000000000000000000000000000000000000"

	for _, path := range []string{
		"/api/silo/v1/libraries/" + id + "/chunks/" + hexID,
		"/api/silo/v1/libraries/" + id + "/objects/" + hexID,
	} {
		code, _ := call(t, "POST", base+path, token, "")
		if code == http.StatusNotFound {
			t.Errorf("POST %s: 404, so the route is not registered "+
				"(a registered route answers 405 to a method it does not serve)", path)
		}
	}
}

// TestAListingEntryCarriesExactlyTheseKeys pins what a client actually
// receives, because the brief had been promising a key that no code path sets.
//
// dirEntry carried a Modifier field, tagged omitempty and never once assigned,
// so it could not reach the wire — but it could and did reach the
// documentation, whose listing example showed "modifier":"…" on every file row.
// A client written to that example waits for a field that will never arrive.
// Nothing catches that: the server is correct, the struct compiles, and the
// only disagreement is between a document and a value no test looked at.
func TestAListingEntryCarriesExactlyTheseKeys(t *testing.T) {
	base, token := wire(t)
	id := makeLibrary(t, base, token)
	lib := base + "/api/silo/v1/libraries/" + id

	if code, body := call(t, "PUT", lib+"/entries/sub?type=dir", token, ""); code >= 300 {
		t.Fatalf("mkdir: %d %s", code, body)
	}
	if code, body := call(t, "PUT", lib+"/entries/a.txt", token, "hi"); code >= 300 {
		t.Fatalf("put file: %d %s", code, body)
	}

	_, body := call(t, "GET", lib+"/entries/", token, "")
	var rows []map[string]any
	if err := json.Unmarshal([]byte(body), &rows); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2: %s", len(rows), body)
	}

	want := map[string][]string{
		"dir":  {"id", "mtime", "name", "type"},
		"file": {"id", "mtime", "name", "size", "type"},
	}
	for _, row := range rows {
		kind, _ := row["type"].(string)
		got := make([]string, 0, len(row))
		for k := range row {
			got = append(got, k)
		}
		sort.Strings(got)
		if !slices.Equal(got, want[kind]) {
			t.Errorf("a %s row carries %v, want %v", kind, got, want[kind])
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
		{"POST", gone + "/" + id + "/chunks/missing"},
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

// TestServerInfoNamesTheVocabulary is the ask silo-drive made when told this was
// coming, and it is worth more than it looks. A silo-drive built for /libraries
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
// client gets it wrong. This one is a string in a frame, and silo-drive's frame
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
	_ = dbutil.Prepare // the schema under test is the one this builds
}

// The access-token lane is gone. It minted a stateful capability token for
// /files/{token}/, and that route was deleted with the rest of the legacy
// lane -- so the endpoint went on handing clients a token nothing could
// redeem, and protocol.md went on advertising it. docs/capability-urls.md is
// the record of why a signed URL, not this, is what to build if a browser
// ever needs one.
func TestTheAccessTokenLaneIsGone(t *testing.T) {
	base, token := wire(t)
	id := makeLibrary(t, base, token)

	body := `{"library_id":"` + id + `","op":"download"}`
	if code, _ := call(t, "POST", base+"/api/silo/v1/access-tokens", token, body); code != http.StatusNotFound {
		t.Errorf("POST /access-tokens: status %d, want 404", code)
	}
}

// The account ring has a feature name, because the fallback is a whole code
// path: without it a client cannot tell "this server has no account mode"
// from "this account is quiet", and those two look identical from the
// outside. One name covers both rings -- a client sends the same frame and
// the credential decides what it gets.
func TestServerInfoAdvertisesTheAccountRing(t *testing.T) {
	orig := option.EnableNotification
	t.Cleanup(func() { option.EnableNotification = orig })
	option.EnableNotification = true

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
	if !slices.Contains(out.Features, "notifications-account") {
		t.Errorf("features = %v, want \"notifications-account\"", out.Features)
	}
}
