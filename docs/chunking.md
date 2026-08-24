# Chunking

Why Silo cuts files at fixed 8 MiB offsets, what that costs, and the case for
moving to content-defined boundaries. This supersedes the verdict in
[`protocol-gaps.md`](protocol-gaps.md), which reached the opposite conclusion
on facts that have since changed.

Direction, not schedule. The measurements in "Suggested order" come first:
what the workload actually holds, and which hash the target architecture likes.

## Where 8 MiB comes from

Nowhere, is the honest answer. `git log -S "1 << 23" -- fileserver/option/option.go`
returns one commit: the initial fork from `haiwen/seafile-server`. It is an
inherited constant (`option/option.go:221`), settable as `fixed_block_size` in
MB (`option.go:446`), and nobody has revisited it.

It is not a bad number for what it optimises:

- **fsync amortisation.** `SILO_SYNC_OBJECT_WRITES` fsyncs per *object*, and
  fsync cost is per-object, not per-byte. One fsync per 8 MiB rather than eight.
- **Small file objects.** A `Seafile` (`fsmgr.go:30`) is a JSON list of block
  ids, loaded whole and cached against `FsCacheLimit`. A 10 GB file is 1,250
  ids at 8 MiB; 10,000 at 1 MiB.
- **One HTTP round trip per 8 MiB** on the sync lane.

It is tuned for *many medium files, written once* — photos, documents. It is
badly tuned for *few large files, edited in place* — VM images, databases,
video projects.

## The cost: boundaries are anchored to position

`chunkFile` (`fileop.go:2684`) reads exactly `FixedBlockSize` bytes from a
computed offset, and the caller steps offsets by the same amount
(`fileop.go:2565`). So a block id is `sha1(file[offset : offset+8MiB])`, and a
boundary is a function of *where you are*, not *what is there*.

Insert or remove a single byte and every boundary downstream lands on different
content. Every block id after the edit changes. `blocks/missing` will honestly
report that every block of a file it has held for months is missing, because
under the new names, they are.

### Worked example: video faststart

A 4 GB MP4, 2 MB `moov` atom, `-movflags faststart` moving it from tail to
head. The `mdat` payload is byte-identical afterwards — faststart rewrites the
chunk offset table inside `moov`, not the media — but now begins 2 MB later.

| | sent | stored |
|---|---|---|
| fixed 8 MiB (today) | 4 GB | 4 GB |
| fixed 8 MiB + rsync-style wire delta | ~18 MB | 4 GB |
| CDC, 1 MiB average | ~3 MB | ~3 MB |

All 512 blocks change today, for a file whose content moved and did not alter.

## What content-defined chunking is

Cut by content instead of position. Slide a window of ~64 bytes along the file,
keep a rolling hash of just that window, and declare a boundary wherever the
hash's low *k* bits are zero. Clamp with a minimum and maximum so the
distribution cannot produce 12-byte or 400 MB chunks.

```
h = ((h << 1) + GEAR[b]) & M64      # roll one byte in
if n < MIN:      continue           # too small
if (h & MASK) == 0 or n >= MAX:     # content says cut, or forced
    emit_chunk()
```

`GEAR` is 256 fixed random 64-bit values. The `<< 1` is the rolling part: after
~64 bytes a byte has shifted out of the register and stops influencing `h`. So
**the cut decision depends only on the bytes under the window, never on the
offset** — the same byte sequence cuts in the same places wherever it appears.

Chunks come out variable-sized, and the distribution is geometric rather than
tight. FastCDC's normalised chunking — a stricter mask before the average
point, looser after — costs about ten lines and is what makes the spread
usable.

Measured on 200 KB of pseudo-random data with 10 bytes inserted near the front:

| | chunks | sizes | ids surviving the edit | resent |
|---|---|---|---|---|
| fixed 4 KiB | 49 | 3392–4096 | 0 / 49 (0%) | 200,010 B |
| CDC ~4 KiB | 37 | 1045–16384 | 36 / 37 (97%) | 1,339 B |

## Why the earlier verdict changes

[`protocol-gaps.md`](protocol-gaps.md) rejected CDC as "a second store" and
preferred a wire delta built on rsync's algorithm. Two things undermine that.

**The wire delta never fixes storage.** rsync's rolling search does find the
shifted content — it scans every byte offset, so alignment is irrelevant to it,
and the faststart case above compresses to ~18 MB on the wire. But the server
then reconstructs into a temp file and hands it to `chunkFile`, which cuts at
fixed offsets again. Every resulting block is new. You send 18 MB and write
4 GB. It is a bandwidth fix wearing the costume of a storage fix.

