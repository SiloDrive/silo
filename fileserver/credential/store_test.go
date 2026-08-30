package credential

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/dkam/silo/fileserver/account"
	"github.com/dkam/silo/fileserver/dbutil"
	"github.com/dkam/silo/fileserver/option"
)

func testDB(t *testing.T) *dbutil.DBPair {
	t.Helper()

	origTimeout := option.DBOpTimeout
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

	origRead, origWrite := readDB, writeDB
	Init(pair.Read, pair.Write)
	account.Init(pair.Read, pair.Write)

	t.Cleanup(func() {
		readDB, writeDB = origRead, origWrite
		option.DBOpTimeout = origTimeout
		_ = pair.Close()
	})
	return pair
}

// addUser mints an account and returns its id. active = false stands in for a
// user an operator has disabled.
func addUser(t *testing.T, pair *dbutil.DBPair, email string, active bool) account.ID {
	t.Helper()
	ctx, cancel := option.WithDBTimeout(context.Background())
	defer cancel()
	id, _, err := account.Create(ctx, email, "", account.RoleUser)
	if err != nil {
		t.Fatalf("creating account: %v", err)
	}
	if !active {
		if _, err := pair.Write.Exec("UPDATE Account SET is_active = 0 WHERE id = ?", id); err != nil {
			t.Fatalf("disabling account: %v", err)
		}
	}
	return id
}

type credOpts struct {
	account   account.ID
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
		`INSERT INTO Credential (id, kind, secret_hash, public_key, account_id, label, scope,
		                         perm, client_id, ctime, expires_at, last_used)
		 VALUES (?, ?, ?, ?, ?, 'test', ?, ?, NULL, ?, ?, NULL)`,
		tok.ID, string(kind), hash, o.publicKey, o.account, o.scope, o.perm,
		time.Now().Unix(), expires); err != nil {
		t.Fatalf("inserting credential: %v", err)
	}
	return s, tok
}

func bearer(s string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/api2/libraries/", nil)
	r.Header.Set("Authorization", "Bearer "+s)
	return r
}

func TestResolveAcceptsAValidCredential(t *testing.T) {
	pair := testDB(t)
	dan := addUser(t, pair, "dan@example.com", true)
	s, tok := mint(t, pair, KindDevice, credOpts{account: dan, scope: "library-1:/photos", perm: "r"})

	cred, err := Resolve(bearer(s), KindDevice)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if cred.ID != tok.ID {
		t.Errorf("id = %q, want %q", cred.ID, tok.ID)
	}
	if cred.AccountID != dan {
		t.Errorf("account = %s, want %s", cred.AccountID, dan)
	}
	if got, want := cred.Scope.String(), "library-1:/photos"; got != want {
		t.Errorf("scope = %q, want %q", got, want)
	}
	if cred.Perm != "r" {
		t.Errorf("perm = %q, want %q", cred.Perm, "r")
	}
	if !cred.Bearer() {
		t.Error("credential should be bearer")
	}
}

// A wrong secret and an id that does not exist must be indistinguishable, or
// the pair answers "is this credential real?" for anyone who asks.
func TestResolveDoesNotDistinguishMissingFromWrong(t *testing.T) {
	pair := testDB(t)
	dan := addUser(t, pair, "dan@example.com", true)
	s, _ := mint(t, pair, KindDevice, credOpts{account: dan})

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
	dan := addUser(t, pair, "dan@example.com", true)
	disabled := addUser(t, pair, "disabled@example.com", false)

	valid, _ := mint(t, pair, KindDevice, credOpts{account: dan})
	expired, _ := mint(t, pair, KindDevice, credOpts{account: dan, expiresAt: time.Now().Add(-time.Hour).Unix()})
	inactive, _ := mint(t, pair, KindDevice, credOpts{account: disabled})
	pop, _ := mint(t, pair, KindDevice, credOpts{account: dan, publicKey: []byte("spki")})

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
		{"proof of possession", bearer(pop), KindDevice, ErrSignatureNotImplemented},
	}

	for _, tt := range tests {
		_, err := Resolve(tt.req, tt.kind)
		if !errors.Is(err, tt.want) {
			t.Errorf("%s: got %v, want %v", tt.name, err, tt.want)
		}
	}
}

// A credential is a reference to an account, and the schema says so. Deleting
// the account out from under one is refused rather than leaving a row that
// authenticates nobody — which is why Resolve has no "deleted account" case to
// answer, only a disabled one.
func TestAnAccountHoldingCredentialsCannotBeDeleted(t *testing.T) {
	pair := testDB(t)
	dan := addUser(t, pair, "dan@example.com", true)
	mint(t, pair, KindDevice, credOpts{account: dan})

	if _, err := pair.Write.Exec("DELETE FROM Account WHERE id = ?", dan); err == nil {
		t.Error("an account with a live credential was deleted")
	}
}

