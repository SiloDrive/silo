# Target

What Silo is trying to become, described as an end state rather than a route to
one. There are no steps here and no dates — the point of writing the
destination down separately is that arguments about *where* stop contaminating
arguments about *next*.

A row here is a commitment to a shape. Where something is genuinely undecided it
is in [Open](#open) rather than guessed at, because a target document that
guesses is worse than a short one.

## What Silo is for

**An on-demand filesystem over storage you own.** A large library, mounted on
every machine you use, browsable in full without downloading any of it, with
content fetched when something reads it and evicted when space runs short.

The comparison is Dropbox's selective sync or SeaDrive, and the difference is
that the server is yours and the format is not a vendor's. It is explicitly
*not* a sync product: nothing maintains a complete second copy of a library on
a client's disk as its normal mode of operation.

The workload is mixed — photographs and video alongside documents and working
files that get edited in place. Both ends of the file-size range are first-class,
and a design that is good at one and bad at the other has not hit the target.

## The shape

```
  porter-fuse (Linux)   porter-mac (macOS)   porter-ios   web UI
            │                    │                │          │
            └────────────────────┴────────────────┴──────────┘
                                 │  HTTP, /api/silo/v1
                                 ▼
                          ┌─────────────┐
                          │    Silo     │   one Go binary
                          └──────┬──────┘
                                 │
                     ┌───────────┴───────────┐
                     ▼                       ▼
                  SQLite                 pack files
              accounts, shares,      content-addressed chunks
              namespace index,       + a local pack index
              history
```

One server binary, one database, one object store, and a family of first-party
clients that all speak the same lane. There is no second protocol and no
third-party client to stay compatible with.

**porter-mac exists. porter-fuse exists. porter-ios is a File Provider
extension that shares most of its implementation with porter-mac**, which is
what makes a third client affordable rather than a third project. The web UI is
for casual access from a device that has no mount — chiefly a phone before
porter-ios lands, and a borrowed machine after.

## The store

**Chunks are content-addressed and immutable.** A chunk's name is the hash of
its bytes; that name is its dedup key, its integrity proof, and the only handle
anything outside the store ever holds. Nothing is updated in place — a write
produces new chunks and a new file object.

**The hash is 256-bit and cryptographic.** BLAKE3 unless the benchmark on the
machine this actually runs on says SHA-256 wins there. Non-cryptographic hashes
are disqualified regardless of speed: the hash is the address, so a collision
substitutes content.

**Boundaries are content-defined**, averaging around 1 MiB, FastCDC-normalised,
with the parameters advertised in `server-info`. Fixed offsets lose an entire
file to a one-byte insertion, and the mixed workload contains exactly the files
that suffer for it.

**Chunks live inside packs.** A pack is a large append-only file holding many
chunks, with a local index mapping `id → (pack, offset, length)`. Packs are a
storage detail and never appear on the wire; a pack id or offset in a response
would make every compaction a compatibility break.

**Small files are inlined.** Below a threshold the bytes live in the file object
or the namespace index rather than as a separate chunk, so a small file costs
one lookup instead of three. This is what makes a directory of ten thousand text
files behave, and it is half of what "good at both ends" means.

**Space is reclaimed by compaction, driven by a tracing mark.** Chunk liveness
is a global reachability property, so a mark phase walks live history and the
sweep rewrites packs whose dead fraction crosses a threshold. It is rate-limited
and continuous rather than scheduled for a quiet hour that never comes. Marking
and the per-library garbage collector are one mechanism, not two.

## The namespace

**Reads go to an index, not to the object graph.** A `readdir` is an indexed
query and a delta is a sequence scan. Walking commit and directory objects to
answer "what is in this folder" is the wrong cost on the path a mount hits
hardest.

**History stays a commit DAG**, and keeps two jobs: it is what trash, versions
and restore are built from, and it is what the mark phase walks. It is no longer
what a client reads through.

## What a client sees

One lane, `/api/silo/v1`, and two ways to read.

- **Ranged reads.** `GET entries/{path}` with a `Range`; the server resolves
  offset to chunk to pack and returns bytes. The client needs to know nothing
  about chunking. Right for anything read once.
- **Chunk-addressed reads.** `GET entries/{path}?type=blocks` for the ordered
  list of `(id, size)`, then `GET blocks/{id}`. Right for anything the client
  means to keep, because a cache keyed by chunk id survives edits, dedups across
  files and libraries, and can verify what it was handed.

Writes are one shape: name the chunks, ask which are missing, send those, then
name the list — with `If-Match` so a concurrent remote edit is refused rather
than clobbered. Small files skip the negotiation and go in a single request.

Clients keep a local SQLite database for a durable write journal and for cache
accounting. They do not need one to detect change: a mount is told what changed
by the kernel, and is told what changed remotely by a push notification and a
delta call.

## Who it serves

A small group of accounts who broadly trust each other — one person today, more
by invitation later. That means real accounts, sharing between them, and quota
that works, but it does not mean a threat model where users attack each other's
storage. Cryptographic hashing is kept anyway, because it costs nothing at
these speeds and because the assumption is much easier to keep than to add back.

## Deliberately not

- **Seafile wire compatibility.** Given up on purpose. It was the constraint
  that pinned the hash, the chunking, the object encoding and the on-disk
  compression all at once, and every one of those is a decision worth making on
  merit. `seafile-compat-end` is the tag to revert to.
- **A sync agent that maintains a full local mirror.** Different product; the
  mount is the goal.
- **Peer-to-peer anything.** A mesh of instances agreeing on shared state is
  consensus, and consensus is a different project.
- **Erasure coding and parity schemes.** Considered and dropped: replication
  buys the same durability at household scale for the price of disk, which is
  the cheapest thing in the system.
- **Third-party clients.** The lane is ours and has one family of consumers.

Object storage sits outside this list rather than in it. `storageBackend`
(`objstore.go:140`) is the seam, and it is already pack-shaped for exactly this
reason. S3 and a NAS mount are planned durable tiers rather than non-goals —
see [`storage.md`](storage.md) § Durable tiers — but they are not built, and
nothing in this target may be shaped around them before they are.

Replication with a single authority (a primary, some number of read-only
replicas, a minimum copy count that makes dropping safe) is a genuine stretch
goal rather than a non-goal. It is described in
[`future-features.md`](future-features.md) and nothing in this target should
make it harder later.

## Open

- **Which hash.** BLAKE3 against SHA-256 and SHA-512/256, benchmarked on the
  architecture the server actually runs on, checking whether each library has
  assembly for that GOARCH or is falling back to portable Go. The server's
  answer decides the format.
- **The chunk size target.** ~1 MiB is the working assumption; the real number
  comes from measuring object count and read amplification against a real
  library.
- **Whether the commit DAG survives.** Demoting it off the read path is
  decided. Whether history is best served by a DAG at all, once nothing reads
  through it, is not.
- **When porter-ios happens**, and whether the web UI is a stopgap for it or a
  permanent surface in its own right.
