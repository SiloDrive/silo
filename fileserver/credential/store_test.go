package credential

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/dkam/silo/fileserver/dbutil"
	"github.com/dkam/silo/fileserver/option"
)

func testDB(t *testing.T) *dbutil.DBPair {
	t.Helper()

	if option.DBOpTimeout <= 0 {
		option.DBOpTimeout = 30 * time.Second
	}

	pair, err := dbutil.OpenSQLite(filepath.Join(t.TempDir(), "silo.db"))
	if err != nil {
		t.Fatalf("opening test database: %v", err)
	}
	if err := dbutil.CreateSiloTables(pair.Write); err != nil {
		t.Fatalf("creating test tables: %v", err)
	}
	t.Cleanup(func() {
		_ = pair.Read.Close()
		_ = pair.Write.Close()
	})

	Init(pair.Read, pair.Write)
	return pair
}

func addUser(t *testing.T, pair *dbutil.DBPair, email string, active bool) {
	t.Helper()
	if _, err := pair.Write.Exec(
		"INSERT INTO EmailUser (email, passwd, is_staff, is_active, ctime) VALUES (?, '!', 0, ?, ?)",
		email, active, time.Now().Unix()); err != nil {
		t.Fatalf("inserting user: %v", err)
	}
}

type credOpts struct {
	email     string
	scope     string
	perm      string
	expiresAt int64
	publicKey []byte
}

// mint writes a credential row and returns the string a client would present.
func mint(t *testing.T, pair *dbutil.DBPair, kind Kind, o credOpts) (string, Token) {
	t.Helper()

	tok, s, err := NewToken(kind)
	if err != nil {
		t.Fatalf("NewToken: %v", err)
	}
	if o.email == "" {
		o.email = "dan@example.com"
	}
	if o.perm == "" {
		o.perm = "rw"
	}

	var hash []byte
	if o.publicKey == nil {
		hash = tok.SecretHash()
	}
	var expires any
	if o.expiresAt != 0 {
		expires = o.expiresAt
	}
	if _, err := pair.Write.Exec(
		`INSERT INTO Credential (id, kind, secret_hash, public_key, email, label, scope,
		                         perm, client_id, ctime, expires_at, last_used)
		 VALUES (?, ?, ?, ?, ?, 'test', ?, ?, NULL, ?, ?, NULL)`,
		tok.ID, string(kind), hash, o.publicKey, o.email, o.scope, o.perm,
		time.Now().Unix(), expires); err != nil {
		t.Fatalf("inserting credential: %v", err)
	}
	return s, tok
}

func bearer(s string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/api2/repos/", nil)
	r.Header.Set("Authorization", "Bearer "+s)
	return r
}

func TestResolveAcceptsAValidCredential(t *testing.T) {
	pair := testDB(t)
	addUser(t, pair, "dan@example.com", true)
	s, tok := mint(t, pair, KindDevice, credOpts{scope: "repo-1:/photos", perm: "r"})

	cred, err := Resolve(bearer(s), KindDevice)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if cred.ID != tok.ID {
		t.Errorf("id = %q, want %q", cred.ID, tok.ID)
	}
	if cred.Email != "dan@example.com" {
		t.Errorf("email = %q", cred.Email)
	}
	if got, want := cred.Scope.String(), "repo-1:/photos"; got != want {
		t.Errorf("scope = %q, want %q", got, want)
	}
	if !cred.Bearer() {
		t.Error("credential should be bearer")
	}
}

