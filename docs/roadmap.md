# Roadmap

What is built, what is next, and what each thing waits on. This file owns
**ordering and dependency** and nothing else.

That restriction is the whole design of the document. Every item below is
described somewhere else in more detail, by a document that is normative for
it; what no other document held was the sequence, because a dependency lives
between two plans rather than inside either one. So each entry here is a
sentence of what and a link to who knows — and where this file and an owning
document disagree, **the owning document wins.**

The rule exists because this repository has already paid for the alternative
twice. [`plan.md`](plan.md) stated four constraints as binding and three
stopped holding without a word changing here; `porter-brief.md` described an
all-zeros empty-object sentinel for some time after typed objects gave empty
directories and zero-byte files distinct hashes. Both were restatements that
outlived the thing they restated. A roadmap that summarises a design will decay
the same way, so this one does not summarise designs.

## How to read the three lists

**The critical path** is where the ordering is forced by a dependency rather
than by preference — read it as a chain, and note where an entry says it can be
worked beside its neighbour rather than after it. **Independent work** is
blocked by nothing and can start on any morning. **Parked** is designed or
sketched, with nobody currently asking for it.

Nothing here carries a date, and nothing is a commitment. For the end state
these steps are walking towards, read [`target.md`](target.md) — that document
is the destination and this one is the route.

## What is built

| capability | owning document |
|---|---|
| The object store: keyed content-defined chunking, SHA-256 ids, four object kinds, inlining | [`storage.md`](storage.md) Part 1, bound by [`spec/store-format.md`](spec/store-format.md) |
| The wire: every endpoint, every status code, the delta feed, batch, the block and id-addressed surfaces | [`protocol.md`](protocol.md), [`responses.md`](responses.md), [`porter-brief.md`](porter-brief.md) |
| Credentials: one token format, one `Resolve`, scopes, permission ceilings, revocation, password enrolment | [`auth.md`](auth.md) Part 1 |
| Quota: enforcement at admission, per-owner serialization, `507`, and `silo user quota` | [`quota.md`](quota.md) |
| Reclaiming: deleted-library GC, orphan collection, history expiry, and the retention policy behind them | [`storage.md`](storage.md) § Reclaiming, [`quota.md`](quota.md) § Order of work |
| Push notifications over `/notification`, with the gap-after-reconnect contract | [`protocol.md`](protocol.md) |
| Backup and restore, and why `cp silo.db` is not one | [`backup.md`](backup.md) |
| Error reporting and issue grouping | [`error-reporting.md`](error-reporting.md) |
| The E2EE content format — codecs, name encryption, key wrapping, key-free readers, test vectors | [`spec/store-format.md`](spec/store-format.md), oriented by [`encryption.md`](encryption.md) |
| Claiming a fresh server: the setup token, `POST auth/setup`, `silo setup-token` | [`auth.md`](auth.md) § Claiming a server that has no accounts |
| The account side of E2EE — `client_kdf_params`, `AccountIdentityKey`, `AccountRecoveryWrap`, `POST auth/kdf`, and encrypted-library creation | [`auth.md`](auth.md) § The account's key material, [`storage.md`](storage.md) § What the server can read |

Three things are built and worth naming separately because they are easy to
assume are missing. The **changes feed works on a library the server cannot
read**, which is what makes E2EE a storage property rather than a feature with
holes in it. **`silo df` measures history against live data per library**,
which is the number every retention decision is argued from. And **an encrypted
library can now be created** — `POST /libraries` with `"e2ee": true`, behind
the `account-keys` and `e2ee-libraries` feature names — which was the item that
gated more of this document than anything else, and no longer gates any of it.

The clients are separate projects. The macOS File Provider client has landed
M0–M3 — enumeration, on-demand fetch, and change notification with a
per-library sync anchor — and its milestones and field notes are in
[`macos-fileprovider-plan.md`](macos-fileprovider-plan.md).

## The critical path

Each step is blocked by the one before it. The chain used to start with the
account side of E2EE; that landed, which unblocked both of the first two items
below and is why they can now be worked in either order.

### 1. Grants, roles, and invites

The grant table, `CheckPerm` unification across the three principals, the
`role` column, and invite redemption. Owned by
[`plans/sharing.md`](plans/sharing.md), step 1 of its build order.

