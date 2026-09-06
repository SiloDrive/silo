package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/dkam/silo/fileserver/account"
	"github.com/dkam/silo/fileserver/credential"
	"github.com/dkam/silo/fileserver/dbutil"
	"github.com/dkam/silo/fileserver/option"
)

func testDB(t *testing.T) *dbutil.DBPair {
	t.Helper()

	origTimeout := option.DBOpTimeout
	option.DBOpTimeout = 5 * time.Second

	pair, err := dbutil.OpenSQLite(filepath.Join(t.TempDir(), "silo.db"))
	if err != nil {
		t.Fatalf("opening test database: %v", err)
	}
	if err := dbutil.CreateSiloTables(pair.Write); err != nil {
		t.Fatalf("creating test tables: %v", err)
	}
	account.Init(pair.Read, pair.Write)
	credential.Init(pair.Read, pair.Write)

	t.Cleanup(func() {
		option.DBOpTimeout = origTimeout
		_ = pair.Close()
	})
	return pair
}

func addUser(t *testing.T, pair *dbutil.DBPair, email string) account.ID {
	t.Helper()
	ctx, cancel := option.WithDBTimeout(context.Background())
	defer cancel()

	id, _, err := account.Create(ctx, email, "", account.RoleUser)
	if err != nil {
		t.Fatalf("creating %s: %v", email, err)
	}
	return id
}

func issue(t *testing.T, id account.ID, kind credential.Kind, lifetime time.Duration) (string, string) {
	t.Helper()
	return issueWith(t, credential.IssueOpts{Kind: kind, AccountID: id, Lifetime: lifetime})
}

// issueScoped is issue for a device credential cut to one library, or to a
// path inside one.
func issueScoped(t *testing.T, id account.ID, scope string) (string, string) {
	t.Helper()
	return issueWith(t, credential.IssueOpts{
		Kind: credential.KindDevice, AccountID: id, Lifetime: 24 * time.Hour, Scope: parseScope(t, scope),
	})
}

// issueWith issues a read-write credential with a test label, returning its id
// and secret.
func issueWith(t *testing.T, opts credential.IssueOpts) (string, string) {
	t.Helper()
	ctx, cancel := option.WithDBTimeout(context.Background())
	defer cancel()

	opts.Label, opts.Perm = "test", "rw"
	c, secret, err := credential.Issue(ctx, opts)
	if err != nil {
		t.Fatalf("issuing a %s credential: %v", opts.Kind, err)
	}
	return c.ID, secret
}

func parseScope(t *testing.T, s string) credential.Scope {
	t.Helper()
	sc, err := credential.ParseScope(s)
	if err != nil {
		t.Fatalf("parsing scope %q: %v", s, err)
	}
	return sc
}

// seen records what the handler behind the middleware was given, so a test can
// assert on the context rather than on a status code alone.
type seen struct {
	reached bool
	acct    *account.Account
	cred    *credential.Credential
}

func run(t *testing.T, mw func(http.Handler) http.Handler, header string) (*httptest.ResponseRecorder, *seen) {
	t.Helper()
	return runAt(t, mw, "/api/silo/v1/libraries", header)
}

// runAt is run against a route of the caller's choosing.
func runAt(t *testing.T, mw func(http.Handler) http.Handler, path, header string) (*httptest.ResponseRecorder, *seen) {
	t.Helper()

	got := &seen{}
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.reached = true
		got.acct = GetAccount(r)
		got.cred = GetCredential(r)
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, path, nil)
	if header != "" {
		req.Header.Set("Authorization", header)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec, got
}

// The handler gets both: the account, because every handler already speaks in
// accounts, and the credential, because the permission ceiling is a property
// of the credential rather than of the user behind it.
func TestRequireCredentialPassesAccountAndCredential(t *testing.T) {
	pair := testDB(t)
	dan := addUser(t, pair, "dan@example.com")
	id, secret := issue(t, dan, credential.KindSession, 24*time.Hour)

	rec, got := run(t, RequireCredential, "Bearer "+secret)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}
	if !got.reached {
		t.Fatal("the handler was not reached")
	}
	if got.acct == nil || got.acct.ID != dan {
		t.Errorf("account = %v, want %s", got.acct, dan)
	}
	if got.acct.Email != "dan@example.com" {
		t.Errorf("email = %q", got.acct.Email)
	}
	if got.cred == nil || got.cred.ID != id {
		t.Errorf("credential = %v, want %s", got.cred, id)
	}
}