It also needs a second, much finer block size purely for signatures — rsync
sizes those near √filesize, capped around 128 KiB — because at 8 MiB
granularity a scattered edit invalidates the whole basis block and the delta
degenerates to sending the file. So the stored block ids cannot serve as the
strong hashes, and the server pays an O(filesize) rolling scan per request,
which is the first path in Silo that genuinely *is* hash-bound and contradicts
the table in [`sync-design.md`](sync-design.md).

**The cross-lane dedup being defended may not exist.** (This second argument
is now moot — there is no second lane — but it is what broke the deadlock at
the time, and the read-path observation in it still matters.) Silo's *read* path
already assumes variable-size blocks: `doFileRange` (`fileop.go:355`) resolves
a byte offset by `blockmgr.Stat`-ing every block to build a size map, cached in
`blockMapCacheTable`, and never divides by `FixedBlockSize`. `Seafile`
(`fsmgr.go:30`) records `FileSize` and `BlkIDs` and no per-block sizes —
exactly the design you would choose if sizes were unknown until you looked.
`FixedBlockSize` appears in `fileop.go` only on the write side.

And [`notes.md`](notes.md) line 124 describes the data model's blocks as
"variable-size, Rabin CDC chunked, 4KB-8MB". If that is right about upstream
clients, SeaDrive blocks are already content-defined, Silo's fixed chunking
already disagrees with them, and cross-lane dedup is already broken.

## What decided it

> **The gate below is spent.** Silo no longer targets Seafile clients, so the
> cross-lane dedup this test was meant to adjudicate has no lane to cross. CDC
> is unblocked and the only remaining question is whether it pays for the
> workload — see the order at the end. The test is kept because it explains
> what the argument used to hinge on.

The original gate was: sync one file with a real Seafile client and look at the
block sizes on disk. Uniform 8 MiB with a short tail meant `notes.md` was wrong
and the cross-lane argument stood; a spread meant it was already lost and CDC
cost nothing that had not already been spent.

```sh
find "$SILO_DATA_DIR"/storage/blocks -type f -printf '%s\n' | sort -n | uniq -c
```

What replaces it is a workload measurement rather than a compatibility one. CDC
pays on large files edited in place and pays nothing on append-only media, and
which of those a library holds is now the whole decision.

## What CDC does not change

The seam is `objstore.go:62`, the `storageBackend` interface. What sits above it
keeps its shape:

- a chunk id is still `hash(plaintext)` — dedup key, integrity proof, wire
  identifier — and is still the only name anything outside the store uses
- `blocks/missing` and `PUT blocks/{id}` keep working unchanged
- the read path already assumes variable sizes (`doFileRange`, `fileop.go:355`,
  stats every block rather than dividing by a constant)

## What compat-dropping changed about this

An earlier draft listed two more invariants — that ids stay `sha1` and that the
file object stays a bare list with no sizes. Both were consequences of wire
compatibility, and both are now reversed.

**The hash changes, and this is the only cheap moment to do it.** CDC renames
every chunk in existence regardless, so the migration is already being paid for.
`sync-design.md`'s case for SHA-1 was entirely shared identity across the two
lanes; with one lane it collapses, and what remains is that SHA-1's collision
resistance is broken and a crafted collision in a content-addressed store
substitutes content. BLAKE3 is the intended answer, with SHA-256 the contender
on ARM, where the crypto extensions may beat a library falling back to portable
Go. Benchmark on the machine that will run this rather than on a laptop.

A non-cryptographic hash cannot take this job however fast it is. XXH3-64 fails
even on accidental collisions at scale — the birthday probability at a billion
chunks is about 2.7% — and no xxHash variant claims resistance to a crafted
one. Its place is the client's local change-detection gate, as
[`sync-design.md`](sync-design.md) already argues, where it never crosses the
wire and has no adversary.

**The file object grows per-chunk sizes**, which CDC makes mandatory rather than
optional: a client cannot map a read offset to a chunk without them. That is
casync's `.caibx` shape, and it is available because the hand-rolled
C-compatible JSON encoder in `fsmgr.go` — its `", "` spacing and key ordering
were byte-compatibility with a client that no longer exists — dies with the
lane.

Encoding matters more than it looks. Moving to 1 MiB chunks multiplies chunk
count by eight, and a 32-byte id is 1.6× a 20-byte one, so a 10 GB file's chunk
list grows roughly thirteenfold. Hex-in-JSON puts that near 680 KB; a binary
`(size, id)` record puts it near 400 KB before compression. A client reads this
on every cold open, so the encoding is a read-latency decision rather than a
storage one.

