// Package admin holds administrative authority: who may perform which
// administrative operation, and the rule that answers it.
//
// It is deliberately not the grant model. A grant answers "may this principal
// do op at (library, path)" and is a fact about a relationship;
// docs/plans/sharing.md owns that. A capability answers "may this account
// perform this administrative operation" and is a fact about the account.
// Neither is derivable from the other, which is why they are two tables and
// two packages rather than one clever one.
//
// docs/plans/admin.md is the owning document.
package admin

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/dkam/silo/fileserver/account"
)

// Capability is one administrative operation, and the vocabulary is closed.
type Capability string

// The six. Each is traceable to a `silo` command that exists today, because
// the roadmap says the admin endpoints call the same account functions the CLI
// does -- which makes the CLI the inventory and the inventory a fact rather
// than a guess. A seventh arrives when a seventh operation does; the point of
// storing these as rows is that waiting costs nothing.
const (
	// CapUsers covers silo user add, list, disable and enable.
	CapUsers Capability = "users"
	// CapPasswords covers silo user passwd, and is split from CapUsers on
	// purpose: creating and disabling accounts is administration, and setting
	// somebody's password is stepping into their account. An install may
	// reasonably want a person who can onboard staff without being able to
	// become them.
	CapPasswords Capability = "passwords"
	// CapQuota covers silo user quota, and the server ceiling when it lands.
	CapQuota Capability = "quota"
	// CapTokens covers silo token and credential.RevokeAll.
	CapTokens Capability = "tokens"
	// CapRetention covers silo retention, GC and expiry.
	CapRetention Capability = "retention"
	// CapGrant may change another account's capabilities. It is SQL's GRANT
	// OPTION: the operation that changes who may perform operations, which is
	// what makes it the escalation boundary rather than a seventh verb.
	CapGrant Capability = "grant"
)

// All is the vocabulary, in the order the six are argued for in the plan.
//
// A function rather than an exported slice, because a package-level slice is
// writable by every importer and this one is a closed set.
func All() []Capability {
	return []Capability{CapUsers, CapPasswords, CapQuota, CapTokens, CapRetention, CapGrant}
}

// ParseCapability turns stored or typed text into a capability, and refuses
// anything else -- strictly, including about case and surrounding space, for
// the reason account.ParseRole is strict.
func ParseCapability(s string) (Capability, error) {
	for _, c := range All() {
		if Capability(s) == c {
			return c, nil
		}
	}
	return "", fmt.Errorf("unknown capability %q: one of %s", s, Join(All()))
}

// ParseCapabilities parses a comma-separated list, as the CLI and the HTTP
// surface both take one. An empty list is an error: a call that changes
// nothing is a caller that meant something else.
func ParseCapabilities(s string) ([]Capability, error) {
	var out []Capability
	for _, part := range strings.Split(s, ",") {
		c, err := ParseCapability(strings.TrimSpace(part))
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	if len(out) == 0 {
		return nil, errors.New("no capabilities given")
	}
	return out, nil
}

// Join renders a set for a message or a listing.
func Join(caps []Capability) string {
	parts := make([]string, len(caps))
	for i, c := range caps {
		parts[i] = string(c)
	}
	return strings.Join(parts, ", ")
}

// ErrNeedsGrant reports an actor that may not change authority at all: not an
// admin, or an admin without CapGrant.
var ErrNeedsGrant = errors.New("changing capabilities needs the grant capability")

// ErrNotHeld reports the invariant that keeps grant from meaning everything:
// nobody hands on, or takes away, an authority they do not hold themselves.
var ErrNotHeld = errors.New("that capability is not yours to give or take")

// ErrTargetOutranks reports an action that would hand the actor an authority
// they do not hold, by handing them the account that holds it.
var ErrTargetOutranks = errors.New("that account holds a capability you do not")

// ErrLastGrant reports the last holder of grant trying to drop it.
var ErrLastGrant = errors.New("this is the only account holding the grant capability")

var readDB, writeDB *sql.DB

// Init sets the database handles.
func Init(read, write *sql.DB) { readDB, writeDB = read, write }

// Can is the whole rule, and it has exactly one spelling: the account is an
// admin AND a row exists for this capability.
//
// A conjunction rather than either half, and no implicit set. "An admin with
// no rows means all of them" is the shape that turns every later capability
// into a silent widening of what the first one meant, which is the argument
// the whole table exists to answer.
//
// A nil account is the unauthenticated caller and holds nothing, and so does a
// disabled one. Deactivation stops every lane at once -- credential.load
// refuses an inactive account outright -- so an account that cannot present a
// credential cannot hold authority either. On the request path that term is
// already true, since Resolve would not have produced the account otherwise;
// it is here for the other caller, which counts accounts it has read straight
// from the table and would otherwise count a disabled administrator as a live
// one.
func Can(ctx context.Context, acct *account.Account, c Capability) (bool, error) {
	if acct == nil || !acct.IsActive || !acct.Role.IsAdmin() {
		return false, nil
	}
	return holds(ctx, acct.ID, c)
}

// holds asks only the row half of the rule. Unexported: a caller reading it
// without the role check has half a rule, which is the failure Can exists to
// make unavailable.
func holds(ctx context.Context, id account.ID, c Capability) (bool, error) {
	var one int
	err := readDB.QueryRowContext(ctx,
		"SELECT 1 FROM AccountCapability WHERE account_id = ? AND capability = ?", id, c).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("reading a capability: %v", err)
	}
	return true, nil
}

