// Package invite is the only way an account comes into being through HTTP.
//
// Registration is invite-only, and the reason is address verification rather
// than exclusivity: an invite is minted for one address and delivered to that
// inbox, so redeeming it proves the redeemer reads that mail. A signup form
// where the person types their own address proves nothing, and every later
// thing that trusts an address -- a share, a grant, an invite -- would be
// trusting it.
//
// docs/plans/sharing.md § Accounts is the owning document, and decision 9 is
// the shape: an invite is a credential, single-use, expiring, bound to an
// address, and redemption is where E2EE bootstrap happens because it is the one
// moment a client is guaranteed present.
package invite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/dkam/silo/fileserver/account"
	"github.com/dkam/silo/fileserver/credential"
)

// DefaultLifetime is how long an invite lives when the caller says nothing.
//
// Seven days, and not forever. An invite is a bearer token for an identity that
// does not exist yet, so one left outstanding is a standing offer to become
// somebody -- and the person who minted it will not remember it exists.
const DefaultLifetime = 7 * 24 * time.Hour

var (
	// ErrSpent reports an invite that has already been redeemed. Single-use is
	// what makes an invite a verification of one delivery rather than a
	// reusable door.
	ErrSpent = errors.New("this invite has already been redeemed")
	// ErrActiveAccount reports an address somebody is already using. Claiming
	// one is refused outright: activating it would hand a stranger whatever was
	// shared to that address, and minting a second account beside it would
	// recreate the double-mint the identity split made unrepresentable.
	ErrActiveAccount = errors.New("an active account already holds that address")
	// ErrNotFound reports a token, or a credential id, that names no invite.
	// A revoked one still names one: revocation expires the credential and
	// leaves the row, so a withdrawn link is refused as expired rather than as
	// unknown -- the same answer a lapsed one gets, which is the point. See
	// Revoke.
	ErrNotFound = errors.New("no such invite")
)

// Invite is one outstanding or spent invitation.
type Invite struct {
	CredentialID string
	Email        string
	Role         account.Role
	// AccountID is the account this invite's address belongs to: the tombstone
	// minting created, or one that already stood for a share to an address
	// nobody had enrolled.
	AccountID account.ID
	CreatedBy account.ID
	Ctime     int64
	ExpiresAt int64
	// RedeemedAt is zero while the invite is outstanding.
	RedeemedAt int64
}

// Options is what minting an invite needs.
type Options struct {
	Email string
	Role  account.Role
	// By is the administrator minting it, kept so that an invite can be traced
	// to a decision rather than appearing to have made itself.
	By account.ID
	// Lifetime defaults to DefaultLifetime. It is not allowed to be zero-means-
	// forever the way a credential's is: see DefaultLifetime.
	Lifetime time.Duration
}

var readDB, writeDB *sql.DB

// Init sets the database handles.
func Init(read, write *sql.DB) { readDB, writeDB = read, write }

// Mint creates an invite for an address, and the inactive account that address
// will belong to.
//
// It has to make the account: the credential row references one, and the whole
// point is that this person does not have an account yet. What it makes is a
// tombstone -- inactive, no password -- which opens no lane at all, because
// credential.load refuses an inactive account before it looks at anything else.
// So the row is a placeholder for an address rather than a way in.
//
// An address that already has a tombstone is reused rather than duplicated,
// which is the same claim redemption performs: an address appearing in a share
// before its person arrived is exactly the case invites exist to close.
func Mint(ctx context.Context, o Options) (*Invite, string, error) {
	email := account.Normalize(o.Email)
	if email == "" {
		return nil, "", errors.New("an invite needs an address")
	}
	if _, err := account.ParseRole(string(o.Role)); err != nil {
		return nil, "", err
	}
	if o.By.IsZero() {
		return nil, "", errors.New("an invite needs the administrator who minted it")
	}
	if o.Lifetime <= 0 {
		o.Lifetime = DefaultLifetime
	}

	// Asked before anything is written. An active account is a person, and
	// inviting somebody to an address they are already using is either a
	// mistake or an attempt to take it.
	switch existing, err := account.ByEmail(ctx, email); {
	case err == nil && existing.IsActive:
		return nil, "", fmt.Errorf("%w: %s", ErrActiveAccount, email)
	case err != nil && !errors.Is(err, account.ErrNotFound):
		return nil, "", err
	}

	// Idempotent on the address: an existing tombstone is returned rather than
	// duplicated, which is what keeps one address to one account. Inactive
	// until somebody redeems, because an account that could log in before its
	// person arrived would be an account the invite was not needed for.
	//
	// The same function a share to an unenrolled address calls. That is the
	// point of it being a function: the two paths mint the same row, and an
	// address that was shared to before it was invited must not end up with a
	// second account beside the first.
	id, err := account.Tombstone(ctx, email, o.Role)
	if err != nil {
		return nil, "", err
	}

	cred, token, err := credential.Issue(ctx, credential.IssueOpts{
		Kind:      credential.KindInvite,
		AccountID: id,
		Label:     "invite for " + email,
		// Read, and it opens exactly one route. The ceiling is not what keeps
		// an invite from being a session -- the API's kind list is -- but a
		// credential that could write if that list ever loosened is a worse
		// starting point than one that could not.
		Perm:     "r",
		Lifetime: o.Lifetime,
	})
	if err != nil {
		return nil, "", err
	}

	now := time.Now().Unix()
	if _, err := writeDB.ExecContext(ctx,
		`INSERT INTO Invite (credential_id, email, role, created_by, ctime, redeemed_at)
		 VALUES (?, ?, ?, ?, ?, NULL)`,
		cred.ID, email, o.Role, o.By, now); err != nil {
		return nil, "", fmt.Errorf("recording an invite: %v", err)
	}
	return &Invite{
		CredentialID: cred.ID, Email: email, Role: o.Role, AccountID: id,
		CreatedBy: o.By, Ctime: now, ExpiresAt: cred.ExpiresAt,
	}, token, nil
}

