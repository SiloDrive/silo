# Plan: self-service credential management

**Status: steps 0, 1 and 2 are built. Next is reconsidering the password
change, then the drive clients.** A person can see the
credentials their account holds, and revoke one of them, without an
administrator and without signing everything out.

What shipped, and how it differed from the proposal:

- **Step 0 landed as written**, except on one detail this document had wrong:
  redemption does not `Expire` the invite credential. `invite.Redeem` leaves
  the row untouched deliberately, so a second attempt reports "already
  redeemed" rather than "expired". The bug is the same — `invite.Mint` issues
  the invite against the *invitee's own* tombstone account, so a bulk delete
  meets a foreign key it cannot satisfy — and so is the fix.
- **`RevokeKind` refuses the invite kind rather than filtering it.** Filtering
  would make `RevokeKind(invite)` report a successful revocation of nothing,
  which is the exact failure mode that function already refuses unrecognised
  kinds to prevent.
- **The two endpoints landed as specified**, including the `404`, the
  self-revoke, the absent-rather-than-zero timestamps, and no write permission.
  `docs/responses.md` states the rule as whose-resource-is-it rather than as a
  fact about these two routes.
- **The CLI is `silo credential list|revoke`**, singular, as recommended;
  `silo token` is untouched. It labels its own session through a new
  `APIClient.UserAgent` -- the server takes a credential's label from that
  header -- and signs itself out on every path, so listing credentials does not
  add one. `client.Logout` clears the stored password as well as the token,
  because `doRequest` re-logs in and retries once on a `401` and would
  otherwise mint a replacement for the row just discarded.
- **`expiryState`, `lastUsed`, `formatTime` and `blanks` moved to
  `internal/format`** as proposed, and `silo user` picked up the same `Time`.

**The littering is wider than this command.** `cli.Run` logs in for *every*
subcommand and none of them sign out, so `silo ls` leaves a twenty-four-hour
session behind too. Only the credential command is fixed here, because it is
the one that displays the table it would pollute. Making `Run` sign out for all
of them is the obvious follow-up and is deliberately not in this change.

## Why: the hammer exists because the scalpel does not

Today the whole self-service revocation vocabulary is three verbs:
`auth/logout` signs out the credential that asked, `auth/logout/everywhere`
signs out all of them, and `auth/password` signs out all of them as a side
effect of changing the password. There is no way to *see* what an account
holds, and no way to kill one credential and keep the rest.

That absence is the reason the password change is as blunt as it is. The note
on `api.ChangePasswordHandler` argues the case on its own terms and the
argument is sound — a device credential is ninety days and renews from itself,
so a stolen one outlives the password it was minted under indefinitely, and
changing the password is what a person reaches for when they think something
has been taken. But it is also the only thing pointed at that credential, and a
tool that is the only option gets built to cover every case, which is how it
ended up revoking the credential that asked.

So the order matters, and it is the main thing this plan fixes: **build the
selective revoke first, and reconsider the blanket one after.** Loosening the
password change while it is still the only panic button would leave the stolen
ninety-day credential with nothing aimed at it at all.

## What already exists, and the one thing that is broken

Most of it, which is why this is small:

- **The schema carries what a useful list needs.** `Credential` has `label`,
  `kind`, `scope`, `perm`, `client_id`, `ctime`, `expires_at`, `last_used` and
  `last_ua`. Nothing new is stored, and nothing new should be — see *What not
  to build*.
- **The store layer is written.** `credential.ListByAccount` returns an
  account's rows newest first. `credential.Revoke` takes an owner alongside the
  id and scopes the delete to it, so it structurally cannot reach another
  account's row.
- **The caller's own row is in hand.** `middleware.GetCredential` returns the
  credential a request authenticated with, which is what marks one entry in the
  list as *this one*.
- **The rendering exists admin-side.** `silo token list` already prints this
  table against the database directly, with `expiryState` and `lastUsed`
  doing the formatting. They are unexported in package `fileserver`, and the
  HTTP command lives elsewhere, so they move — `internal/format` is the
  obvious home — rather than get written twice.

**But revocation does not work for an invited account.** The `Invite` table
references `Credential(id)` with no `ON DELETE`, foreign keys are on, and
redemption expires the invite credential rather than deleting it because the
`Invite` row is the trail of who was invited and when they arrived. So every
account that came in through an invite — which is every account an
administrator did not make by hand with `silo user add` — holds one row that `DELETE FROM Credential` cannot remove, and the
statement fails as a whole:

