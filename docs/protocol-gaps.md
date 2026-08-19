# Protocol gaps

What the Silo lane would need to be an ideal protocol between a Dropbox-shaped
client and its server, measured against what it is today.

[`protocol.md`](protocol.md) says which endpoints exist and
[`responses.md`](responses.md) says what the status codes promise. This document
says what is *missing*, ranked, with the reasoning attached so the ordering
doesn't get re-argued from scratch each time someone reads the endpoint table and
notices a hole.

Nothing here is a defect. Every item is a capability that was never built.

## The verdict, split in two

The question splits, and the two halves score very differently.

| | how close |
|---|---|
| Read, browse, on-demand file access — the File Provider / FUSE case | close. The coordination primitives are all present and several are better than what the same client would get from a commercial API |
| Full bidirectional sync of large libraries — the actual Dropbox case | not close. The distance is concentrated in bulk writes and in enumeration at scale |

The coordination layer is done. What is missing is everything about moving a
lot of bytes efficiently, and today the honest answer to most of it is "mint a
sync token and cross to the Seafile lane" — which is precisely the answer the
Silo lane exists to stop giving.

## What is already right

Stated because a gap list read on its own is misleading about the shape of the
thing.

- **`ETag` is the content hash.** A revalidation costs one dirent lookup and
  reads no blocks. Cheap revalidation is a sync client's largest single lever,
  and it falls out of the store for free. The `v1-` prefix versions the
  representation rather than the object, which is the part most implementations
  get wrong.
- **Conditional writes on every mutating method**, including `DELETE` and
  `POST {"op":"move"}` — the precondition there applies to the source. Lost
  updates are opt-out rather than inevitable; last-writer-wins has to be chosen.
- **A server-computed delta feed with an anchor**, and `410 Gone` as an
  instruction rather than an error. Renames are emitted by the server because
  the server holds both trees. This is the endpoint that lets a client be
  hundreds of lines instead of tens of thousands.
- **Push carries `commit_id`, and clients are told not to trust it as an
  anchor.** Push is a hint; the pull is the truth. Pairing them that way is
  correct and is written down rather than left to be discovered.
- **`GET /repos` returns `head_commit_id` per library**, which quietly answers
  the account-wide-cursor problem: one call tells a client which of forty
  libraries moved.
- **Status codes are written as instructions to the client**, with `Retry-After`
  as the retryability tell and a registry that stops two handlers assigning the
  same code to opposite meanings.

## Tier 1 — a real client will hit these

### 1. No resumable, dedup-aware upload

`PUT` on `entries` spools the entire request body to `httptemp` before indexing,
because `chunkFile` needs a seekable source. There is no chunk protocol, no
offset, no resume, and no way to ask the server which blocks it already holds.
Consequences, in order of severity:

- A large file over a flaky link never completes. There is no partial progress
  to resume from; every attempt starts at byte zero.
- Every concurrent upload costs one full copy of the file in temp space.
- Re-uploading a file that differs in one block transfers the whole file. The
  store is content-addressed and would store one new block, but the wire doesn't
  know that.

The workaround in [`native-client.md`](native-client.md) is to mint a repo sync
token and call `POST /repo/{id}/check-blocks` on the frozen Seafile lane. That
means two credentials and two protocol shapes to perform the single most common
operation a sync client performs.

The fix is small because the client can already compute block ids: chunking is
at fixed 8 MiB offsets, so any client with stdlib SHA-1 and a loop arrives at
the same names the server would. Roughly:

```
POST   /api/silo/v1/repos/{repo}/blocks/missing   {"blocks":[sha1,…]} → {"missing":[sha1,…]}
PUT    /api/silo/v1/repos/{repo}/blocks/{sha1}    raw bytes
PUT    /api/silo/v1/repos/{repo}/entries/{path}?type=blocks   {"blocks":[sha1,…]}
```

That is `check-blocks` re-spelled in this lane's idiom, and it buys resume,
dedup and parallel block upload in one move. Resume falls out for free: the
client re-asks `blocks/missing` and uploads what is still listed.