// Redeem spends an invite and activates the account it names.
//
// The redeemer does not choose their address. Delivery of the invite to that
// inbox is the verification step, so an address the redeemer supplied would
// verify nothing -- which is why this takes a token and nothing else.
//
// What it does not do is set a password or publish key material. That is the
// client's half and it happens next, over the ordinary account routes, because
// the wrapKey that seals an identity key is derived from a password the server
// must never see. docs/plans/sharing.md § Accounts sequences the three steps.
func Redeem(ctx context.Context, token string) (*Invite, error) {
	cred, err := credential.ResolveInvite(ctx, token)
	if err != nil {
		return nil, err
	}

	// The spend is the first thing that happens and the only gate on it.
	//
	// A conditional update, so two redemptions that arrive together produce one
	// winner: whoever sets redeemed_at from NULL proceeds, and everybody else
	// sees no rows affected. There is deliberately no "is it already redeemed"
	// read in front of it. That read looks like a courtesy and is a second gate
	// -- it answers first, so the conditional update stops being exercised by
	// anything, and a later edit that dropped the condition would pass every
	// test while admitting two people on one invite.
	//
	// It also fixes what the read got wrong. With a check before the spend, a
	// racing loser was told the address already had an active account: true, and
	// about the winner's own work a moment earlier, which is not an answer
	// anybody can act on.
	now := time.Now().Unix()
	res, err := writeDB.ExecContext(ctx,
		"UPDATE Invite SET redeemed_at = ? WHERE credential_id = ? AND redeemed_at IS NULL",
		now, cred.ID)
	if err != nil {
		return nil, fmt.Errorf("spending an invite: %v", err)
	}
	spent, err := res.RowsAffected()
	if err != nil {
		return nil, fmt.Errorf("spending an invite: %v", err)
	}

	inv, err := byCredential(ctx, cred.ID)
	if err != nil {
		// No invite row for a credential that proved itself an invite means the
		// two were written inconsistently. It is the not-found answer either
		// way, and it is read after the spend so that no lookup precedes the
		// gate.
		return nil, err
	}
	if spent != 1 {
		return nil, ErrSpent
	}

	acct, err := account.ByID(ctx, inv.AccountID)
	if err != nil {
		return nil, err
	}
	if acct.IsActive {
		// Reachable only if something outside this path activated the account
		// between minting and redeeming -- Mint refuses an active address, and
		// the spend above means no other redemption did it. The invite is burnt
		// either way, which is not a loss: an invite to an address somebody is
		// already using is dead whatever we do with the row.
		return nil, fmt.Errorf("%w: %s", ErrActiveAccount, inv.Email)
	}

	if err := account.SetRole(ctx, inv.AccountID, inv.Role); err != nil {
		return nil, err
	}
	if err := account.SetActive(ctx, inv.AccountID, true); err != nil {
		return nil, err
	}
	// The credential row is left exactly as it is, and that is deliberate.
	//
	// Expiring or deleting it looks like tidying and costs two things. The
	// Invite row references it and is the record of who was invited and when
	// they arrived, so deleting is refused by the foreign key that exists to
	// protect that. And expiring makes a second redemption report "expired"
	// instead of "already redeemed" -- true, useless, and the wrong half of the
	// answer for somebody who is holding a link they have already used.
	//
	// It costs nothing to leave. A spent invite's token opens exactly one route
	// and that route is this one, which refuses it on redeemed_at; no other
	// lane accepts the invite kind at all; and the credential's own expiry
	// still runs out underneath it.
	inv.RedeemedAt = now
	return inv, nil
}