```
Revoke(invite cred):  revoking credential: constraint failed: FOREIGN KEY constraint failed (787)
RevokeAll(invitee):   revoking credentials: constraint failed: FOREIGN KEY constraint failed (787)
```

That is `auth/logout/everywhere` and `auth/password` answering `500` today
for almost everybody, and the existing tests miss it because they revoke
accounts made by `account.Create`, not by redemption. It is step 0 below, and
the new endpoint is not started until it is fixed: a `DELETE` that inherits
this would be a scalpel that 500s on the one row it should never have offered.

## Step 0: revocation must survive the invite row

The fix is in the delete statements, not the schema. `Revoke`, `RevokeAll`
and `RevokeKind` gain `AND kind != 'invite'`, and the reasoning goes on
`Revoke` where the others can point at it:

- An invite credential is already dead by the time anybody could revoke it.
  Redemption calls `Expire`, and an unredeemed one is revoked by
  `invite.Revoke`, which also calls `Expire` — "how a credential is killed
  without deleting it" is already decided in one place, and this keeps it
  there.
- The `Invite` row is a record, and cascading the delete would make signing
  out everywhere erase who invited you. `ON DELETE SET NULL` is not available
  because `credential_id` is the primary key.
- Excluding the kind means the count `logout/everywhere` reports stays the
  number of *live* credentials that went, which is the sentence a client
  shows.

The test comes first and fails first: mint an invite, redeem it, then call
`RevokeAll` on the account it made and expect no error. A second test does
the same through `POST auth/logout/everywhere`, because the handler is where
the `500` is observed. Then the same pair for `Revoke` on a non-invite row of
an invited account.

## The two endpoints

```
GET    /api/silo/v1/account/credentials
DELETE /api/silo/v1/account/credentials/{id}
```

Both mount inside `apiRouter`, beside `auth/password` and
`auth/logout/everywhere` and for the same reason: they are account-wide, so
they take the narrowing and a credential scoped to one library is refused
before the handler. Note the contrast with `auth/logout` and `auth/renew`,
which are mounted outside it under `middleware.RequireOwnCredential` because
they are about the row presenting them rather than about the account — a mount
cut to one library must still be able to sign itself out.

The list returns one object per credential: `id`, `kind`, `label`, `scope`,
`perm`, `client_id`, `created`, `expires_at`, `last_used`, `last_ua`, and
**`current: true`** on the row the request authenticated with. That flag is the
point of the endpoint as much as the list is — without it a client cannot
render "this device" and a person cannot avoid revoking the session they are
looking at.

**The list omits `kind: invite`.** The only invite row a self-service caller
can ever hold is their own spent one, expired at redemption and referenced by
the `Invite` table. It is not a way in, the person cannot act on it, and after
step 0 the `DELETE` would answer `404` for it anyway — so it is not offered.
`silo token list` keeps showing it, because an operator is reading the record.

The `DELETE` answers with the body the logout routes already use,
`{"revoked": 1}`, plus `"current": true` when the row was the caller's own,
so a client can tell it has just signed itself out. `200` with a body rather
than the `204` that `DELETE …/shares/{principal}` answers: this family
already speaks in counts, and `docs/auth.md` § Discarding a credential
explains why a count beats a `204`.

Four behaviours to settle here rather than in review:

- **A foreign or unknown id is `404`, not `403`.** `credential.Revoke` is
  already owner-scoped, so a row belonging to somebody else and a row that
  never existed are indistinguishable to it — and that is the correct answer to
  give, because the alternative tells a caller which ids exist on other
  accounts. This is the answer `DELETE account/keys/recovery/{n}` already
  gives for a sub-resource of the caller's own account; the `403` that
  `docs/responses.md` reserves for a library you cannot see is the
  cross-account case, and this is not one. The decision goes into
  `responses.md` next to that rule, so the next sub-resource does not
  relitigate it.
- **Revoking your own id is allowed**, and is just `auth/logout` reached by
  another name. Refusing it would make every client special-case the one row it
  can most easily identify. The response says what happened so a client can
  tell it has signed itself out.
- **No write permission is required.** This is the rule the two logout
  routes already hold, stated in `docs/auth.md` and on `api.LogoutHandler`:
  revocation only ever takes access away, so a read-only credential may do
  it. Requiring write here would make the selective revoke stricter than
  `logout/everywhere`, which a read-only credential may already fire and
  which takes the laptop's credential with everything else. `auth/password`
  is the odd one out because it *sets* something, and the rule in auth.md
  is widened from "neither route" to "every revocation route" when these
  land.