#### What block-level dedup will not fix, and what rsync has to say about it

Silo chunks at fixed offsets, so inserting a byte near the front of a file
shifts every boundary after it and nothing downstream dedups. Fixed chunking
wins on unchanged and append-only files and loses on edits in the middle — VM
images, databases, video projects. `blocks/missing` inherits that limit exactly.

There are two ways out, and they are not equally priced.

**Content-defined chunking** fixes it at the source and costs the store. New
boundaries mean new block ids, which means the same file carries two names, a
SeaDrive upload stops deduping against a Silo one, and both lanes share one
object store. That is the same argument `sync-design.md` makes about changing
the address hash, and it lands the same way: not a migration, a second store.

**A delta protocol on the wire** leaves the stored form untouched, which is the
"shared identity, per-lane encoding" rule again. rsync's algorithm — rolling
checksum over the basis file, then literal-and-copy instructions — is immune to
shifts by construction, and it lands on machinery that already exists here:

- **push**: the server sends signatures of its current version, the client sends
  a delta, the server reconstructs into a temp file, indexes, commits. The
  reconstruct-to-a-seekable-file step is what `PUT` already does — `spoolBody`
  exists because `chunkFile` needs a seekable source.
- **pull**: the client sends signatures, the server rolls a checksum over the
  old file streaming out of blocks and emits a delta.

Worth being precise about what is being borrowed. rsync's **algorithm** fits;
rsync's **protocol** does not. The protocol is defined by the C implementation
rather than a specification — version negotiation across protocol 27 and up,
multiplexed IO, and a whole file list transferred before any bytes move, which
is the part that hurts on a large tree. The Go implementations of the daemon
side serve files out of a filesystem, which Silo is not.

Nor is SSH the question it looks like. rsync's daemon transport is plain TCP on
port 873 with its own shared-secret challenge-response, so speaking rsync would
not require an SSH server. And having one would not help: rsync over a remote
shell simply runs `rsync --server` on the far side and speaks the same protocol
down the pipe, so the protocol still has to be implemented. SSH buys SFTP, which
[`protocol-frontends.md`](protocol-frontends.md) already ranks tier 1 for its
public-key auth. It does not buy rsync.

So the shape worth building is rdiff semantics on this lane — a signature on the
way out, a delta on the way in — and not an rsyncd. It is a refinement of
`blocks/missing`, not a substitute: build the block negotiation first, because it
is smaller and it covers the traffic most clients actually generate. And if what
is wanted is rsync's ergonomics rather than its wire efficiency, rclone over a
WebDAV frontend supplies that with no protocol work at all.

### 2. No pagination, anywhere

`ListDirByID` marshals an entire directory into one JSON array. `changes`
marshals an entire diff into one response — no `limit`, no cursor, no
`has_more`. A directory with two hundred thousand entries, or a client returning
after a month offline, produces one enormous body that the server materialises
in memory first.

The anchor already *is* a cursor; what it lacks is the ability to name an
intermediate commit and a flag saying there is more behind it. The commit DAG
supplies intermediate points already, so the shape is:

```
GET /repos/{repo}/changes?since={commit}&limit=1000
→ {"anchor":"<intermediate or head>", "changes":[…], "has_more":true}
```

with the contract being that a client loops until `has_more` is false, and that
each returned anchor is safely resumable. Directory listings want the same
treatment with an opaque cursor over the sorted dirent list.

### 3. No batching, so one commit per file

Five hundred files dragged into a folder is five hundred requests, five hundred
`GenNewCommit` calls, five hundred rounds of branch-head contention, and a
history five hundred commits deep for one drag-and-drop.
[`protocol-frontends.md`](protocol-frontends.md) already names commit coalescing
as a decide-before-the-first-frontend problem; it applies to `entries` today,
not only to a hypothetical WebDAV.

Minimum viable is a multi-operation `POST` at the repo level that lands as one
commit — creates, deletes and moves in a single ordered list.