// Of lists what an account holds, in the vocabulary's own order rather than
// the table's. It reports rows, not authority -- an account that is no longer
// an admin still has its rows, and Can is what says they mean nothing.
func Of(ctx context.Context, id account.ID) ([]Capability, error) {
	rows, err := readDB.QueryContext(ctx,
		"SELECT capability FROM AccountCapability WHERE account_id = ?", id)
	if err != nil {
		return nil, fmt.Errorf("listing capabilities: %v", err)
	}
	defer func() { _ = rows.Close() }()

	held := map[Capability]bool{}
	for rows.Next() {
		var c Capability
		if err := rows.Scan(&c); err != nil {
			return nil, fmt.Errorf("listing capabilities: %v", err)
		}
		held[c] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing capabilities: %v", err)
	}
	// Ordered by the vocabulary, so two accounts holding the same set render
	// identically and a row holding something no longer in the vocabulary is
	// simply not listed rather than sorted in among the live ones.
	var out []Capability
	for _, c := range All() {
		if held[c] {
			out = append(out, c)
		}
	}
	return out, nil
}

// OfAll reads every account's capabilities in one query, for the listing.
//
// One query rather than one per account: the operator's listing is the place
// where an N+1 is least visible and most certain, since it grows with the
// install rather than with the request.
func OfAll(ctx context.Context) (map[account.ID][]Capability, error) {
	rows, err := readDB.QueryContext(ctx,
		"SELECT account_id, capability FROM AccountCapability")
	if err != nil {
		return nil, fmt.Errorf("listing capabilities: %v", err)
	}
	defer func() { _ = rows.Close() }()

	held := map[account.ID]map[Capability]bool{}
	for rows.Next() {
		var id account.ID
		var c Capability
		if err := rows.Scan(&id, &c); err != nil {
			return nil, fmt.Errorf("listing capabilities: %v", err)
		}
		if held[id] == nil {
			held[id] = map[Capability]bool{}
		}
		held[id][c] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing capabilities: %v", err)
	}

	out := make(map[account.ID][]Capability, len(held))
	for id, set := range held {
		for _, c := range All() {
			if set[c] {
				out[id] = append(out[id], c)
			}
		}
	}
	return out, nil
}

// Grant gives capabilities to an account, on behalf of an actor.
//
// Two checks, and they are the two invariants the plan names. The actor must
// hold CapGrant, which is what makes authority-changing a capability rather
// than a side effect of being an admin. And the actor must hold every
// capability it is handing on -- otherwise CapGrant is CapGrant-plus-
// everything, reachable in two steps instead of one.
//
// It is idempotent. The caller's intent is a state rather than an increment,
// and a second identical call is somebody making sure.
func Grant(ctx context.Context, actor *account.Account, target account.ID, caps ...Capability) error {
	if err := mayChange(ctx, actor, caps); err != nil {
		return err
	}
	return assign(ctx, target, caps)
}

// Revoke takes capabilities away, on behalf of an actor.
//
// The same two checks as Grant, the second one read the other way: an account
// holding only CapGrant must not be able to strip every other administrator of
// an authority it was never trusted with itself. That is not escalation, but
// it is an install left unable to do its own work by somebody who could never
// do it either.
func Revoke(ctx context.Context, actor *account.Account, target account.ID, caps ...Capability) error {
	if err := mayChange(ctx, actor, caps); err != nil {
		return err
	}
	return withdraw(ctx, target, caps)
}

