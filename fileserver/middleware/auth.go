package middleware

import (
	"context"
	"net/http"
	"strings"

	"github.com/dkam/silo/fileserver/authmgr"
)

type contextKey string

const UserEmailKey contextKey = "user_email"

// APITokenKey carries the raw API token that authenticated a request, set by
// RequireAPIToken only. Bearer-JWT requests do not set it.
const APITokenKey contextKey = "api_token"

// RequireAuth is middleware that validates a Bearer JWT token and injects
// the user email into the request context.
func RequireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHeader := r.Header.Get("Authorization")
		if authHeader == "" {
			http.Error(w, "Authorization header required", http.StatusUnauthorized)
			return
		}

		parts := strings.SplitN(authHeader, " ", 2)
		if len(parts) != 2 || !strings.EqualFold(parts[0], "bearer") {
			http.Error(w, "Invalid authorization format", http.StatusUnauthorized)
			return
		}

		email, err := authmgr.ValidateSessionToken(parts[1])
		if err != nil {
			http.Error(w, "Invalid or expired token", http.StatusUnauthorized)
			return
		}

		ctx := context.WithValue(r.Context(), UserEmailKey, email)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// GetUserEmail extracts the authenticated user's email from the request context.
func GetUserEmail(r *http.Request) string {
	email, _ := r.Context().Value(UserEmailKey).(string)
	return email
}
