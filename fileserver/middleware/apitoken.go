package middleware

import (
	"context"
	"errors"
	"net/http"

	"github.com/dkam/silo/fileserver/account"
	"github.com/dkam/silo/fileserver/apitokenstore"
)

// RequireAPIToken validates a Seahub/DRF-style "Authorization: Token <token>"
// header and injects the authenticated account into the request context.
func RequireAPIToken(next http.Handler) http.Handler {
	return requireCredential("token", apiTokenLookup, carryAPIToken)(next)
}

// apiTokenLookup separates "no such token" from a store that is unreachable,
// so a database outage does not masquerade as every client being signed out.
func apiTokenLookup(secret string) (account.ID, error) {
	id, err := apitokenstore.Lookup(secret)
	if errors.Is(err, apitokenstore.ErrNotFound) {
		return account.Zero, ErrInvalidCredential
	}
	return id, err
}

// carryAPIToken puts the token itself on the context so a handler can revoke
// the exact credential that authenticated the request without re-parsing the
// header.
func carryAPIToken(ctx context.Context, secret string) context.Context {
	return context.WithValue(ctx, APITokenKey, secret)
}

// GetAPIToken returns the API token that authenticated the request, or "" if
// the request did not come through RequireAPIToken.
func GetAPIToken(r *http.Request) string {
	token, _ := r.Context().Value(APITokenKey).(string)
	return token
}