// Revoke withdraws an outstanding invite.
//
// The credential is expired rather than deleted and the Invite row is left
// standing, because that row is the record of who was invited and by whom. An
// invite withdrawn before anybody used it is a thing an operator did, and a
// trail that forgets the invites nobody redeemed is a trail that only remembers
// the decisions that worked out.
//
// A withdrawn invite and one that ran out of its seven days give the holder the
// same answer, credential.ErrExpired, and that is the intent rather than a
// shortcut. Neither is an answer they can act on except by asking for another
// one, and a distinct "this was taken away from you" would say something about
// an administrator's decision to somebody outside it. Which of the two it was
// is the audit log's question, and docs/plans/events.md gives it a name --
// `credential.revoked` -- along with the actor this function deliberately does
// not take a parameter for until there is somewhere to put it.
//
// A spent invite is refused rather than expired. Nothing is left to withdraw:
// the person arrived, the account is active, and the credential's expiry is no
// longer what stands between anybody and it. Expiring it would report success
// for a decision it did not carry out. Taking that account back is a separate
// operation, and ErrSpent is what sends an operator to it.
// This one reads before it writes, which is what Redeem was fixed for not
// doing, and the difference is what the gate is protecting. Redeem's read was a
// gate on admission and losing it let two people in on one invite. This read
// gates a message: a revocation that arrives in the same instant as the
// redemption it was too late for finds redeemed_at still NULL and expires a
// credential that is now spent. The person is in either way -- that decision
// was made by the spend, which is still the one conditional write -- and the
// only casualty is that presenting the token again reports "expired" instead of
// "already redeemed".
//
// Closing it would mean putting the condition inside the write, which means
// invite issuing an UPDATE against Credential. Then expires_at is set from two
// packages and "how a credential is killed without deleting it" is decided in
// two places, which is the coupling RevokeByLibrary exists to have removed.
// Paying that permanently to sharpen one message in one race is the wrong
// trade.
func Revoke(ctx context.Context, credentialID string) error {
	inv, err := byCredential(ctx, credentialID)
	if err != nil {
		return err
	}
	if inv.RedeemedAt != 0 {
		return fmt.Errorf("%w: %s", ErrSpent, inv.Email)
	}
	return credential.Expire(ctx, credentialID)
}

// List reports every invite, spent and outstanding, for the admin surface.
func List(ctx context.Context) ([]Invite, error) {
	rows, err := readDB.QueryContext(ctx,
		`SELECT i.credential_id, i.email, i.role, c.account_id, i.created_by, i.ctime,
		        c.expires_at, i.redeemed_at
		 FROM Invite i JOIN Credential c ON c.id = i.credential_id
		 ORDER BY i.ctime DESC`)
	if err != nil {
		return nil, fmt.Errorf("listing invites: %v", err)
	}
	return scanInvites(rows)
}

func byCredential(ctx context.Context, credentialID string) (*Invite, error) {
	rows, err := readDB.QueryContext(ctx,
		`SELECT i.credential_id, i.email, i.role, c.account_id, i.created_by, i.ctime,
		        c.expires_at, i.redeemed_at
		 FROM Invite i JOIN Credential c ON c.id = i.credential_id
		 WHERE i.credential_id = ?`, credentialID)
	if err != nil {
		return nil, fmt.Errorf("reading an invite: %v", err)
	}
	found, err := scanInvites(rows)
	if err != nil {
		return nil, err
	}
	if len(found) == 0 {
		return nil, ErrNotFound
	}
	return &found[0], nil
}

func scanInvites(rows *sql.Rows) ([]Invite, error) {
	defer func() { _ = rows.Close() }()
	var out []Invite
	for rows.Next() {
		var i Invite
		var expires, redeemed sql.NullInt64
		if err := rows.Scan(&i.CredentialID, &i.Email, &i.Role, &i.AccountID,
			&i.CreatedBy, &i.Ctime, &expires, &redeemed); err != nil {
			return nil, err
		}
		i.ExpiresAt = expires.Int64
		i.RedeemedAt = redeemed.Int64
		out = append(out, i)
	}
	return out, rows.Err()
}
