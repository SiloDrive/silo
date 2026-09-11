package api

// The invite surface: three administrative routes and one that is on no lane
// at all.
//
// It is the second half of docs/plans/sharing.md decision 9, and the first
// half -- the model -- is the invite package. Nothing here re-decides anything
// that package settled: single use is enforced by its conditional spend, the
// address is bound by its row, and a withdrawn invite reports what a lapsed one
// does because Revoke chose that. What these handlers add is the wire, and the
// one thing the model deliberately left to its caller: the secret the redeemer
// picks.
//
// That is why redemption sets the password rather than leaving it to the
// ordinary account routes. POST auth/password requires the current password --
// docs/auth.md's rule that a credential must not become the account -- and a
// redeemed account has none, so a redemption that only activated would leave
// the person holding a session and no way to make it renewable. The client's
// remaining half, publishing key material, is over the ordinary routes with
// the session this hands back, which is where it belongs: PUT account/keys
// needs a credential and now there is one.

import (
	"errors"
	"net/http"
	"time"

	"github.com/dkam/silo/fileserver/account"
	"github.com/dkam/silo/fileserver/credential"
	"github.com/dkam/silo/fileserver/invite"
	"github.com/dkam/silo/fileserver/middleware"
	"github.com/dkam/silo/fileserver/option"
	"github.com/gorilla/mux"
	log "github.com/sirupsen/logrus"
)

// invitePayload is one row of the listing, and the mint response with a token
// added. Ordered the way an operator reads it: who, as what, and where it is up
// to.
//
// It never carries the token on the listing path. A secret that can be read
// back is a secret stored to be read back, and the mint response is the one
// moment this one exists -- the same discipline the credential surface already
// holds.
type invitePayload struct {
	CredentialID string `json:"credential_id"`
	Email        string `json:"email"`
	Role         string `json:"role"`
	CreatedBy    string `json:"created_by"`
	Ctime        int64  `json:"ctime"`
	ExpiresAt    int64  `json:"expires_at"`
	// RedeemedAt is zero while the invite is outstanding, which is the
	// operator's whole question about it.
	RedeemedAt int64 `json:"redeemed_at"`
	// Token is present on mint and never on a listing.
	Token string `json:"token,omitempty"`
}

func invitePayloadOf(i invite.Invite) invitePayload {
	return invitePayload{
		CredentialID: i.CredentialID,
		Email:        i.Email,
		Role:         string(i.Role),
		CreatedBy:    i.CreatedBy.String(),
		Ctime:        i.Ctime,
		ExpiresAt:    i.ExpiresAt,
		RedeemedAt:   i.RedeemedAt,
	}
}