**Now unblocked, and the largest available product step.** It depended on the
credential table and on the account side of E2EE, and both are built — invite
redemption is where E2EE bootstrap happens, the one moment a client is
guaranteed present to generate an identity keypair, and the schema it writes
into now exists. It is also what makes `is_staff` mean something, which is the
gate designed in [`plans/admin-check.md`](plans/admin-check.md) and consumed by
nothing today.

The event log lands alongside: [`plans/events.md`](plans/events.md) phase 1 can
start now, and its phase 2 must ship *with* the share surface rather than after
it, on that plan's rule that the share surface does not ship without its trail.

### 2. Split-derivation login

The client derives twice from one password: an auth key that goes up, a wrap
key that never leaves. Owned by [`storage.md`](storage.md) item 1 and
[`auth.md`](auth.md) item 2.

**Mostly a client change now.** The server already hashes whatever arrives, and
`POST auth/kdf` already serves the parameters it must arrive under. What is
left on the server is that switching an existing account over has to be atomic
— the new hash and the new `client_kdf_params` in one write — or the account is
left with parameters describing a password the stored hash was not made from.

Argon2id behind a concurrency semaphore is sequenced with it and is cheap to
defer, because once `AccountPassword.hash` is a hash of a 256-bit `authKey`
rather than of a password, it becomes a fast hash and the semaphore is moot.

### 3. Packs

The pack format, per-pack indexes, seal-on-size-or-age-or-shutdown,
`storage.key` and its backup wiring, the recovery scan, and the loose-store
ingest that scan doubles as. Owned by [`storage.md`](storage.md) § Packs.

Loose objects do not survive the measured workload — roughly six million chunks
at 6 TB — and every item below this one is a pack operation. The ingest path is
not optional: the first install will be running the loose store, which is how
the one migration this project claims not to need becomes the one it discovers
in production.

### 4. The tracing mark, and compaction

`PackStats`, the dead-fraction threshold, throttled rewrite, locality and
undersize as scheduling inputs, and two budgets because disk I/O and egress are
two bills. Owned by [`storage.md`](storage.md) § The tracing mark, and
[`chunking.md`](chunking.md) for how the decision to chunk small forced it.

**This is the same project as the per-library garbage collector.** Both need a
mark phase, chunk liveness is global reachability and cannot be refcounted, and
building them apart means building the mark twice.

### 5. Durable tiers

NAS and S3 against the four-verb floor, async upload of sealed packs,
verified-then-evictable local cache, and the tier attributes that make a
multi-tier deployment safe. Owned by [`storage.md`](storage.md) § Durable
tiers.

The background workers arrive here and bring their panic recovery and their
three error-level conditions with them.

### 6. Charging occupied blocks instead of logical size

Owned by [`quota.md`](quota.md), step 4 of its order of work, and deliberately
last: it is the step users feel, and it must not land until the space it makes
chargeable can also be released — which is step 5 above. A server-level ceiling
follows it at the same admission point.

## Independent work

Blocked by nothing above. Ordered by how much they cost against what they buy,
not by dependency.

**The admin HTTP surface.** Every *administrative* operation is CLI-only today
— `silo user`, `silo token`, `silo retention`, `silo user quota` — and a web UI
or a remote operator needs all of it over HTTP. These endpoints should call the
same `account` functions the CLI does rather than reimplement them beside it.
The gate they hang off is [`plans/admin-check.md`](plans/admin-check.md); the
roles they enforce are [`plans/sharing.md`](plans/sharing.md)'s; the quota
endpoints they expose are listed in [`quota.md`](quota.md). Two decisions are
still open and shape the result: whether an admin implicitly sees every library
(upstream conflates the two; admin-check says no), and soft versus hard delete
for an account.

Note what is *not* missing, because the gap is narrower than "no quota API"
suggests: `GET /account/usage` already answers the self lookup with both usage
and cap, and absent-means-no-ceiling is already a documented contract. What has
no HTTP surface is an admin reading or setting someone else's cap.

**Proof of possession.** Public keys registered at enrolment, RFC 9421
signatures on the Silo lane. [`auth.md`](auth.md) item 1, independent of OIDC —
whichever is wanted first.

**OIDC.** The device grant run by Silo *against* the IdP, so no browser surface
is added and a shipped client never speaks to the IdP at all. Login is
brokered, not federated: consulted once per enrolment, never once per request,
because a mounted filesystem issues requests at `ls` rate. [`auth.md`](auth.md)
item 5.

