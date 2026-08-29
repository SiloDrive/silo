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

The coordination layer is done, and bulk transfer is now negotiated in both
directions rather than only on the way up. What remains is narrower than it
was: one request per chunk *going up*, and no delta for a file whose bytes are
rewritten wholesale.

## What is already right

Stated because a gap list read on its own is misleading about the shape of the
thing.

- **`ETag` is the content hash.** A revalidation costs one dirent lookup and
  reads no chunks. Cheap revalidation is a sync client's largest single lever,
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
- **`GET /libraries` returns `head_commit_id` per library**, which quietly answers
  the account-wide-cursor problem: one call tells a client which of forty
  libraries moved.
- **Status codes are written as instructions to the client**, with `Retry-After`
  as the retryability tell and a registry that stops two handlers assigning the
  same code to opposite meanings.

## Tier 1 — a real client will hit these

### 1. No delta for an edit in the middle of a file

Resumable, dedup-aware upload landed — see the closed list below. What it did
not fix, and could not, is the shift problem.

> **Largely closed.** This section was written when Silo chunked at fixed 8 MiB
> offsets, so inserting a byte near the front of a file shifted every boundary
> after it and `chunks/missing` would report every chunk of an already-uploaded
> file as missing, each under a new name. Chunking is now
> `fastcdc-gear64/v1` (`store/params.go`) — content-defined, 256 KiB minimum,
> 1 MiB target, 4 MiB maximum — so boundaries re-sync within a chunk or two of
> an edit and a change in the middle of a large file re-transfers single-digit
> megabytes. [`chunking.md`](chunking.md) is the decision and the measurements.
>
> What is left is narrower than the heading, and is kept because it is real: CDC
> gives chunk-level dedup, not a delta. A file whose bytes are rewritten
> wholesale on every save — a VM image, a database file — still moves in full,
> because no boundary survives to dedup against. The rdiff analysis below is
> what would close that, and it is ranked against everything else in the tier
> list rather than assumed.

There were two ways out, and they were not equally priced.

> **Superseded.** The verdict below — prefer the wire delta, reject
> content-defined chunking — is no longer the position.
> [`chunking.md`](chunking.md) argues the other way on two facts not known when
> this was written: the wire delta is a bandwidth fix only and never reduces
> what is stored, and the cross-lane dedup being defended may already be lost.
> The analysis of rsync's algorithm below stands on its own merits and is worth
> keeping; the conclusion it feeds is not.

**Content-defined chunking** fixes it at the source and costs the store. New
boundaries mean new chunk ids, which means the same file carries two names, a
legacy-lane upload stops deduping against a Silo one, and both lanes share one
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
  old file streaming out of chunks and emits a delta.

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
way out, a delta on the way in — and not an rsyncd. It is a refinement of the
chunk surface, not a substitute, and the chunk surface was the right half to
build first: it is smaller, and it covers the traffic most clients actually
generate. And if what is wanted is rsync's ergonomics rather than its wire
efficiency, rclone over a WebDAV frontend supplies that with no protocol work at
all.

How much this matters depends entirely on the library. A photo and document
library is append-mostly and the chunk surface already handles it. A library of
VM images or database files re-uploads whole files on every edit, and no amount
of chunk negotiation helps.

### 2. One request per chunk, going up

> **Half closed.** The download side has it: `POST chunks/fetch` takes up to
> 256 ids and answers with one framed body, `store/chunkstream.go`. The heading
> used to read *still*, when neither direction had a batch. The upload side is
> what remains, and the framing to send is already written.

A 1 GB file is roughly a thousand `PUT`s, one per chunk: the chunker targets
1 MiB (`store/params.go`), so the count follows from the file size. That is the
right trade against an unresumable single request, and it is still a thousand
round trips. The same argument that produced `chunks/fetch` applies going up: a
`POST chunks` taking several chunks in one framed body — the same frames, read
rather than written — which is also where zstd-on-the-wire would earn its keep.

This paragraph said 128, a figure carried over from the 8 MiB fixed blocks this
surface started with. The correction makes the case stronger rather than weaker,
which is the reason to fix it rather than round it off: an eightfold undercount
of the round trips is an eightfold undercount of what batching buys.

Two things soften it, and neither removes it. Concurrent requests on one HTTP/2
connection recover much of the latency with no protocol work, so this is an
optimisation rather than a blocker — `sync-design.md` says as much. And a client
reading a file it has nothing cached for should not be on this surface at all:
`GET entries/{path}` is ranged and the server assembles, so a cold read is one
request. The chunk surface earns its keep when a client holds a previous version
and wants only what moved.

Commit batching is done — see the closed list — so what is left here is purely
the transfer, not the history.

### 3. No stable per-file identity

