package silod

import (
	"testing"
	"time"

	"github.com/dkam/silo/fileserver/apitokenstore"
	"github.com/dkam/silo/fileserver/option"
	"github.com/dkam/silo/fileserver/repomgr"
)

const (
	victim    = "victim@example.com"
	bystander = "bystander@example.com"

	victimSyncA    = "aaaa1111aaaa1111aaaa1111aaaa1111aaaa1111"
	victimSyncB    = "bbbb2222bbbb2222bbbb2222bbbb2222bbbb2222"
	bystanderSync  = "cccc3333cccc3333cccc3333cccc3333cccc3333"
	victimAPI      = "dddd4444dddd4444dddd4444dddd4444dddd4444"
	bystanderAPI   = "eeee5555eeee5555eeee5555eeee5555eeee5555"
	sharedRepoID   = "b1f2ad61-9164-418a-a47f-ab805dbd5694"
	otherRepoID    = "c2e3be72-a275-429b-b580-bc916ece6705"
	unknownTokenID = "ffff6666ffff6666ffff6666ffff6666ffff6666"
)

// tokenTestStore seeds both token tables with two users, so every test can
// check that a revocation stopped at the user it named.
func tokenTestStore(t *testing.T) {
	t.Helper()

	sqliteTestDB(t)
	repomgr.Init(seafilePair.Read, seafilePair.Write)
	apitokenstore.Init(seafilePair.Read, seafilePair.Write)

	origTTL := option.APITokenTTL
	option.APITokenTTL = 30 * 24 * time.Hour
	t.Cleanup(func() { option.APITokenTTL = origTTL })

	now := time.Now().Unix()
	for _, tok := range []struct {
		repoID, email, token string
	}{
		{sharedRepoID, victim, victimSyncA},
		{otherRepoID, victim, victimSyncB},
		{sharedRepoID, bystander, bystanderSync},
	} {
		dbExec(t, "INSERT INTO RepoUserToken (repo_id, email, token, ctime) VALUES (?, ?, ?, ?)",
			tok.repoID, tok.email, tok.token, now)
	}
	for _, tok := range []struct {
		token, email string
	}{
		{victimAPI, victim},
		{bystanderAPI, bystander},
	} {
		dbExec(t, "INSERT INTO ApiToken (token, email, ctime, expires_at) VALUES (?, ?, ?, ?)",
			tok.token, tok.email, now, now+86400)
	}
}

func countRows(t *testing.T, query string, args ...interface{}) int {
	t.Helper()
	var n int
	if err := seafilePair.Read.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("failed to count with %q: %v", query, err)
	}
	return n
}

// A sync token has no expiry — Seafile clients persist it and treat it as
// durable — so revocation is the only thing that can ever invalidate one. It
// had no reachable caller, which left a token copied off a stolen device with
// permanent read/write access that a password change did not touch.
func TestRevokeAllTokensRemovesBothKinds(t *testing.T) {
	tokenTestStore(t)

	if err := revokeAllTokens(victim); err != nil {
		t.Fatalf("revokeAllTokens returned %v", err)
	}

	if n := countRows(t, "SELECT COUNT(*) FROM RepoUserToken WHERE email = ?", victim); n != 0 {
		t.Errorf("%d sync tokens survived revocation, want 0", n)
	}
	if n := countRows(t, "SELECT COUNT(*) FROM ApiToken WHERE email = ?", victim); n != 0 {
		t.Errorf("%d API tokens survived revocation, want 0", n)
	}

	// Revoking one account must not sign out the rest of the server.
	if n := countRows(t, "SELECT COUNT(*) FROM RepoUserToken WHERE email = ?", bystander); n != 1 {
		t.Errorf("bystander has %d sync tokens, want 1", n)
	}
	if n := countRows(t, "SELECT COUNT(*) FROM ApiToken WHERE email = ?", bystander); n != 1 {
		t.Errorf("bystander has %d API tokens, want 1", n)
	}
}

// Revoking one device must leave the user's other devices syncing — that is
// the whole reason tokens are minted per login rather than reused.
func TestRevokeOneSyncTokenLeavesTheOthers(t *testing.T) {
	tokenTestStore(t)

	if err := revokeOneToken(victim, victimSyncA); err != nil {
		t.Fatalf("revokeOneToken returned %v", err)
	}

	if n := countRows(t, "SELECT COUNT(*) FROM RepoUserToken WHERE token = ?", victimSyncA); n != 0 {
		t.Errorf("the revoked token is still present")
	}
	if n := countRows(t, "SELECT COUNT(*) FROM RepoUserToken WHERE token = ?", victimSyncB); n != 1 {
		t.Errorf("the user's other sync token was removed too")
	}
	if n := countRows(t, "SELECT COUNT(*) FROM ApiToken WHERE email = ?", victim); n != 1 {
		t.Errorf("the user's API token was removed by a sync-token revocation")
	}
}

func TestRevokeOneAPIToken(t *testing.T) {
	tokenTestStore(t)

	if err := revokeOneToken(victim, victimAPI); err != nil {
		t.Fatalf("revokeOneToken returned %v", err)
	}

	if n := countRows(t, "SELECT COUNT(*) FROM ApiToken WHERE token = ?", victimAPI); n != 0 {
		t.Errorf("the revoked API token is still present")
	}
	if n := countRows(t, "SELECT COUNT(*) FROM RepoUserToken WHERE email = ?", victim); n != 2 {
		t.Errorf("sync tokens were removed by an API-token revocation")
	}
}

// The token is matched against the named user's own tokens before anything is
// deleted. Deleting by value alone would let a mistyped email revoke a
// credential belonging to someone else, and report success for it.
func TestRevokeOneTokenRefusesAnotherUsersToken(t *testing.T) {
	tokenTestStore(t)

	for _, token := range []string{bystanderSync, bystanderAPI, unknownTokenID} {
		if err := revokeOneToken(victim, token); err == nil {
			t.Errorf("revokeOneToken(%s, %s) succeeded, want an error", victim, token)
		}
	}

	if n := countRows(t, "SELECT COUNT(*) FROM RepoUserToken WHERE email = ?", bystander); n != 1 {
		t.Errorf("bystander's sync token was revoked")
	}
	if n := countRows(t, "SELECT COUNT(*) FROM ApiToken WHERE email = ?", bystander); n != 1 {
		t.Errorf("bystander's API token was revoked")
	}
}

func TestListTokensOnAnAccountWithNone(t *testing.T) {
	tokenTestStore(t)

	if err := listTokens("nobody@example.com"); err != nil {
		t.Errorf("listTokens returned %v for an account with no tokens", err)
	}
	if err := listTokens(victim); err != nil {
		t.Errorf("listTokens returned %v", err)
	}
}
