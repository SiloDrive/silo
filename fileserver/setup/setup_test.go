package setup

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/dkam/silo/fileserver/account"
	"github.com/dkam/silo/fileserver/admin"
	"github.com/dkam/silo/fileserver/dbutil"
	"github.com/dkam/silo/fileserver/option"
)

// storedHash stands in for whatever authmgr.HashPassword produces. Claim takes
// a hash it never inspects, so these tests should not restate the KDF's own
// encoding -- doing so put the storage format in a third package and would have
// made a change to it fail here, in tests about a token.
const storedHash = "not-a-real-hash"

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
	if err := dbutil.Prepare(pair.Write); err != nil {
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

// addAccount creates one directly, the way `silo user add` would -- without
// going anywhere near the setup token.
func addAccount(t *testing.T, email string) account.ID {
	t.Helper()
	ctx, cancel := option.WithDBTimeout(context.Background())
	defer cancel()

	id, created, err := account.Create(ctx, email, storedHash, account.RoleUser)
	if err != nil {
		t.Fatalf("creating %s: %v", email, err)
	}
	if !created {
		t.Fatalf("creating %s: address already taken", email)
	}
	return id
}

func countAccounts(t *testing.T, pair *dbutil.DBPair) int {
	t.Helper()
	var n int
	if err := pair.Read.QueryRow("SELECT COUNT(*) FROM Account").Scan(&n); err != nil {
		t.Fatalf("counting accounts: %v", err)
	}
	return n
}

func tokenRows(t *testing.T, pair *dbutil.DBPair) int {
	t.Helper()
	var n int
	if err := pair.Read.QueryRow("SELECT COUNT(*) FROM SetupToken").Scan(&n); err != nil {
		t.Fatalf("counting setup tokens: %v", err)
	}
	return n
}

func ctx(t *testing.T) context.Context {
	t.Helper()
	c, cancel := option.WithDBTimeout(context.Background())
	t.Cleanup(cancel)
	return c
}

// The token is the same on every boot until it is claimed. An operator who
// scrolled past the first boot's log restarts and reads the same string, rather
// than wondering which of two is live.
func TestEnsureMintsOneTokenAndKeepsIt(t *testing.T) {
	pair := testDB(t)

	first, err := Ensure(ctx(t))
	if err != nil {
		t.Fatalf("first Ensure: %v", err)
	}
	if first.IsZero() {
		t.Fatal("a server with no accounts does not need setting up")
	}

	second, err := Ensure(ctx(t))
	if err != nil {
		t.Fatalf("second Ensure: %v", err)
	}
	if second.IsZero() {
		t.Fatal("the second call says setup is no longer needed")
	}
	if !second.Equal(first) {
		t.Errorf("Ensure minted a second token: %q then %q", first, second)
	}
	if n := tokenRows(t, pair); n != 1 {
		t.Errorf("%d token rows, want 1", n)
	}
}

// A server that has an account is finished with setup, and says so.
func TestEnsureMintsNothingWhenAnAccountExists(t *testing.T) {
	pair := testDB(t)
	addAccount(t, "someone@example.com")

	tok, err := Ensure(ctx(t))
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if !tok.IsZero() {
		t.Errorf("Ensure returned a token (%q) for a server that has an account", tok)
	}
	if n := tokenRows(t, pair); n != 0 {
		t.Errorf("%d token rows, want 0", n)
	}
}

// `silo user add` can create the first account without ever going through
// Claim, which leaves a live token behind. The next boot has to clear it, or
// the log advertises a token that Claim will refuse.
func TestEnsureSweepsAStaleTokenOnceAnAccountExists(t *testing.T) {
	pair := testDB(t)

	if _, err := Ensure(ctx(t)); err != nil {
		t.Fatalf("minting: %v", err)
	}
	addAccount(t, "someone@example.com")

	tok, err := Ensure(ctx(t))
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if !tok.IsZero() {
		t.Error("Ensure still advertises setup after an account was added behind its back")
	}
	if n := tokenRows(t, pair); n != 0 {
		t.Errorf("the stale token row survived: %d rows, want 0", n)
	}
}

// Required is what the wire asks. It must agree with Ensure without writing.
func TestRequiredTracksWhetherAnAccountExists(t *testing.T) {
	pair := testDB(t)

	if req, err := Required(ctx(t)); err != nil || req {
		t.Errorf("Required on a server with no token = %v, %v; want false", req, err)
	}

	tok, err := Ensure(ctx(t))
	if err != nil {
		t.Fatalf("minting: %v", err)
	}
	if req, err := Required(ctx(t)); err != nil || !req {
		t.Errorf("Required with a live token = %v, %v; want true", req, err)
	}

	if _, err := Claim(ctx(t), tok, "me@example.com", storedHash); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if req, err := Required(ctx(t)); err != nil || req {
		t.Errorf("Required after setup = %v, %v; want false", req, err)
	}
	if n := tokenRows(t, pair); n != 0 {
		t.Errorf("%d token rows after the claim, want 0", n)
	}
}

// The operator chooses the address and the password, and gets admin.
func TestClaimCreatesTheFirstAccountAsStaff(t *testing.T) {
	pair := testDB(t)

	tok, err := Ensure(ctx(t))
	if err != nil {
		t.Fatalf("minting: %v", err)
	}

	id, err := Claim(ctx(t), tok, "Chosen@Example.COM", storedHash)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}

	acct, err := account.ByID(ctx(t), id)
	if err != nil {
		t.Fatalf("reading back the account: %v", err)
	}
	if acct.Email != "chosen@example.com" {
		t.Errorf("address is %q, want it normalised to chosen@example.com", acct.Email)
	}
	if !acct.Role.IsAdmin() {
		t.Errorf("the first account is %q, want admin", acct.Role)
	}
	// The role and the full capability set land in the same transaction, for
	// the reason the token and the account do: a first boot that produced a
	// server nobody can administer is not a recoverable state, and an admin
	// holding no capability rows can do nothing at all.
	admin.Init(pair.Read, pair.Write)
	held, err := admin.Of(ctx(t), id)
	if err != nil {
		t.Fatalf("reading the first account's capabilities: %v", err)
	}
	if len(held) != len(admin.All()) {
		t.Errorf("the first account holds %v, want all six", held)
	}
	if !acct.IsActive {
		t.Error("the first account is not active")
	}
	if n := countAccounts(t, pair); n != 1 {
		t.Errorf("%d accounts, want 1", n)
	}
}