// ListInvitesHandler handles GET /api/silo/v1/admin/invites.
func ListInvitesHandler(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := option.WithDBTimeout(r.Context())
	defer cancel()

	all, err := invite.List(ctx)
	if err != nil {
		log.Errorf("Failed to list invites: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
	out := make([]invitePayload, 0, len(all))
	for _, i := range all {
		out = append(out, invitePayloadOf(i))
	}
	writeJSON(w, http.StatusOK, out)
}

type createInviteRequest struct {
	Email string `json:"email"`
	Role  string `json:"role"`
	// LifetimeDays overrides invite.DefaultLifetime. Days rather than seconds
	// because the number an operator has in mind is a number of days, and a
	// unit nobody has to convert is a unit nobody gets wrong by a factor of
	// sixty.
	LifetimeDays int `json:"lifetime_days"`
}

// CreateInviteHandler handles POST /api/silo/v1/admin/invites.
//
// Gated by the users capability, because this is `silo user add` by another
// door: it brings an account row into being for an address. An invite naming
// the admin role is not an escalation past that gate -- admin.Can is a
// conjunction, so the account arrives with a role and no capability rows and
// can do nothing administrative until somebody holding grant says otherwise.
func CreateInviteHandler(w http.ResponseWriter, r *http.Request) {
	by := middleware.GetAccount(r)
	if by == nil {
		log.Errorf("The invite route reached %s with no account in context", r.URL.Path)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	var req createInviteRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Email == "" {
		http.Error(w, "email is required", http.StatusBadRequest)
		return
	}
	// Empty means the default rather than an error, for the reason the account
	// route gives: a caller with nothing to say about the role is asking for an
	// ordinary account.
	role := account.DefaultRole
	if req.Role != "" {
		parsed, err := account.ParseRole(req.Role)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		role = parsed
	}
	if req.LifetimeDays < 0 {
		http.Error(w, "lifetime_days cannot be negative", http.StatusBadRequest)
		return
	}

	ctx, cancel := option.WithDBTimeout(r.Context())
	defer cancel()

	minted, token, err := invite.Mint(ctx, invite.Options{
		Email:    req.Email,
		Role:     role,
		By:       by.ID,
		Lifetime: time.Duration(req.LifetimeDays) * 24 * time.Hour,
	})
	switch {
	case errors.Is(err, invite.ErrActiveAccount):
		// 409, the same answer setup gives a server that already has an
		// account: the caller asked to bring something into being and it is
		// already there. Naming the address costs nothing -- the caller typed
		// it -- and saves an operator wondering which half was wrong.
		http.Error(w, "An active account already holds that address", http.StatusConflict)
		return
	case err != nil:
		log.Errorf("Failed to mint an invite for %s: %v", account.Normalize(req.Email), err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	log.Infof("Invite minted for %s as %s by %s", minted.Email, minted.Role, by.Email)

	out := invitePayloadOf(*minted)
	out.Token = token
	writeJSON(w, http.StatusCreated, out)
}

// RevokeInviteHandler handles DELETE /api/silo/v1/admin/invites/{id}.
func RevokeInviteHandler(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]

	ctx, cancel := option.WithDBTimeout(r.Context())
	defer cancel()

	err := invite.Revoke(ctx, id)
	switch {
	case errors.Is(err, invite.ErrNotFound):
		http.Error(w, "No such invite", http.StatusNotFound)
		return
	case errors.Is(err, invite.ErrSpent):
		// 409 rather than 204, because withdrawing a spent invite does not do
		// what the caller means by it. The person is already in; taking the
		// account back is a different operation, and reporting success here
		// would leave an operator believing they had performed it.
		http.Error(w, "That invite has already been redeemed; the account it created is still active",
			http.StatusConflict)
		return
	case err != nil:
		log.Errorf("Failed to withdraw invite %s: %v", id, err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
	log.Infof("Invite %s withdrawn", id)
	w.WriteHeader(http.StatusNoContent)
}

// redeemRequest is what the person the invite was sent to presents.
//
// There is deliberately no address in it. The redeemer does not choose one --
// delivery to that inbox is the verification step -- so a field for it would be
// a field that either does nothing or undoes the whole point, and a client
// sending one is a client that has misread the model rather than one to
// accommodate.
type redeemRequest struct {
	InviteToken string `json:"invite_token"`
	// Password is the account's first secret, or its authKey when
	// ClientKDFParams is present. Same two shapes as POST auth/password, and
	// the same function writes them, so an account crossed over at redemption
	// is crossed over by the code that crosses over any other.
	Password        string `json:"password"`
	ClientKDFParams string `json:"client_kdf_params"`
}

// RedeemInviteHandler handles POST /api/silo/v1/auth/redeem: the one request
// besides setup that brings an account into use without a credential.
//
// It is unauthenticated for the reason setup is -- there is nothing yet to
// authenticate as -- and it is guarded by the invite token instead. docs/auth.md
// says every secret a client presents is a Credential row resolved by
// credential.Resolve; an invite is exactly that, resolved by ResolveInvite
// because the account it names is inactive until this request, which is the
// one check that lane drops and the only one.
func RedeemInviteHandler(w http.ResponseWriter, r *http.Request) {
	var req redeemRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.InviteToken == "" || req.Password == "" {
		http.Error(w, "invite_token and password are required", http.StatusBadRequest)
		return
	}
	releaseAttempt, ok := allowRedeemAttempt(w, r)
	if !ok {
		return
	}
	defer releaseAttempt()

	ctx, cancel := option.WithDBTimeout(r.Context())
	defer cancel()

	redeemed, err := invite.Redeem(ctx, req.InviteToken)
	switch {
	case errors.Is(err, invite.ErrSpent):
		// 409, and it says which refusal this is. An invite somebody already
		// used is not one that ran out, and the difference is the whole of
		// what the holder can act on: one of them means asking for another.
		http.Error(w, "This invite has already been redeemed", http.StatusConflict)
		return
	case errors.Is(err, invite.ErrActiveAccount):
		http.Error(w, "An active account already holds that address", http.StatusConflict)
		return
	case err != nil:
		// Everything else is one answer: a malformed token, an unknown one, a
		// wrong secret, a withdrawn invite and a lapsed one are indistinguishable
		// to the holder on purpose, so a guesser learns nothing about which half
		// of the guess to keep and a withdrawal says nothing about an
		// administrator's decision to somebody outside it.
		redeemFailed(r)
		log.Infof("An invite redemption was refused: %v", err)
		http.Error(w, "Invalid invite", http.StatusUnauthorized)
		return
	}

	acct, err := account.ByID(ctx, redeemed.AccountID)
	if err != nil {
		log.Errorf("Redeemed invite %s names an account that cannot be read: %v",
			redeemed.CredentialID, err)
		http.Error(w, inviteSpentButNoAccount, http.StatusInternalServerError)
		return
	}

	// The same function POST auth/password writes a secret with, so the
	// crossover rule and the parameter check are enforced once. An account
	// arriving here has no stored hash at all, so neither of that function's
	// refusals can fire -- but it is the one place either is spelled, and a
	// second spelling here is the drift the reuse exists to avoid.
	if !writeNewSecret(w, ctx, acct, changePasswordRequest{
		NewPassword:     req.Password,
		ClientKDFParams: req.ClientKDFParams,
	}) {
		// It has already answered, and the invite is spent either way: the
		// account is active and its password is not set, which is the state an
		// operator reset exists for.
		log.Errorf("Invite %s was spent but the account's secret could not be written",
			redeemed.CredentialID)
		return
	}

	opts := defaultSessionOpts()
	opts.AccountID = acct.ID
	opts.Label = credentialLabel(r)
	_, token, err := credential.Issue(ctx, opts)
	if err != nil {
		// Say which failure this is, for setup's reason: an operator who read
		// "internal server error" and presented the invite again would be told
		// it was already redeemed and conclude they were locked out of an
		// account that works.
		log.Errorf("Failed to issue a session credential after redemption: %v", err)
		http.Error(w, accountRedeemedButNoCredential, http.StatusInternalServerError)
		return
	}

	log.Infof("Invite %s redeemed; %s is active as %s",
		redeemed.CredentialID, redeemed.Email, redeemed.Role)

	// Setup's shape, for the same reason: 201 because this request brought
	// something into use, and login's body because a client already parses it.
	writeJSON(w, http.StatusCreated, loginResponse{Token: token})
}

// The two bodies for the window where redemption spent the invite and then
// could not finish. Both name the state the account is actually in, because the
// invite cannot be presented again and a client that retried would be told
// something true and useless.
const (
	inviteSpentButNoAccount = "The invite was redeemed, but the account it names could not be read. " +
		"Ask an administrator: the invite is spent and cannot be presented again."
	accountRedeemedButNoCredential = "The account is active and its password is set, but a session " +
		"could not be issued. Log in with the address you were invited at and the password you just chose; " +
		"the invite is spent."
)
