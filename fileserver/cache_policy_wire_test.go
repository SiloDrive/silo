package silod

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// callCC runs one authenticated GET through the real stack and returns the
// status and the Cache-Control the server sent.
func callCC(t *testing.T, url, token string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, resp.Header.Get("Cache-Control")
}

// The authenticated routes are mutable and none of them said so.
//
// They are not heuristically cacheable today only because they carry no
// validator, which makes it a latent trap rather than a safe default: the day
// one of them grows an ETag for good reasons it silently becomes cacheable,
// and nobody finds out. changes?since={commit} is the one to be most careful
// with, because the same URL legitimately returns *more* as commits land — so
// a cached answer is wrong in a way that looks exactly like "nothing has
// changed", which is the failure that started all this.
func TestTheMutableAPIRoutesAreNotCacheable(t *testing.T) {
	base, token := wire(t)
	id := makeLibrary(t, base, token)

	for _, path := range []string{
		"/api/silo/v1/libraries",
		"/api/silo/v1/libraries/" + id + "/changes?since=0",
		"/api/silo/v1/libraries/" + id + "/commits",
		"/api/silo/v1/account",
		"/api/silo/v1/account/usage",
	} {
		code, cc := callCC(t, base+path, token)
		if code == http.StatusNotFound {
			t.Errorf("%s: 404 — the route moved and this test is measuring nothing", path)
			continue
		}
		if cc == "" {
			t.Errorf("%s: no Cache-Control; a mutable authenticated route should not be left to a heuristic", path)
			continue
		}
		if !strings.Contains(cc, "no-store") && !strings.Contains(cc, "no-cache") {
			t.Errorf("%s: Cache-Control = %q, want no-cache or no-store", path, cc)
		}
		if !strings.Contains(cc, "private") {
			t.Errorf("%s: Cache-Control = %q, want private", path, cc)
		}
	}
}

// A default is only safe if the routes that mean something else can still say
// it. objects/{id} is content-addressed and wants its year of immutability; if
// a blanket default overwrote that, closing the heuristic hole would have cost
// the one genuinely free cache in the API.
func TestARouteWithItsOwnPolicyKeepsIt(t *testing.T) {
	base, token := wire(t)
	id := makeLibrary(t, base, token)

	body := strings.Repeat("content-addressed. ", 4096)
	req, err := http.NewRequest(http.MethodPut,
		base+"/api/silo/v1/libraries/"+id+"/entries/big.bin", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT: %v", err)
	}
	etag := resp.Header.Get("ETag")
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("PUT = %d, want 201", resp.StatusCode)
	}

	objectID := strings.TrimPrefix(strings.Trim(etag, `"`), etagPrefix)
	code, cc := callCC(t, base+"/api/silo/v1/libraries/"+id+"/objects/"+objectID, token)
	if code != http.StatusOK {
		t.Fatalf("GET object = %d, want 200", code)
	}
	if !strings.Contains(cc, "immutable") || !strings.Contains(cc, "max-age=") {
		t.Errorf("Cache-Control = %q; the handler's own policy was overwritten by the default", cc)
	}
	if strings.Contains(cc, "public") {
		t.Errorf("Cache-Control = %q, want private", cc)
	}
}

// The listing's own policy has to survive the stack too — the handler sets it,
// and a default that ran after the handler would replace a deliberate header
// with a generic one.
func TestTheListingPolicySurvivesTheStack(t *testing.T) {
	base, token := wire(t)
	id := makeLibrary(t, base, token)

	code, cc := callCC(t, base+"/api/silo/v1/libraries/"+id+"/entries/", token)
	if code != http.StatusOK {
		t.Fatalf("GET listing = %d, want 200", code)
	}
	wantsRevalidation(t, cc, "listing over the wire")
}