## What it does change

**Parameters become protocol.** `server-info` used to advertise `block_size`
(`api/api.go:31` records why it no longer does) precisely because a client must
chunk identically for its ids to match. CDC replaces one integer with an
algorithm id, the gear table or its seed, mask bits, min, max, and the
normalisation scheme, and moves them off the server and onto the library, where
the listing reports them per library (`api/api.go:265`). Changing any of them is
still a new store, so they are still effectively frozen — but the freeze is
softer than it was. With porter as the only client, and shipped by us, getting
them wrong costs a migration rather than a break with software we do not
control. The chunker itself is under a hundred lines.

**Chunking becomes sequential.** `fileop.go:2565` computes offsets up front so
`createChunkPool` workers can seek straight to them. CDC boundaries depend on
every preceding byte. Two passes recover the parallelism — sequential scan for
cut points, then parallel hashing — and in practice this barely matters today:
`MaxIndexingThreads` defaults to `1` (`option.go:222`).

**Object count rises ~8×** at a 1 MiB average. Which is the next section.

## Packing

8× the objects means 8× the inodes, 8× the fsyncs on ingest, 8× the files for
the `rsync -a storage/` step in [`backup.md`](backup.md), and 8× the GC walk.
Every system that went down this road hit this and packed: git has packfiles,
BitKeeper had an indexed compressed format, casync has chunk stores plus
`.caibx` indexes.

**The id stays the address.** A pack index maps `id → (pack, offset, length)`.
The offset is a lookup result, never an identity — make it one and dedup,
verification, and the ability to compact at all go with it.

**Pack size is chosen by reclaim granularity, not inode count.** At a 1 MiB
average, a 512 MB pack holds ~500 blocks: the file-count problem is already
solved 500×. Doubling to 1 GB buys 2× on a solved problem and doubles the cost
of the thing that hurts. For reference: restic uses ~16 MB packs deliberately,
because prune must rewrite partially-dead ones; borg uses ~500 MB segments with
log-structured compaction; git uses multi-GB packs and treats `gc` as a
scheduled event.

**The upside:** one fsync per pack instead of per object. A CDC-plus-packs
store could plausibly ingest *faster* than today's, not slower.

**The cost is deletion**, which gets its own section below. Note that
`storageBackend` (`objstore.go:62`) has no delete method at all today; that
absence is the shape of the work.

**Crash safety changes shape.** `fsBackend.write` (`backend_fs.go:80`) is
write-temp → fsync → rename, so a crash leaves a whole object or nothing.
Appending into a pack admits a torn tail: append → fsync → atomic index update,
plus a recovery scan that truncates a pack to its last indexed offset.

**Keep the index out of SQLite.** Per-pack index files, mmap'd. Block lookup is
the hottest path during a sync, but the deciding reason is that it keeps packs
self-describing — the object store stays reconstructible from packs alone, with
no database, which is the property that makes `backup.md`'s "databases first,
objects second" ordering safe. Put the block index in SQLite and that ordering
no longer protects anything.

**Compression composes here**, and only here. Once a pack is one large file
holding independently-addressable chunks, the ZSTD seekable format — skippable
frames carrying an index of uncompressed offsets — is the right shape for
random access into it. See [`compression.md`](compression.md) for the identity
rule that makes per-chunk compression legal at all: hash the plaintext, store
the compressed form.

## Compaction

You cannot unlink one block out of a pack. Reclaiming space means rewriting the
pack without its dead blocks, which raises two questions: how you know what is
dead, and when you do the work.

### Deadness is not a local fact

This is the constraint everything else follows from. Deleting a file does not
kill its blocks — an old commit, another file, or another library sharing a store
may still reference them. A block carries no marker saying whether anyone still
wants it. Liveness is a *global reachability property*, computable only by
walking every live commit.

So a dead-bytes counter cannot be maintained incrementally as writes happen.
The only way would be refcounting the store, which is the classic
content-addressable trap: refcounts drift, and a refcount that drifts low
deletes live data.

**The dirty index is therefore an output of the mark phase, not a running
tally.** Something like `PackStats(pack_id, total_bytes, live_bytes, gc_id)`,
written when a mark completes and read by the compactor to decide what to
rewrite. It is stale by construction between runs, and that is fine, because it
only ever decides *scheduling*. Correctness comes from the mark, which the
compactor re-reads before it touches anything.

`GCID(library_id, gc_id)` and `LastGCID` already exist in the schema
(`dbutil/schema.go:125`), inherited from upstream and unused. That is the
generation stamp this needs: any block written after the mark's generation is
presumed live, which is what stops a sweep reaping the blocks of an upload that
was in flight while the mark ran.

