package silod

import (
	"context"
	"net/http"
	"testing"

	"github.com/dkam/silo/fileserver/account"
	"github.com/dkam/silo/fileserver/credential"
	"github.com/dkam/silo/fileserver/option"
)

// A credential can only ever narrow. docs/auth.md states the rule as
//
//	effective = min(CheckPerm(library, account), cred.perm within cred.scope)
//
// and these tests measure it through the real router, because the failure it
// prevents is a handler that authenticates the credential correctly and then
// asks share.CheckPerm what the *user* may do -- which is what every handler
// did until the ceiling was wired in.

// zeroCommit is a syntactically valid anchor that names nothing, so `changes`
// gets past its argument check and reaches the permission check this measures.
const zeroCommit = "0000000000000000000000000000000000000000000000000000000000000000"

// narrowed issues a credential for the account the wire harness logs in as,
// and returns the string a client would present.
func narrowed(t *testing.T, perm string, scope credential.Scope) string {
	t.Helper()
	ctx, cancel := option.WithDBTimeout(context.Background())
	defer cancel()

	acct, err := account.ByEmail(ctx, "wire@example.com")
	if err != nil {
		t.Fatalf("looking up the wire account: %v", err)
	}
	_, secret, err := credential.Issue(ctx, credential.IssueOpts{
		Kind:      credential.KindDevice,
		AccountID: acct.ID,
		Label:     "narrowed, for a test",
		Scope:     scope,
		Perm:      perm,
	})
	if err != nil {
		t.Fatalf("issuing a %q credential: %v", perm, err)
	}
	return secret
}

// The case a backup tool or an untrusted mount actually wants: it may read
// everything and change nothing.
func TestAReadOnlyCredentialCannotWrite(t *testing.T) {
	base, token := wire(t)
	id := makeLibrary(t, base, token)
	readonly := narrowed(t, "r", credential.Scope{})

	if code, body := call(t, "GET", base+"/api/silo/v1/libraries/"+id+"/entries/", readonly, ""); code != http.StatusOK {
		t.Errorf("reading with a read-only credential: status %d, body %s", code, body)
	}

	// The account behind this credential owns the library and may write to it.
	// Only the ceiling stops the request, which is the whole point.
	writes := []struct{ method, path, body string }{
		{"POST", "/api/silo/v1/libraries/" + id + "/batch", `{"ops":[{"op":"mkdir","path":"/nope"}]}`},
		{"PUT", "/api/silo/v1/libraries/" + id + "/entries/nope.txt", `hello`},
		{"DELETE", "/api/silo/v1/libraries/" + id + "/entries/nope.txt", ""},
	}
	for _, w := range writes {
		if code, body := call(t, w.method, base+w.path, readonly, w.body); code != http.StatusForbidden {
			t.Errorf("%s %s with a read-only credential: status %d, want 403, body %s",
				w.method, w.path, code, body)
		}
	}

	// And the same account, presenting its unnarrowed session credential,
	// still can -- otherwise this test would pass on a server that had simply
	// broken writes.
	if code, body := call(t, "PUT", base+"/api/silo/v1/libraries/"+id+"/entries/yes.txt", token, `hello`); code != http.StatusCreated && code != http.StatusOK {
		t.Errorf("writing with the session credential: status %d, body %s", code, body)
	}
}

// A credential cut to one library is the other half of the same rule.
func TestALibraryScopedCredentialCannotReachAnother(t *testing.T) {
	base, token := wire(t)
	mine := makeLibrary(t, base, token)
	other := makeLibrary(t, base, token)
	scoped := narrowed(t, "rw", credential.Scope{LibraryID: mine})

	if code, body := call(t, "GET", base+"/api/silo/v1/libraries/"+mine+"/entries/", scoped, ""); code != http.StatusOK {
		t.Errorf("reading the library it is scoped to: status %d, body %s", code, body)
	}

	// 403 rather than 404: a library you may not reach and one that does not
	// exist answer the same thing, or the scope becomes a way to probe for
	// valid library ids. See docs/responses.md.
	// `since` is supplied because changes validates it before it authorizes,
	// so without one this would measure a 400 rather than the ceiling.
	for _, path := range []string{
		"/api/silo/v1/libraries/" + other + "/entries/",
		"/api/silo/v1/libraries/" + other + "/changes?since=" + zeroCommit,
		"/api/silo/v1/libraries/" + other + "/commits",
	} {
		if code, body := call(t, "GET", base+path, scoped, ""); code != http.StatusForbidden {
			t.Errorf("GET %s out of scope: status %d, want 403, body %s", path, code, body)
		}
	}
}

// A path scope is the narrowest form: one folder and everything beneath it.
func TestAPathScopedCredentialReachesOnlyThatSubtree(t *testing.T) {
	base, token := wire(t)
	id := makeLibrary(t, base, token)

	seed := `{"ops":[{"op":"mkdir","path":"/photos"},{"op":"mkdir","path":"/documents"}]}`
	if code, body := call(t, "POST", base+"/api/silo/v1/libraries/"+id+"/batch", token, seed); code != http.StatusOK {
		t.Fatalf("seeding directories: status %d, body %s", code, body)
	}
	for _, path := range []string{"photos/inside.txt", "documents/outside.txt"} {
		if code, body := call(t, "PUT", base+"/api/silo/v1/libraries/"+id+"/entries/"+path, token, "x"); code != http.StatusCreated && code != http.StatusOK {
			t.Fatalf("seeding %s: status %d, body %s", path, code, body)
		}
	}

	scoped := narrowed(t, "rw", credential.Scope{LibraryID: id, Path: "/photos"})

	if code, body := call(t, "GET", base+"/api/silo/v1/libraries/"+id+"/entries/photos/inside.txt", scoped, ""); code != http.StatusOK {
		t.Errorf("reading inside the scope: status %d, body %s", code, body)
	}
	if code, body := call(t, "GET", base+"/api/silo/v1/libraries/"+id+"/entries/documents/outside.txt", scoped, ""); code != http.StatusForbidden {
		t.Errorf("reading outside the scope: status %d, want 403, body %s", code, body)
	}

	// A library-wide operation is refused for a path-scoped credential: the
	// delta feed covers the whole library, and there is no way to answer it
	// partially without telling the holder about paths it may not reach.
	if code, body := call(t, "GET", base+"/api/silo/v1/libraries/"+id+"/changes?since="+zeroCommit, scoped, ""); code != http.StatusForbidden {
		t.Errorf("changes with a path-scoped credential: status %d, want 403, body %s", code, body)
	}
}