// A device credential is silo-drive's, and the management API is the lane it uses.
func TestRequireCredentialAcceptsADeviceCredential(t *testing.T) {
	pair := testDB(t)
	dan := addUser(t, pair, "dan@example.com")
	_, secret := issue(t, dan, credential.KindDevice, 90*24*time.Hour)

	if rec, _ := run(t, RequireCredential, "Bearer "+secret); rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200: %s", rec.Code, rec.Body)
	}
}

// Every caller-side refusal is the same 401 with the same body. Telling a
// client whether its credential was unknown, revoked, expired, disabled or
// simply for another lane answers questions about rows it has not proved it
// holds.
func TestRequireCredentialRefusals(t *testing.T) {
	pair := testDB(t)
	dan := addUser(t, pair, "dan@example.com")
	_, good := issue(t, dan, credential.KindSession, 24*time.Hour)
	revokedID, revoked := issue(t, dan, credential.KindSession, 24*time.Hour)
	expiredID, expired := issue(t, dan, credential.KindSession, time.Hour)
	_, wrongLane := issue(t, dan, credential.KindAccess, time.Hour)

	ctx, cancel := option.WithDBTimeout(context.Background())
	defer cancel()
	if _, err := credential.Revoke(ctx, revokedID, dan); err != nil {
		t.Fatalf("Revoke: %v", err)
	}

	// Backdated rather than issued with a negative lifetime, which Issue now
	// refuses: an expiry in the past is a row that has aged out, not a request
	// anybody should be able to make.
	if _, err := pair.Write.Exec("UPDATE Credential SET expires_at = ? WHERE id = ?",
		time.Now().Add(-time.Hour).Unix(), expiredID); err != nil {
		t.Fatalf("backdating the expiry: %v", err)
	}

	disabled := addUser(t, pair, "eve@example.com")
	_, evesToken := issue(t, disabled, credential.KindSession, 24*time.Hour)
	if err := account.SetActive(ctx, disabled, false); err != nil {
		t.Fatalf("SetActive: %v", err)
	}

	cases := []struct {
		name   string
		header string
	}{
		{"no header", ""},
		{"no scheme", good},
		{"unknown scheme", "Basic " + good},
		{"empty bearer", "Bearer "},
		{"not a silo credential", "Bearer abcdef"},
		{"truncated, so the checksum fails", "Bearer " + good[:len(good)-4]},
		{"a character mistyped", "Bearer " + mistype(good)},
		{"unknown credential", "Bearer " + mutateID(good)},
		{"revoked", "Bearer " + revoked},
		{"expired", "Bearer " + expired},
		{"a lane this router does not serve", "Bearer " + wrongLane},
		{"account disabled", "Bearer " + evesToken},
		{"legacy Token scheme, no longer mounted", "Token " + "0123456789abcdef0123456789abcdef01234567"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec, got := run(t, RequireCredential, tc.header)
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401", rec.Code)
			}
			if got.reached {
				t.Error("the handler ran on a refused request")
			}
		})
	}
}

// Authorization: Silo is proof of possession, which is designed and not built.
// It answers 501 rather than 401 because it is the server's gap: a 401 would
// send a correct client away to re-enrol against a lane that fails identically.
func TestRequireCredentialSaysSignaturesAreNotImplemented(t *testing.T) {
	testDB(t)

	rec, got := run(t, RequireCredential, "Silo abcdefghij")
	if rec.Code != http.StatusNotImplemented {
		t.Errorf("status = %d, want 501", rec.Code)
	}
	if got.reached {
		t.Error("the handler ran on a refused request")
	}
}

// The notification socket predates the header, so a client that offers nothing
// still gets in -- and gets in as nobody, so a route that needs an account has
// to ask for one.
func TestOptionalCredentialLetsAnAnonymousRequestThrough(t *testing.T) {
	testDB(t)

	rec, got := run(t, OptionalCredential, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !got.reached {
		t.Fatal("the handler was not reached")
	}
	if got.acct != nil {
		t.Errorf("account = %v, want nil", got.acct)
	}
	if got.cred != nil {
		t.Errorf("credential = %v, want nil", got.cred)
	}
}

func TestOptionalCredentialAuthenticatesWhenOffered(t *testing.T) {
	pair := testDB(t)
	dan := addUser(t, pair, "dan@example.com")
	_, secret := issue(t, dan, credential.KindSession, 24*time.Hour)

	rec, got := run(t, OptionalCredential, "Bearer "+secret)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}
	if got.acct == nil || got.acct.ID != dan {
		t.Errorf("account = %v, want %s", got.acct, dan)
	}
}

