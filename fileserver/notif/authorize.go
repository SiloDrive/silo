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
	"github.com/dkam/silo/fileserver/credential"
	"github.com/dkam/silo/fileserver/middleware"
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
