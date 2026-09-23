package api

// Renewal: a live credential mints its successor.
//
// A device credential lives 90 days, absolute, and until this route existed
// there was no way for it to produce a replacement -- the only path to
// credential number two was POST auth/login with the password. For a client
// with a window that is a nuisance; somebody types the password again. For a
// headless one it decides the architecture: a File Provider extension has no UI
// to ask in and does not own its own lifecycle, so the only shape that survives
// day 90 is keeping the password on the device forever, which is precisely what
// docs/protocol.md asks clients to stop doing. The narrow "authenticate this
// request" interface buys nothing if the password has to sit behind it anyway.
//
// **This is not the sliding expiry docs/auth.md refuses, and the distinction is
// the whole argument.** Sliding means a credential that polls never ages out,
// which makes the TTL unreachable in the case it was written for. This is an
// explicit request that writes a new row and leaves the old one to die on its
// own absolute expires_at: every credential still lasts 90 days and no longer,
// revoking a row still stops that row, and last_used still answers "is anybody
// using this?". A client that has been off for 91 days enrols from the password,
// as before.

import (
	"net/http"

	"github.com/SiloDrive/silo/fileserver/credential"
	"github.com/SiloDrive/silo/fileserver/middleware"
	"github.com/SiloDrive/silo/fileserver/option"
	log "github.com/sirupsen/logrus"
)

// RenewHandler handles POST /api/silo/v1/auth/renew.
//
// It takes no body, deliberately. Every field of the new row is copied from the
// presenting one -- kind, label, scope, perm and client_id -- so there is no
// input a request could widen a ceiling with, and the question "may they ask
// for this?" does not arise. A caller that wants a different perm or scope is
// asking for a different credential rather than for this one again, and enrols.
//
// Inheriting the label is what keeps renewal visible. A successor carries the
// name its parent carried, so an operator running `silo token list` who goes
// looking for "dan's macbook" finds every row that has ever been minted under
// it, rather than one row and a chain they cannot see.
func RenewHandler(w http.ResponseWriter, r *http.Request) {
	cred, acct := middleware.GetCredential(r), middleware.GetAccount(r)
	if cred == nil || acct == nil {
		// The route is mounted under RequireOwnCredential, so this is a route
		// registered outside it rather than an unauthenticated caller.
		log.Errorf("Renewal reached %s with no credential in context", r.URL.Path)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	// The device lane only. A session lasts a day because a person walked away
	// from a terminal, and a session that can renew itself is one that never
	// ends -- which is the sliding expiry this route is at pains not to be. The
	// client with nobody to ask is the device, and it is the case this exists
	// for. 403 rather than 400: the request is well formed and the credential
	// making it is the thing that may not.
	if cred.Kind != credential.KindDevice {
		log.Debugf("Refused renewal of a %s credential on %s", cred.Kind, r.URL.Path)
		http.Error(w, "Only a device credential can renew itself", http.StatusForbidden)
		return
	}

	ctx, cancel := option.WithDBTimeout(r.Context())
	defer cancel()

	next, token, err := credential.Issue(ctx, credential.IssueOpts{
		Kind:      cred.Kind,
		AccountID: cred.AccountID,
		Label:     cred.Label,
		Scope:     cred.Scope,
		Perm:      cred.Perm,
		ClientID:  cred.ClientID,
		Lifetime:  deviceLifetime,
	})
	if err != nil {
		log.Errorf("Failed to renew credential %s: %v", cred.ID, err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	// The old row is deliberately left alone. Expiring it here would be
	// one-in-one-out and tidier to reason about, and it would also mean that a
	// response lost in transit costs a headless client both the credential it
	// was holding and the one it never received -- the exact lockout this route
	// exists to prevent. It dies on its original expires_at instead, which is
	// what an absolute lifetime promised.
	log.Infof("Credential %s renewed as %s for %s, label %q",
		cred.ID, next.ID, acct.Email, next.Label)

	// The enrolment shape, byte for byte, because this answers the same
	// question enrolment answers -- here is your credential and here is when it
	// dies -- and a client should not need a second decoder for it.
	writeJSON(w, http.StatusCreated, enrolmentResponse{
		Credential: token, ExpiresAt: next.ExpiresAt, Email: acct.Email,
	})
}
