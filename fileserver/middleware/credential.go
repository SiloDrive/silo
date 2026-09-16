package middleware

// The credential lane. Every authenticated route resolves through
// credential.Resolve, which is what makes disabling an account, revoking a
// credential and narrowing one single operations rather than one per store.
//
// It replaced a bearer JWT signed against a server-wide secret. That token
// could not be revoked, could not be named and could not be scoped, which is
// findings 3, 5 and 6 in docs/auth.md; all three close by the routes reading
// this instead.

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/dkam/silo/fileserver/account"
	"github.com/dkam/silo/fileserver/credential"
	"github.com/dkam/silo/fileserver/option"
	"github.com/dkam/silo/fileserver/share"
	"github.com/dkam/silo/fileserver/utils"
	"github.com/gorilla/mux"
	log "github.com/sirupsen/logrus"
)

// CredentialKey carries the credential that authenticated the request.
//
// The account is written under AccountKey as well, and deliberately: the
// handlers speak in accounts, and every one of them would otherwise have to
// learn about credentials to keep doing what it already does. What the
// credential is *for* is the permission ceiling -- see GetCredential.
const CredentialKey contextKey = "credential"

// apiKinds is the set of lanes the management API accepts.
//
// session is the CLI and the TUI; device is silo-drive and the File Provider
// extension. access is deliberately absent: a capability URL's credential
// authorizes one object and one operation, and the handlers that consume those
// live on their own routes and check the token itself.
var apiKinds = []credential.Kind{credential.KindSession, credential.KindDevice}

// RequireCredential rejects a request that does not carry a valid credential
// for one of the lanes this API serves.
func RequireCredential(next http.Handler) http.Handler {
	return resolveCredential(next, resolveOpts{})
}

// RequireOwnCredential authenticates a route whose subject is the presenting
// credential itself: logging out, and asking for a successor.
//
// It differs from RequireCredential in one thing: it does not apply the
// narrowing at the door. A scoped credential is refused every route that
// names no library because such a route answers about the account, which is
// strictly wider than the scope. These are strictly narrower -- they are about
// the row that carries the scope -- and refusing them would leave a mount cut
// to one library unable to sign itself out, or to renew before its credential
// lapses, needing an operator with shell access to do what it is entitled to
// do to itself.
func RequireOwnCredential(next http.Handler) http.Handler {
	return resolveCredential(next, resolveOpts{skipDoorCheck: true})
}

// RequireSocketCredential authenticates the notification socket.
//
// It used to be OptionalCredential, which let an anonymous request through:
// the endpoint predated the Authorization header, and a subscribe frame
// carried a library-scoped JWT that authorized itself, so a socket could
// arrive as nobody and still prove something. With that lane gone a socket
// without a credential can subscribe to nothing, ever, so it is refused here
// -- once, with a status code -- rather than upgraded into a connection whose
// every frame will be denied.
//
// It skips the narrowing at the door, because the socket answers about a
// library only once a subscribe frame names one. The rule scopeReachesRoute
// enforces reads "a route that names no library answers about the account,
// which is wider than the scope" -- and that is true of every request-shaped
// route, because the response is already decided by the time the middleware
// runs. It is false here: the upgrade answers nothing, and each subscribe
// frame afterwards names its own library and is checked against this same
// credential through PermFor.
//
// Refusing at the door instead was a real loss and a badly-shaped one. A mount
// cut to one library got no push at all, and it found out as a 403 on the
// upgrade -- which a client cannot fall back from the way it falls back from
// an absent feature name, because 403 on a WebSocket handshake is
// indistinguishable from a dozen other reasons a proxy might refuse it.
// It is RequireOwnCredential's option set arrived at from the other
// direction, and the two are kept apart because the reason is what a reader
// needs: that one skips the door check because its routes are narrower than
// the scope, this one because its route has not asked anything yet.
func RequireSocketCredential(next http.Handler) http.Handler {
	return resolveCredential(next, resolveOpts{skipDoorCheck: true})
}

// resolveOpts is how the wrappers above differ. It is a field rather than a
// boolean parameter because a call site reading (next, true) says nothing
// about which switch is being thrown.
type resolveOpts struct {
	// skipDoorCheck admits a scoped credential to a route that names no
	// library. The zero value refuses it, which is the safety net the door
	// check exists to be; each wrapper that opts out says why at its own
	// declaration, and the two reasons so far are different ones for the
	// same switch.
	skipDoorCheck bool
}

func resolveCredential(next http.Handler, opts resolveOpts) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cred, err := credential.Resolve(r, apiKinds...)
		if err != nil {
			credentialRefused(w, r, err)
			return
		}

		// The library half of the ceiling is enforced here, not left to the
		// handler. Perm is a helper a handler has to remember to call, and
		// four of them did not: listing libraries, account usage, create and
		// delete all read the account's own authority, so a credential cut to
		// one library enumerated every library its account could see. An
		// enforcement point that must be remembered is one the fifth handler
		// will forget.
		//
		// Only the library is checked here, because it is the only part of the
		// question this layer can answer: the route carries a library id or it
		// does not. Path granularity and read-versus-write stay with the
		// handler, which is what knows the path and what the operation does.
		if !opts.skipDoorCheck && !scopeReachesRoute(cred, r) {
			log.Debugf("Credential %s is scoped to %q and may not reach %s, from %s", cred.ID, cred.Scope, r.URL.Path, caller(r))
			http.Error(w, "Permission denied", http.StatusForbidden)
			return
		}

		ctx := context.WithValue(r.Context(), AccountKey, cred.Account())
		ctx = context.WithValue(ctx, CredentialKey, cred)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// credentialRefused turns Resolve's errors into one of two answers.
