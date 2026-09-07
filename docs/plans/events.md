# Plan: events — an append-only log for everything the schema forgets

## The premise

The database records what is true and destroys every record of how it got
there. The pattern repeats across the schema: `LibraryInfo.update_time` is a
last-write-wins stamp, and the inherited `LibrarySyncError` table kept exactly
one error per token — the latest — with a PRIMARY KEY that guaranteed the one
before it was gone. (That table, the `LibraryTokenPeerInfo.sync_time` stamp,
and the per-folder-permission and per-file-lock timestamp tables beside them
are gone from `fileserver/dbutil/schema.go`; the pattern is what matters, not
any particular table.) `Credential.last_used` continues it: a single lossy stamp
standing in for a history.

The question an operator actually asks is never "when was this last
touched". It is *"what did this credential reach before I revoked it"*,
*"who had access to this library in March"*, *"why did this file revert"* —
and today every one of those answers is unrecoverable in principle, not
merely unindexed.

One history in the system is already kept properly: the commit DAG.
Append-only, hash-linked, sealed under E2EE. That is not a coincidence to
admire; it is the boundary that scopes this plan.

## Decisions

- **The log records the control plane and access, never content.** "File X
  changed at T" is derivable from commits, and the store's rule — *store
  what cannot be derived, advise what can* — applies to the server's own
  tables too. A content event in this log would be the sealed-name-list
  mistake at server scale: two encodings of one history, drifting, with
  the log guaranteed to lose (it is advisory; the DAG is sealed).
- **The log is history, never authority.** Authorization reads the tables;
  the log records that they changed. Nothing ever replays the log to
  decide access — the moment something does, there are two sources of
  truth and the reconciliation bug is scheduled, not hypothetical.
- **The log records what the server already sees, never more.** Under E2EE
  this is automatic — paths in events are SIV ciphertext, names do not
  exist server-side. In plain libraries it means the log widens *when*
  things were seen, not *what* is visible. No new confidentiality surface
  in either library type.
- **The actor is a credential id, not an email.** The Credential table
  made every action attributable to a specific secret on a specific
  device. An email answers "who"; a credential id answers "who, from
  where, holding what" — and it is the thing revocation actually acts on.
  The email is one join away and can change; the credential id is forever.
  The same rule reaches into payloads: grantees, targets, and owners are
  **account ids, never emails** — which is why this plan sequences after
  the identity split. An append-only, chained log must not be born
  recording identities about to be re-keyed; every pre-split payload
  would carry an email as a permanent legacy alias, immutable by design.
- **Two event classes, because retention and tamper-evidence want opposite
  things.** `audit` events (the control plane) are small, kept forever,
  and hash-chained. `trace` events (access, operations) are the volume,
  pruned on a schedule, and unchained — a chain over prunable rows breaks
  the first time retention runs.

## Taxonomy

Event names are `noun.verb`, lowercase, dotted. The set is closed per
release — an unknown name in the table is a bug, not an extension point.

| Event | Class | Payload (JSON) |
|---|---|---|
| `library.created` / `library.deleted` / `library.converted` | audit | library_id, name¹, e2ee flag |
| `member.granted` / `member.changed` / `member.revoked` | audit | library_id, grantee, permission, grantee public key³ |
| `credential.created` / `credential.revoked` / `credential.expired` | audit | credential id, kind, label, scope² |
| `share.created` / `share.revoked` | audit | share id, flavor, library_id |
| `quota.changed`, `user.deactivated`, `user.reactivated` | audit | before/after, target |
| `credential.used` | trace | credential id, remote addr — rate-limited, see below |
| `share.opened` | trace | share id, remote addr, user agent |
| `gc.completed` | trace | library_id, gc_id, bytes reclaimed, duration |
| `sync.error` | trace | token, error — what `LibrarySyncError` kept one of |

¹ Library names are server-visible metadata in both library types today.
² A scope in an E2EE library is ciphertext here exactly as it is in the
Credential row — the log stores the stored form, unreadable and honest.
³ The grantee's X25519 identity public key, recorded because the log is the
only thing that can vouch for it. A content key is wrapped *to* that key, and
the wrap binds it — so a server that substitutes its own key at share time gets
a wrap the sharer built correctly for the wrong recipient, and the binding
defends the attack perfectly. Nothing in the wrapping can close that; it is key
distribution, not key wrapping. Carrying the key in the audit payload makes a
substitution **evident** to a client that pins the chain head: a different key
for the same member in a chain already pinned is a server rewriting history.
Tamper-evident, not tamper-proof — detection is after the fact, and out-of-band
fingerprint comparison remains the thing that closes it outright. See
[`spec/store-format.md`](../spec/store-format.md) § What the wraps do not vouch
for.

