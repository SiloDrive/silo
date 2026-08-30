# Roadmap

What is built, what is next, and what each thing waits on. This file owns
**ordering and dependency** and nothing else.

That restriction is the whole design of the document. Every item below is
described somewhere else in more detail, by a document that is normative for
it; what no other document holds is the sequence, because a dependency lives
between two plans rather than inside either one. So each entry here is a
sentence of what and a link to who knows — and where this file and an owning
document disagree, **the owning document wins.**

The rule exists because a restatement outlives the thing it restates. A
roadmap that summarises a design drifts from that design the moment the design
moves, without a word changing here, and nobody notices until someone builds
from the summary. So this one does not summarise designs.

## Where state lives

Work is tracked on git.booko.info, in `Silo/silo`'s
[milestones](https://git.booko.info/Silo/silo/milestones) and issues, with
porter's share in `Silo/porter-fuse` and `Silo/porter-macos`. **The issues
carry state; this document carries order and the reasoning for it.** Each
entry below names the milestone and issues that track it, and the document is
not updated as they close — an issue that reads closed here is the tracker's
business, not this file's.

## How to read the three lists

**The critical path** is where the ordering is forced by a dependency rather
than by preference — read it as chains, and note where an entry says it can be
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
| The wire: every endpoint, every status code, the delta feed, batch, the chunk and id-addressed surfaces, and the native-client contract with captured responses | [`protocol.md`](protocol.md), [`responses.md`](responses.md) |
| Credentials: one token format, one `Resolve`, scopes, permission ceilings, revocation, password enrolment | [`auth.md`](auth.md) Part 1 |
| Quota: enforcement at admission, per-owner serialization, `507`, and `silo user quota` | [`quota.md`](quota.md) |
| Reclaiming: deleted-library GC, orphan collection, history expiry, and the retention policy behind them | [`storage.md`](storage.md) § Reclaiming, [`quota.md`](quota.md) § Order of work |
| Push notifications over `/notification`, with the gap-after-reconnect contract | [`protocol.md`](protocol.md) |
| Backup and restore, and why `cp silo.db` is not one | [`backup.md`](backup.md) |
| Error reporting and issue grouping | [`error-reporting.md`](error-reporting.md) |
| The E2EE content format — codecs, name encryption, key wrapping, key-free readers, test vectors | [`spec/store-format.md`](spec/store-format.md), oriented by [`encryption.md`](encryption.md) |
| Claiming a fresh server: the setup token, `POST auth/setup`, `silo setup-token` | [`auth.md`](auth.md) § Claiming a server that has no accounts |
| The account side of E2EE — `client_kdf_params`, `AccountIdentityKey`, `AccountRecoveryWrap`, `POST auth/kdf`, and encrypted-library creation | [`auth.md`](auth.md) § The account's key material, [`storage.md`](storage.md) § What the server can read |
| The sealing client — key bootstrap from a password, library creation, the object-graph read, the spine rewrite, and one interface over both library types | [`protocol.md`](protocol.md#the-e2ee-client-shape), oriented by [`encryption.md`](encryption.md) |
| Split-derivation login, both halves — enrolment publishes an identity key and crosses the account over, and login sends a derived key rather than the password | [`auth.md`](auth.md) § Split-derivation login |

Three things are built and worth naming separately because they are easy to
assume are missing. The **changes feed works on a library the server cannot
read**, which is what makes E2EE a storage property rather than a feature with
holes in it. **`silo df` measures history against live data per library**,
which is the number every retention decision is argued from. And **an encrypted
library can be created** — `POST /libraries` with `"e2ee": true`, behind the
`account-keys` and `e2ee-libraries` feature names — which is what every E2EE
entry on the critical path stands on.

The clients are separate projects. The macOS File Provider client keeps its
plan and field notes in the porter-macos repository; the FUSE client keeps its
own in porter-fuse. Both consume [`protocol.md`](protocol.md).

## The critical path

Two chains, and they do not wait on each other. **The storage chain** runs
at-rest encryption → packs → compaction → durable tiers → occupied-block
charging; its head is built but for the ingest, and packs are unblocked. **The E2EE chain** has one
unblocked head — grants and invites — and one entry, sharing an encrypted
library, that waits on it. The sequence of the E2EE chain is owned by
[`plans/e2ee-completion.md`](plans/e2ee-completion.md); this file only places
it beside the storage chain.

### The storage chain

#### 1. At-rest encryption: the sealed frame and `storage.key`

The frame codec with vectors, key generation at first start with its backup
wiring and the one-time warning, and `objstore` reading and writing one frame
per loose object. Owned by [`storage.md`](storage.md) § At rest, sequenced by
[`plans/at-rest-encryption.md`](plans/at-rest-encryption.md).

**It landed ahead of packs, and it is built.** A frame is self-describing
whether the file holding it contains one or a thousand, so the loose store an
install is actually running got at-rest encryption without waiting for a
container format. Cannot-lose and cannot-rotate hold from the first frame
written, which is why the key's backup wiring was part of this step and not a
later one.

**No ingest of existing plaintext objects was written**, and none is planned.
This server has one install; a store from before framing is discarded and
rebuilt rather than migrated, which is the freedom the top of
[`storage.md`](storage.md) says expires the first time someone else runs this.
A missing key over a non-empty store is a refusal to start that says so.

#### 2. Packs

The pack format, per-pack indexes, seal-on-size-or-age-or-shutdown, the
recovery scan, and the loose-store ingest that scan doubles as — the frame
ingest above, pointed at a pack. Owned by [`storage.md`](storage.md) § Packs.

Loose objects do not survive the measured workload — roughly six million chunks
at 6 TB — and every item below this one is a pack operation. The ingest path is
not optional: the first install runs the loose store, and the migration this
project claims not to need is the one it would otherwise discover in
production.

Tracked: milestone `packs`, #17 — blocked by #18, which is done.

#### 3. The tracing mark, and compaction

`PackStats`, the dead-fraction threshold, throttled rewrite, locality and
undersize as scheduling inputs, and two budgets because disk I/O and egress are
two bills. Owned by [`storage.md`](storage.md) § The tracing mark, and
[`chunking.md`](chunking.md) for how the decision to chunk small forced it.

**This is the same project as the per-library garbage collector.** Both need a
mark phase, chunk liveness is global reachability and cannot be refcounted, and
building them apart means building the mark twice.

Tracked: milestone `compaction`, #19 — blocked by #17.

#### 4. Durable tiers

NAS and S3 against the four-verb floor, async upload of sealed packs,
verified-then-evictable local cache, and the tier attributes that make a
multi-tier deployment safe. Owned by [`storage.md`](storage.md) § Durable
tiers.

The background workers arrive here and bring their panic recovery and their
three error-level conditions with them.

Tracked: no milestone yet; follows `compaction`.

#### 5. Charging occupied blocks instead of logical size

Owned by [`quota.md`](quota.md), step 4 of its order of work, and deliberately
last: it is the step users feel, and it must not land until the space it makes
chargeable can also be released — which is step 4 above. A server-level ceiling
follows it at the same admission point.

Tracked: no milestone yet; follows durable tiers.

### The E2EE chain

#### 6. Grants, roles, and invites

The grant table, `CheckPerm` unification across the three principals, the
`role` column, and invite redemption. Owned by
[`plans/sharing.md`](plans/sharing.md), step 1 of its build order.

**Unblocked, and the largest available product step.** It stands on the credential table and on the account side of
E2EE, both built — invite redemption is where E2EE bootstrap happens, the one
moment a client is guaranteed present to generate an identity keypair, and the
schema it writes into exists. The `role` column has landed ahead of it; what
an `admin` may do is designed in [`plans/admin.md`](plans/admin.md) and
consumed by nothing today.

The event log lands alongside: [`plans/events.md`](plans/events.md) phase 1 can
start now, and its phase 2 must ship *with* the share surface rather than after
it, on that plan's rule that the share surface does not ship without its trail.

Tracked: milestone `grants-and-invites`, #14.

#### 7. Split-derivation login

The client derives twice from one password: an auth key that goes up, a wrap
key that never leaves. Owned by [`storage.md`](storage.md) § One password,
split client-side, and [`auth.md`](auth.md) item 2.

**The server half is built.** The atomic switch-over landed — hash and
`client_kdf_params` in one statement — along with the fast hash that names its
own format, the refusal of a change that would undo a crossover by omission,
and an operator reset that puts an account back on password login and says
what that costs its key material. Owned by [`auth.md`](auth.md) § Split-
derivation login, on this side.

**The client half is built.** `client.Enrol` publishes an identity key and
crosses the account over in one call; `client.OpenAccount` derives under the
parameters `auth/kdf` serves and logs in with the `authKey`. Argon2id behind a
concurrency semaphore is moot for a crossed-over account and still wanted for
the accounts that have not crossed.

An account crosses over when a client enrols it or changes its password, and no
server-side batch can do it: crossing over needs `master`, which the server does
not have. That is a property rather than a shortfall — an account created by
`POST auth/setup` has no client to enrol it yet, so password login has to keep
working, and the client tries the derived key first for exactly that reason.

This is the gate on *offering* an E2EE client to a person, not on building
one: item 8 can be built and tested before it and cannot ship before it.

Tracked: milestone `split-login`, #12 (server switch-over, done), #13 (client
`authKey`).

#### 8. The sealing client

Key bootstrap, `CreateEncryptedLibrary`, the read path, the write path that
rewrites the spine, one interface with a plain and an E2EE implementation, and
the round-trip test over HTTP that does not exist yet. Owned by
[`plans/e2ee-completion.md`](plans/e2ee-completion.md) step 1, with porter's
adoption of it as that plan's step 4.

**Built.** `client.LibraryFS` is the interface and `Account.Open` makes the
choice per library; the round trip runs against a real server, from a password
to a written tree to a second client that reads it back and a keyless view that
sees the shape and not the names. It was built before item 7 deliberately: it
gives the login change a consumer to test against, and `OpenAccount` is the one
function both touch.

**Recovery codes are minted here too, and that placement is the argument for
it.** `client.Enrol` wraps the identity private half once per code and returns
the set to its caller, in the same call that generates the key — the only
moment it can, since minting a set means holding the private half and after
enrolment returns nothing does. `client.Recover` takes a code and a password,
opens the identity, re-wraps it under fresh parameters and retires the spent
code in one publish. Built later it would have arrived to a population of
accounts holding no codes and needed a migration to give them some, which is
the shape of problem this project keeps declining to create.

What that makes survivable is the operator reset. `silo user passwd` cannot
re-wrap an identity key — re-wrapping means unwrapping and that needs the old
password — so it leaves the blob in place and unopenable and says so. A code is
the way back to it, and the two halves are deliberately in different hands: the
operator restores login and cannot open a blob, the code opens the blob and
cannot restore login.

Tracked: milestone `e2ee-client`, #6 (key bootstrap), #7
(`CreateEncryptedLibrary`), #8 (read path), #9 (write path), #10 (one
interface, two implementations), #11 (round-trip test), #32 (recovery codes).
Porter's side: porter-fuse #1, #2; porter-macos #1 — each an independent
implementation against [`spec/store-format.md`](spec/store-format.md), so each
reproduces the kind-2 wrap rather than inheriting it.

