package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dkam/silo/fileserver/middleware"
	"github.com/dkam/silo/fileserver/option"
	"github.com/dkam/silo/fileserver/utils"
	"github.com/golang-jwt/jwt/v5"
	"github.com/gorilla/mux"
)

// postNotifyToken invokes CreateNotifyTokenHandler as `user` would, seeding the
// context the way RequireAuth does and the route var the way mux would.
func postNotifyToken(t *testing.T, user, repoID string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest("POST", "/api/silo/v1/repos/"+repoID+"/notify-token", nil)
	req = req.WithContext(context.WithValue(req.Context(), middleware.AccountKey, accountOf(t, user)))
	req = mux.SetURLVars(req, map[string]string{"repoid": repoID})

	rr := httptest.NewRecorder()
	CreateNotifyTokenHandler(rr, req)
	return rr
}

func TestCreateNotifyTokenPermissions(t *testing.T) {
	setupPerms(t)
	option.EnableNotification = true

	tests := []struct {
		name     string
		user     string
		repoID   string
		wantCode int
	}{
		{"owner can mint", ownerUser, testRepoID, http.StatusOK},
		{"rw share can mint", rwShareUser, testRepoID, http.StatusOK},
		// Read-only is enough: subscribing to a library you can read tells you
		// nothing you could not learn by polling it.
		{"read-only share can mint", roShareUser, testRepoID, http.StatusOK},
		{"stranger cannot mint", strangerUser, testRepoID, http.StatusForbidden},
		// A repo that doesn't exist looks the same as one you can't see, so the
		// endpoint can't be used to probe for valid repo IDs.
		{"missing repo is forbidden not 404", ownerUser, missingRepoID, http.StatusForbidden},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rr := postNotifyToken(t, tt.user, tt.repoID)
			if rr.Code != tt.wantCode {
				t.Errorf("expected %d, got %d (%s)", tt.wantCode, rr.Code, strings.TrimSpace(rr.Body.String()))
			}
			if rr.Code != http.StatusOK && strings.Contains(rr.Body.String(), "token") {
				t.Errorf("denied request leaked a token: %s", rr.Body.String())
			}
		})
	}
}

// TestCreateNotifyTokenClaims checks the minted token is the same token the
// Seafile-lane endpoint issues — the notification server verifies a signature,
// an audience and a repo id, and has never known how the holder authenticated.
func TestCreateNotifyTokenClaims(t *testing.T) {
	setupPerms(t)
	option.EnableNotification = true

	before := time.Now()
	rr := postNotifyToken(t, ownerUser, testRepoID)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", rr.Code, rr.Body.String())
	}

	var resp notifyTokenResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Token == "" {
		t.Fatal("expected a token, got empty string")
	}

	claims := &utils.MyClaims{}
	tok, err := jwt.ParseWithClaims(resp.Token, claims,
		func(*jwt.Token) (any, error) { return []byte(option.JWTPrivateKey), nil },
		jwt.WithValidMethods([]string{utils.SigningAlg}),
		jwt.WithAudience(utils.AudNotif))
	if err != nil || !tok.Valid {
		t.Fatalf("minted token does not verify as a notification token: %v", err)
	}
	if claims.RepoID != testRepoID {
		t.Errorf("expected repo %s, got %s", testRepoID, claims.RepoID)
	}
	if claims.UserName != ownerUser {
		t.Errorf("expected user %s, got %s", ownerUser, claims.UserName)
	}

	// expires_at must agree with the exp claim, or a client that re-mints on
	// the advertised deadline gets disconnected before it acts.
	if claims.ExpiresAt == nil || claims.ExpiresAt.Unix() != resp.ExpiresAt {
		t.Errorf("expires_at %d does not match exp claim %v", resp.ExpiresAt, claims.ExpiresAt)
	}
	wantLo, wantHi := before.Add(notifyTokenTTL).Unix(), time.Now().Add(notifyTokenTTL).Unix()
	if resp.ExpiresAt < wantLo || resp.ExpiresAt > wantHi {
		t.Errorf("expires_at %d outside expected window [%d,%d]", resp.ExpiresAt, wantLo, wantHi)
	}
}

// TestCreateNotifyTokenWithNotificationDisabled: a server with no notification
// endpoint says so, rather than answering 403 about a library the caller can
// read perfectly well.
func TestCreateNotifyTokenWithNotificationDisabled(t *testing.T) {
	setupPerms(t)
	option.EnableNotification = false

	rr := postNotifyToken(t, ownerUser, testRepoID)
	if rr.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d (%s)", rr.Code, strings.TrimSpace(rr.Body.String()))
	}
}
