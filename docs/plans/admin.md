# Plan: administrative authority, and the surface it gates

Date: 2026-08-30
Status: **partly built** — the `role` column, the capability table and both
`grant` invariants have landed; the middleware and the HTTP surface are
designed and not built. Depends on
[`sharing.md`](sharing.md) § Accounts, which owns the role vocabulary, and on
[`../auth.md`](../auth.md)'s credential table, which has landed.

Scope: how an account comes to hold administrative authority, what that
authority can and cannot reach, the HTTP surface it gates, and the web page
that surface exists for. Grants — "may this principal do op at (library,
path)" — are [`sharing.md`](sharing.md)'s question and stay there.

## Decisions

| # | Decision |
|---|---|
| 1 | **Role and capability are two questions, not two spellings of one.** `role` says what kind of account this is; a capability says which administrative operation it may perform. Neither is derivable from the other. |
| 2 | Capabilities are **rows, not columns**. `AccountCapability(account_id, capability)`. Adding an administrative operation is data; it is not a migration. |
| 3 | The rule is a conjunction and has exactly one spelling: **`role == admin` AND a row exists for `(account, capability)`**. No implicit set, no deny-list, no "an admin with no rows means all of them". |
| 4 | The vocabulary is **closed and parsed**, like `Role`. An unrecognised capability is an error at the door, not a value read back later that matches no rule. |
| 5 | The initial six names come from **the CLI commands that already exist**, not from endpoints nobody has written. |
| 6 | `grant` — may change another account's role or capabilities — is **separated from the first commit**. It is the privilege-escalation boundary, and it is the one capability that cannot be retrofitted without demoting people who already had it in practice. |
| 7 | Nobody may grant what they do not hold, and **the last holder of `grant` may not drop it**. A server nobody can administer is a data-loss event with extra steps. |
| 8 | No capability reaches library **content**. There is no `read_any_library` and there will not be one; see the bound below. |

## Why not one flag

`is_admin` is the cheap answer and it is cheap in the direction that costs
later. Widening a single flag is silent: every administrative feature added
after it enlarges what the flag means, and nothing anywhere records that it
grew. Narrowing it is not silent — by the time granularity is wanted, everyone
holding the flag holds everything, and splitting it takes access away from real
people who had it yesterday.

The asymmetry is the whole argument. A capability set costs perhaps a day now
and is the version that never has to be taken back.

## Why not one column per capability

`can_add_user`, `can_set_quota`, `can_revoke_tokens` — the vocabulary is right
and the storage is wrong. A column per operation means a schema migration, a
CLI flag, a JSON field and a checkbox for every administrative feature, forever;
and "what can this person do" becomes a read of N columns that no query can ask
generically. It also bumps `SchemaVersion` each time, which
[`../dbutil`](../../fileserver/dbutil/schema.go) is explicit is for shapes that
`CREATE TABLE IF NOT EXISTS` cannot apply — a cost worth paying for a real shape
change and not for a new verb.

Rows ask one question:

```sql
CREATE TABLE IF NOT EXISTS AccountCapability (
  account_id BLOB NOT NULL REFERENCES Account(id),
  capability TEXT NOT NULL,
  PRIMARY KEY (account_id, capability)
);
```

The admin page renders the set without knowing the vocabulary in advance, and
the check reads the way the flag would have:

```go
if !admin.Can(acct, admin.CapUsers) { ... }
```

## The six

They are the clusters that `silo` already implements, because
[`../roadmap.md`](../roadmap.md) says the admin endpoints "should call the same
`account` functions the CLI does rather than reimplement them beside it". That
makes the CLI the inventory, and the inventory a fact rather than a guess.

| capability | the operations it covers |
|---|---|
| `users` | `silo user add`, `list`, `disable`, `enable` |
| `passwords` | `silo user passwd`. Separate, because a reset is impersonation and not administration |
| `quota` | `silo user quota`, and the server ceiling when it lands |
| `tokens` | `silo token`, and `credential.RevokeAll` |
| `retention` | `silo retention`, GC, expiry |
| `grant` | may change another account's role or capabilities |