### 4. No stable per-file identity

Identifiers never cross the wire. Every request is `(repo_id, path)`; identity
across a move is reconstructed client-side from an `IdMap` plus the rename ops
the delta feed emits.

This works, and a content-addressed store has no natural inode to hand out, so
it may well be the right trade. But it is a trade: it moves the hardest
invariant in a sync client — a file keeping its identity while its name changes
— onto the client, and it makes offline move replay and conflict-copy naming
harder for every client that will ever be written. Dropbox shipped `id:`
identifiers after learning this. Worth deciding deliberately rather than by
default.

## Tier 2 — real, survivable, mostly cheap

| gap | cost today | fix |
|---|---|---|
| **No quota or account surface** | a client cannot say "you are out of space" until a write fails, and then only on the other lane | `GET /account` with usage and total; `507` is already reserved for the write failure |
| **No trash, restore or history** | deletion is unrecoverable from a client's point of view, and it is the capability users most associate with the word Dropbox | the data is already there — immutable commits and no GC |
| **`POST {"op":"move"}` is not idempotent** | a move whose response is lost `404`s on retry, or moves the wrong thing if something was created at the source meanwhile | recommend `If-Match` on the source in the brief; consider an idempotency key |
| **24h JWT, no refresh** | the client must keep the account password to survive expiry, and there is no server-side device revocation | the device-grant design in [`auth.md`](auth.md) gives revocable per-device credentials as a side effect |
| **Encrypted libraries are unreadable over this lane, at all** | a whole class of library the client can only grey out | see [`encryption.md`](encryption.md) — the format is being replaced, not patched |
| **No search of any kind** | acknowledged in [`future-features.md`](future-features.md) | — |
| **No compression negotiation** | blocks are raw on disk, so per-connection zstd is available for free and is not offered | see [`compression.md`](compression.md) |

## Closed since this list was written

Kept here rather than deleted, because a gap list is only trustworthy if it
records what came off it.

| | what changed |
|---|---|
| **Capability discovery** | `server-info` now returns a `features` array beside the version. Names are added when a capability ships and never removed or reused, so a client tests for a name instead of a version range. `notifications` is conditional on how the server was started |
| **`409` is no longer overloaded** | a GC conflict answers `503` with `Retry-After`, which is what it always meant — retry the identical request. `409` is now only a destination collision or an attempt to create the library root, and both want the same handling. The Seafile lane keeps its own `409`, which is upstream's contract |
| **Copy** | `POST entries/{path}` with `{"op":"copy","to":…}`. The new dirent names the object the source already names, so a copy costs one dirent and one commit at any size and moves no content. `201` with the source's `ETag` |
| **Library rename on this lane** | `PATCH /repos/{repoid}` with `{"name":…}`. `PATCH` because the body names only what changes, so it keeps meaning the same thing when a second mutable field arrives. No more crossing to `/api2/` with a second credential to rename a library you can already create and delete |
| **`Accept-Ranges` tells the truth** | an encrypted library answers `Accept-Ranges: none` rather than advertising `bytes` and then ignoring `Range`. Ignoring a range is allowed; promising to honour one and then ignoring it is what breaks a client that seeks |
| **Upload integrity is now a contract** | `PUT` always returned the new id, and a client chunking at the same fixed 8 MiB offsets can compute that id itself — so comparing the two is a complete end-to-end check on the transfer. It was true and documented nowhere; `porter-brief.md` now says so |
| **`HEAD` is in the contract** | it was implemented, and in `porter-brief.md`, but missing from the endpoint table in [`protocol.md`](protocol.md) |

None of these are Tier 1. Nothing above changes the verdict: the coordination
half of this protocol is done, and the bulk-transfer half is not.

## What this is not

Sharing, groups, multi-user permissions, file locking and federation are out of
scope here — they are product surface, not protocol shape, and they live in
[`future-features.md`](future-features.md). A protocol gap is something a
single-user Dropbox-shaped client would feel on day one.