// Assign gives capabilities with no actor: the install's own hand, which is
// what setup.Claim and the CLI have. Both already hold the database, so an
// actor check would be a formality asked of a caller that could write the row
// directly.
func Assign(ctx context.Context, target account.ID, caps ...Capability) error {
	return assign(ctx, target, caps)
}

// Withdraw takes capabilities away with no actor, and still refuses to remove
// the last CapGrant.
//
// That invariant is about the install rather than about the caller, so it
// holds on this path too. It costs the operator nothing: the CLI can hand
// CapGrant to somebody else first, which is the thing they meant to do.
func Withdraw(ctx context.Context, target account.ID, caps ...Capability) error {
	return withdraw(ctx, target, caps)
}

// AssignTx is Assign inside a transaction the caller already holds, for
// setup.Claim -- which writes the first account's role and its full capability
// set in one commit, because a first boot that produced a server nobody can
// administer is not a recoverable state.
//
// It cannot be done by calling Assign from inside another transaction: writeDB
// is a pool of exactly one connection, so the nested write would wait for the
// connection the outer transaction holds until the context times out.
func AssignTx(ctx context.Context, tx *sql.Tx, target account.ID, caps ...Capability) error {
	return insertCaps(ctx, tx, target, caps)
}

// MayTakeOver reports whether actor may do something that amounts to becoming
// target.
//
// Resetting somebody's password is impersonation -- the route that does it says
// so in its own doc comment, and gates itself on CapPasswords for exactly that
// reason -- and impersonating an account is acquiring its capabilities. Set the
// password, log in, and every row that account holds is yours. So it is an
// authority transfer, and the invariant every other authority transfer is held
// to applies: nobody acquires an authority they do not already hold.
//
// Without this the split is decorative. CapPasswords reaches CapGrant in two
// requests -- reset, then log in -- and CapGrant reaches everything else. An
// install that separated "may onboard staff" from "may become staff" got both
// from the first one.
//
// It reads rows rather than authority, which is stricter than Can and
// deliberately so. A demoted administrator's rows survive their demotion, and
// an account holding a dormant grant row is one role change away from holding
// grant: taking it over is taking that, and the refusal names the capability so
// an operator can see what to clear first.
func MayTakeOver(ctx context.Context, actor *account.Account, target account.ID) error {
	held, err := Of(ctx, target)
	if err != nil {
		return err
	}
	for _, c := range held {
		ok, err := Can(ctx, actor, c)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("%w: %s", ErrTargetOutranks, c)
		}
	}
	return nil
}

// mayChange is the actor half of both Grant and Revoke, in one place so the
// two cannot drift on the case nobody tested.
func mayChange(ctx context.Context, actor *account.Account, caps []Capability) error {
	if len(caps) == 0 {
		return errors.New("no capabilities named")
	}
	canGrant, err := Can(ctx, actor, CapGrant)
	if err != nil {
		return err
	}
	if !canGrant {
		return ErrNeedsGrant
	}
	for _, c := range caps {
		held, err := Can(ctx, actor, c)
		if err != nil {
			return err
		}
		if !held {
			return fmt.Errorf("%w: %s", ErrNotHeld, c)
		}
	}
	return nil
}

// execer is the one method the writes below need, and both *sql.DB and *sql.Tx
// have it.
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func assign(ctx context.Context, target account.ID, caps []Capability) error {
	if err := validate(caps); err != nil {
		return err
	}
	return insertCaps(ctx, writeDB, target, caps)
}

func insertCaps(ctx context.Context, db execer, target account.ID, caps []Capability) error {
	if err := validate(caps); err != nil {
		return err
	}
	for _, c := range caps {
		if _, err := db.ExecContext(ctx,
			"INSERT OR IGNORE INTO AccountCapability (account_id, capability) VALUES (?, ?)",
			target, c); err != nil {
			return fmt.Errorf("granting %s: %v", c, err)
		}
	}
	return nil
}

func withdraw(ctx context.Context, target account.ID, caps []Capability) error {
	if err := validate(caps); err != nil {
		return err
	}
	for _, c := range caps {
		if c == CapGrant {
			last, err := wouldStrandTheServer(ctx, target)
			if err != nil {
				return err
			}
			if last {
				return ErrLastGrant
			}
		}
	}
	for _, c := range caps {
		if _, err := writeDB.ExecContext(ctx,
			"DELETE FROM AccountCapability WHERE account_id = ? AND capability = ?",
			target, c); err != nil {
			return fmt.Errorf("revoking %s: %v", c, err)
		}
	}
	return nil
}

