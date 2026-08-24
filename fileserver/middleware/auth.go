package middleware

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/dkam/silo/fileserver/account"
	"github.com/dkam/silo/fileserver/authmgr"
	log "github.com/sirupsen/logrus"
)

type contextKey string

// AccountKey carries the authenticated account. It replaced a bare email
// string: the address is still on the account for the responses and commit
// authors that speak in addresses, but permission checks now take the id.
const AccountKey contextKey = "account"

// APITokenKey carries the raw API token that authenticated a request, set by
// RequireAPIToken only. Bearer-JWT requests do not set it.
const APITokenKey contextKey = "api_token"

// ErrInvalidCredential is what a lookup returns when the secret is simply not
// good: unknown, malformed, or expired. Anything else a lookup returns is
// treated as the store being broken rather than the caller being wrong, so an
// outage answers 500 instead of telling every client its token was revoked.
var ErrInvalidCredential = errors.New("invalid credential")

// lookupFunc is the shape every authentication lane has: turn the secret out
// of the header into the account it names. The lanes differ only in how they
// do that, which is why they can share everything around it.
//
// It returns the account rather than its id so that a lane backed by a table
// can join Account into its own lookup and answer in one query, the way
// credential.load does. Returning an id would have made a second round trip
// per authenticated request structural.
type lookupFunc func(ctx context.Context, secret string) (*account.Account, error)

// requireCredential is the one authenticated-request body. A lane supplies the
// scheme word it answers to and the lookup that resolves its secret; the
// header parse, the is-it-still-active check, the responses and the context
// write are shared. carry, when non-empty, is a context key the raw secret is
// stored under for lanes whose handlers need it back.
//
// One body is the point rather than a convenience: deactivating an account has
// to stop every lane at once, and it only does if every lane asks. Written
// twice it is two places to remember, and the next lane — device tokens,
// access URLs, the Silo proof-of-possession scheme credential.Resolve already
// anticipates — would make it three.
func requireCredential(next http.Handler, scheme string, lookup lookupFunc, carry contextKey) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHeader := r.Header.Get("Authorization")
		if authHeader == "" {
			http.Error(w, "Authorization header required", http.StatusUnauthorized)
			return
		}

		parts := strings.SplitN(authHeader, " ", 2)
		if len(parts) != 2 || !strings.EqualFold(parts[0], scheme) {
			http.Error(w, "Invalid authorization format", http.StatusUnauthorized)
			return
		}
		secret := parts[1]

		acct, err := lookup(r.Context(), secret)
		if errors.Is(err, ErrInvalidCredential) {
			http.Error(w, "Invalid or expired token", http.StatusUnauthorized)
			return
		}
		if err != nil {
			log.Errorf("Credential lookup failed: %v", err)
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}

		// Asked here rather than in each lane, because this is the check the
		// identity split exists to make unskippable: deactivating an account
		// has to stop every lane at once, and it only does if every lane asks.
		// A token that only carried an email never asked, so it kept working
		// until it expired.
		if !acct.IsActive {
			http.Error(w, "Invalid or expired token", http.StatusUnauthorized)
			return
		}

		ctx := context.WithValue(r.Context(), AccountKey, acct)
		if carry != "" {
			ctx = context.WithValue(ctx, carry, secret)
		}
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// RequireAuth is middleware that validates a Bearer JWT token and injects
// the authenticated account into the request context.
func RequireAuth(next http.Handler) http.Handler {
	return requireCredential(next, "bearer", sessionLookup, "")
}

// sessionLookup treats a bad signature as a bad token: a JWT is verified from
// its signature alone, so ValidateSessionToken has no store that could be
// down and no error that means anything but "this token is not good".
//
// The account read is this lane's only query — a JWT has no row to join
// against — which is why it lives here rather than in requireCredential. It
// separates "no such account" from a store that is unreachable, the same way
// apiTokenLookup does, so a database outage does not masquerade as every
// session having expired.
func sessionLookup(ctx context.Context, secret string) (*account.Account, error) {
	id, err := authmgr.ValidateSessionToken(secret)
	if err != nil {
		return nil, ErrInvalidCredential
	}
	acct, err := account.ByID(ctx, id)
	if errors.Is(err, account.ErrNotFound) {
		return nil, ErrInvalidCredential
	}
	return acct, err
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