#### 9. Sharing an encrypted library

The sharer-written wrap row, and revocation as re-keying. Owned by
[`plans/e2ee-completion.md`](plans/e2ee-completion.md) step 3 and
[`plans/sharing.md`](plans/sharing.md).

**Blocked by 6 and 8.** The wrap row needs a grant to hang off, and a client
that can produce a wrap against a recipient's public key.

Tracked: milestone `sharing-e2ee`, #15 (sharer-written wrap), #16 (revoke).

## Independent work

Blocked by nothing above. Ordered by how much they cost against what they buy,
not by dependency.

**The admin HTTP surface.** Every *administrative* operation is CLI-only today
— `silo user`, `silo token`, `silo retention`, `silo user quota` — and a web UI
or a remote operator needs all of it over HTTP. These endpoints should call the
same `account` functions the CLI does rather than reimplement them beside it.
The gate they hang off, the capabilities they enforce and the routes
themselves are [`plans/admin.md`](plans/admin.md); the roles it stands on are
[`plans/sharing.md`](plans/sharing.md) § Accounts, and the quota endpoints it
exposes are listed in [`quota.md`](quota.md). Two decisions are still open and
shape the result: whether an admin implicitly sees every library, and soft
versus hard delete for an account.

Note what is *not* missing, because the gap is narrower than "no quota API"
suggests: `GET /account/usage` answers the self lookup with both usage and cap,
and absent-means-no-ceiling is a documented contract. What has no HTTP surface
is an admin reading or setting someone else's cap.