### Rewrite, do not reuse

Reusing holes in place — a free-space map inside each pack — is the obvious
idea and the wrong one. Under CDC blocks are variable-sized, so a 1.3 MB hole
takes a block of at most 1.3 MB, and you are running a best-fit allocator with
a fragmentation problem inside every pack.

Every comparable system reached the same conclusion: git repacks, restic
rewrites packs below a used-fraction, borg does log-structured segment
compaction. None maintains a free list. Beyond fragmentation, rewriting keeps
packs **append-only**, which is what makes crash recovery a truncate and keeps
the `rsync -a storage/` backup path cheap.

Compaction must be idempotent and interruptible: write the new pack, fsync,
atomically swap the index entries, then unlink the old one. A crash at any
point leaves the old pack authoritative and the partial new pack as garbage to
discard. Never mutate a pack in place.

### Schedule it the way Postgres does

The instinct is "compact in quiet times", and quiet is the wrong trigger. On a
busy server it never arrives; on a home server it arrives constantly and you do
not want disks spinning at 3am to reclaim nothing.

Postgres already solved this shape. autovacuum fires on a **threshold** — the
dead-tuple fraction — rather than a clock, and `vacuum_cost_delay` throttles it
by an I/O budget so it runs continuously and slowly instead of hunting for
idle. Both translate directly: compact a pack when its dead fraction crosses a
threshold, rate-limited so compaction never competes meaningfully with sync
traffic. `size_sched.go:49` already runs a `workerpool` for background size
recomputation, so the in-tree precedent exists.

The analogy is worth taking seriously but not literally. It holds for the
sweep: separate marking from reclaiming, respect a visibility horizon (PG's
oldest snapshot ≈ the GC generation plus in-flight writes), throttle in the
background, and `VACUUM` vs `VACUUM FULL` maps onto reuse-in-place vs rewrite.

It breaks on the mark. A Postgres tuple carries its own `xmax`, so deadness is
local and `n_dead_tup` can be a live counter maintained by the stats collector.
A block carries nothing. Silo's mark half is a **tracing garbage collector**;
only its sweep half is a vacuum.

## Three layers, and what crosses the wire

Packing introduces a layer whose whole value depends on staying invisible.

| layer | what it is | who knows |
|---|---|---|
| pack | a large file holding many chunks | the server, only ever |
| chunk | a content-addressed run of bytes | the server; the client if it asks |
| file | an ordered list of `(chunk id, size)` | both |

**A pack never crosses the wire.** Not a pack id, not an offset within one, not
how many there are. This is not tidiness — compaction *rewrites* packs, so a
client holding a pack offset holds something that goes stale the moment the
compactor runs. `id → (pack, offset, length)` is a lookup result; make it an
identity and every compaction becomes a compatibility break.

Chunks are variable-sized, packs are not. A pack fills to a target and is
sealed, so its size is "roughly 512 MB" rather than a design parameter.

### The client can ignore chunks, and sometimes should

Two read paths, both correct, and the server serves both:

- `GET entries/{path}` with a `Range` — the server resolves offset → chunk →
  pack → bytes. The client knows nothing about any of it.
- `GET entries/{path}?type=blocks`, then `GET blocks/{id}` — the client fetches
  the chunk list once and thereafter names chunks.

A ranged read is right for anything read once: a small file, a random seek into
a large one, a thumbnailer taking the first 64 KB. The chunk list is right for
anything the client means to *keep*, and buys three things a byte range
structurally cannot.

- **Cache dedup.** A cache keyed by `(file id, window)` dies whole when the file
  changes: append one byte to a 4 GB file and every window is invalidated,
  though every chunk but the last is identical. Keyed by chunk id it survives
  edits, and dedups across files and libraries. This is a purely local argument
  and does not depend on there ever being a second source.
- **Verification.** A byte range is not an addressed object, so what arrived
  cannot be checked — including a client checking its own cache after an
  unclean shutdown.
- **A source that is not the server.** S3, a peer, a cache of uncertain
  provenance: all illegal without a hash to check against.

Note the asymmetry that already exists: a client names chunks when it *writes*
(`blocks/missing`, `PUT blocks/{sha1}`) and has no way to name them when it
reads.

### CDC makes the list mandatory rather than optional

Under fixed chunking a client derives boundaries from the file size —
`offset / FixedBlockSize`. Content-defined boundaries depend on the bytes, so
the list has to be transmitted, and **per-chunk sizes become mandatory in the
file object**: without them a client cannot map a read offset to a chunk.

