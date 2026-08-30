# Protocol gaps

What the Silo lane would still need to be an ideal protocol between a
Dropbox-shaped client and its server. [`protocol.md`](protocol.md) is the
record of what exists; this is only what does not, ranked, with the reasoning
attached so the ordering is not re-argued each time someone reads the endpoint
table and notices a hole.

Nothing here is a defect. Every item is a capability that was never built.

## The verdict

| | how close |
|---|---|
| Read, browse, on-demand file access — the File Provider / FUSE case | close. The coordination primitives are present, and several are better than what the same client gets from a commercial API |
| Full bidirectional sync of large libraries — the actual Dropbox case | close on coordination, and batched in both directions on bulk transfer. What remains is one library shape |

## Tier 1 — a real client will hit these

### 1. No delta for a file rewritten wholesale

Content-defined chunking ([`chunking.md`](chunking.md)) gives chunk-level
dedup: an edit in the middle of a large file re-transfers single-digit
megabytes. What it cannot give is a delta for a file whose bytes are rewritten
on every save — a VM image, a database file — because no boundary survives to
dedup against, and such a file still moves in full.

What would close it is rdiff semantics on this lane: a signature on the way
out, a delta on the way in, reconstructed server-side into the seekable spool
`PUT` already uses. It is a refinement of the chunk surface rather than a
substitute, and rsync's *algorithm* fits where rsync's *protocol* — defined by
the C implementation, with a whole file list before any bytes move — does not.
A wire delta is a bandwidth fix only; it never reduces what is stored.

How much it matters depends entirely on the library. Batching already removed
the per-chunk round trips (128 MiB went from 118 requests to 7), so what is
left for a delta to win is the transfer itself, on the one shape where CDC
finds nothing. Re-measure against a real VM-image workload before scheduling
it rather than inheriting the ranking from here.

### 2. No stable per-file identity

Identifiers never cross the wire. Every request is `(library_id, path)`;
identity across a move is reconstructed client-side from an `IdMap` plus the
rename ops the delta feed emits. A content-addressed store has no natural
inode to hand out, so this may well be the right trade — but it moves the
hardest invariant in a sync client, a file keeping its identity while its name
changes, onto every client that will ever be written, and it makes offline
move replay and conflict-copy naming harder for each of them. Dropbox shipped
`id:` identifiers after learning this. Worth deciding deliberately.

## Tier 2 — real, survivable

| gap | cost today | fix |
|---|---|---|
| **No search of any kind** | a client enumerates to find a name | designed in [`plans/search.md`](plans/search.md), parked |
| **A big directory is still read whole server-side** | paging bounds the response and the client's loop, not the read: dirents are one JSON object addressed by the hash of all of them, so a range of one cannot be read without changing what a directory *is*. A level up, a Merkle diff is proportional to what changed and cannot be resumed part-way, so `changes` recomputes per page | store-level, much larger than it sounds, and nobody is asking |
| **A plain write cannot carry the file's mtime** | `PUT entries/{path}` stamps the server's clock, so a tree uploaded to a plain library records every file as modified at upload time. The encrypted write path does not have this problem — the client builds the dirent — so the two halves of `client.LibraryFS` disagree on it | an mtime on the write, and the same on `?type=chunks`, the batch `create` op and `mkdir` |
| **No compression negotiation** | chunks are stored as `storage.md` § Compression describes; nothing is negotiated per connection | see [`storage.md`](storage.md#compression) |

## What this is not

Sharing, groups, multi-user permissions, file locking and federation are out
of scope here — they are product surface, not protocol shape, and they live in
[`roadmap.md`](roadmap.md). A protocol gap is something a single-user
Dropbox-shaped client would feel on day one.
