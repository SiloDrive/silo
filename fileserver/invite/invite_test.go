package invite

import (
	"context"
	"errors"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/dkam/silo/fileserver/account"
	"github.com/dkam/silo/fileserver/credential"
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
		t.Fatalf("open db: %v", err)
	}
	if err := dbutil.Prepare(pair.Write); err != nil {
		t.Fatalf("create tables: %v", err)
	}
	t.Cleanup(func() { _ = pair.Close() })
	account.Init(pair.Read, pair.Write)
	credential.Init(pair.Read, pair.Write)
	Init(pair.Read, pair.Write)
	return pair
}

func ctx(t *testing.T) context.Context {
	t.Helper()
	c, cancel := option.WithDBTimeout(context.Background())
	t.Cleanup(cancel)
	return c
}

func admin(t *testing.T) account.ID {
	t.Helper()
	id, _, err := account.Create(ctx(t), "root@example.com", "", account.RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// Minting an invite makes the account it is for, inactive and with no password.
//
// It has to make one: the credential row references an account, and the whole
// point of the invite is that the person does not have one yet. An inactive
// account with no password opens no lane -- credential.load refuses inactive
// outright -- so the row is a placeholder for an address rather than a way in.
func TestMintingAnInviteMakesTheAccountItNames(t *testing.T) {
	testDB(t)
	by := admin(t)

	inv, token, err := Mint(ctx(t), Options{Email: "New@Example.COM", Role: account.RoleUser, By: by})
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if token == "" {
		t.Fatal("Mint returned no token")
	}
	if inv.Email != "new@example.com" {
		t.Errorf("the invite names %q, want it normalised", inv.Email)
	}

	acct, err := account.ByEmail(ctx(t), "new@example.com")
	if err != nil {
		t.Fatalf("the invite did not make an account for its address: %v", err)
	}
	if acct.IsActive {
		t.Error("the account an invite made is active before anybody redeemed it")
	}
}

// The redeemer does not choose their address: delivery of the invite to that
// inbox is the verification step, and an address the redeemer could pick would
// verify nothing.
func TestRedeemingActivatesTheAccountTheInviteNames(t *testing.T) {
	testDB(t)
	by := admin(t)
	inv, token, err := Mint(ctx(t), Options{Email: "new@example.com", Role: account.RoleGuest, By: by})
	if err != nil {
		t.Fatal(err)
	}

	redeemed, err := Redeem(ctx(t), token)
	if err != nil {
		t.Fatalf("Redeem: %v", err)
	}
	if redeemed.Email != "new@example.com" {
		t.Errorf("redeemed %q", redeemed.Email)
	}

	acct, err := account.ByEmail(ctx(t), "new@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if !acct.IsActive {
		t.Error("redeeming did not activate the account")
	}
	if acct.Role != account.RoleGuest {
		t.Errorf("role = %q, want the one the invite carried", acct.Role)
	}
	if redeemed.AccountID != acct.ID {
		t.Error("Redeem named a different account than the invite's address holds")
	}
	// The invite is spent, and says so.
	if inv.RedeemedAt != 0 {
		t.Error("the invite was minted already redeemed")
	}
}

// Single-use, and enforced by the write rather than by a check a caller might
// skip.
func TestAnInviteIsSpentOnce(t *testing.T) {
	testDB(t)
	by := admin(t)
	_, token, err := Mint(ctx(t), Options{Email: "new@example.com", Role: account.RoleUser, By: by})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Redeem(ctx(t), token); err != nil {
		t.Fatal(err)
	}
	if _, err := Redeem(ctx(t), token); !errors.Is(err, ErrSpent) {
		t.Errorf("redeeming twice = %v, want ErrSpent", err)
	}
}

// Claiming an account somebody is already using is refused outright. Minting a
// second account beside it would recreate the double-mint the identity split
// made unrepresentable, and activating it would hand a stranger the libraries
// shared to that address.
func TestAnInviteForAnActiveAccountIsRefused(t *testing.T) {
	testDB(t)
	by := admin(t)
	if _, _, err := account.Create(ctx(t), "taken@example.com", "hash", account.RoleUser); err != nil {
		t.Fatal(err)
	}

	if _, _, err := Mint(ctx(t), Options{Email: "taken@example.com", Role: account.RoleUser, By: by}); !errors.Is(err, ErrActiveAccount) {
		t.Errorf("minting for an active account = %v, want ErrActiveAccount", err)
	}
}

// A tombstone is exactly what an invite is for: an address that appeared in a
// share before its person arrived. Redemption claims it, so whatever was shared
// to the address is already theirs.
func TestAnInviteClaimsATombstoneRatherThanMintingBesideIt(t *testing.T) {
	pair := testDB(t)
	by := admin(t)

	// A tombstone: an account for an address nobody has enrolled.
	tomb, _, err := account.Create(ctx(t), "shared-with@example.com", "", account.RoleUser)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pair.Write.Exec("UPDATE Account SET is_active = 0 WHERE id = ?", tomb); err != nil {
		t.Fatal(err)
	}

	_, token, err := Mint(ctx(t), Options{Email: "shared-with@example.com", Role: account.RoleUser, By: by})
	if err != nil {
		t.Fatalf("Mint over a tombstone: %v", err)
	}
	redeemed, err := Redeem(ctx(t), token)
	if err != nil {
		t.Fatal(err)
	}
	if redeemed.AccountID != tomb {
		t.Error("redemption made a second account beside the tombstone")
	}
	var n int
	if err := pair.Read.QueryRow("SELECT COUNT(*) FROM AccountEmail WHERE email = ?",
		"shared-with@example.com").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("the address is held by %d accounts, want 1", n)
	}
}

// An expired invite is refused, and the refusal is about the invite rather than
// about the address -- an invite that had lapsed and one that never existed are
// the same answer to somebody holding a link.
func TestAnExpiredInviteIsRefused(t *testing.T) {
	pair := testDB(t)
	by := admin(t)
	inv, token, err := Mint(ctx(t), Options{
		Email: "late@example.com", Role: account.RoleUser, By: by, Lifetime: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pair.Write.Exec("UPDATE Credential SET expires_at = ? WHERE id = ?",
		time.Now().Add(-time.Hour).Unix(), inv.CredentialID); err != nil {
		t.Fatal(err)
	}
	if _, err := Redeem(ctx(t), token); err == nil {
		t.Error("an expired invite was redeemed")
	}
}

func TestAGarbledInviteIsRefused(t *testing.T) {
	testDB(t)
	for _, bad := range []string{"", "not-a-token", "silo_session_aaaa_bbbb"} {
		if _, err := Redeem(ctx(t), bad); err == nil {
			t.Errorf("Redeem accepted %q", bad)
		}
	}
}

// Listing is the admin surface's question: who has been invited, and which of
// those are still outstanding.
func TestListingReportsOutstandingInvites(t *testing.T) {
	testDB(t)
	by := admin(t)
	if _, _, err := Mint(ctx(t), Options{Email: "a@example.com", Role: account.RoleUser, By: by}); err != nil {
		t.Fatal(err)
	}
	_, token, err := Mint(ctx(t), Options{Email: "b@example.com", Role: account.RoleUser, By: by})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Redeem(ctx(t), token); err != nil {
		t.Fatal(err)
	}

	all, err := List(ctx(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("listed %d invites, want 2", len(all))
	}
	for _, i := range all {
		switch i.Email {
		case "a@example.com":
			if i.RedeemedAt != 0 {
				t.Error("an unredeemed invite reports a redemption")
			}
		case "b@example.com":
			if i.RedeemedAt == 0 {
				t.Error("a redeemed invite does not say when")
			}
		}
	}
}

// The property everything else here rests on: an invite is not a session.
//
// It is a bearer token delivered to an inbox, for an account that does not yet
// have a person behind it. If it authenticated an ordinary API request it would
// be a way to read that address's libraries -- the ones shared to it before its
// person arrived, which is exactly the case invites exist to hand over
// deliberately.
//
// Two things stop it and they are independent. The API's lane list does not
// include the invite kind, so Resolve refuses it before looking at anything.
// And the account is inactive until redemption, which Resolve refuses
// separately. Either alone would do; both is the point.
func TestAnInviteIsNotASession(t *testing.T) {
	testDB(t)
	by := admin(t)
	_, token, err := Mint(ctx(t), Options{Email: "new@example.com", Role: account.RoleUser, By: by})
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("GET", "/api/silo/v1/libraries", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	if _, err := credential.Resolve(req, credential.KindSession, credential.KindDevice); err == nil {
		t.Fatal("an invite token authenticated an ordinary API request")
	} else if !errors.Is(err, credential.ErrWrongKind) {
		t.Errorf("refusal = %v, want ErrWrongKind", err)
	}

	// And after redemption, when the account is active and the kind is the only
	// thing left standing between the token and the account.
	if _, err := Redeem(ctx(t), token); err != nil {
		t.Fatal(err)
	}
	if _, err := credential.Resolve(req, credential.KindSession, credential.KindDevice); err == nil {
		t.Fatal("a redeemed invite token authenticated an ordinary API request")
	}
}

// Two redemptions racing produce one winner.
//
// The sequential test above is caught by the read before the write, which
// proves the ordinary case and not the interesting one: two requests that both
// read an unredeemed invite before either writes. What separates them is that
// the spend is a conditional update -- whoever sets redeemed_at from NULL
// proceeds -- and nothing else here would notice if it stopped being one.
//
// Without it both callers activate the account and both are told they redeemed
// it, which is one invite admitting two people.
func TestTwoRedemptionsRacingProduceOneWinner(t *testing.T) {
	testDB(t)
	by := admin(t)
	_, token, err := Mint(ctx(t), Options{Email: "new@example.com", Role: account.RoleUser, By: by})
	if err != nil {
		t.Fatal(err)
	}

	const racers = 8
	var wg sync.WaitGroup
	results := make([]error, racers)
	start := make(chan struct{})
	for i := range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			c, cancel := option.WithDBTimeout(context.Background())
			defer cancel()
			_, results[i] = Redeem(c, token)
		}()
	}
	close(start)
	wg.Wait()

	var won int
	for _, err := range results {
		if err == nil {
			won++
			continue
		}
		if !errors.Is(err, ErrSpent) {
			t.Errorf("a loser got %v, want ErrSpent", err)
		}
	}
	if won != 1 {
		t.Errorf("%d of %d redemptions succeeded, want exactly 1", won, racers)
	}
}

// Revoking an outstanding invite closes the link and leaves the trail.
//
// The link has to actually stop working, which is the half a no-op Revoke would
// pass. The trail has to survive, which is the half a delete would fail: the
// Invite row is the record of who was invited and by whom, and it is the reason
// revocation expires the credential instead of removing it.
//
// The tombstone account stays too, and stays inactive. Revoke cannot tell an
// account it caused to exist from one that was already standing for a share to
// an address nobody had enrolled -- Mint reuses rather than duplicates -- so
// deleting it would sometimes take a share's referent with it. Leaving it costs
// nothing, because a tombstone opens no lane.
func TestRevokingAnInviteClosesTheLinkAndKeepsTheTrail(t *testing.T) {
	testDB(t)
	by := admin(t)
	inv, token, err := Mint(ctx(t), Options{
		Email: "gone@example.com", Role: account.RoleUser, By: by,
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := Revoke(ctx(t), inv.CredentialID); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if _, err := Redeem(ctx(t), token); err == nil {
		t.Fatal("a revoked invite was redeemed")
	} else if !errors.Is(err, credential.ErrExpired) {
		t.Errorf("refusal = %v, want ErrExpired", err)
	}

	// Revoking again is not an error. There is no state to conflict over: the
	// invite is outstanding both times, and the second call writes the same
	// answer the first one did.
	if err := Revoke(ctx(t), inv.CredentialID); err != nil {
		t.Errorf("revoking twice: %v", err)
	}

	found, err := List(ctx(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 {
		t.Fatalf("List reported %d invites, want 1 -- revocation took the record with it", len(found))
	}
	if found[0].Email != "gone@example.com" || found[0].CreatedBy != by {
		t.Errorf("trail = %+v, want the address and the administrator who minted it", found[0])
	}
	if found[0].RedeemedAt != 0 {
		t.Errorf("RedeemedAt = %d, want 0 -- a revoked invite was never redeemed", found[0].RedeemedAt)
	}

	acct, err := account.ByID(ctx(t), inv.AccountID)
	if err != nil {
		t.Fatalf("the tombstone went with the invite: %v", err)
	}
	if acct.IsActive {
		t.Error("revoking an invite activated the account it named")
	}
}

// Revoking a redeemed invite is refused, and does not touch the account.
//
// There is nothing left to withdraw: the person arrived, and the credential's
// expiry is no longer what stands between anybody and that account. Expiring it
// anyway would report success for an operator's decision it did not carry out.
// Taking the account back is a separate operation with a separate name, and the
// refusal is what sends them to it.
func TestRevokingASpentInviteIsRefused(t *testing.T) {
	testDB(t)
	by := admin(t)
	inv, token, err := Mint(ctx(t), Options{
		Email: "arrived@example.com", Role: account.RoleUser, By: by,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Redeem(ctx(t), token); err != nil {
		t.Fatal(err)
	}

	if err := Revoke(ctx(t), inv.CredentialID); err == nil {
		t.Fatal("a redeemed invite was revoked")
	} else if !errors.Is(err, ErrSpent) {
		t.Errorf("refusal = %v, want ErrSpent", err)
	}

	acct, err := account.ByID(ctx(t), inv.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	if !acct.IsActive {
		t.Error("a refused revocation deactivated the account anyway")
	}
}

// An invite that never existed and one that was already revoked away are the
// same answer to an operator as to a redeemer.
func TestRevokingAnInviteThatDoesNotExistIsNotFound(t *testing.T) {
	testDB(t)
	if err := Revoke(ctx(t), "no-such-credential"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Revoke = %v, want ErrNotFound", err)
	}
}