- **The capability string is `credentials`**, on the `GET /` list in
  `api.go`, covering both routes. A client that wants to render the page
  asks for the feature, not the version.

## Order of work

0. **Step 0 above.** Failing test, then the fix, then the suite.
1. **The two endpoints.** Handlers over the store functions above, plus the
   `current` flag and the capability. `docs/protocol.md` gains them,
   `docs/auth.md` § Discarding a credential gains them beside the logout
   routes, and `docs/responses.md` gets the `404` decision.
2. **The `silo` CLI.** This is deliberately before any drive client, because of
   the scenario the feature exists for: if the laptop is stolen, the stolen
   laptop is the one surface that cannot be used to revoke it. Revocation has
   to be reachable from something the person still has, and a CLI that speaks
   HTTP works from any machine with the binary and the password — no client
   release required.
3. **Reconsider the password change**, once step 2 has shipped and a targeted
   revoke actually exists. See below.
4. **The silo-drive clients.** Where a person will most often *notice* they
   have been signed out, and a natural home for the list once the server and
   CLI have settled its shape. Not first.

## The CLI: naming, and not littering its own list

`silo credential list` and `silo credential revoke <id>`, over HTTP, against
`SILO_URL` with `SILO_EMAIL` and `SILO_PASSWORD`, the way `silo tui` already
dials in.

**Singular**, because `silo token` and `silo user` are singular and a person
should not have to remember which subcommand took the `s`.

This sits beside the existing `silo token list <email>`, and the collision is
worth naming rather than discovering: they are genuinely different operations —
`token` is an administrator reading the database directly on the host, and
`credential` is a person asking the server about themselves — but two similar
words for two similar-looking tables is how a CLI gets confusing.

The recommendation is to use **credential** for everything new, because it is
the word the schema, the store package and the middleware already use, and to
leave `silo token` alone: renaming it is a breaking change to an
administrator's muscle memory for no gain. If that is judged too close, the
alternative is to fold self-service into the existing command as
`silo token list --mine`, which has one fewer word but hides a lane change
behind a flag. Decide before writing the command, not after.

**The command signs itself out when it is done.** The CLI's only HTTP lane
today is the TUI's: log in with the password, receive a twenty-four-hour
session credential, and never revoke it. A `silo credential list` built the
same way would add a row to the table every time it printed one, and the
`current` flag would mark the CLI's throwaway session rather than anything the
person cares about. So the command logs in with a label that says what it is
(`silo credential`), marks its own row *this command* in the output so the
person sees it and does not wonder, and calls `auth/logout` on exit — on the
error path too. `revoke` does the same, and if the id it was given is its own,
that is the logout and it says so rather than calling it twice.

## Then reconsider the password change

With a targeted revoke available, the blanket one has options it does not have
today:

- **Exempt the calling credential.** The person changing their password
  demonstrably holds it; signing them out proves nothing and is the part of the
  current behaviour that most feels like a bug.
- **Or make it explicit** — a `revoke_others` field on the request, defaulting
  to true, so the safe thing happens by default and a client that knows better
  can say so.

Either way the reasoning in the `api.ChangePasswordHandler` note should be
updated rather than deleted: it is still right about why the old
sessions-only rule was wrong, and the fix is a third option that was not
available when it was written. This step is **blocked on step 2**, and that is
the whole point of the ordering.

## What not to build

- **No new columns.** `last_ua` and `last_used` are enough to tell two devices
  apart. Geolocating an IP, storing a login history, or naming devices for the
  person are all a different feature with a different privacy story.
- **No schema change for step 0.** Cascading the invite foreign key would
  make a sign-out erase a record; excluding the kind from the delete keeps
  the record and costs one clause.
- **No web session-management page.** `/admin` is an operator page over the
  whole server; this is an account looking at itself. Mixing them means the
  operator page grows a per-user mode.
- **Not a replacement for `auth/logout/everywhere`.** It stays. A person who
  thinks something has been taken should not have to identify *which* row is
  the intruder before they can act.

## Related

- [`../auth.md`](../auth.md) § Discarding a credential — the rule that
  revocation needs no write permission, which these routes inherit; and
  § Logout and revocation under Part 2, the backchannel-logout lane that is
  designed and not built. That lane wants `credential.RevokeKind`, which
  exists and currently has no caller.
- [`sharing.md`](sharing.md) § Accounts — invites, the tombstone an invite
  claims, and why the invite credential is expired rather than deleted at
  redemption. The step 0 bug lives on the seam between that section and
  auth.md's revocation.