**The rest of history on the wire.** The commit log is built —
`GET /libraries/{id}/commits`, with `?at=` for browsing a snapshot, bounded by
the retention window and answering `410` past it. What is left is exposure
rather than storage work: **per-path history** — walk commits server-side and
return only those where that path's id changed — plus revert, a
deleted-but-reachable listing, and restore. [`quota.md`](quota.md) § The one
endpoint still missing argues per-path history is the piece that turns the
client's `.snapshot` directory from possible into usable, and it is cheap for
the server (compare ids, skip unchanged subtrees unread) and expensive for
everyone else, which is the whole case for it living here.

**Bulk transfer on the wire**, which is where the protocol is thinnest. One
request per block is still the shape — roughly 128 round trips for a 1 GB file
— and a framed multi-block `POST` going up with a `pack-blocks` response coming
down is the highest-value change available. Beneath it, no delta exists for an
edit in the middle of a file: rdiff semantics on this lane, a signature out and
a delta in, explicitly not an rsyncd. Both are ranked with their reasoning in
[`protocol-gaps.md`](protocol-gaps.md), which also raises the one decision that
gets harder to change later — whether files get a stable identity that survives
a move, or whether that stays reconstructed client-side forever.

**Small things that are each an afternoon.** A persistent JWT keyfile at mode
0600, so a restart stops disconnecting every watching client. A `server-info` feature name for the id-addressed surface, which is
undiscoverable today — `blocks` covers the chunk half and `objects/{id}` and
`PUT head` have no name. Structured logs behind a flag, expanded metrics
coverage, and a `/healthz` that actually pings the database handles.

**One scheduler nobody has written on purpose.** `expireHistoryByPolicy` takes
no window precisely so a scheduler can call it, and today an operator runs the
command or a cron does. An in-process timer wants a kill switch and an interval
before it wants code, because it would be the first thing in Silo that deletes
user data with nobody watching.

**One piece of debris.** Nothing creates a `VirtualLibrary` row while several
queries still join the table.

## Parked

Designed or sketched, with nobody currently asking. Each has a document because
the reasoning was worth keeping, not because it is scheduled.

| what | where | why it is parked |
|---|---|---|
| Filename search | [`plans/search.md`](plans/search.md) | motivated by the macOS search pushdown; no client is asking yet |
| File locking | [`plans/locking.md`](plans/locking.md) | wants the notification hook built with it, and no multi-writer deployment exists |
| Replication, and parity | [`plans/replication.md`](plans/replication.md) | the merge engine is already there; the missing piece is a merge base. Primary-plus-replicas is worth building alone, and needs none of it |
| A library as a distributable read-only store | [`plans/distributable-library.md`](plans/distributable-library.md) | the format already does the work; recorded so it is not drifted away from by accident |
| A web UI | — | not short-term. It is the one consumer that cannot set an `Authorization` header, which is the point at which signed URLs become worth building — [`capability-urls.md`](capability-urls.md) records what they should look like |
| Other protocol frontends — WebDAV, S3, SFTP | [`protocol-frontends.md`](protocol-frontends.md) | a survey, ranked, with what each costs |
| Compression | [`compression.md`](compression.md), and [`storage.md`](storage.md) § Compression for where it actually goes | measure first; it pays nothing on media |
| A master key, and S3 key derivation | [`auth.md`](auth.md) item 6 | gates only S3; defer until S3 is wanted |
| `silo convert` | [`storage.md`](storage.md) § Conversion is a named operation | needed first by public libraries, which are sharing's step 2 |

## Not being built

The non-goals live in [`target.md`](target.md) § Deliberately not, which is the
document that can actually settle one — a non-goal is a statement about the
destination rather than about the route. Four are worth restating here only
because they get proposed often: peer-to-peer federation (a mesh of untrusting
instances agreeing on shared state is consensus, and consensus is a different
project), a plugin system, LDAP and SAML (OIDC covers the ground with far less
surface to get wrong, and a reverse proxy doing header auth stays acceptable),
and mobile apps from this repository — a mobile client would be a Porter-family
project against `/api/silo/v1`, not a server feature.

Seafile wire compatibility is gone deliberately and `seafile-compat-end` is the
tag to revert to. There is no compat surface left to have gaps in; where the
Silo lane itself is thin, [`protocol-gaps.md`](protocol-gaps.md) is the list.