Six, each traceable to code that exists today. A seventh arrives when a seventh
operation does, and not before — the point of rows is that it costs nothing to
wait.

`passwords` is split from `users` on purpose. Creating and disabling accounts
is administration; setting somebody's password is stepping into their account,
and an install may reasonably want a person who can onboard staff without being
able to become them.

## `grant` is the boundary

Everything else in the table is an operation. `grant` is the operation that
changes who may perform operations, which makes it SQL's `GRANT OPTION` and
makes it the one that has to exist from the first commit. Add it later and
there are only two moves, both bad: hand it to every existing admin, which
makes the split cosmetic, or take it from people who have been using it, which
is decision 6's whole point.

Two invariants, and both want a test:

**Nobody grants what they do not hold.** An account with `users` and `grant`
can create an account and give it `users`; it cannot give it `retention`.
Otherwise `grant` is `grant`-plus-everything, spelled indirectly.

**The last holder of `grant` may not drop it.** Not the last admin — the last
holder of `grant` specifically, since an install can have several admins of whom
one manages authority. The operation is refused with the reason, at the same
layer for the CLI and the HTTP route, because two spellings of this rule would
disagree on the case nobody tested. `setup.Claim` is the other end of the same
invariant: it writes `role = admin` and all six capabilities in one transaction,
because a first boot that produces a server nobody can administer is not a
recoverable state.

## What administrative authority cannot reach

There is no capability over library content, and the reason is structural
rather than a policy anyone has to keep enforcing.

`silo user passwd` calls `authmgr.SetAccountPassword` and then
`credential.RevokeAll`. It writes the password hash and stops every credential
the account holds; it touches neither `AccountIdentityKey` nor
`AccountRecoveryWrap` (see [`../../fileserver/account/keys.go`](../../fileserver/account/keys.go)).
So an administrative reset hands over the *account* and not its content: the
wrapped identity key stays sealed under the old `wrapKey`, which was derived
from a password the server never saw, and only a recovery code opens it.

That is worth stating once here so it does not get relitigated every time a
capability is proposed. An administrator curates an install. They do not read
what is in it.

The bound has a visible edge, and it should be visible in the UI too: an
administrative password reset is destructive to key material, and
`warnAboutKeyMaterial` already says so at the CLI. The HTTP route and the page
say the same thing in the same words.

## The HTTP surface

None of it exists. Every administrative operation today is CLI-only, which
[`../roadmap.md`](../roadmap.md) calls out as independent work, and which is
right for the reasons `user_cmd.go` gives: the moment administration is most
needed is the one where nobody can log in, and anyone who can run the CLI
already owns the data directory.

The HTTP surface does not replace that. It is the same operations for the
ordinary case, built on the same `account` and `authmgr` functions rather than
beside them, so that a fix to a rule is a fix in one place.

```
GET    /api/silo/v1/admin/accounts              users
POST   /api/silo/v1/admin/accounts              users
POST   /api/silo/v1/admin/accounts/{id}/active  users
POST   /api/silo/v1/admin/accounts/{id}/password passwords
GET    /api/silo/v1/admin/accounts/{id}/quota   quota
PUT    /api/silo/v1/admin/accounts/{id}/quota   quota
PUT    /api/silo/v1/admin/accounts/{id}/role    grant
PUT    /api/silo/v1/admin/accounts/{id}/caps    grant
GET    /api/silo/v1/admin/libraries             users
GET    /api/silo/v1/admin/storage               retention
```

Gated by one middleware, `RequireAdmin`, which is `RequireCredential` plus the
conjunction from decision 3. It sits beside `CredentialCanWrite`, which exists
for the same shape of question — the handlers that have no library to ask
`share.CheckPerm` about. Server administration is the largest such handler: it
has no library, which is exactly why it does not belong in the grant model.

## The page, and what it can honestly show