// Single use: the token dies with the claim that spent it.
func TestClaimConsumesTheToken(t *testing.T) {
	pair := testDB(t)

	tok, err := Ensure(ctx(t))
	if err != nil {
		t.Fatalf("minting: %v", err)
	}
	if _, err := Claim(ctx(t), tok, "first@example.com", storedHash); err != nil {
		t.Fatalf("first Claim: %v", err)
	}

	_, err = Claim(ctx(t), tok, "second@example.com", storedHash)
	if !errors.Is(err, ErrAlreadySetUp) {
		t.Errorf("second Claim gave %v, want ErrAlreadySetUp", err)
	}
	if n := countAccounts(t, pair); n != 1 {
		t.Errorf("%d accounts after a replayed token, want 1", n)
	}
}

// A wrong guess must not lock the operator out: the real token still works.
func TestAWrongTokenDoesNotBurnTheRealOne(t *testing.T) {
	pair := testDB(t)

	real, err := Ensure(ctx(t))
	if err != nil {
		t.Fatalf("minting: %v", err)
	}
	wrong, err := Generate()
	if err != nil {
		t.Fatalf("generating a wrong token: %v", err)
	}

	if _, err := Claim(ctx(t), wrong, "attacker@example.com", storedHash); !errors.Is(err, ErrBadToken) {
		t.Fatalf("Claim with a wrong token gave %v, want ErrBadToken", err)
	}
	if n := countAccounts(t, pair); n != 0 {
		t.Errorf("a refused claim created %d accounts", n)
	}
	if n := tokenRows(t, pair); n != 1 {
		t.Fatalf("a refused claim left %d token rows, want 1", n)
	}

	if _, err := Claim(ctx(t), real, "owner@example.com", storedHash); err != nil {
		t.Errorf("the real token stopped working after a wrong guess: %v", err)
	}
}

// A leftover row on a server that has an account is unredeemable, whatever the
// boot-time sweep did or did not get to.
func TestClaimRefusesOnceAnAccountExists(t *testing.T) {
	pair := testDB(t)

	tok, err := Ensure(ctx(t))
	if err != nil {
		t.Fatalf("minting: %v", err)
	}
	addAccount(t, "someone@example.com")

	if _, err := Claim(ctx(t), tok, "second@example.com", storedHash); !errors.Is(err, ErrAlreadySetUp) {
		t.Errorf("Claim against a stale row gave %v, want ErrAlreadySetUp", err)
	}
	if n := countAccounts(t, pair); n != 1 {
		t.Errorf("%d accounts, want the 1 that already existed", n)
	}
}

// Claim runs a three-table insert inside its own transaction. account.Create
// opens one of its own, and writeDB is a pool of exactly one connection -- so a
// Claim built on Create rather than CreateTx does not nest, it waits for a
// connection it is itself holding until the context expires. The failure looks
// like a slow database, which is why it is pinned here rather than left to be
// recognised.
func TestClaimDoesNotDeadlockOnTheSingleWriteConnection(t *testing.T) {
	testDB(t)

	tok, err := Ensure(ctx(t))
	if err != nil {
		t.Fatalf("minting: %v", err)
	}

	deadline, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, err := Claim(deadline, tok, "me@example.com", storedHash)
		done <- err
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Claim: %v", err)
		}
	case <-deadline.Done():
		t.Fatal("Claim did not finish in 5s: it is waiting for the write connection it holds")
	}
}

// The race rule. Many claims of one token produce one account, and only one
// caller is told it succeeded.
func TestConcurrentClaimsCreateOneAccount(t *testing.T) {
	pair := testDB(t)

	tok, err := Ensure(ctx(t))
	if err != nil {
		t.Fatalf("minting: %v", err)
	}

	const claimers = 8
	var wg sync.WaitGroup
	errs := make([]error, claimers)
	start := make(chan struct{})

	for i := 0; i < claimers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			<-start
			_, errs[i] = Claim(c, tok, fmt.Sprintf("claimer%d@example.com", i), storedHash)
		}(i)
	}
	close(start)
	wg.Wait()

	won := 0
	for i, err := range errs {
		switch {
		case err == nil:
			won++
		case errors.Is(err, ErrAlreadySetUp):
			// The expected loss.
		default:
			t.Errorf("claimer %d failed with an unexpected error: %v", i, err)
		}
	}

	if won != 1 {
		t.Errorf("%d of %d concurrent claims succeeded, want exactly 1", won, claimers)
	}
	if n := countAccounts(t, pair); n != 1 {
		t.Errorf("%d accounts after %d concurrent claims, want 1", n, claimers)
	}
	if n := tokenRows(t, pair); n != 0 {
		t.Errorf("%d token rows survived the claim, want 0", n)
	}
}
