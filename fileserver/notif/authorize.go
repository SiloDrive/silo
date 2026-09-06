package notif

// The credential lane onto the socket.
//
// Every other route in Silo answers "may this caller reach this library?" by
// reading the Authorization header and calling middleware.Perm. /notification
// did not, because it was ported from a process that had no database: the
// answer had to arrive pre-signed, as a library-scoped JWT the client fetched
// from a separate endpoint and handed over in the subscribe frame.
//
// It runs in-process now, so it can simply ask. What stops it asking directly
// is that share.CheckPerm needs a database and this package has never needed
// one -- and a package that gains a database gains it in its tests too. So the
// question is asked through a hook, the way libmgr.OnLibraryDeleted lets a
// lower package reach the fileserver without importing it.

import "github.com/dkam/silo/fileserver/credential"

// Authorize reports whether cred may watch libraryID.
//
// Set once at startup, to the same permission check the API routes make, so
// there is one answer to this question rather than two that can drift. Nil
// means no subscribe on this lane is granted: a server that forgot to wire it
// refuses everything rather than granting everything.
//
// It is asked at subscribe and again on every sweep. The second is what bounds
// a withdrawn share: a token expired within 72 hours whether or not anyone
// revoked anything, and a credential need never expire at all.
var Authorize func(cred *credential.Credential, libraryID string) bool

// authorized is Authorize with the two answers a caller must not have to
// remember: no credential is not authorized, and neither is an unwired hook.
func authorized(cred *credential.Credential, libraryID string) bool {
	if cred == nil || libraryID == "" || Authorize == nil {
		return false
	}
	return Authorize(cred, libraryID)
}
