# Sync design notes

Reasoning and constraints behind Silo's storage and sync protocol. Not a spec —
the endpoints are in the code and in
[`macos-fileprovider-plan.md`](macos-fileprovider-plan.md). This is the *why*,
written down so the same ground doesn't get re-argued.

## Two lanes, one store

Silo serves two kinds of client, and only one of them is ours to change.

| Surface | Auth | Owner |
|---|---|---|
| `/repo/…`, `/seafhttp/repo/…` | `Seafile-Repo-Token` | upstream — frozen |
| `/api2/…`, `/api/v2.1/…` | `Authorization: Token` | upstream — frozen |
| `/api/silo/v1/…` | `Authorization: Bearer` (JWT) | ours |

The frozen lanes exist because Silo's reason for being is that unmodified
Seafile and SeaDrive clients keep working. They are never extended and never
"improved" — a change there is a compatibility break with software we do not
ship.

Everything new goes in the Silo lane. It is not versioned against Seahub's
numbering: `/api/v3` would read as the successor to `/api/v2.1`, which is
exactly the confusion the `silo` path segment exists to prevent. Version
numbers only need to be unique within a namespace.

**Both lanes share one object store.** That is the constraint that decides most
of what follows.

## SHA-1 is the address, not a checksum

A block's name *is* the SHA-1 of its bytes — `/repo/{repoid}/block/{id}` where
`{id}` is 40 hex characters, which is also its path on disk. Three properties
fall out of that and are not separable from it:

- **Dedup** — identical content anywhere hashes to one id, so it is stored once
- **Integrity** — fetch a block, hash it, and you have proof you got what you
  asked for. This is what `VerifyClientBlocks` and `VerifyFSObjectHashes` do
- **Cheap diffing** — the tree is Merkle. A dir object lists child ids, a commit
  points at a root. One changed byte changes the block id, so the dir id, so the
  root. `diff.DiffCommitRoots` compares two whole trees in time proportional to
  what changed, by stopping wherever two ids are equal

### Why it stays

Changing the address hash is not a migration, it is a second store. The same
file would carry two ids, be stored twice, and dedup would break *across* lanes
— a SeaDrive upload would be invisible to a Silo-native client. That cost is
enormous and the benefit is theoretical.

It also isn't a performance problem. Measured on one 8MB block (the default
`FixedBlockSize`):

| | throughput | per 8MB block |
|---|---|---|
| SHA-1 | ~2.1 GB/s | 3.8 ms |
| SHA-256 | ~2.2 GB/s | 3.6 ms |
| BLAKE3 | ~3–7 GB/s on x86 (SIMD) | — |
| XXH3-64 | ~13 GB/s | 0.6 ms |

Measured on Apple Silicon, where SHA-1/SHA-256 get ARM crypto instructions and
`zeebo/blake3` falls back to pure Go — it only ships `amd64` assembly, so its
number there is a library artifact, not a property of BLAKE3. The ordering to
carry away is xxHash ≫ BLAKE3 > SHA-1, roughly an order of magnitude at each
step.

But nothing in the server is hash-bound:

| Path | Hash cost | Real limit |
|---|---|---|
| Ingest | ~2 GB/s | gigabit ≈ 0.125 GB/s — 16× headroom |
| Scrub / GC | ~2 GB/s | spinning disk ≈ 0.15 GB/s — 13× headroom |
| Serving | none | blocks are stored raw and served raw |

Every path is dominated by something an order of magnitude slower. A faster
address hash optimises nothing and costs the store.

If the collision property matters — SHA-1 is broken for collision resistance,
and in content-addressed storage a crafted collision substitutes content —
hardened SHA-1 (sha1dc) at ingest is the cheap mitigation: identical ids,
identical wire format, identical layout. BLAKE3 is the right answer only for a
store built from scratch.

### Where xxHash belongs