Tracked: no issue yet.

**Proof of possession.** Public keys registered at enrolment, RFC 9421
signatures on the Silo lane. [`auth.md`](auth.md) item 1, independent of OIDC —
whichever is wanted first. Tracked: no issue yet.

**OIDC.** The device grant run by Silo *against* the IdP, so no browser surface
is added and a shipped client never speaks to the IdP at all. Login is
brokered, not federated: consulted once per enrolment, never once per request,
because a mounted filesystem issues requests at `ls` rate. [`auth.md`](auth.md)
item 5. Tracked: no issue yet.

**The rest of history on the wire.** The commit log is built —
`GET /libraries/{id}/commits`, with `?at=` for browsing a snapshot, bounded by
the retention window and answering `410` past it. What is left is exposure
rather than storage work: **per-path history** — walk commits server-side and
return only those where that path's id changed — plus revert, a
deleted-but-reachable listing, and restore. [`quota.md`](quota.md) § The one
endpoint still missing argues per-path history is the piece that turns the
client's `.snapshot` directory from possible into usable, and it is cheap for
the server (compare ids, skip unchanged subtrees unread) and expensive for
everyone else, which is the whole case for it living here. Tracked: no issue
yet.

**Bulk transfer on the wire** is batched in both directions: `POST chunks/fetch`
takes up to 256 ids and answers with one framed body, and `POST chunks` takes
the same frames read rather than written, so a 1 GB file is a couple of dozen
round trips. What is left beneath it is the harder half: no delta exists for a
file whose bytes are rewritten wholesale — rdiff semantics on this lane, a
signature out and a delta in, explicitly not an rsyncd. It is ranked with its
reasoning in [`protocol-gaps.md`](protocol-gaps.md), which also raises the one
decision that gets harder to change later — whether files get a stable identity
that survives a move, or whether that stays reconstructed client-side forever.
Tracked: no issue yet.

**`objmgr` and `objstore` cleanups.** Small, separable, and each its own issue
rather than a project. Tracked: #24–#29, no milestone.

**Small things that are each an afternoon.** A persistent JWT keyfile at mode
0600, so a restart stops disconnecting every watching client. Structured logs
behind a flag, expanded metrics coverage, and a `/healthz` that actually pings
the database handles. A status table in the docs saying which encryption layer
is built, so the question is answered without reading the code — tracked as
#22, no milestone.

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
| Compression | [`storage.md`](storage.md) § Compression | measure first; it pays nothing on media |
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

Upstream wire compatibility is gone deliberately and `compat-end` is the
tag to revert to. There is no compat surface left to have gaps in; where the
Silo lane itself is thin, [`protocol-gaps.md`](protocol-gaps.md) is the list.
