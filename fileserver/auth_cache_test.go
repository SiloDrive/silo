package silod

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/dkam/silo/fileserver/option"
	"github.com/dkam/silo/fileserver/repomgr"
)

// authCacheTest points the package at a throwaway database with one sync
// token, and empties the process-global caches so one test cannot see
// another's entries.
func authCacheTest(t *testing.T) {
	t.Helper()

	sqliteTestDB(t)
	repomgr.Init(siloPair.Read, siloPair.Write)

	origTTL := option.AuthCacheTTL
	option.AuthCacheTTL = 5 * time.Minute
	t.Cleanup(func() { option.AuthCacheTTL = origTTL })

	clearAuthCaches()
	t.Cleanup(clearAuthCaches)

	dbExec(t, "INSERT INTO RepoUserToken (repo_id, email, token, ctime) VALUES (?, ?, ?, ?)",
		sharedRepoID, victim, victimSyncA, time.Now().Unix())
}

func clearAuthCaches() {
	tokenCache.Range(func(k, _ interface{}) bool { tokenCache.Delete(k); return true })
	permCache.Range(func(k, _ interface{}) bool { permCache.Delete(k); return true })
	virtualRepoInfoCache.Range(func(k, _ interface{}) bool { virtualRepoInfoCache.Delete(k); return true })
}

func tokenRequest(token string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Seafile-Repo-Token", token)
	return req
}

// A cache hit used to be served without looking at its expiry — only a
// five-minute sweeper ever removed a stale entry, so a token stayed valid for
// its full TTL plus however long until the sweep. Revoking it changed nothing
// until then.
func TestValidateTokenRejectsAnExpiredCacheEntry(t *testing.T) {
	authCacheTest(t)

	email, appErr := validateToken(tokenRequest(victimSyncA), sharedRepoID, false)
	if appErr != nil {
		t.Fatalf("validateToken returned %d: %v", appErr.Code, appErr.Message)
	}
	if email != victim {
		t.Fatalf("validateToken returned %q, want %q", email, victim)
	}

	// Revoke out of process, the way `silo token revoke` does.
	dbExec(t, "DELETE FROM RepoUserToken WHERE token = ?", victimSyncA)

	// The entry is still live, so the token still works — this is the window
	// AuthCacheTTL bounds, and it is deliberate.
	if _, appErr := validateToken(tokenRequest(victimSyncA), sharedRepoID, false); appErr != nil {
		t.Fatalf("a live cache entry stopped working: %d %v", appErr.Code, appErr.Message)
	}

	// Age the entry out. Once expired it must be re-checked, not served.
	value, ok := tokenCache.Load(victimSyncA)
	if !ok {
		t.Fatal("the token was not cached at all")
	}
	value.(*tokenInfo).expireTime = time.Now().Unix() - 1

	if _, appErr := validateToken(tokenRequest(victimSyncA), sharedRepoID, false); appErr == nil {
		t.Error("an expired cache entry authenticated a revoked token")
	} else if appErr.Code != http.StatusForbidden {
		t.Errorf("status = %d, want %d", appErr.Code, http.StatusForbidden)
	}
}

// With caching off, a revocation takes effect on the very next request.
func TestValidateTokenWithCachingDisabled(t *testing.T) {
	authCacheTest(t)
	option.AuthCacheTTL = 0

	if _, appErr := validateToken(tokenRequest(victimSyncA), sharedRepoID, false); appErr != nil {
		t.Fatalf("validateToken returned %d: %v", appErr.Code, appErr.Message)
	}
	if _, ok := tokenCache.Load(victimSyncA); ok {
		t.Error("the token was cached even though caching is disabled")
	}

	dbExec(t, "DELETE FROM RepoUserToken WHERE token = ?", victimSyncA)
	if _, appErr := validateToken(tokenRequest(victimSyncA), sharedRepoID, false); appErr == nil {
		t.Error("a revoked token still authenticated with caching disabled")
	}
}

// Deleting a library has to drop its cached authorisations. A cached
// permission outlives the rows it came from, so the server would keep
// accepting uploads into a library that no longer exists — recreating the
// storage directories gc had just reclaimed.
func TestInvalidateRepoAuthDropsOnlyThatRepo(t *testing.T) {
	authCacheTest(t)

	if _, appErr := validateToken(tokenRequest(victimSyncA), sharedRepoID, false); appErr != nil {
		t.Fatalf("validateToken returned %d: %v", appErr.Code, appErr.Message)
	}
	permCache.Store(sharedRepoID+":"+victim+":upload", &permInfo{
		expireTime: time.Now().Add(time.Hour).Unix(),
	})
	permCache.Store(otherRepoID+":"+victim+":upload", &permInfo{
		expireTime: time.Now().Add(time.Hour).Unix(),
	})
	virtualRepoInfoCache.Store(sharedRepoID, &virtualRepoInfo{storeID: sharedRepoID})

	invalidateRepoAuth(sharedRepoID)

	if _, ok := tokenCache.Load(victimSyncA); ok {
		t.Error("the deleted repo's token is still cached")
	}
	if _, ok := permCache.Load(sharedRepoID + ":" + victim + ":upload"); ok {
		t.Error("the deleted repo's permission is still cached")
	}
	if _, ok := virtualRepoInfoCache.Load(sharedRepoID); ok {
		t.Error("the deleted repo's store id is still cached")
	}
	if _, ok := permCache.Load(otherRepoID + ":" + victim + ":upload"); !ok {
		t.Error("another repo's permission was dropped")
	}
}

// Revoking in-process takes effect immediately rather than at the next expiry,
// and stops at the user named.
func TestInvalidateUserAuthDropsOnlyThatUser(t *testing.T) {
	authCacheTest(t)
	dbExec(t, "INSERT INTO RepoUserToken (repo_id, email, token, ctime) VALUES (?, ?, ?, ?)",
		sharedRepoID, bystander, bystanderSync, time.Now().Unix())

	for _, token := range []string{victimSyncA, bystanderSync} {
		if _, appErr := validateToken(tokenRequest(token), sharedRepoID, false); appErr != nil {
			t.Fatalf("validateToken(%s) returned %d: %v", token, appErr.Code, appErr.Message)
		}
	}

	invalidateUserAuth(victim)

	if _, ok := tokenCache.Load(victimSyncA); ok {
		t.Error("the revoked user's token is still cached")
	}
	if _, ok := tokenCache.Load(bystanderSync); !ok {
		t.Error("another user's token was dropped")
	}
}

// The hook has to be reachable from repomgr, which cannot import this package.
func TestRepomgrRevocationHookIsWired(t *testing.T) {
	authCacheTest(t)

	orig := repomgr.OnTokensRevoked
	repomgr.OnTokensRevoked = invalidateUserAuth
	t.Cleanup(func() { repomgr.OnTokensRevoked = orig })

	if _, appErr := validateToken(tokenRequest(victimSyncA), sharedRepoID, false); appErr != nil {
		t.Fatalf("validateToken returned %d: %v", appErr.Code, appErr.Message)
	}
	if _, err := repomgr.DeleteRepoTokensByEmail(victim); err != nil {
		t.Fatalf("DeleteRepoTokensByEmail returned %v", err)
	}

	if _, ok := tokenCache.Load(victimSyncA); ok {
		t.Fatal("revoking through repomgr left the token cached")
	}
	if _, appErr := validateToken(tokenRequest(victimSyncA), sharedRepoID, false); appErr == nil {
		t.Error("a token revoked in-process still authenticated")
	}
}
