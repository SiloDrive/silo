package middleware

import (
	"context"
	"net/http"
	"strings"

	"github.com/dkam/silo/fileserver/account"
	"github.com/dkam/silo/fileserver/authmgr"
)

type contextKey string

// AccountKey carries the authenticated account. It replaced a bare email
// string: the address is still on the account for the responses and commit
// authors that speak in addresses, but permission checks now take the id.
const AccountKey contextKey = "account"

// APITokenKey carries the raw API token that authenticated a request, set by
// RequireAPIToken only. Bearer-JWT requests do not set it.
const APITokenKey contextKey = "api_token"

// RequireAuth is middleware that validates a Bearer JWT token and injects
// the authenticated account into the request context.
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

		id, err := authmgr.ValidateSessionToken(parts[1])
		if err != nil {
			http.Error(w, "Invalid or expired token", http.StatusUnauthorized)
			return
		}

		// The token names an account rather than an address, so the account
		// has to be read to get one — which is what makes a session stop
		// working the moment the account is deactivated. A token that only
		// carried an email never asked, so it kept working until it expired.
		acct, err := account.ByID(r.Context(), id)
		if err != nil || !acct.IsActive {
			http.Error(w, "Invalid or expired token", http.StatusUnauthorized)
			return
		}

		ctx := context.WithValue(r.Context(), AccountKey, acct)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// WithAccount returns a request carrying an authenticated account, for the
// lanes that authenticate some other way than a session token.
func WithAccount(r *http.Request, acct *account.Account) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), AccountKey, acct))
}

// GetAccount returns the authenticated account, or nil.
func GetAccount(r *http.Request) *account.Account {
	acct, _ := r.Context().Value(AccountKey).(*account.Account)
	return acct
}

// GetAccountID returns the authenticated account's id, or the zero id when
// there is no account. Every permission check keys on this.
func GetAccountID(r *http.Request) account.ID {
	if acct := GetAccount(r); acct != nil {
		return acct.ID
	}
	return account.Zero
}

// GetUserEmail returns the authenticated account's primary address.
//
// It survives the identity split because an address is still what a commit
// records as its author and what the /api2 responses hand back. What it is no
// longer is a key: nothing looks a user up by the string this returns.
func GetUserEmail(r *http.Request) string {
	if acct := GetAccount(r); acct != nil {
		return acct.Email
	}
	return ""
}
