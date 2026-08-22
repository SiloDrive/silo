package middleware

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/dkam/silo/fileserver/account"
	"github.com/dkam/silo/fileserver/apitokenstore"
	log "github.com/sirupsen/logrus"
)

// RequireAPIToken validates a Seahub/DRF-style "Authorization: Token <token>"
// header and injects the authenticated account into the request context.
func RequireAPIToken(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHeader := r.Header.Get("Authorization")
		if authHeader == "" {
			http.Error(w, "Authorization header required", http.StatusUnauthorized)
			return
		}

		parts := strings.SplitN(authHeader, " ", 2)
		if len(parts) != 2 || !strings.EqualFold(parts[0], "token") {
			http.Error(w, "Invalid authorization format", http.StatusUnauthorized)
			return
		}

		id, err := apitokenstore.Lookup(parts[1])
		if errors.Is(err, apitokenstore.ErrNotFound) {
			http.Error(w, "Invalid or expired token", http.StatusUnauthorized)
			return
		}
		if err != nil {
			log.Errorf("API token lookup failed: %v", err)
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}

		// The token names an account, so the account is read to get the
		// address the /api2 responses still speak in — and, on the way, to
		// find out whether it is still allowed to do anything.
		acct, err := account.ByID(r.Context(), id)
		if err != nil || !acct.IsActive {
			http.Error(w, "Invalid or expired token", http.StatusUnauthorized)
			return
		}

		ctx := context.WithValue(r.Context(), AccountKey, acct)
		// Carry the token itself so a handler can revoke the exact credential
		// that authenticated the request without re-parsing the header.
		ctx = context.WithValue(ctx, APITokenKey, parts[1])
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// GetAPIToken returns the API token that authenticated the request, or "" if
// the request did not come through RequireAPIToken.
func GetAPIToken(r *http.Request) string {
	token, _ := r.Context().Value(APITokenKey).(string)
	return token
}