Client-side, in the sync client's local index: `path → (size, mtime, xxh64,
sha1)`. A sync client's expensive hashing is the *scan* — "what changed since
last time?" — over the whole library, on every wake. xxh64 is the cheap gate;
SHA-1 gets computed only for what the gate says actually changed.

That hash never crosses the wire and has no adversary, so it needs no server
support at all. Which is why the server does **not** compute or accept a second
hash: it would buy nothing the client can't have for free, while adding a
permanent "which namespace is this id in?" ambiguity to the most load-bearing
concept in the design.

## Compression is a wire encoding, not a format

Where compression sits today:

- **Blocks are raw.** `blockmgr` imports no compression at all — stored raw,
  served raw
- **fs and commit objects are zlib**, and are stored *in the exact compressed
  form they arrived in* (`WriteRawIngested` → `WriteRaw`), so they can be handed
  back to Seafile clients byte-for-byte with no transcode

That pass-through is why the server is cheap, and it is why changing the storage
codec is not free: every fs object served to a legacy client would need
re-compressing.

So zstd goes on the wire, not on disk. Because blocks are raw on disk, the Silo
lane can zstd them per-connection with zero storage change and zero impact on
SeaDrive — a negotiated `Content-Encoding`, not a format change.

The rule: **shared identity, per-lane encoding.** Ids and canonical bytes are
common to both lanes; compression and framing are per-protocol. Negotiate how
bytes travel; never negotiate what they are called.

Worth measuring before committing: a library of JPEGs and MP4s is already
compressed, and zstd will spend CPU to save nothing.

## The change that actually makes sync fast

Not codecs — **batching**. Today there is one HTTP request per block, so a 1GB
file costs ~128 round trips. `pack-fs` batches fs objects; there is no
equivalent for blocks.

A `pack-blocks` that streams N blocks in one response is the highest-value
change available: additive, invisible to SeaDrive, and it dwarfs any compression
win. It is also what zstd-on-the-wire should apply to.

This is an optimisation, not a prerequisite. A new client can speak today's sync
protocol and negotiate `pack-blocks` later.

## Delta endpoint

`GET /api/silo/v1/repos/{repoid}/changes?since={commit}` returns everything that
differs between a commit and the current head, plus the new anchor.

It exists so a sync client doesn't have to replicate the object store to learn
what moved. The server already holds both trees and already has the diff;
SeaDrive carries tens of thousands of lines to answer this question without
server cooperation, and we have server cooperation.

An unresolvable `since` is `410 Gone`, not an error — the commit isn't wrong, it
is merely unreachable (garbage collected, or the library was reset), and the
caller's recovery is to enumerate from scratch. That's a different instruction
from "retry", so it gets a different status.

Renames are emitted server-side rather than inferred client-side, because the
server has both trees in hand. The inference is imperfect either way — deleting
one file and creating another with identical bytes looks like a rename — but the
failure mode is benign: an identifier follows the "wrong" copy of byte-identical
content.

### What a client has to know

Two details are not obvious from the response shape:

- **The first anchor comes from the library listing.** `GET /api/silo/v1/repos`
  returns `head_commit_id` per library. Without it a client's opening move is to
  enumerate a library with no way to name the state it just enumerated, so its
  first delta call has nothing to pass as `since`.
- **Apply with `mkdir -p` semantics.** A directory is reported in its own right
  only when it is *empty*. One that arrives with content appears solely as the
  paths inside it, because the diff walks to where the trees actually differ.
  The same holds in reverse for deleting a non-empty directory.

## Resolved: the shape of the Silo lane

`/api/silo/v1` grew endpoint by endpoint and was not internally consistent:
verbs in paths (`mkdir`, `rename`, `move`, `download`) mixed with nouns
(`repos`, `dir`, `file`); the same resource under two names (`file` for DELETE,
`download` for GET); the path passed as `?path=`, in a JSON body, or not at all;
a trailing slash on `dir/` and nowhere else.

The consistent shape is one addressable noun with HTTP methods as the verbs,
and that is what `entries` now is:

```
GET    /api/silo/v1/repos/{repo}/entries/{path}   dir → listing, file → bytes
HEAD   /api/silo/v1/repos/{repo}/entries/{path}   metadata only
PUT    /api/silo/v1/repos/{repo}/entries/{path}   create dir (?type=dir)
DELETE /api/silo/v1/repos/{repo}/entries/{path}
POST   /api/silo/v1/repos/{repo}/entries/{path}   {"op":"move","to":"/x/y"}
```

Rename disappears — renaming is moving. Files and directories share a noun
because in a content-addressed tree they are the same kind of thing, and a
client shouldn't have to know which before it asks.

GET sets an `ETag` of `"v1-{id}"` and honours `If-None-Match` with a 304. The
id is the object's content hash, so it is already a strong validator, and
checking one costs a single dirent lookup in the parent directory — no blocks
are read. The `v1-` prefix versions the *representation*: without it, changing
the listing JSON would leave clients holding cache entries that validate
against a body shape that no longer exists.

**`PUT` of file content landed in 0.4.4.** The body is the content; there is no
multipart wrapper and no access token to mint first. It costs a spool to
`httptemp` on the way in, because `chunkFile` needs a seekable source, which is
why it is the small-file path rather than the only one.

**The block surface is the large-file path**, and it is the sync lane's
`check-blocks` negotiation re-spelled in this lane's idiom: ask which blocks are
missing, send those, then name the whole list. It is deliberately *not* general
sync — one file, one path, and the client says which file it means — but it is
the same insight, that a client that can name content by hash should ask before
it sends. The spool disappears with it: blocks arrive already chunked, so there
is nothing to seek over.

**The timing mattered more than the shape.** A version number exists to protect
consumers you can't upgrade atomically. `/api/silo/v1` had exactly one consumer,
`client/client.go`, shipping in the same binary as the server — so it was fixed
in place, with no v2, and the method signatures didn't change, so the TUI and
CLI carried over untouched. That window closes the moment a separately-installed
client points at it, which is why this happened before Porter and not after.

The older `dir`/`download`/`file`/`mkdir`/`rename`/`move` endpoints were removed
in 0.4.4. Nothing outside this repository had ever used them and each had an
equivalent on the entries surface, so they went while that was still true;
`protocol.md` lists what to call instead.

Sync itself stays out of this. `entries` is human-shaped — one thing at a time,
ls/get/put. Sync is machine-shaped: batch negotiation over content-addressed
objects. Expressing that as REST is how protocols get bad. Give it its own space
and let it be blunt.
