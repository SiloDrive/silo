package notif

// The credential lane onto the socket.
//
// Every other route in Silo answers "may this caller reach this library?" by
// reading the Authorization header and calling middleware.Perm. /notification
// did not, because it was ported from a process that had no database: the
// answer had to arrive pre-signed, as a library-scoped JWT the client fetched
// from a separate endpoint and handed over in the subscribe frame.
//
// It runs in-process now, so it simply asks.

import (
	"context"

	"github.com/dkam/silo/fileserver/account"
	"github.com/dkam/silo/fileserver/credential"
	"github.com/dkam/silo/fileserver/libmgr"
	"github.com/dkam/silo/fileserver/middleware"
	"github.com/dkam/silo/fileserver/option"
	"github.com/dkam/silo/fileserver/share"
)

// authorize reports whether cred may watch libraryID.
//
// It is the same permission check the API routes make, so there is one answer
// to this question rather than two that can drift. A variable rather than a
// call because share.CheckPerm needs a database and this package has never
// needed one: the tests replace it, and assert what the socket does with an
// answer without owning a store to produce one.
var authorize = func(cred *credential.Credential, libraryID string) bool {
	return middleware.PermFor(cred, libraryID, "") != ""
}

// authorized is authorize with the answer a caller must not have to remember:
// no credential is not authorized, and the question is not asked.
func authorized(cred *credential.Credential, libraryID string) bool {
	return cred != nil && authorize(cred, libraryID)
}

// visibleLibraries is every library an account can see: owned, and granted
// whole to it or to a group it is in. It is the union GET /libraries answers
// with, read from the same two tables, so that a socket subscribed to the
// account rings for exactly what the listing would show.
//
// A variable for the reason authorize is one.
var visibleLibraries = func(acct account.ID) ([]string, error) {
	ctx, cancel := option.WithDBTimeout(context.Background())
	defer cancel()

	owned, err := libmgr.OwnedLibraryIDs(ctx, acct)
	if err != nil {
		return nil, err
	}
	granted, err := share.LibrariesFor(ctx, share.PrincipalsFor(acct))
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool, len(owned)+len(granted))
	ids := make([]string, 0, len(owned)+len(granted))
	for _, id := range owned {
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	for id := range granted {
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	return ids, nil
}

// recheckCredential re-reads the credential behind an account socket, so the
// resync tick can drop a socket whose credential is gone, disabled, expired
// or narrowed since the handshake. A variable for the reason authorize is
// one.
var recheckCredential = func(id string) (*credential.Credential, error) {
	ctx, cancel := option.WithDBTimeout(context.Background())
	defer cancel()
	return credential.ByID(ctx, id)
}
