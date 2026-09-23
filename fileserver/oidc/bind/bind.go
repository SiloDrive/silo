// Package bind decides which Silo account a verified IdP login is.
//
// Package oidc ends at verified claims and knows nothing about accounts; this
// is where claims meet the database. The rules are docs/plans/oidc.md § Who
// gets an account, and the one idea under all of them is worth stating first:
// linking an identity to the account that holds a verified address, and taking
// an account over by asserting its address, are the same operation. They
// differ only in whether the IdP's claim is true, so every path that trusts an
// address is gated on the operator having said this IdP's addresses can be
// believed -- and the path that looks like it trusts nothing, creating an
// account, must not quietly be one of them.
//
// The whole decision is one transaction on the write handle. The write pool is
// a single connection, so two logins for the same newcomer run one after the
// other rather than both deciding the identity is new, and every read below
// sees the writes before it.
package bind

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/SiloDrive/silo/fileserver/account"
	"github.com/SiloDrive/silo/fileserver/invite"
	"github.com/SiloDrive/silo/fileserver/oidc"
	log "github.com/sirupsen/logrus"
)

// The refusals. Each carries a message the person who was refused can act on,
// because it reaches them as the body of a 403 on the client's poll and there
// is nobody else to explain it.
var (
	// ErrDomain is an address outside the allowed domains.
	ErrDomain = errors.New("addresses at that domain cannot sign in to this server")
	// ErrDisabled is an account an administrator switched off. Signing in at
	// the IdP is not a way to switch it back on.
	ErrDisabled = errors.New("this account has been disabled")
	// ErrNoAccount is nobody matching, under a policy that creates nothing.
	ErrNoAccount = errors.New("there is no Silo account for this address; ask an administrator for an invite")
	// ErrAddressTaken is an address that belongs to an account this login may
	// not have.
	ErrAddressTaken = errors.New("this address already belongs to a Silo account; " +
		"sign in with its password, or ask an administrator to link your identity to it")
	// ErrNoVerifiedAddress is a login that would need an account made for it
	// and has no address the IdP vouches for.
	ErrNoVerifiedAddress = errors.New("the identity provider did not vouch for an email address, " +
		"which this server needs to create an account")
)

var writeDB *sql.DB

// Init points the package at the write handle. It has no use for the read one:
// every question it asks is part of a decision it is about to write down.
func Init(write *sql.DB) { writeDB = write }

// Account returns the account a verified login is, linking or creating one
// where the policy allows, or a refusal from the list above.
func Account(ctx context.Context, cfg *oidc.Config, c *oidc.Claims) (account.ID, error) {
	// Step 0, before the identity lookup rather than after it. Somebody who
	// has left keeps their IdP subject; the domain list is how an operator
	// says their address no longer counts, and a list consulted only for
	// newcomers would say nothing about them.
	if !cfg.DomainAllowed(c.Email) {
		return account.Zero, fmt.Errorf("%w: %s", ErrDomain, c.Email)
	}

	tx, err := writeDB.BeginTx(ctx, nil)
	if err != nil {
		return account.Zero, fmt.Errorf("binding an identity: %v", err)
	}
	defer func() { _ = tx.Rollback() }()

	id, err := decide(ctx, tx, cfg, c)
	if err != nil {
		return account.Zero, err
	}
	if err := tx.Commit(); err != nil {
		return account.Zero, fmt.Errorf("binding an identity: %v", err)
	}
	return id, nil
}

