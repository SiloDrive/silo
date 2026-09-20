package middleware

// The account a request is authenticated as, and how a handler reads it.
//
// Authentication itself lives in credential.go: one lane, one Resolve, one
// place that decides whether a caller is who they say. This file is only the
// context keys and the accessors, so that a handler asking "who is this?"
// does not have to know how the question was answered.

import (
	"context"
	"net/http"

	"github.com/SiloDrive/silo/fileserver/account"
)

type contextKey string

// AccountKey carries the authenticated account. It replaced a bare email
// string: the address is still on the account for the responses and commit
// authors that speak in addresses, but permission checks now take the id.
const AccountKey contextKey = "account"

// WithAccount returns a request carrying an authenticated account, for the
// lanes that authenticate some other way than a credential -- the access
// tokens behind capability URLs, and tests.
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
// records as its author and what a response body hands back. What it is no
// longer is a key: nothing looks a user up by the string this returns.
func GetUserEmail(r *http.Request) string {
	if acct := GetAccount(r); acct != nil {
		return acct.Email
	}
	return ""
}