`credential.used` is rate-limited **per credential id, one event per
hour**, because a mounted filesystem authenticates at the rate of `stat`,
not the rate of logins, and a per-request INSERT is how the log becomes
the hot path. `Credential.last_used` updates on the same cadence — one
write, two artifacts, and the column becomes what it should have been from
the start: a derived cache of the log's most recent `credential.used` row.
`share.opened` is *not* rate-limited: anonymous access is exactly where a
complete trail earns its storage.

Plain-library content reads are never logged. That is a policy line, not
an oversight: logging them re-creates the access-pattern surveillance the
threat model attributes to a *hostile* server, in first-party code.

## Shape

```sql
CREATE TABLE IF NOT EXISTS EventLog (
  seq        INTEGER PRIMARY KEY AUTOINCREMENT,
  ts         INTEGER NOT NULL,          -- unix seconds, UTC; seq orders within a second
  class      TEXT    NOT NULL,          -- 'audit' | 'trace'
  event      TEXT    NOT NULL,          -- the closed taxonomy above
  actor      TEXT,                      -- credential id; NULL for the server itself (gc, expiry)
  library_id    CHAR(37),                  -- NULL for account-level events
  payload    TEXT    NOT NULL,          -- JSON; stored bytes are the canonical bytes
  chain_hash BLOB                       -- audit rows only; see The chain
);
CREATE INDEX IF NOT EXISTS eventlog_library_idx  ON EventLog (library_id, seq);
CREATE INDEX IF NOT EXISTS eventlog_actor_idx ON EventLog (actor, seq);
```

- **AUTOINCREMENT is load-bearing, not style.** Retention deletes rows,
  and without AUTOINCREMENT SQLite may reuse the top rowid after the
  highest row is deleted or a backup is restored. A reused `seq` forks the
  chain and lies to every cursor. This is the one table in the schema
  where the keyword earns its cost.
- **Append-only by construction**: the `eventlog` package exports
  `Append` and readers, and no UPDATE or DELETE statement exists outside
  the retention sweeper — which touches `trace` rows only, by `class`,
  enforced in the one place deletes are written.
- One writer goroutine, batched inserts. SQLite has a single writer
  anyway; the log should queue behind it deliberately rather than
  contending accidentally. `Append` is fire-and-forget from request
  handlers — a dropped trace event under load is a shrug, a blocked
  request handler is an incident. `audit` appends are the exception:
  they commit in the same transaction as the table change they record,
  because an unrecorded revocation is precisely what the log exists to
  prevent.

## Retention

- `audit`: forever. These rows are small (a deployment that revokes a
  thousand credentials a year writes kilobytes), and they are the audit
  trail — pruning them is deleting the product.
- `trace`: 90 days by default, per-deployment configurable, swept by
  `class` on a timer. The sweeper is the only DELETE in the package.

## The chain

Every `audit` row carries
`chain_hash = SHA-256(prev_chain_hash ‖ seq ‖ ts ‖ event ‖ actor ‖ library_id ‖ payload)`
over the **stored bytes, verbatim** — canonicalisation-by-storage, no
re-serialization to disagree about. The hash must exist before the row
does, and `seq` is what the insert produces — the manifest circularity
from the store format's first review, one layer up — so **the writer supplies
`seq` explicitly in the INSERT**: the single writer goroutine reads
`sqlite_sequence` and allocates the next value itself. AUTOINCREMENT's
no-reuse guarantee survives (an explicit value above the stored sequence
advances it), the append-only rule survives (no UPDATE patches a hash in
afterwards), and the chain still binds position explicitly rather than
leaning on `prev` alone. Fields are joined by `0x00`; NULL is encoded as
the single byte `0xFF` — impossible in UTF-8, hence unambiguous — never
as empty. Library ids and credential ids are non-empty in practice, but
"NULL and empty hash alike" is the canonicalisation class the format work
spent nine rounds exterminating, and the marker byte costs nothing.
Genesis is `prev = SHA-256("silo/events/v1")`, domain-separated
like every other constant in this system. The head `(seq, chain_hash)` is
served on an authenticated endpoint.