func decide(ctx context.Context, tx *sql.Tx, cfg *oidc.Config, c *oidc.Claims) (account.ID, error) {
	// Step 1: an identity already linked. From here the address plays no part
	// at all -- a person who changes theirs at the IdP to somebody else's
	// stays on their own account, because the other one is linked to its own
	// subject.
	switch id, err := account.IdentityTx(ctx, tx, c.Issuer, c.Subject); {
	case err == nil:
		a, err := account.ByIDTx(ctx, tx, id)
		if err != nil {
			return account.Zero, err
		}
		if !a.IsActive {
			return account.Zero, ErrDisabled
		}
		return id, nil
	case !errors.Is(err, account.ErrNotFound):
		return account.Zero, err
	}

	// Everything after this trusts or creates by address, and an address the
	// IdP has not vouched for is a claim anybody can make. It matches
	// nothing, and nothing is made under it: an address belongs to exactly
	// one account, so provisioning an unproven one locks its real owner out.
	if c.Email == "" || !c.EmailVerified {
		if cfg.Policy == oidc.PolicyLink {
			return account.Zero, ErrNoAccount
		}
		return account.Zero, ErrNoVerifiedAddress
	}

	// Step 2: a verified address that an account already holds.
	holder, err := account.ByEmailTx(ctx, tx, c.Email)
	switch {
	case err == nil:
		return claimByAddress(ctx, tx, cfg, c, holder)
	case !errors.Is(err, account.ErrNotFound):
		return account.Zero, err
	}

	// Step 3: nobody holds it.
	if cfg.Policy == oidc.PolicyLink {
		return account.Zero, fmt.Errorf("%w: %s", ErrNoAccount, c.Email)
	}
	id, created, err := account.CreateTx(ctx, tx, c.Email, "", account.DefaultRole)
	if err != nil {
		return account.Zero, err
	}
	// The line the whole package exists for. CreateTx is idempotent on the
	// address: asked for an account at a taken one, it returns the account
	// that has it. Nothing above can reach here with a taken address -- the
	// lookup just said nobody holds it, inside the same transaction -- so
	// this is not expected to fire. It is here because if it ever does,
	// trusting the answer would perform step 2 with none of step 2's checks,
	// and hand over an account on the strength of an address.
	if !created {
		return account.Zero, fmt.Errorf("%w: %s", ErrAddressTaken, c.Email)
	}
	if err := account.LinkIdentityTx(ctx, tx, c.Issuer, c.Subject, id); err != nil {
		return account.Zero, err
	}
	log.Infof("OIDC: created account %s (%s) for %s subject %s", id, c.Email, c.Issuer, c.Subject)
	return id, nil
}

// claimByAddress is step 2: the IdP has proven an address an account holds.
func claimByAddress(ctx context.Context, tx *sql.Tx, cfg *oidc.Config, c *oidc.Claims, holder *account.Account) (account.ID, error) {
	// Isolated means this IdP's address claims are not believed. Neither
	// linking nor creating is safe, and an invite at the address is not spent
	// either, since finding it would be believing the address.
	if cfg.Policy == oidc.PolicyIsolated {
		return account.Zero, fmt.Errorf("%w: %s", ErrAddressTaken, c.Email)
	}

	arrived, err := account.ArrivedTx(ctx, tx, c.Email)
	if err != nil {
		return account.Zero, err
	}
	if arrived {
		// Somebody's account. Switched off is switched off, whichever way
		// in it is approached from.
		if !holder.IsActive {
			return account.Zero, ErrDisabled
		}
		if err := account.LinkIdentityTx(ctx, tx, c.Issuer, c.Subject, holder.ID); err != nil {
			return account.Zero, err
		}
		// Warning rather than info: this is a merge, and the operator should
		// be able to find one they did not expect.
		log.Warnf("OIDC: linked %s subject %s to the existing account %s (%s) by verified address",
			c.Issuer, c.Subject, holder.ID, c.Email)
		return holder.ID, nil
	}

	// A tombstone: an address that was shared to or invited before its person
	// arrived. An outstanding invite is the administrator having already said
	// this address may have an account, and with what role.
	role := account.DefaultRole
	switch inv, err := invite.SpendForAddressTx(ctx, tx, c.Email); {
	case err == nil:
		role = inv.Role
	case errors.Is(err, invite.ErrNotFound):
		// No invite. Claiming the tombstone is creating an account, which
		// only create does.
		if cfg.Policy != oidc.PolicyCreate {
			return account.Zero, fmt.Errorf("%w: %s", ErrNoAccount, c.Email)
		}
	default:
		return account.Zero, err
	}

	if err := account.ActivateTx(ctx, tx, holder.ID, role); err != nil {
		return account.Zero, err
	}
	if err := account.LinkIdentityTx(ctx, tx, c.Issuer, c.Subject, holder.ID); err != nil {
		return account.Zero, err
	}
	log.Infof("OIDC: %s (%s) arrived through %s as %s", holder.ID, c.Email, c.Issuer, role)
	return holder.ID, nil
}
