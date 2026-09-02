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
	"net/http"

	"github.com/dkam/silo/fileserver/account"
	"github.com/dkam/silo/fileserver/credential"
	"github.com/dkam/silo/fileserver/share"
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
// credential itself -- logging out, and nothing else so far.
//
// It differs from RequireCredential in one thing: it does not apply the
// narrowing. A scoped credential is refused every route that names no library
// because such a route answers about the account, which is strictly wider
// than the scope. This one is strictly narrower -- it is about the row that
// carries the scope -- and refusing it would leave a mount cut to one library
// unable to sign itself out, needing an operator with shell access to do what
// it is entitled to do to itself.
func RequireOwnCredential(next http.Handler) http.Handler {
	return resolveCredential(next, resolveOpts{aboutSelf: true})
}

// OptionalCredential resolves a credential when one is offered and lets the
// request through either way.
//
// It exists for the notification socket, which has to accept clients that
// predate the header while giving the ones that send it something for it. A
// credential that is offered and bad is still refused -- a rejected credential
// must never be quietly downgraded to anonymous, because the caller believes
// it is authenticated and would learn otherwise only from the permissions it
// silently stopped having.
func OptionalCredential(next http.Handler) http.Handler {
	return resolveCredential(next, resolveOpts{optional: true})
}

// resolveOpts is how the three wrappers above differ. They are fields rather
// than two boolean parameters because a call site reading (next, false, true)
// says nothing about which false and which true.
type resolveOpts struct {
	// optional lets an anonymous request through. A credential that is
	// offered and bad is still refused.
	optional bool

	// aboutSelf says the route's subject is the credential presenting it, so
	// a narrowing does not exclude it. See RequireOwnCredential.
	aboutSelf bool
}

func resolveCredential(next http.Handler, opts resolveOpts) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cred, err := credential.Resolve(r, apiKinds...)
		if err != nil {
			if errors.Is(err, credential.ErrMissing) && opts.optional {
				next.ServeHTTP(w, r)
				return
			}
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
		if !opts.aboutSelf && !scopeReachesRoute(cred, r) {
			log.Debugf("Credential %s is scoped to %q and may not reach %s", cred.ID, cred.Scope, r.URL.Path)
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
		http.Error(w, "Authorization header required", http.StatusUnauthorized)

	case errors.Is(err, credential.ErrSignatureNotImplemented):
		// A client presented Authorization: Silo. The scheme is designed and
		// the verifier is not built, so this is the server's gap rather than
		// the caller's mistake, and it says so -- a 401 would send a correct
		// client away to re-enrol against a lane that will fail identically.
		log.Infof("Refused a proof-of-possession credential on %s: not implemented", r.URL.Path)
		http.Error(w, "Signature authentication is not implemented", http.StatusNotImplemented)

	case errors.Is(err, credential.ErrMalformed),
		errors.Is(err, credential.ErrInvalid),
		errors.Is(err, credential.ErrWrongKind),
		errors.Is(err, credential.ErrExpired),
		errors.Is(err, credential.ErrInactive),
		errors.Is(err, credential.ErrProofUnsupported):
		log.Debugf("Credential refused on %s: %v", r.URL.Path, err)
		http.Error(w, "Invalid or expired token", http.StatusUnauthorized)

	default:
		// The store is broken rather than the caller being wrong. Answering
		// 401 here would tell every client at once that it had been signed
		// out, and they would all re-enrol against a database that is down.
		log.Errorf("Credential lookup failed on %s: %v", r.URL.Path, err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
	}
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

// Perm is what the caller may do to path inside libraryID: "" for nothing,
// "r" for read, "rw" for read and write.
//
// It is the only place docs/auth.md's ceiling rule is applied --
//
//	effective = min(CheckPerm(library, account), cred.perm within cred.scope)
//
// -- and it is one function rather than two calls at each site because the
// failure it prevents is precisely a handler that remembers CheckPerm and
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
// It is for the handlers that have no library to ask share.CheckPerm about --
// creating one, where it does not exist yet, and deleting one, which gates on
// ownership instead. Everywhere else, ask Perm: it answers the whole question
// rather than half of it.
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