Identifiers never cross the wire. Every request is `(library_id, path)`; identity
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
| ~~**Encrypted libraries are unreadable over this lane, at all**~~ | **stale.** They read and write by id — `objects/{id}`, `chunks/{id}`, `chunks/fetch`, `PUT head` — and `entries/{path}` still routes on ciphertext names, so structure is by path and content is by id. What is refused, deliberately and permanently, is a path-addressed *write* and a ranged content read: the server cannot chunk what it cannot read | — |
| **No search of any kind** | designed in [`plans/search.md`](plans/search.md), parked | — |
| **A big directory is still read whole server-side** | paging bounds the response and the client's loop, not the read: dirents are one JSON object addressed by the hash of all of them, so a range of one cannot be read without changing what a directory *is*. The same holds a level up — a Merkle diff is proportional to what changed and cannot be resumed part-way, so `changes` recomputes per page | store-level, much larger than it sounds, and nobody is asking |
| **No compression negotiation** | chunks are raw on disk, so per-connection zstd is available for free and is not offered | see [`compression.md`](compression.md) |

## Closed since this list was written

Kept here rather than deleted, because a gap list is only trustworthy if it
records what came off it.

| | what changed |
|---|---|
| **Capability discovery** | `server-info` now returns a `features` array beside the version. Names are added when a capability ships and never removed or reused, so a client tests for a name instead of a version range. `notifications` is conditional on how the server was started |
| **`409` is no longer overloaded** | a GC conflict answers `503` with `Retry-After`, which is what it always meant — retry the identical request. `409` is now only a destination collision or an attempt to create the library root, and both want the same handling. (The legacy lane's own `409` usage, mentioned here originally, is moot — that lane was deleted in `5d4baa0`.) |
| **Copy** | `POST entries/{path}` with `{"op":"copy","to":…}`. The new dirent names the object the source already names, so a copy costs one dirent and one commit at any size and moves no content. `201` with the source's `ETag` |
| **Library rename on this lane** | `PATCH /libraries/{libraryid}` with `{"name":…}`. `PATCH` because the body names only what changes, so it keeps meaning the same thing when a second mutable field arrives. No more crossing to `/api2/` with a second credential to rename a library you can already create and delete |
| **`Accept-Ranges` tells the truth** | an encrypted library answers `Accept-Ranges: none` rather than advertising `bytes` and then ignoring `Range`. Ignoring a range is allowed; promising to honour one and then ignoring it is what breaks a client that seeks |
| **Upload integrity is now a contract** | `PUT` always returned the new id, and a client chunking at the same fixed 8 MiB offsets can compute that id itself — so comparing the two is a complete end-to-end check on the transfer. It was true and documented nowhere; `porter-brief.md` now says so |
| **Batching** | `POST libraries/{id}/batch` applies many operations as one commit: mkdir, delete, move, copy, and create from already-uploaded chunks. Ordered, so an operation sees the ones before it, and all-or-nothing, so a failure names the index that stopped it and writes nothing. Five hundred files dragged into a folder is one commit and one round of branch-head contention rather than five hundred of each. The tree operations were already the right shape — each takes a root id and returns a new one — so the change was threading that root through a list instead of committing after every step |
| **Pagination** | `?limit=N` on `changes` and on directory listings, with the next page in a `Link: …; rel="next"` header so the body shape did not change. Opt-in with no default, because a truncated answer that looks complete is worse than a large one. A cursor pins the commit or directory object the first page came from, so a sequence of pages is a consistent snapshot. On `changes` the anchor is absent until the last page, which makes "record it whenever you see it" the correct client behaviour rather than a rule to remember |
| **Resumable, dedup-aware upload** | the chunk surface: `POST chunks/missing`, `PUT chunks/{sha1}`, `PUT entries/{path}?type=chunks`. A client computes chunk ids itself — fixed offsets, SHA-1 of the bytes — so it can ask what the server holds before sending anything. Nothing exists at the destination until the last call, which is what makes an interrupted upload resumable with no session, offset or upload id to keep: ask again and the answer is shorter. `server-info` reports `block_size` so the chunking is not a guess. No more minting a sync token to reach `check-blocks` on the frozen lane |
| **The read half of the chunk surface** | `GET chunks/{id}` answers one chunk and `POST chunks/fetch` answers up to 256 in one framed body (`application/vnd.silo.chunks`, layout in `store/chunkstream.go`), and `GET entries/{path}?type=manifest` hands over a file's chunk list at a path a narrowed credential can reach. Together they close the asymmetry this list did not name for a long time: the write side had been able to ask "which of these do you hold?" and send only the answer since 0.4.5, while the read side had no equivalent, so a client editing one byte of a 1 GiB file uploaded a few chunks and downloaded the whole file to build them |
| **The id-addressed surface has a name** | `GET/PUT objects/{id}`, `GET chunks/{id}` and `PUT head` all shipped with store-v2 and none of them appeared in `features`. porter-fuse asked for two of them as if they were unbuilt, which is what an undiscoverable capability costs — it is not merely unused, it gets asked for again. The names are `objects`, `chunks-fetch` and `entries-manifest` |
| **`HEAD` is in the contract** | it was implemented, and in `porter-brief.md`, but missing from the endpoint table in [`protocol.md`](protocol.md) |

None of these are Tier 1. Nothing above changes the verdict: the coordination
half of this protocol is done, and the bulk-transfer half is not.

## What this is not

Sharing, groups, multi-user permissions, file locking and federation are out of
scope here — they are product surface, not protocol shape, and they live in
[`roadmap.md`](roadmap.md). A protocol gap is something a
single-user Dropbox-shaped client would feel on day one.
