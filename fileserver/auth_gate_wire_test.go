package silod

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/dkam/silo/fileserver/account"
	"github.com/dkam/silo/fileserver/credential"
	"github.com/dkam/silo/fileserver/option"
	"github.com/gorilla/mux"
)

// The door, asked about every route at once.
//
// Every other test in this package picks a route and asks what it does. That
// leaves the gate proved exactly where somebody thought to prove it, which is
// the shape of coverage that misses the route added last -- and the route
// added last is the one nobody has looked at. So this sweep does not carry a
// list of routes. It asks the router for its own, walks it, and requires each
// one to refuse a caller who has proved nothing.
//
// The consequence is the point: a new route is covered the day it is
// registered, and a new *unauthenticated* route fails this test until somebody
// writes it into unauthenticatedRoutes below and says why. Opening a route to
// the world becomes an edit somebody has to make on purpose rather than an
// omission nothing notices.

// unauthenticatedRoutes is every path that is meant to answer a caller
// carrying nothing, with the reason it is on the list. Adding to it is the
// deliberate act described above; nothing else belongs here.
var unauthenticatedRoutes = map[string]string{
	// There is nothing yet to authenticate as. Each of these is guarded by
	// something other than a credential, and server.go says what at each
	// registration.
	"/api/silo/v1/auth/login":  "this is how a caller gets a credential",
	"/api/silo/v1/auth/setup":  "creates the first account; guarded by the setup token",
	"/api/silo/v1/auth/kdf":    "pre-login parameters; a client needs these to build what it sends",
	"/api/silo/v1/auth/redeem": "activates an invited account; guarded by the invite token",

	// Carries no data of its own.
	"/api/silo/v1/server-info": "the feature list, which a client reads before it can log in",
	"/admin":                   "a form and two empty tables; every number on it arrives by fetch",
}

// routeVars is what to put where a path template has a variable. The value has
// to match the route's own regex or the request lands on no route at all and
// answers 404 -- which would fail this test for the wrong reason, so each one
// is chosen to match.
type routeVars struct {
	libraryID string
	accountID string
}

func (v routeVars) value(spec string) (string, bool) {
	name, _, _ := strings.Cut(spec, ":")
	switch {
	// An object or chunk id, pinned to sixty-four hex characters by the route
	// itself. Checked before the name, because the admin routes also spell
	// their variable "id" and want something else entirely.
	case strings.Contains(spec, "[0-9a-f]{64}"):
		return strings.Repeat("0", 64), true
	case name == "libraryid":
		return v.libraryID, true
	case name == "id":
		return v.accountID, true
	case name == "ordinal":
		return "0", true
	case name == "principal":
		return "user:" + v.accountID, true
	case name == "path":
		// The library root. entries/{path:.*} matches an empty path, which is
		// the listing.
		return "", true
	}
	return "", false
}

// fillRouteVars turns a mux path template into a path a client could send.
//
// It counts braces rather than matching to the first "}", because a route's
// regex has braces of its own: {id:[0-9a-f]{64}} would otherwise be cut in the
// middle of its own repetition count and leave a stray brace in the URL.
func fillRouteVars(t *testing.T, tmpl string, vars routeVars) string {
	t.Helper()
	var out strings.Builder
	for i := 0; i < len(tmpl); {
		if tmpl[i] != '{' {
			out.WriteByte(tmpl[i])
			i++
			continue
		}
		depth, j := 1, i+1
		for ; j < len(tmpl) && depth > 0; j++ {
			switch tmpl[j] {
			case '{':
				depth++
			case '}':
				depth--
			}
		}
		spec := tmpl[i+1 : j-1]
		value, ok := vars.value(spec)
		if !ok {
			t.Fatalf("route %s has a variable %q this test does not know how to fill; "+
				"teach routeVars.value what a valid one looks like, or the probe will "+
				"miss the route and report an open door as a 404", tmpl, spec)
		}
		out.WriteString(value)
		i = j
	}
	return out.String()
}

// routeProbe is one request that must be refused.
type routeProbe struct {
	method   string
	template string
	path     string
}