// The account join is the property that could not be retrofitted onto three
// separate stores: disabling a user has to kill every lane at once.
func TestResolveFollowsTheAccountImmediately(t *testing.T) {
	pair := testDB(t)
	dan := addUser(t, pair, "dan@example.com", true)
	s, _ := mint(t, pair, KindDevice, credOpts{account: dan})

	if _, err := Resolve(bearer(s), KindDevice); err != nil {
		t.Fatalf("before disabling: %v", err)
	}

	if _, err := pair.Write.Exec(
		"UPDATE Account SET is_active = 0 WHERE id = ?", dan); err != nil {
		t.Fatalf("disabling user: %v", err)
	}

	if _, err := Resolve(bearer(s), KindDevice); !errors.Is(err, ErrInactive) {
		t.Errorf("after disabling: got %v, want ErrInactive", err)
	}
}

// awaitLastUsed waits for the detached stamp to land. It fails the test rather
// than returning zero, so "the write never happened" is a failure here and not
// a confusing zero compared somewhere below.
func awaitLastUsed(t *testing.T, read func() int64) int64 {
	t.Helper()
	for i := 0; i < 100; i++ {
		if v := read(); v != 0 {
			return v
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("last_used was never stamped")
	return 0
}

func TestResolveStampsLastUsedCoarsely(t *testing.T) {
	pair := testDB(t)
	dan := addUser(t, pair, "dan@example.com", true)
	s, tok := mint(t, pair, KindDevice, credOpts{account: dan})

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

	// Polled, because the write is deliberately off the request's goroutine --
	// it goes to a one-connection pool and would otherwise queue in front of
	// the caller. That makes last_used eventually consistent, which is the
	// right trade for a field whose whole purpose is answering "is anybody
	// still using this?" to five-minute precision.
	first := awaitLastUsed(t, readLastUsed)
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
	// Given time to be wrong: a stamp that was going to happen has had far
	// longer than it needs, so an unchanged value means none was attempted.
	time.Sleep(100 * time.Millisecond)
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
	if got := awaitChange(t, readLastUsed, stale); got == stale {
		t.Error("last_used was not refreshed once it went stale")
	}
}

// awaitChange waits for the detached stamp to move a value off `was`.
func awaitChange(t *testing.T, read func() int64, was int64) int64 {
	t.Helper()
	for i := 0; i < 100; i++ {
		if v := read(); v != was {
			return v
		}
		time.Sleep(10 * time.Millisecond)
	}
	return was
}

func TestEffectivePerm(t *testing.T) {
	const library = "library-1"

	tests := []struct {
		name          string
		scope         string
		credPerm      string
		accountPerm   string
		library, path string
		want          string
	}{
		{"unscoped credential passes the account through", "", "rw", "rw", library, "/x", "rw"},
		{"a read-only credential narrows a read-write account", "", "r", "rw", library, "/x", "r"},
		{"a read-write credential cannot widen a read-only account", "", "rw", "r", library, "/x", "r"},
		{"withdrawn account permission wins", "", "rw", "", library, "/x", ""},
		{"library scope covers its own library", library, "rw", "rw", library, "/x", "rw"},
		{"library scope excludes another", library, "rw", "rw", "library-2", "/x", ""},
		{"path scope covers below it", library + ":/photos", "rw", "rw", library, "/photos/2024", "rw"},
		{"path scope excludes a sibling", library + ":/photos", "rw", "rw", library, "/documents", ""},
		{"path scope and a read-only account", library + ":/photos", "rw", "r", library, "/photos", "r"},
		{"an unrecognised permission grants nothing", "", "admin", "rw", library, "/x", ""},
	}

	for _, tt := range tests {
		scope, err := ParseScope(tt.scope)
		if err != nil {
			t.Fatalf("ParseScope(%q): %v", tt.scope, err)
		}
		c := &Credential{Scope: scope, Perm: tt.credPerm}
		if got := c.EffectivePerm(tt.accountPerm, tt.library, tt.path); got != tt.want {
			t.Errorf("%s: got %q, want %q", tt.name, got, tt.want)
		}
	}
}

// The legacy lane is gone: there are no clients that cannot be changed, so a
// scheme Silo does not mint for is a client that has misread the model rather
// than one to accommodate. It must be refused before a lookup, so nothing
// about which credentials exist is learned by presenting forty hex characters.
func TestTheLegacySchemeIsRefused(t *testing.T) {
	testDB(t)

	r := httptest.NewRequest(http.MethodGet, "/api/silo/v1/libraries", nil)
	r.Header.Set("Authorization", "Token 0401fc662e3bc87a41f299a907c056aaf8322a27")

	_, err := Resolve(r, KindSession, KindDevice)
	if !errors.Is(err, ErrMalformed) {
		t.Errorf("Resolve = %v, want ErrMalformed", err)
	}
}