// A wrong secret and an id that does not exist must be indistinguishable, or
// the pair answers "is this credential real?" for anyone who asks.
func TestResolveDoesNotDistinguishMissingFromWrong(t *testing.T) {
	pair := testDB(t)
	addUser(t, pair, "dan@example.com", true)
	s, _ := mint(t, pair, KindDevice, credOpts{})

	// A credential whose row was never written.
	_, unknown, err := NewToken(KindDevice)
	if err != nil {
		t.Fatalf("NewToken: %v", err)
	}

	_, errUnknown := Resolve(bearer(unknown), KindDevice)
	if !errors.Is(errUnknown, ErrInvalid) {
		t.Fatalf("unknown credential: got %v, want ErrInvalid", errUnknown)
	}

	// Now the right id with the wrong secret. Rebuild the token by hand so the
	// checksum stays valid.
	tok, err := ParseToken(s)
	if err != nil {
		t.Fatalf("ParseToken: %v", err)
	}
	wrong := Token{Kind: tok.Kind, ID: tok.ID, Secret: make([]byte, secretBytes)}
	_, errWrong := Resolve(bearer(wrong.String()), KindDevice)
	if !errors.Is(errWrong, ErrInvalid) {
		t.Fatalf("wrong secret: got %v, want ErrInvalid", errWrong)
	}

	if errUnknown.Error() != errWrong.Error() {
		t.Errorf("the two failures are distinguishable: %q vs %q", errUnknown, errWrong)
	}
}

func TestResolveRejects(t *testing.T) {
	pair := testDB(t)
	addUser(t, pair, "dan@example.com", true)
	addUser(t, pair, "disabled@example.com", false)

	valid, _ := mint(t, pair, KindDevice, credOpts{})
	expired, _ := mint(t, pair, KindDevice, credOpts{expiresAt: time.Now().Add(-time.Hour).Unix()})
	inactive, _ := mint(t, pair, KindDevice, credOpts{email: "disabled@example.com"})
	orphan, _ := mint(t, pair, KindDevice, credOpts{email: "deleted@example.com"})
	pop, _ := mint(t, pair, KindDevice, credOpts{publicKey: []byte("spki")})

	tests := []struct {
		name string
		req  *http.Request
		kind Kind
		want error
	}{
		{"no header", httptest.NewRequest(http.MethodGet, "/", nil), KindDevice, ErrMissing},
		{"malformed", bearer("not-a-credential"), KindDevice, ErrMalformed},
		{"truncated", bearer(valid[:len(valid)-1]), KindDevice, ErrMalformed},
		{"wrong lane", bearer(valid), KindSession, ErrWrongKind},
		{"expired", bearer(expired), KindDevice, ErrExpired},
		{"disabled account", bearer(inactive), KindDevice, ErrInactive},
		{"deleted account", bearer(orphan), KindDevice, ErrInactive},
		{"proof of possession", bearer(pop), KindDevice, ErrSignatureNotImplemented},
	}

	for _, tt := range tests {
		_, err := Resolve(tt.req, tt.kind)
		if !errors.Is(err, tt.want) {
			t.Errorf("%s: got %v, want %v", tt.name, err, tt.want)
		}
	}
}

// The account join is the property that could not be retrofitted onto three
// separate stores: disabling a user has to kill every lane at once.
func TestResolveFollowsTheAccountImmediately(t *testing.T) {
	pair := testDB(t)
	addUser(t, pair, "dan@example.com", true)
	s, _ := mint(t, pair, KindDevice, credOpts{})

	if _, err := Resolve(bearer(s), KindDevice); err != nil {
		t.Fatalf("before disabling: %v", err)
	}

	if _, err := pair.Write.Exec(
		"UPDATE EmailUser SET is_active = 0 WHERE email = ?", "dan@example.com"); err != nil {
		t.Fatalf("disabling user: %v", err)
	}

	if _, err := Resolve(bearer(s), KindDevice); !errors.Is(err, ErrInactive) {
		t.Errorf("after disabling: got %v, want ErrInactive", err)
	}
}