// gatedRoutes asks the router what it serves and returns one probe per method
// per route, skipping the ones unauthenticatedRoutes accounts for.
func gatedRoutes(t *testing.T, vars routeVars) []routeProbe {
	t.Helper()

	var probes []routeProbe
	err := newHTTPRouter().Walk(func(route *mux.Route, _ *mux.Router, _ []*mux.Route) error {
		// The route that carries the authenticated subrouter is a prefix
		// matcher with no handler of its own. Walk descends into its children,
		// which are the routes being measured; probing the prefix itself would
		// reach no handler and answer 404.
		if route.GetHandler() == nil {
			return nil
		}
		tmpl, err := route.GetPathTemplate()
		if err != nil {
			return nil // matched by something other than a path
		}
		if _, ok := unauthenticatedRoutes[tmpl]; ok {
			return nil
		}
		methods, err := route.GetMethods()
		if err != nil || len(methods) == 0 {
			// No .Methods() on the route, so it serves all of them --
			// entries/{path} is the one that does. GET is enough to ask
			// whether the door is shut.
			methods = []string{http.MethodGet}
		}
		path := fillRouteVars(t, tmpl, vars)
		for _, m := range methods {
			probes = append(probes, routeProbe{method: m, template: tmpl, path: path})
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking the router: %v", err)
	}
	if len(probes) == 0 {
		t.Fatal("the walk found no routes, so this test would pass for the wrong reason")
	}
	return probes
}

// anonymous sends a request with no Authorization header at all.
//
// call cannot do this: it sets the header unconditionally, so an empty token
// arrives as "Bearer " and is refused as malformed. That is a different
// question -- and a weaker one, since a malformed credential is refused by the
// parser before anything looks at the route.
func anonymous(t *testing.T, method, url string) int {
	t.Helper()
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode
}

// TestEveryRouteRefusesACallerCarryingNothing is the sweep.
//
// 401 and nothing else. Not "not 200": a handler that ran far enough to answer
// 400 because it did not like the body, or 404 because the id was not one it
// held, has already done work on behalf of somebody who proved nothing, and
// the next change to it decides what that work leaks. The gate is meant to be
// the first thing that happens.
func TestEveryRouteRefusesACallerCarryingNothing(t *testing.T) {
	// So the notification socket is registered and gets swept with the rest.
	// Its middleware is a third wrapper with its own reason to exist, which
	// makes it exactly the route a hand-written list would omit.
	orig := option.EnableNotification
	t.Cleanup(func() { option.EnableNotification = orig })
	option.EnableNotification = true

	base, token := wire(t)
	acctID, _ := makeAccount(t, base, "gate@example.com", "a password", account.RoleUser)
	vars := routeVars{libraryID: makeLibrary(t, base, token), accountID: acctID.String()}

	for _, p := range gatedRoutes(t, vars) {
		if code := anonymous(t, p.method, base+p.path); code != http.StatusUnauthorized {
			t.Errorf("%s %s with no credential: status %d, want 401", p.method, p.template, code)
		}
	}
}

// The same sweep, against credentials that exist in some sense and must not
// open anything: a string that is not a credential at all, and a real row that
// has passed its expiry.
//
// Expiry is the one worth having here rather than only in the middleware's own
// tests. It is checked in credential.Resolve, several layers below any route,
// and that is precisely why a route could be mounted outside the lane that
// calls it and nothing would notice -- the unit test would still pass.
func TestEveryRouteRefusesAGarbageOrExpiredCredential(t *testing.T) {
	orig := option.EnableNotification
	t.Cleanup(func() { option.EnableNotification = orig })
	option.EnableNotification = true

	base, token := wire(t)
	acctID, _ := makeAccount(t, base, "stale@example.com", "a password", account.RoleUser)
	vars := routeVars{libraryID: makeLibrary(t, base, token), accountID: acctID.String()}

	expired, id := issueCredentialWithID(t, credential.IssueOpts{
		Kind:  credential.KindDevice,
		Perm:  "rw",
		Label: "expired for a test",
	})
	ctx, cancel := option.WithDBTimeout(context.Background())
	defer cancel()
	if err := credential.Expire(ctx, id); err != nil {
		t.Fatalf("expiring the credential: %v", err)
	}

	probes := gatedRoutes(t, vars)
	for _, presented := range []struct{ what, token string }{
		{"a string that is not a credential", "not-a-credential"},
		{"a credential past its expiry", expired},
	} {
		for _, p := range probes {
			code, _ := call(t, p.method, base+p.path, presented.token, "")
			if code != http.StatusUnauthorized {
				t.Errorf("%s %s with %s: status %d, want 401",
					p.method, p.template, presented.what, code)
			}
		}
	}
}