What this buys is a threat-model sentence. Rollback is currently
*accepted*: a server can serve an older but internally consistent state
and no client can tell. With the chain, **control-plane rollback becomes
evident**: un-revoking a credential or resurrecting a membership by
restoring last week's database means presenting a chain head that does not
extend the one a client already pinned. A client stores its last-seen
`(seq, head)` and, on reconnect, asks for the audit events since `seq` and
recomputes — cheap, offline-verifiable, and the server cannot fake it
without a second preimage.

What it does not buy: fork *consistency*. A server can serve each client
its own consistent chain, and detecting that requires clients to exchange
heads out of band. There is a beautiful hook for this already reserved —
a client could carry its last-seen head inside a commit's sealed section,
which a malicious server can neither forge nor strip, and the store format's
reserved flags bits with their reject-if-set rule are exactly the
deployment mechanism. That is a **named future occupant, not a proposal to
reopen the format**: it goes in the same queue as per-entry xattrs and
waits for evidence anyone needs it.

## Wire

- `GET /events?since=<seq>` — authenticated, filtered by what the
  credential's scope can see (an account-scoped credential sees its
  account's events; an admin sees everything). Cursor is `seq`; the response
  is rows, newest last, bounded page size.
- `GET /events/head` — the audit chain head, for pinning.
- **Later, explicitly not phase 1**: a long-poll/SSE tail as silo-drive's
  wake-up channel — "head moved on a library you can see" instead of
  per-library polling. The tail is a *notification*, not truth: the client
  wakes and syncs from the DAG exactly as it would have. Deferred until
  silo-drive's polling cost is measured and hurts.

## Build order

1. **Table + `eventlog` package + audit events**, landing **after the
   identity split** (auth.md's ordering, which this plan reinforces
   rather than displaces — and which has now landed): the log's first
   words are `account_id`. The
   credential lane is the first writer (`credential.created` at mint,
   `credential.used` rate-limited in `Resolve`, `last_used` as derived
   cache, `credential.revoked` at revocation). Control-plane events for
   libraries and membership as their handlers are touched.
2. **Trace events + the retention sweeper.** `share.opened` lands with
   sharing's phase 1 — the share surface should not ship without its
   trail. `sync.error` starts writing — there is nothing to retire, since
   `LibrarySyncError` has already been dropped.
3. **The chain + `/events/head` + client pinning.** silo-drive pins
   `(seq, head)` in its local index and verifies on reconnect. The
   threat-model paragraph in [`storage.md`](../storage.md) gains its clause.
4. **The tail as wake-up channel** — if and when measurement says
   per-library polling is the pain.

## What not to build

- **Event sourcing.** The tables are the state; the log is the history.
  No replay, no projections, no "the log is the database". Silo does not
  need the consistency machinery and should not pay its complexity.
- **Log-driven authorization.** No code path consults the log to answer
  "may this request proceed". Ever.
- **Content events.** The commit DAG is the content log. `changes?since=`
  is untouched by this plan.
- **An external queue.** No Kafka, no NATS, no Redis stream. One SQLite
  table in the database that already exists, or the operational story of
  "one process, one database" dies for a feature that writes kilobytes.
- **Webhooks.** Not until a consumer exists. The table makes them cheap to
  add later; speculative delivery infrastructure makes everything more
  expensive now.
- **Logging plain-library reads.** Stated above; restated here because it
  will be proposed. The answer is the threat model.

## Doc changes

- `../storage.md` — threat model: when phase 3 lands, the
  rollback sentence gains "control-plane rollback (revocations,
  membership) is *evident* to clients that pin the audit chain head;
  content rollback remains accepted." The reserved-bits rule's candidate
  list gains the sealed commit-head hook beside xattrs.
- `docs/spec/store-format.md` — already written against this plan: the Key
  wrapping section names `member.granted`'s public key field as the answer to
  public-key substitution at share time. The field is specified here; the
  format ships without it and gains the property when this plan lands.
- `docs/auth.md` — `last_used` is documented as a derived cache of
  `credential.used`, and the revocation section points at the log as the
  answer to "what did it touch first".
- `docs/plans/sharing.md` — share phase 1 acceptance includes
  `share.created` / `share.opened` / `share.revoked` events.
- `docs/roadmap.md` — activity feed and webhooks move from
  hand-waving to "read the EventLog table", with this plan as the
  reference.
