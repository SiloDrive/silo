package api

// The administrative HTTP surface: the same operations `silo user` performs,
// for the ordinary case.
//
// It does not replace the CLI, and is not meant to. The moment administration
// is most needed is the one where nobody can log in, and anyone who can run
// the CLI already owns the data directory -- so the shell stays the lane that
// works when this one does not.
//
// Every handler here is built on the account, authmgr, admin and libmgr
// functions the CLI already calls, rather than beside them. That is the whole
// discipline of this file: a rule fixed in one of those packages is fixed for
// both callers, and a rule restated here would be a second copy that drifts.
//
// Each route's capability is named at the mount in server.go, not in the
// handler. docs/plans/admin.md § The HTTP surface holds the table.

import (
	"database/sql"
	"errors"
	"net/http"

	"github.com/dkam/silo/fileserver/account"
	"github.com/dkam/silo/fileserver/admin"
	"github.com/dkam/silo/fileserver/authmgr"
	"github.com/dkam/silo/fileserver/credential"
	"github.com/dkam/silo/fileserver/libmgr"
	"github.com/dkam/silo/fileserver/middleware"
	"github.com/dkam/silo/fileserver/option"
	"github.com/gorilla/mux"
	log "github.com/sirupsen/logrus"
)

// adminAccount is one row of the accounts listing.
//
// The same fields `silo user list -json` prints, and named the same, because
// they answer the same question and an operator moving between the two should
// not have to translate.
type adminAccount struct {
	ID           string   `json:"id"`
	Email        string   `json:"email"`
	IsActive     bool     `json:"is_active"`
	Role         string   `json:"role"`
	Capabilities []string `json:"capabilities"`
	HasPassword  bool     `json:"has_password"`
	Ctime        int64    `json:"ctime"`
}