The page is one `embed.FS` route off the binary. The panels are worth
enumerating now because three of the five things asked for are not measurable
today, and a page that renders a plausible number for them would be worse than
one that says so.

**Libraries.** `libraryInfo` already carries id, name, update time, encrypted,
head commit, size and file count. Two gaps: the listing is scoped to the caller
and carries no owner, and whether an admin implicitly sees every library is an
open product decision that [`../roadmap.md`](../roadmap.md) names. Answer it
before the endpoint, not in the handler.

**Encryption, and why the obvious rendering is a lie.** "Only locally encrypted
or client encrypted" is a false dichotomy in this server. At-rest sealing under
`storage.key` applies to every object regardless of E2EE, so a plain library is
not sitting in cleartext on disk. `Encrypted: true` means the library is
*additionally* end-to-end encrypted — the server holds ciphertext it cannot
open. Two independent facts, and the page shows them as two, because an
either/or control would tell the operator something untrue about the plain half
of their server.

**Size, and the two numbers that disagree on purpose.** `libraryInfo.Size` is
logical size at head. `objmgr.Census` — behind `silo df` — is stored bytes in
three parts: head, history, unreferenced. They will not match, they are not
meant to, and the page labels which is which rather than picking one.

**Maximum size does not exist.** There is no server ceiling anywhere and free
disk is never read. `quota.md` wants a refusal at the lower of a configured
ceiling and free-space-minus-reserve; until that lands the panel says "no limit
set" and shows free disk, which is at least a true number.

**Throughput is unmeasured.** There are no byte counters on the chunk upload or
fetch paths, and `middleware/logging.go` counts requests rather than bytes.
Sentry timings are sampled at 0.1 and live off-box. This needs new counters
before it needs a chart.

**Storage locations, and cache versus full copy.** Blocked behind durable tiers,
which is step 4 of the roadmap's storage chain, behind packs (#17) and
compaction (#19). There is no S3 configuration to display because there is no
S3. And the server does not know what a client holds: a device credential row
says a device enrolled, not what it cached. Cache-versus-copy is a client-
reported fact that nothing currently reports.

## Order of work

1. **`is_staff` becomes `role`.** Built. The flag went rather than gaining a
   neighbour; `SchemaVersion` 3. `silo user add -role admin|user|guest`, a ROLE
   column in the listing, and `role` in its JSON.
2. **`AccountCapability`, the closed vocabulary, and both `grant` invariants.**
   Built, in `fileserver/admin`. `setup.Claim` writes `role` and all six in its
   existing transaction; `admin.Can` is the conjunction and is the only
   spelling of it. `silo user grant` and `silo user revoke` take a
   comma-separated set, and the listing gained a CAPABILITIES column.

   The CLI is the install's own hand and has no actor to check: an operator
   holding the database could write the row directly, so asking them to prove
   authority would be a formality. It still honours the last-holder rule, which
   is about the install rather than the caller — and costs nothing, since
   handing `grant` to somebody else first is the thing the operator meant to
   do. `Grant` and `Revoke` are the actor-bearing pair the HTTP surface will
   use; `Assign` and `Withdraw` are the CLI's.

   **Revoking is checked the same way as granting.** The plan states the
   invariant for handing on; it is applied to taking away as well, because an
   account holding only `grant` must not be able to strip every other
   administrator of an authority it was never trusted with itself. That is not
   escalation, but it is an install left unable to do its own work by somebody
   who could never do it either.
3. **`RequireAdmin`**, and the accounts endpoints over the functions the CLI
   already calls.
4. **The page**, `embed.FS`, with the libraries and accounts panels — the two
   that are honestly answerable today.
5. **Free disk and a server ceiling**, which unblocks the size panel and is
   `quota.md`'s to own.
6. **Byte counters**, which unblocks throughput.
7. **Storage locations**, when durable tiers exist and there is something to
   name.

Steps 5 through 7 are each independently useful and none of them blocks the
page shipping without their panel. A panel that says "not measured yet" is a
correct panel.