func TestResolveStampsLastUsedCoarsely(t *testing.T) {
	pair := testDB(t)
	addUser(t, pair, "dan@example.com", true)
	s, tok := mint(t, pair, KindDevice, credOpts{})

	readLastUsed := func() int64 {
		var v *int64
		if err := pair.Read.QueryRow(
			"SELECT last_used FROM Credential WHERE id = ?", tok.ID).Scan(&v); err != nil {
			t.Fatalf("reading last_used: %v", err)
		}
		if v == nil {
			return 0
		}
		return *v
	}

	if got := readLastUsed(); got != 0 {
		t.Fatalf("last_used = %d before any use, want 0", got)
	}

	if _, err := Resolve(bearer(s), KindDevice); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	first := readLastUsed()
	if first == 0 {
		t.Fatal("last_used was not stamped on first use")
	}

	// A second request inside the granularity window must not write again:
	// a write transaction in front of every read is what this avoids.
	if _, err := pair.Write.Exec(
		"UPDATE Credential SET last_used = ? WHERE id = ?", first-1, tok.ID); err != nil {
		t.Fatalf("nudging last_used: %v", err)
	}
	if _, err := Resolve(bearer(s), KindDevice); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got := readLastUsed(); got != first-1 {
		t.Errorf("last_used was rewritten inside the granularity window: %d, want %d", got, first-1)
	}

	// Once it is stale, it is rewritten.
	stale := time.Now().Add(-2 * lastUsedGranularity).Unix()
	if _, err := pair.Write.Exec(
		"UPDATE Credential SET last_used = ? WHERE id = ?", stale, tok.ID); err != nil {
		t.Fatalf("ageing last_used: %v", err)
	}
	if _, err := Resolve(bearer(s), KindDevice); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got := readLastUsed(); got == stale {
		t.Error("last_used was not refreshed once it went stale")
	}
}

func TestResolveLegacyToken(t *testing.T) {
	pair := testDB(t)
	addUser(t, pair, "dan@example.com", true)

	// A legacy client presents forty hex characters, which auth.md reads as an
	// encoding of the same row rather than a separate store.
	const raw = "0401fc662e3bc87a41f299a907c056aaf8322a27"
	tok, err := ParseLegacyToken(raw)
	if err != nil {
		t.Fatalf("ParseLegacyToken: %v", err)
	}
	if _, err := pair.Write.Exec(
		`INSERT INTO Credential (id, kind, secret_hash, email, label, perm, ctime)
		 VALUES (?, 'legacy', ?, 'dan@example.com', 'seadrive', 'rw', ?)`,
		tok.ID, tok.SecretHash(), time.Now().Unix()); err != nil {
		t.Fatalf("inserting credential: %v", err)
	}

	r := httptest.NewRequest(http.MethodGet, "/api2/repos/", nil)
	r.Header.Set("Authorization", "Token "+raw)

	cred, err := Resolve(r, KindLegacy)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if cred.Kind != KindLegacy || cred.Email != "dan@example.com" {
		t.Errorf("resolved the wrong row: %+v", cred)
	}
}

func TestEffectivePerm(t *testing.T) {
	const repo = "repo-1"

	tests := []struct {
		name        string
		scope       string
		credPerm    string
		accountPerm string
		repo, path  string
		want        string
	}{
		{"unscoped credential passes the account through", "", "rw", "rw", repo, "/x", "rw"},
		{"a read-only credential narrows a read-write account", "", "r", "rw", repo, "/x", "r"},
		{"a read-write credential cannot widen a read-only account", "", "rw", "r", repo, "/x", "r"},
		{"withdrawn account permission wins", "", "rw", "", repo, "/x", ""},
		{"library scope covers its own library", repo, "rw", "rw", repo, "/x", "rw"},
		{"library scope excludes another", repo, "rw", "rw", "repo-2", "/x", ""},
		{"path scope covers below it", repo + ":/photos", "rw", "rw", repo, "/photos/2024", "rw"},
		{"path scope excludes a sibling", repo + ":/photos", "rw", "rw", repo, "/documents", ""},
		{"path scope and a read-only account", repo + ":/photos", "rw", "r", repo, "/photos", "r"},
		{"an unrecognised permission grants nothing", "", "admin", "rw", repo, "/x", ""},
	}

	for _, tt := range tests {
		scope, err := ParseScope(tt.scope)
		if err != nil {
			t.Fatalf("ParseScope(%q): %v", tt.scope, err)
		}
		c := &Credential{Scope: scope, Perm: tt.credPerm}
		if got := c.EffectivePerm(tt.accountPerm, tt.repo, tt.path); got != tt.want {
			t.Errorf("%s: got %q, want %q", tt.name, got, tt.want)
		}
	}
}