// ListAdminAccountsHandler handles GET /api/silo/v1/admin/accounts.
func ListAdminAccountsHandler(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := option.WithDBTimeout(r.Context())
	defer cancel()

	listed, err := account.List(ctx)
	if err != nil {
		log.Errorf("Failed to list accounts: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
	caps, err := admin.OfAll(ctx)
	if err != nil {
		log.Errorf("Failed to list capabilities: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	out := make([]adminAccount, 0, len(listed))
	for _, u := range listed {
		held := []string{}
		for _, c := range caps[u.ID] {
			held = append(held, string(c))
		}
		out = append(out, adminAccount{
			ID:           u.ID.String(),
			Email:        u.Email,
			IsActive:     u.IsActive,
			Role:         string(u.Role),
			Capabilities: held,
			HasPassword:  u.HasPassword,
			Ctime:        u.Ctime,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

type createAccountRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
	Role     string `json:"role"`
}

// CreateAdminAccountHandler handles POST /api/silo/v1/admin/accounts.
//
// The password arrives in the body, which is the one thing about this route
// worth stating: there is no invite flow yet, so an administrator creating an
// account chooses its first password and has to convey it. When invites land
// (silo#14) this becomes the lane that mints one rather than the lane that
// sets a secret somebody else knows.
func CreateAdminAccountHandler(w http.ResponseWriter, r *http.Request) {
	var req createAccountRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Email == "" || req.Password == "" {
		http.Error(w, "email and password are required", http.StatusBadRequest)
		return
	}
	// Empty means the default rather than an error, because a caller with
	// nothing to say about the role is asking for an ordinary account. The
	// default is named here rather than left to the column, so the answer does
	// not depend on which of the two doors an account came through.
	role := account.DefaultRole
	if req.Role != "" {
		parsed, err := account.ParseRole(req.Role)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		role = parsed
	}

	ctx, cancel := option.WithDBTimeout(r.Context())
	defer cancel()

	created, err := authmgr.CreateAccount(ctx, account.Normalize(req.Email), req.Password, role)
	if err != nil {
		log.Errorf("Failed to create an account: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
	if !created {
		// 409 rather than 200: the caller asked for a new account and did not
		// get one, and an answer that looked like success would leave them
		// believing they had set that person's password.
		http.Error(w, "An account already holds that address", http.StatusConflict)
		return
	}
	acct, err := account.ByEmail(ctx, account.Normalize(req.Email))
	if err != nil {
		log.Errorf("Failed to read back a created account: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusCreated, adminAccount{
		ID: acct.ID.String(), Email: acct.Email, IsActive: acct.IsActive,
		Role: string(acct.Role), Capabilities: []string{}, HasPassword: true,
	})
}

type setActiveRequest struct {
	Active bool `json:"active"`
}

// SetAdminAccountActiveHandler handles
// POST /api/silo/v1/admin/accounts/{id}/active.
//
// Disabling stops every lane an account holds at once -- that is what the
// is_active join in credential.Resolve does -- and it revokes nothing, so
// re-enabling restores what they had. The CLI says so in prose; here it is the
// response field.
//
// Which is also why it is guarded. Stopping every lane is what a demotion does
// by another name, so disabling the last account able to administer this
// server is refused here exactly as demoting them is.
func SetAdminAccountActiveHandler(w http.ResponseWriter, r *http.Request) {
	target, ok := adminTarget(w, r)
	if !ok {
		return
	}
	var req setActiveRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	ctx, cancel := option.WithDBTimeout(r.Context())
	defer cancel()

	// Through admin.SetActive rather than account.SetActive, because
	// deactivation is the third door to a server nobody can administer and the
	// guard lives with the other two. account.SetActive writes the column; this
	// is the operation.
	if err := admin.SetActive(ctx, target.ID, req.Active); err != nil {
		adminRefusal(w, err)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Active bool `json:"active"`
	}{req.Active})
}

type setPasswordRequest struct {
	Password string `json:"password"`
}

// SetAdminAccountPasswordHandler handles
// POST /api/silo/v1/admin/accounts/{id}/password.
//
// Gated by the passwords capability rather than users, because a reset is
// impersonation and not administration -- an install may reasonably want
// somebody who can onboard staff without being able to become them.
//
// It answers with what the reset cost, and that is the point of the route
// rather than a nicety. An administrator cannot re-wrap an identity key --
// re-wrapping means unwrapping, which needs the old password -- so the blob is
// left in place and unopenable, and whether there is a way back depends on
// whether the account holds recovery wraps. warnAboutKeyMaterial says exactly
// this at the CLI; this says it in the same words, in JSON.
func SetAdminAccountPasswordHandler(w http.ResponseWriter, r *http.Request) {
	target, ok := adminTarget(w, r)
	if !ok {
		return
	}
	var req setPasswordRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Password == "" {
		http.Error(w, "password is required", http.StatusBadRequest)
		return
	}

	ctx, cancel := option.WithDBTimeout(r.Context())
	defer cancel()

	// Read before the write, because the write is what makes it untrue.
	keys, keysErr := account.GetKeys(ctx, target.ID)

	if err := authmgr.SetAccountPassword(ctx, target.ID, req.Password); err != nil {
		log.Errorf("Failed to set a password: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
	// After the password, for the reason passwdUser gives: a revocation that
	// ran and then failed to change the password would sign every device out
	// and leave the old password working, which is the worst of both.
	revoked, err := credential.RevokeAll(ctx, target.ID)
	if err != nil {
		log.Errorf("Password set for %s, but revoking their credentials failed: %v", target.Email, err)
		http.Error(w, "The password was set, but revoking their credentials failed", http.StatusInternalServerError)
		return
	}

	resp := struct {
		Revoked       int64  `json:"revoked"`
		IdentityKey   bool   `json:"identity_key_stranded"`
		RecoveryWraps int    `json:"recovery_wraps"`
		Recoverable   bool   `json:"identity_recoverable"`
		Says          string `json:"says"`
	}{Revoked: revoked}

	switch {
	case keysErr != nil || keys == nil:
		resp.Says = "This account has published no identity key, so there was nothing here to lose."
	default:
		resp.IdentityKey = true
		resp.RecoveryWraps = len(keys.Recovery)
		resp.Recoverable = len(keys.Recovery) > 0
		if resp.Recoverable {
			resp.Says = "Their identity key was wrapped under the old password and this server cannot " +
				"re-wrap it. Redeeming a recovery code recovers it, after which their client re-wraps " +
				"it under the new password and republishes."
		} else {
			resp.Says = "Their identity key was wrapped under the old password, this server cannot " +
				"re-wrap it, and they hold no recovery wraps — so there is no way back to it. They must " +
				"enrol again, and every end-to-end encrypted library that key opened stays sealed " +
				"unless another member re-shares it."
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

type quotaResponse struct {
	// Quota is the ceiling in bytes, or -2 for no ceiling. The sentinel is
	// option.InfiniteQuota and is what libmgr.AccountQuota already returns; a
	// second encoding here would be a second thing to keep in step.
	Quota int64 `json:"quota"`
	Used  int64 `json:"used"`
	// Files travels with Used because objmgr.Usage carries the two together
	// and splitting them here would mean a second caller reassembling them.
	Files int64 `json:"files"`
}

// GetAdminAccountQuotaHandler handles
// GET /api/silo/v1/admin/accounts/{id}/quota.
func GetAdminAccountQuotaHandler(w http.ResponseWriter, r *http.Request) {
	target, ok := adminTarget(w, r)
	if !ok {
		return
	}
	quota, err := libmgr.AccountQuota(target.ID)
	if err != nil {
		log.Errorf("Failed to read a quota: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
	used, err := libmgr.AccountUsage(target.ID)
	if err != nil {
		log.Errorf("Failed to read usage: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, quotaResponse{Quota: quota, Used: used.Size, Files: used.FileCount})
}

type setQuotaRequest struct {
	// Quota in bytes. Null removes the ceiling, dropping the account back to
	// the server default -- which is the same distinction `silo user quota
	// <email> none` draws, and it needs a third state rather than a zero
	// because a stored zero would mean "no bytes allowed".
	Quota *int64 `json:"quota"`
}

// SetAdminAccountQuotaHandler handles
// PUT /api/silo/v1/admin/accounts/{id}/quota.
func SetAdminAccountQuotaHandler(w http.ResponseWriter, r *http.Request) {
	target, ok := adminTarget(w, r)
	if !ok {
		return
	}
	var req setQuotaRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	switch {
	case req.Quota == nil:
		if err := libmgr.ClearAccountQuota(target.ID); err != nil {
			log.Errorf("Failed to clear a quota: %v", err)
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}
	case *req.Quota <= 0:
		http.Error(w, "quota must be a positive number of bytes, or null for none",
			http.StatusBadRequest)
		return
	default:
		if err := libmgr.SetAccountQuota(target.ID, *req.Quota); err != nil {
			log.Errorf("Failed to set a quota: %v", err)
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}
	}
	GetAdminAccountQuotaHandler(w, r)
}

type setRoleRequest struct {
	Role string `json:"role"`
}

// SetAdminAccountRoleHandler handles
// PUT /api/silo/v1/admin/accounts/{id}/role.
//
// The whole rule lives in admin.SetRole, including the refusal that keeps the
// last account able to administer this server from being demoted out of it.
// This handler's job is to turn that refusal into a status code.
func SetAdminAccountRoleHandler(w http.ResponseWriter, r *http.Request) {
	target, ok := adminTarget(w, r)
	if !ok {
		return
	}
	var req setRoleRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	role, err := account.ParseRole(req.Role)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	ctx, cancel := option.WithDBTimeout(r.Context())
	defer cancel()

	if err := admin.SetRole(ctx, middleware.GetAccount(r), target.ID, role); err != nil {
		adminRefusal(w, err)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Role string `json:"role"`
	}{string(role)})
}

type setCapsRequest struct {
	// Capabilities is the whole set the account should hold afterwards, not an
	// addition to it. PUT rather than POST for that reason, and for the reason
	// account/keys is a PUT: a verb that reads as "append" invites the partial
	// update, and there is no operation here that means "add one and leave the
	// rest alone" that could not be spelled by sending the set.
	Capabilities []string `json:"capabilities"`
}

// SetAdminAccountCapabilitiesHandler handles
// PUT /api/silo/v1/admin/accounts/{id}/caps.
//
// A set rather than a pair of add and remove routes, because the caller's
// intent is a state. It is expressed as one grant and one revoke against what
// the account holds today, so both of admin's invariants apply exactly as they
// do at the CLI: the actor must hold everything appearing on either side of
// the difference, and the last holder of grant keeps it.
func SetAdminAccountCapabilitiesHandler(w http.ResponseWriter, r *http.Request) {
	target, ok := adminTarget(w, r)
	if !ok {
		return
	}
	var req setCapsRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	want := map[admin.Capability]bool{}
	for _, s := range req.Capabilities {
		c, err := admin.ParseCapability(s)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		want[c] = true
	}

	ctx, cancel := option.WithDBTimeout(r.Context())
	defer cancel()

	have, err := admin.Of(ctx, target.ID)
	if err != nil {
		log.Errorf("Failed to read capabilities: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
	held := map[admin.Capability]bool{}
	for _, c := range have {
		held[c] = true
	}

	var add, drop []admin.Capability
	for _, c := range admin.All() {
		switch {
		case want[c] && !held[c]:
			add = append(add, c)
		case held[c] && !want[c]:
			drop = append(drop, c)
		}
	}

	actor := middleware.GetAccount(r)
	// Revoke first, so that a request which both drops the last grant and adds
	// it elsewhere is refused rather than half-applied. There is no single
	// transaction here, and the invariant is worth more than the convenience.
	if len(drop) > 0 {
		if err := admin.Revoke(ctx, actor, target.ID, drop...); err != nil {
			adminRefusal(w, err)
			return
		}
	}
	if len(add) > 0 {
		if err := admin.Grant(ctx, actor, target.ID, add...); err != nil {
			adminRefusal(w, err)
			return
		}
	}

	now, err := admin.Of(ctx, target.ID)
	if err != nil {
		log.Errorf("Failed to read back capabilities: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
	out := []string{}
	for _, c := range now {
		out = append(out, string(c))
	}
	writeJSON(w, http.StatusOK, setCapsRequest{Capabilities: out})
}

// adminTarget resolves the {id} an administrative route names.
//
// A 404 for an id that names nobody, and a 400 for one that is not an id at
// all. The two are different answers -- "there is no such account" and "that
// is not an account id" -- to a caller that has already proved it may ask.
func adminTarget(w http.ResponseWriter, r *http.Request) (*account.Account, bool) {
	id, err := account.ParseID(mux.Vars(r)["id"])
	if err != nil {
		http.Error(w, "That is not an account id", http.StatusBadRequest)
		return nil, false
	}

	ctx, cancel := option.WithDBTimeout(r.Context())
	defer cancel()

	acct, err := account.ByID(ctx, id)
	if errors.Is(err, account.ErrNotFound) {
		http.Error(w, "No such account", http.StatusNotFound)
		return nil, false
	}
	if err != nil {
		log.Errorf("Failed to resolve an administrative target: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return nil, false
	}
	return acct, true
}

// adminRefusal turns the admin package's refusals into status codes.
//
// Every one of them is 403 and every one of them says why. They are not client
// bugs and they are not server errors: they are the answer, and an operator who
// gets a bare "forbidden" from an endpoint they are entitled to call learns
// nothing about what to do instead.
func adminRefusal(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, admin.ErrNeedsGrant),
		errors.Is(err, admin.ErrNotHeld),
		errors.Is(err, admin.ErrLastGrant),
		errors.Is(err, admin.ErrLastAdmin):
		http.Error(w, err.Error(), http.StatusForbidden)
	default:
		log.Errorf("Administrative change failed: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
	}
}

// adminLibrary is one row of the administrative libraries listing.
//
// Metadata, and deliberately only metadata. An administrator may enumerate
// every library on the server and reach into none of them: reading content is
// a grant question, and for an end-to-end encrypted library the server holds
// ciphertext it cannot open at all. That is a structural bound rather than a
// policy this handler enforces, which is why it can be stated once here
// instead of defended at every route that grows later.
type adminLibrary struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Owner      string `json:"owner"`
	OwnerID    string `json:"owner_id"`
	UpdateTime int64  `json:"update_time"`
	// E2EE says the server cannot read this library's content. It is not the
	// whole encryption story and must not be rendered as though it were: every
	// object on this server is sealed at rest under storage.key regardless, so
	// a plain library is not sitting in cleartext on disk. Two independent
	// facts, and an either/or control would tell the operator something untrue
	// about the plain half of their server.
	E2EE         bool   `json:"e2ee"`
	HeadCommitID string `json:"head_commit_id,omitempty"`
	// Size and FileCount are logical size at head, the same currency
	// libraryInfo reports and not bytes on disk. The two numbers disagree on
	// purpose and the page labels which is which; stored bytes divided into
	// head, history and unreferenced is objmgr.Census, behind `silo df`, and it
	// walks a library's whole store -- which is a command an operator runs, not
	// a thing a page does on every load.
	Size      int64 `json:"size"`
	FileCount int64 `json:"file_count"`
}

// ListAdminLibrariesHandler handles GET /api/silo/v1/admin/libraries.
//
// Gated by quota rather than users. A listing of every library with its owner
// and its size is usage information, which is what that capability already
// means and what `silo df` already answers in aggregate -- and it is the view
// the server-level ceiling needs, since a refusal at that ceiling is otherwise
// a refusal with no way to see what filled it.
//
// Virtual libraries are excluded, for the reason libmgr.ServerUsage excludes
// them: they are a view of another library's content, and listing them would
// show the same bytes twice under two names.
func ListAdminLibrariesHandler(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := option.WithDBTimeout(r.Context())
	defer cancel()

	const q = `SELECT o.library_id, i.name, o.account_id, e.email, i.update_time,
	                  f.e2ee, b.commit_id, u.size, u.file_count
	           FROM LibraryOwner o
	           LEFT JOIN LibraryInfo i ON i.library_id = o.library_id
	           LEFT JOIN AccountEmail e ON e.account_id = o.account_id AND e.is_primary = 1
	           LEFT JOIN Library f ON f.library_id = o.library_id
	           LEFT JOIN Branch b ON b.library_id = o.library_id AND b.name = 'master'
	           LEFT JOIN LibraryUsage u ON u.library_id = o.library_id
	           LEFT JOIN VirtualLibrary v ON v.library_id = o.library_id
	           LEFT JOIN GarbageLibraries g ON g.library_id = o.library_id
	           WHERE v.library_id IS NULL AND g.library_id IS NULL
	           ORDER BY i.update_time DESC, o.library_id`

	rows, err := readDB.QueryContext(ctx, q)
	if err != nil {
		log.Errorf("Failed to list libraries for an administrator: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
	defer func() { _ = rows.Close() }()

	// Allocated rather than declared, so an empty server marshals as [] and not
	// null -- which is the state every fresh install is in, and so the first
	// response the page ever sees.
	out := make([]adminLibrary, 0)
	for rows.Next() {
		var l adminLibrary
		var ownerID account.ID
		var name, email, commitID sql.NullString
		var e2ee sql.NullBool
		var updateTime, size, fileCount sql.NullInt64
		if err := rows.Scan(&l.ID, &name, &ownerID, &email, &updateTime,
			&e2ee, &commitID, &size, &fileCount); err != nil {
			log.Warnf("Failed to scan a library row: %v", err)
			continue
		}
		l.Name = name.String
		l.OwnerID = ownerID.String()
		l.Owner = email.String
		l.UpdateTime = updateTime.Int64
		l.E2EE = e2ee.Bool
		l.HeadCommitID = commitID.String
		l.Size = size.Int64
		l.FileCount = fileCount.Int64
		out = append(out, l)
	}
	if err := rows.Err(); err != nil {
		log.Errorf("Failed to read libraries for an administrator: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, out)
}
