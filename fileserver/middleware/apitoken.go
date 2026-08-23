package middleware

import (
	"context"
	"errors"
	"net/http"

	"github.com/dkam/silo/fileserver/account"
	"github.com/dkam/silo/fileserver/apitokenstore"
)

// RequireAPIToken validates an "Authorization: Token <token>" header and
// injects the authenticated account into the request context.
//
// APITokenKey carries the raw token through so a handler can revoke the exact
// credential that authenticated the request without re-parsing the header.
//
// ORPHANED: the routes that used this were deleted with the sync lanes, so
// nothing mounts it today. It is left standing because whether the credential
// itself survives is an auth decision rather than a consequence of deleting a
// file format. See docs/auth.md.
func RequireAPIToken(next http.Handler) http.Handler {
	return requireCredential(next, "token", apiTokenLookup, APITokenKey)
}

// apiTokenLookup resolves a token and its account in one statement. The join
// is the difference between one query per request and two, the same trade
// credential.load makes.
//
// It separates "no such token" from a store that is unreachable, so a database
// outage does not masquerade as every client being signed out.
func apiTokenLookup(ctx context.Context, secret string) (*account.Account, error) {
	acct, err := apitokenstore.LookupAccount(ctx, secret)
	if errors.Is(err, apitokenstore.ErrNotFound) {
		return nil, ErrInvalidCredential
	}
	return acct, err
}

// GetAPIToken returns the API token that authenticated the request, or "" if
// the request did not come through RequireAPIToken.
func GetAPIToken(r *http.Request) string {
	token, _ := r.Context().Value(APITokenKey).(string)
	return token
}