At ~40 bytes per entry in a binary encoding, that is 200 bytes for a 5 MB
document, 20 KB for a 500 MB video, and 400 KB for a 10 GB archive — amortised
across every read of the file and cacheable against a file id that is stable
until the content changes.

Nothing is ever updated in place. Chunks are immutable; a write creates new
chunks and a new file object, which is already how the block surface works.

## Remote backends

`storageBackend` (`objstore.go:62`) is the seam, and packs sit below it, so an
S3 backend is a second implementation rather than a second design.

Packs suit object storage better than loose chunks do. A ranged GET costs one
request whether the chunk is a standalone object or a slice of a 512 MB pack,
so reads are unchanged — but on the write side packing turns five hundred PUTs
into one, and request count is what object storage bills for. Immutability is
not an obstacle either: packs are already append-only and compaction already
rewrites rather than mutates, which is exactly what an S3 object permits.

Two properties have to hold.

**The index stays local.** A chunk lookup cannot be a round trip to the
backend. Packs remote, `id → (pack, offset, length)` on local disk and mmap'd —
which is what the packing section argues for on other grounds, and what restic
does for the same reason.

**Object storage is a tier, not a substitution.** Fifty to a hundred
milliseconds to first byte is fine for backup and unusable for a mount. So:
local disk as cache, remote as the durable tier. That is the same shape as the
replica-and-cache split in [`future-features.md`](future-features.md), reached
from the opposite direction, and one mechanism should serve both.

## Files at both ends of the size range

**Large files need pack locality, deliberately.** Write a file's chunks
contiguously and in order into the same pack, so a sequential read is one
contiguous range rather than a scattered walk across a dozen packs. It falls
out of packing in write order — but only if dedup is never allowed to reorder,
so it is a rule to write down rather than a property to assume.

**Small files are a different problem, and packing chunks does not solve it.** A
4 KB file still costs a file object, a dirent and a chunk: three lookups for
four kilobytes, and over a remote backend, a request each.

The answer is inlining. Below a threshold — 64 KB is the usual neighbourhood —
store the bytes in the file object, or directly in the directory index, instead
of as a separate chunk. Btrfs does this with inline extents and NTFS with
resident data. For a mount it is the difference between a directory of small
text files costing one fetch each and costing none, and it is only available
because the object format stopped being someone else's.

## Suggested order

1. **Measure the workload.** CDC pays on large files edited in place and pays
   nothing on append-only media. This is now the only gate; the compatibility
   one is spent. Same discipline [`compression.md`](compression.md) asks for.
2. **Benchmark the hash on the target architecture.** BLAKE3 against SHA-256
   and SHA-512/256, at 64 KiB through 8 MiB, checking for each library whether
   it has assembly for that GOARCH or is falling back to portable Go. The
   answer may differ between the server and the machine running the client, and
   the server's answer decides the format.
3. **Cheap lever first, if it is enough.** `fixed_block_size` is already
   configurable. 8 MiB → 1 MiB makes same-length in-place edits cost 8× less,
   does nothing for insertions, costs 8× objects, and needs no new code. It
   also collapses the read amplification described below, which is reason
   enough on its own.
4. **The chunker, the hash and the file object format, in one migration.**
   FastCDC normalisation from the start; parameters in `server-info`; per-chunk
   sizes in the object; binary encoding. All four change every id, so doing
   them together costs one migration instead of four, and none of them has an
   independent justification for going first.
5. **Packing**, once object counts justify it.
6. **Compaction**, alongside the per-library GC in
   [`future-features.md`](future-features.md) — they are the same problem. The
   mark phase that GC needs is the same one the dirty index is derived from;
   build them together or build them twice.

Inlining small files and the remote-backend tier are independent of all six and
can land whenever they are wanted.

## What the mount contributes to the argument

The dedup case for CDC is about what gets stored twice. There is a second case,
which is about latency, and it points the same way for a different reason.

Chunk size sets the floor on what every `open(2)` costs. porter fetches 2 MiB
windows deliberately — matching an 8 MiB block would mean a 4 KiB read pulls
8 MiB — but the amplification did not go away, it moved to the server:
`doFileRange` (`fileop.go:406`) reads the *whole* block into a `bytes.Buffer`
and slices the window out of it. A sequential scan therefore reads every block
four times server-side, and allocates 8 MiB per window to do it.

At a ~1 MiB average, chunk and window converge and the amplification disappears
on both sides. That argument is independent of dedup, applies to append-only
media where the dedup argument does not, and is the strongest reason to move
off 8 MiB even if CDC itself is deferred.

Streaming instead of buffering whole blocks is worth doing regardless, and does
not need any of the above.