// wouldStrandTheServer reports whether target is the only account that can
// administer this server -- the only one for which Can(CapGrant) is true.
//
// One question behind three doors, because there are three ways to reach the
// same state and each of them was found separately. Taking the row away is a
// revocation. Taking the role away leaves every row in place meaning nothing,
// because the rule is a conjunction. Disabling the account stops every lane at
// once. All three end with nobody able to hand authority out, and a server
// nobody can administer is a data-loss event with extra steps.
//
// Asked as the whole conjunction rather than as any part of it. Counting rows
// alone credits a holder who is not an admin; counting admins alone credits one
// who holds nothing; counting either without is_active credits an account that
// cannot log in. Each of those is a second administrator that does not exist.
func wouldStrandTheServer(ctx context.Context, target account.ID) (bool, error) {
	const q = `SELECT COUNT(*) FROM Account a
	           JOIN AccountCapability c ON c.account_id = a.id AND c.capability = ?
	           WHERE a.role = ? AND a.is_active = 1 AND a.id <> ?`
	var others int
	if err := readDB.QueryRowContext(ctx, q, CapGrant, account.RoleAdmin, target).Scan(&others); err != nil {
		return false, fmt.Errorf("counting the accounts that can administer this server: %v", err)
	}
	if others > 0 {
		return false, nil
	}
	// Nobody else can. Whether refusing is right now depends on whether this
	// account can either: taking authority from somebody who never had it, or
	// disabling an ordinary account, must not be reported as the last
	// administrator going.
	acct, err := account.ByID(ctx, target)
	if errors.Is(err, account.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return Can(ctx, acct, CapGrant)
}

// validate refuses a set containing anything outside the vocabulary, at the
// door rather than at the read.
func validate(caps []Capability) error {
	if len(caps) == 0 {
		return errors.New("no capabilities named")
	}
	for _, c := range caps {
		if _, err := ParseCapability(string(c)); err != nil {
			return err
		}
	}
	return nil
}

// ErrLastAdmin reports a role change that would leave the server with nobody
// who can administer it.
var ErrLastAdmin = errors.New("this is the only account that is both an admin and holds the grant capability")

// SetRole changes an account's role, on behalf of an actor.
//
// Gated by CapGrant because the plan defines that capability as "may change
// another account's role or capabilities", and because the conjunction makes
// the two operations equally powerful: an account demoted out of admin keeps
// every row it held and none of them mean anything, which is a revocation of
// all six spelled a different way.
//
// The actor's must-hold rule does not apply -- a role is not a capability, so
// there is nothing to hold -- but the install-integrity rule does, and it is
// the other half of the one Revoke enforces. Revoke protects the row; this
// protects the role. Taking either away from the last account that has both
// leaves a server nobody can administer, and until now only one of those two
// doors was watched.
func SetRole(ctx context.Context, actor *account.Account, target account.ID, role account.Role) error {
	if _, err := account.ParseRole(string(role)); err != nil {
		return err
	}
	canGrant, err := Can(ctx, actor, CapGrant)
	if err != nil {
		return err
	}
	if !canGrant {
		return ErrNeedsGrant
	}
	if !role.IsAdmin() {
		last, err := wouldStrandTheServer(ctx, target)
		if err != nil {
			return err
		}
		if last {
			return ErrLastAdmin
		}
	}
	return account.SetRole(ctx, target, role)
}

// SetActive enables or disables an account, refusing the disable that would
// leave nobody able to administer this server.
//
// The third door, and the one that was open longest because it does not look
// like an authority change. Disabling stops every lane at once -- that is what
// the is_active join in credential.Resolve is for -- so it reaches the same
// state as a demotion by a route neither of the other two guards watched.
//
// Worse while it was open, it inverted the escalation boundary: this operation
// is gated by CapUsers, so an admin holding only users could lock the install
// out, while an admin holding only grant is refused the identical outcome by
// SetRole. The fix is the guard rather than a capability change -- once this
// door asks the same question, users can no longer do what grant cannot.
//
// Enabling is never guarded: it can only add an administrator.
func SetActive(ctx context.Context, target account.ID, active bool) error {
	if !active {
		last, err := wouldStrandTheServer(ctx, target)
		if err != nil {
			return err
		}
		if last {
			return ErrLastAdmin
		}
	}
	return account.SetActive(ctx, target, active)
}
