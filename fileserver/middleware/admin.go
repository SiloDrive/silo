package middleware

// The administrative gate.
//
// It sits beside CredentialCanWrite because it answers the same shape of
// question: what a handler with no library to ask share.CheckPerm about is
// allowed to do. Server administration is the largest such handler -- it has no
// library at all, which is exactly why it is not in the grant model.
//
// docs/plans/admin.md § The HTTP surface is the owning document.

import (
	"net/http"

	"github.com/dkam/silo/fileserver/admin"
	"github.com/dkam/silo/fileserver/option"
	log "github.com/sirupsen/logrus"
)

// RequireAdmin gates a route on one administrative capability.
//
// Named per route rather than applied to a subtree, because the capability is
// the point: a subtree gate would be a single admin bit wearing a table's
// clothes, and the whole argument for rows is that "administrator" is not one
// permission. The route table in docs/plans/admin.md pairs every path with the
// capability that opens it, and this is where that pairing is written down in
// code.
//
// It assumes a credential has already been resolved -- mount it inside the
// subrouter that applies RequireCredential, so that "no credential" is 401
// from there and "not enough authority" is 403 from here. A request that
// reaches this with no account in context is a routing mistake rather than a
// caller's, and is refused as one.
//
// A method other than GET additionally needs a credential whose own ceiling
// permits writing. That is checked here rather than in each handler for the
// reason the pairing above is: eight handlers each remembering to ask is eight
// chances to forget, and the failure is silent -- a read-only token creating
// accounts, discovered by reading the code rather than by anything going wrong.
func RequireAdmin(c admin.Capability, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		acct := GetAccount(r)
		if acct == nil {
			log.Errorf("Admin route %s reached with no account in context", r.URL.Path)
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}
		if r.Method != http.MethodGet && !CredentialCanWrite(r) {
			http.Error(w, "Permission denied", http.StatusForbidden)
			return
		}

		ctx, cancel := option.WithDBTimeout(r.Context())
		defer cancel()

		can, err := admin.Can(ctx, acct, c)
		if err != nil {
			log.Errorf("Failed to read administrative capabilities: %v", err)
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}
		if !can {
			// Named, rather than a bare refusal. The vocabulary is documented
			// and closed, so saying which capability is missing tells an
			// administrator what to ask for and tells anybody else nothing
			// they could not read in the docs.
			http.Error(w, "This operation needs the "+string(c)+" capability", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}