//
// The split is between "the caller got it wrong" and "we did", and every
// caller-side reason collapses to the same 401 with the same body on purpose:
// telling a client whether its credential was unknown, revoked, expired,
// disabled or simply for another lane answers questions about rows it has not
// proved it holds. The distinction survives in the log, where the operator is
// the audience.
func credentialRefused(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, credential.ErrMissing):
		// Logged, where it used to be silent. A client that has lost its
		// credential and one that never sent one look identical from the
		// outside, and both of them retry -- so the anonymous case is exactly
		// as worth attributing as the others.
		log.Debugf("No credential on %s, from %s", r.URL.Path, caller(r))
		http.Error(w, "Authorization header required", http.StatusUnauthorized)

	case errors.Is(err, credential.ErrSignatureNotImplemented):
		// A client presented Authorization: Silo. The scheme is designed and
		// the verifier is not built, so this is the server's gap rather than
		// the caller's mistake, and it says so -- a 401 would send a correct
		// client away to re-enrol against a lane that will fail identically.
		log.Infof("Refused a proof-of-possession credential on %s, from %s: not implemented", r.URL.Path, caller(r))
		http.Error(w, "Signature authentication is not implemented", http.StatusNotImplemented)

	case errors.Is(err, credential.ErrMalformed),
		errors.Is(err, credential.ErrInvalid),
		errors.Is(err, credential.ErrWrongKind),
		errors.Is(err, credential.ErrExpired),
		errors.Is(err, credential.ErrInactive),
		errors.Is(err, credential.ErrProofUnsupported):
		log.Debugf("Credential refused on %s, from %s: %v", r.URL.Path, caller(r), err)
		http.Error(w, "Invalid or expired token", http.StatusUnauthorized)

	default:
		// The store is broken rather than the caller being wrong. Answering
		// 401 here would tell every client at once that it had been signed
		// out, and they would all re-enrol against a database that is down.
		log.Errorf("Credential lookup failed on %s, from %s: %v", r.URL.Path, caller(r), err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
	}
}

// caller describes who knocked, for the refusal lines above.
//
// A refusal used to name the path and the reason and nothing else, which made
// a client retrying on a timer an unattributable line repeating in the
// journal: an operator could see that something was being refused every thirty
// seconds and had no way to tell which device, from where, or with which
// credential. All three are knowable at the door without proving anything.
//
// The address is read the way the rate limiter reads it, through
// utils.ClientIP, so the two agree about who a request came from -- behind a
// proxy that means the forwarded address when the deployment says the headers
// may be believed, and the peer's when it does not. The credential is
// Describe's secret-free rendering, and the user agent is the client's own
// claim about what it is: worth printing, worth trusting no further than that.
func caller(r *http.Request) string {
	ua := r.UserAgent()
	if ua == "" {
		ua = "no user-agent"
	}
	return fmt.Sprintf("%s (%s, %q)", utils.ClientIP(r, option.TrustProxyHeaders), credential.Describe(r), ua)
}

// scopeReachesRoute reports whether a scoped credential may address this route
// at all.
//
// An unscoped credential reaches everything, which is the common case and the
// first branch. A scoped one may reach only its own library -- and a route
// that names no library is answering about the account as a whole, which is
// strictly wider than the scope and so is refused. There is nothing to narrow
// by on such a route and no library id to compare against; refusing is the
// only answer that respects the narrowing.
func scopeReachesRoute(cred *credential.Credential, r *http.Request) bool {
	if cred.Scope.LibraryID == "" {
		return true
	}
	libraryID, ok := mux.Vars(r)["libraryid"]
	if !ok {
		return false
	}
	return libraryID == cred.Scope.LibraryID
}

// GetCredential returns the credential that authenticated the request, or nil
// when the request came through an unauthenticated lane.
//
// Handlers should reach for Perm rather than this. What a caller may do is the
// account's permission intersected with the credential's ceiling, and a
// handler that reads only the account is asking what the *user* may do rather
// than what *this credential* may do.
func GetCredential(r *http.Request) *credential.Credential {
	cred, _ := r.Context().Value(CredentialKey).(*credential.Credential)
	return cred
}

// WithCredential returns a request carrying a credential, for the lanes that
// authenticate some other way and for tests. It sets the account too, since a
// credential that named no account would pass Perm and fail everywhere after.
func WithCredential(r *http.Request, cred *credential.Credential, acct *account.Account) *http.Request {
	ctx := context.WithValue(r.Context(), CredentialKey, cred)
	ctx = context.WithValue(ctx, AccountKey, acct)
	return r.WithContext(ctx)
}