// The rule that makes "optional" safe: a credential that is offered and bad is
// refused, never quietly downgraded to anonymous. A caller that believes it is
// authenticated would otherwise learn otherwise only from the permissions it
// silently stopped having.
func TestOptionalCredentialStillRefusesABadOne(t *testing.T) {
	pair := testDB(t)
	dan := addUser(t, pair, "dan@example.com")
	_, good := issue(t, dan, credential.KindSession, 24*time.Hour)

	for _, header := range []string{"Bearer " + mutateID(good), "Bearer nonsense", "Bearer "} {
		rec, got := run(t, OptionalCredential, header)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%q: status = %d, want 401", header, rec.Code)
		}
		if got.reached {
			t.Errorf("%q: the handler ran on a refused request", header)
		}
	}
}

// A scoped credential may open the notification socket.
//
// The route-level narrowing refuses a scoped credential every route that names
// no library, because such a route answers about the account. The socket is
// not such a route: it answers nothing at all until a subscribe frame names a
// library, and each of those is checked on its own. Refusing at the upgrade
// leaves a mount cut to one library with no push at all -- and 403 before the
// upgrade is a failure a client cannot fall back from the way it falls back
// from a missing feature name.
func TestOptionalCredentialAdmitsAScopedCredential(t *testing.T) {
	pair := testDB(t)
	dan := addUser(t, pair, "dan@example.com")
	_, secret := issueScoped(t, dan, "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee")

	rec, got := runAt(t, OptionalCredential, "/notification", "Bearer "+secret)

	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200: a library-scoped credential could not open the socket", rec.Code)
	}
	if got.cred == nil {
		t.Fatal("the socket was opened without the credential that authorizes its subscribes")
	}
	if got.cred.Scope.LibraryID != "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee" {
		t.Errorf("the handler got scope %q; the narrowing must survive to the subscribe check", got.cred.Scope)
	}
}

// The exemption is for this one wrapper, and must not widen the others.
//
// RequireCredential refusing a scoped credential a route that names no library
// is the finding docs/auth.md's ceiling rule exists for: listing libraries,
// account usage, create and delete all read the account's own authority.
func TestRequireCredentialStillRefusesAScopedCredentialARoutelessOfLibrary(t *testing.T) {
	pair := testDB(t)
	dan := addUser(t, pair, "dan@example.com")
	_, secret := issueScoped(t, dan, "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee")

	rec, got := runAt(t, RequireCredential, "/api/silo/v1/libraries", "Bearer "+secret)

	if rec.Code != http.StatusForbidden {
		t.Errorf("got %d, want 403", rec.Code)
	}
	if got.reached {
		t.Error("a scoped credential enumerated every library its account can see")
	}
}

// PermFor answers the ceiling for a caller holding a credential rather than a
// request, and answers it the same way.
func TestPermForAppliesTheNarrowing(t *testing.T) {
	const mine = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	const other = "11111111-2222-3333-4444-555555555555"

	scoped := func(s string) *credential.Credential {
		return &credential.Credential{Perm: "rw", Scope: parseScope(t, s)}
	}

	// Another library is refused without reaching the database, which is the
	// point of asking Covers before CheckPerm.
	if got := PermFor(scoped(mine), other, ""); got != "" {
		t.Errorf("a credential scoped to one library reached another: %q", got)
	}
	// A folder scope cannot answer about the library as a whole. This is
	// Perm's documented rule, and it is what a path-scoped mount is told when
	// it tries to watch its library.
	if got := PermFor(scoped(mine+":/photos"), mine, ""); got != "" {
		t.Errorf("a path-scoped credential was granted the whole library: %q", got)
	}
	// No credential is no access, as on the request path.
	if got := PermFor(nil, mine, ""); got != "" {
		t.Errorf("a nil credential was granted %q", got)
	}
}

// mistype changes the last character of a credential, which is the last
// character of its checksum, so the string is malformed rather than invalid.
//
// The replacement is chosen against the character it replaces. Writing "z"
// unconditionally left one token in thirty-two unchanged -- and an unchanged
// token is a valid one, so the case passed a request the test believed it had
// broken and failed at a rate that read as an unrelated flake.
func mistype(s string) string {
	last := s[len(s)-1]
	replacement := byte('z')
	if last == replacement {
		replacement = 'a'
	}
	return s[:len(s)-1] + string(replacement)
}

// mutateID rewrites the id half of a token and re-checksums it, producing a
// well-formed credential that no row matches. Editing a character in place
// would fail the checksum instead, which is a different refusal.
func mutateID(s string) string {
	tok, err := credential.ParseToken(s)
	if err != nil {
		panic(err)
	}
	swapped := "aaaaaaaaaaaaaaaa"
	if tok.ID == swapped {
		swapped = "bbbbbbbbbbbbbbbb"
	}
	tok.ID = swapped
	return tok.String()
}