// Perm is PermFor for the credential that authenticated r.
//
// **No credential means no access.** Every route that reaches a handler is
// mounted under RequireCredential, so a nil credential is a route registered
// outside the authenticated subrouter -- a mistake, and one that must fail
// closed rather than quietly granting whatever the account may do.
func Perm(r *http.Request, libraryID, path string) string {
	cred := GetCredential(r)
	if cred == nil {
		log.Errorf("Permission asked on %s with no credential in context; denying", r.URL.Path)
		return ""
	}
	return PermFor(cred, libraryID, path)
}

// PermFor is what cred may do to path inside libraryID: "" for nothing, "r"
// for read, "rw" for read and write.
//
// It is the only place docs/auth.md's ceiling rule is applied --
//
//	effective = min(CheckPerm(library, account), cred.perm within cred.scope)
//
// -- and it is one function rather than two calls at each site because the
// failure it prevents is precisely a caller that remembers CheckPerm and
// forgets the narrowing. A credential can only ever narrow: it cannot exceed
// the account behind it, and if the account's own permission is withdrawn the
// credential follows immediately.
//
// path is the entry being reached, or "" for an operation that is about the
// library as a whole -- listing its commits, reading its delta feed, minting a
// notification token for it. A credential scoped to a folder is refused those,
// deliberately: there is no way to answer "what changed in this library"
// partially without telling the holder about paths it may not reach.
//
// Handlers reach it through Perm. The notification socket calls it directly:
// it resolves a credential at the handshake and then answers subscribes for
// the life of the connection, long after the request that carried it is gone.
//
// A nil credential is no access, for the reason Perm gives, and so is an
// empty library id: nothing is about no library, and an empty id offered to
// CheckPerm is a query about a row that does not exist.
func PermFor(cred *credential.Credential, libraryID, path string) string {
	if cred == nil || libraryID == "" {
		return ""
	}
	// Asked before share.CheckPerm, not inside the call. Go evaluates the
	// argument first, so a credential whose scope excludes this library would
	// otherwise pay CheckPerm's two-to-six queries and have the answer thrown
	// away by Covers -- on exactly the requests an out-of-scope client repeats.
	if !cred.Scope.Covers(libraryID, path) {
		return ""
	}
	return cred.EffectivePerm(share.CheckPerm(libraryID, cred.AccountID), libraryID, path)
}

// CredentialCanWrite reports whether the credential's own ceiling permits a
// write, without asking about any library.
//
// It is for the one handler that has no library to ask about: creating one,
// where it does not exist yet. Everywhere else, ask Perm -- it answers the
// whole question rather than half of it -- or, for an operation about a whole
// library that gates on ownership rather than on a grant, ask
// CredentialCanWriteLibrary.
//
// No credential means no write, for the reason Perm gives.
func CredentialCanWrite(r *http.Request) bool {
	cred := GetCredential(r)
	if cred == nil {
		log.Errorf("Write attempted on %s with no credential in context; denying", r.URL.Path)
		return false
	}
	return cred.Perm == "rw"
}

// CredentialReachesLibrary and CredentialCanWriteLibrary are the ceiling for an
// operation about one whole library whose access rule is ownership: deleting
// it, and deciding who else may have it.
//
// They exist because CredentialCanWrite is only half of that ceiling and the
// two handlers that reached for it took it for the whole. A credential carries
// a permission *and* a scope; asking only the permission let a credential cut
// to one folder delete the library that folder was in, and hand the whole of
// it to somebody else. The middleware's door check could not catch it either,
// because it compares library ids and such a credential names the right
// library -- see scopeReachesRoute, which says in its own comment that path
// granularity stays with the handler.
//
// Every other library-level handler asks this question as Perm(r, libraryID,
// "") and gets the right answer for free: nothing is not inside /photos, so an
// empty path fails a path scope. These two cannot, because Perm folds in
// share.CheckPerm and a grant is not what opens either of these doors --
// ownership is, and the handler checks it for itself immediately after. So
// this is Perm's narrowing half, alone, and it is one call rather than two at
// each site for the reason PermFor gives: the failure being prevented is
// exactly a caller that remembers one of the two.
//
// No credential means no access, for the reason Perm gives.
func CredentialReachesLibrary(r *http.Request, libraryID string) bool {
	return credentialCovers(r, libraryID, false)
}

// CredentialCanWriteLibrary is CredentialReachesLibrary for an operation that
// changes something.
func CredentialCanWriteLibrary(r *http.Request, libraryID string) bool {
	return credentialCovers(r, libraryID, true)
}

func credentialCovers(r *http.Request, libraryID string, write bool) bool {
	cred := GetCredential(r)
	if cred == nil {
		log.Errorf("Permission asked on %s with no credential in context; denying", r.URL.Path)
		return false
	}
	if write && cred.Perm != "rw" {
		return false
	}
	// The empty path is the whole library, which is what both callers are
	// about. A scope cut to a folder does not cover it.
	return cred.Scope.Covers(libraryID, "")
}
