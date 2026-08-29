# Chunking

Silo cuts files at content-defined boundaries: **FastCDC over a keyed 64-bit
gear hash**, algorithm id `fastcdc-gear64/v1`. The binding definition — masks,
cut rules, stream rules, test vectors — is the Chunking section of
[`spec/store-format.md`](spec/store-format.md); the implementation is
[`store/chunker.go`](../store/chunker.go), in the dependency-free `store`
module both the server and any client build from, so there is exactly one
chunker to be compatible with.

This document is the orientation, the reasoning that survives, and the design
notes for the parts not yet built (packing, compaction). It used to be the
case *for* CDC against the fixed 8 MiB chunking inherited from Seafile; that
argument won, the fixed path is deleted, and the long version lives in git
history.

## The scheme

| parameter | value |
|---|---|
| `algorithm` | `fastcdc-gear64/v1` |
| `seed` | 32 bytes; keys the gear table |
| `min_size` | 262144 (256 KiB) — shortest chunk except the last |
| `target_size` | 1048576 (1 MiB) — where the mask changes |
| `max_size` | 4194304 (4 MiB) — forced cut |
| `normalization` | 2 — bits by which the masks differ from the target |

The cut loop, conceptually:

```
h = ((h << 1) + GEAR[b]) & M64      # roll one byte in
if n < MIN:      continue           # too small to cut
if (h & MASK) == 0 or n >= MAX:     # content says cut, or forced
    emit_chunk()
```

`GEAR` is 256 random 64-bit values. The `<< 1` is the rolling part: after ~64
bytes a byte has shifted out of the register and stops influencing `h`, so
**the cut decision depends only on the bytes under the window, never on the
offset** — the same byte sequence cuts in the same places wherever it appears
in any file. That is the property everything downstream rests on: insert bytes
at the front of a file and the chunker resynchronises within about one chunk,
after which every id matches the previous version.

FastCDC's *normalised* chunking tightens the raw geometric size distribution:
a stricter mask (more bits) before `target_size`, a looser one after. The
masks take the **top** bits of `h`, not the low ones — with `h = (h<<1) +
gear[b]`, bit *j* has been fed by the last *j+1* bytes, so the low bits are
decided by a handful of bytes and the high bits by the full window. This is
the one place the format departs from the FastCDC paper's hand-picked
constants; "the top *n* bits" is a rule that generalises rather than a table
to transcribe.

**The gear table is derived, not hardcoded**: `HKDF-SHA256(seed,
"silo/gear/v1")` expanded to 256 little-endian uint64s (`store/chunker.go`).
Derived because E2EE libraries need a *secret* table, and there is no reason
for plain libraries to reach theirs differently:

- **Plain library**: `seed = SHA-256("silo/chunker/plain/v1")` — a public
  constant, so any implementation can reproduce the boundaries and
  cross-library dedup among plain libraries stays possible.
- **E2EE library**: `seed = HKDF-SHA256(CK, salt="silo/chunker/v1")` — cut
  points become a function of plaintext *and a secret*, so a server holding
  only ciphertext cannot fingerprint known files from the chunk-size sequence.
  See [`encryption.md`](encryption.md) for the second job this does.

**Parameters are per-library protocol.** The library listing serves each
library's `chunker` object — algorithm, min, target, max, normalization —
frozen at creation. A client chunks under the library's own parameters or not
at all: chunking under the wrong ones uploads *correctly* (the server verifies
every chunk against its id) and simply dedups nothing, which is why the rule
is refuse-don't-default. A library whose row carries no `chunker` gets whole
files, not guessed boundaries.

**Ids are SHA-256 of the stored bytes** — plaintext in a plain library, the
client's ciphertext in an E2EE one. **Manifests carry a mandatory per-chunk
plaintext size** beside each id, because under content-defined boundaries a
reader cannot map a byte offset to a chunk any other way. **Files under
64 KiB never reach the chunker**: their bytes are inlined into the manifest
(`store.InlineThreshold`), a function of size alone, never a writer's choice.

## Why boundaries are content-defined

The inherited chunker read fixed 8 MiB runs from computed offsets, so a
boundary was a function of *where you are*, not *what is there*. Insert or
remove one byte and every boundary downstream lands on different content;
every block id after the edit changes; `blocks/missing` honestly reports a
file the server has held for months as entirely absent, because under the new
names it is.

The worked example that carried the argument — a 4 GB MP4, 2 MB `moov` atom,
`-movflags faststart` moving it from tail to head, `mdat` byte-identical but
2 MB later:

| | sent | stored |
|---|---|---|
| fixed 8 MiB | 4 GB | 4 GB |
| fixed 8 MiB + rsync-style wire delta | ~18 MB | 4 GB |
| CDC, 1 MiB average | ~3 MB | ~3 MB |

The middle row is why an rsync-style delta protocol was rejected rather than
CDC: the rolling search finds the shifted content on the wire, but the server
then re-cuts the reconstructed file at fixed offsets and stores 4 GB of new
blocks. A wire delta is a bandwidth fix wearing the costume of a storage fix —
and it puts an O(filesize) rolling scan on the server per request, where CDC
puts the chunking work on the client, once per version.

The second, independent reason the 1 MiB target won: **latency for the
mount**. Chunk size sets the floor on what a random read costs, and at 8 MiB
a 4 KiB read pulled 8 MiB through the server. At a ~1 MiB average, the chunk
and a mount's fetch window converge. That argument applies to append-only
media where the dedup argument does not.

## What the migration settled

The move to the current format changed every id anyway, so the decisions that each
individually meant "rename everything" were taken together, once:

- **The hash is SHA-256**, not SHA-1 (broken collision resistance has no place
  in a content-addressed store) and not BLAKE3 (an earlier draft's intent; the
  spec chose the primitive the rest of the format already stands on). A
  non-cryptographic hash was never eligible: at a billion chunks XXH3-64's
  accidental-collision probability alone is ~2.7%, and a crafted collision in
  a content-addressed store substitutes content.
- **Parameters moved off the server and onto the library**, where the listing
  reports them per library. Changing them is still a new store, so they are
  still effectively frozen — but with porter as the only client, and shipped
  by us, getting them wrong costs a migration rather than a break with
  software we do not control.
- **The file object became a binary manifest** with `(chunk_id, size)`
  records — casync's `.caibx` shape — replacing hand-rolled JSON whose
  spacing was byte-compatibility with a client that no longer exists.
- **Small files inline** below 64 KiB rather than costing a chunk, a
  manifest and a round trip each.

## Not built yet: packing

At a 1 MiB average, object counts run ~8× what 8 MiB produced: more inodes,
more fsyncs on ingest, more files for the `rsync -a storage/` step in
[`backup.md`](backup.md), a longer GC walk. Every system that went down this
road packed — git's packfiles, casync's chunk stores, borg's segments. The
`storageBackend` seam (`objstore.go:132`) is where packs would sit; nothing
above it changes shape.

- **The id stays the address.** A pack index maps `id → (pack, offset,
  length)`. The offset is a lookup result, never an identity — make it one
  and dedup, verification, and the ability to compact at all go with it.
- **A pack never crosses the wire.** Not a pack id, not an offset, not how
  many there are. Compaction rewrites packs, so a client holding a pack
  offset holds something that goes stale the moment the compactor runs.
- **Pack size is chosen by reclaim granularity, not inode count.** At 1 MiB
  chunks, a 512 MB pack already solves the file-count problem 500×; making
  packs bigger doubles the cost of the thing that hurts (rewriting
  partially-dead packs). Restic uses ~16 MB packs for exactly this reason;
  git uses multi-GB packs and treats `gc` as a scheduled event.
- **The upside is ingest**: one fsync per pack instead of per object. A
  CDC-plus-packs store could plausibly ingest faster than the loose store.
- **Crash safety changes shape.** Loose objects are write-temp → fsync →
  rename: a whole object or nothing. Appending into a pack admits a torn
  tail: append → fsync → atomic index update, plus a recovery scan that
  truncates a pack to its last indexed offset.
- **The index stays out of SQLite.** Per-pack index files, mmap'd. Partly
  because chunk lookup is the hottest path in a sync — but the deciding
  reason is that packs stay self-describing, the store stays reconstructible
  with no database, and [`backup.md`](backup.md)'s "database first, objects
  second" ordering keeps protecting something.
- **Compression composes here, and only here** — the ZSTD seekable format
  over a sealed pack; see [`compression.md`](compression.md) for the identity
  rule (hash the plaintext form of the stored bytes, store compressed) that
  makes it legal.
- **Large files want pack locality**: a file's chunks written contiguously,
  in order, into the same pack, so a sequential read is one contiguous range.
  It falls out of packing in write order — but only if dedup is never allowed
  to reorder, so it is a rule to write down rather than a property to assume.

## Not built yet: compaction

You cannot unlink one chunk out of a pack; reclaiming space means rewriting
the pack without its dead chunks. Two questions: how you know what is dead,
and when you do the work.

**Deadness is not a local fact.** Deleting a file does not kill its chunks —
an old commit or another file may still reference them, and a chunk carries no
marker saying whether anyone wants it. Liveness is a global reachability
property, computable only by walking every live commit. So a dead-bytes
counter cannot be maintained incrementally; the only incremental option is
refcounting, the classic content-addressable trap — refcounts drift, and a
refcount that drifts low deletes live data. **The dirty index is an output of
the mark phase, not a running tally**: `PackStats(pack, total_bytes,
live_bytes, gc_generation)` written when a mark completes, stale by
construction between runs, deciding only *scheduling*. Correctness comes from
the mark the compactor re-reads before touching anything.

`GCID` / `LastGCID` already exist in the schema (`dbutil/schema.go:255`),
inherited and unused. That is the generation stamp this needs: any chunk
written after the mark's generation is presumed live, which is what stops a
sweep reaping an upload that was in flight while the mark ran.

**Rewrite, do not reuse.** A free-space map inside packs means running a
best-fit allocator with a fragmentation problem inside every pack, because
chunks are variable-sized. Git repacks, restic rewrites below a
used-fraction, borg compacts segments; none maintains a free list. Rewriting
also keeps packs append-only, which keeps crash recovery a truncate and the
`rsync` backup path cheap. Compaction is idempotent and interruptible: write
the new pack, fsync, atomically swap index entries, unlink the old one. Never
mutate a pack in place.

**Schedule on a threshold, not a clock.** "Compact in quiet times" never
fires on a busy server and fires constantly on an idle one. Postgres's
autovacuum has the right shape: trigger on the dead fraction, throttle by an
I/O budget so it runs continuously and slowly. The analogy holds for the
sweep and breaks on the mark — a Postgres tuple carries its own `xmax`, so
deadness is local there; a chunk carries nothing, so Silo's mark half is a
tracing garbage collector and only its sweep half is a vacuum.

**Compaction and the per-library GC in
[`roadmap.md`](roadmap.md) are the same project.** Both need
the mark phase; the dirty index is derived from it. Build them together or
build the mark twice.

## Not built yet: remote backends

`storageBackend` is the seam and packs sit below it, so an S3 backend is a
second implementation rather than a second design. Packs suit object storage
better than loose chunks: reads cost one ranged GET either way, but packing
turns five hundred PUTs into one, and request count is what object storage
bills for. Two properties have to hold: the `id → (pack, offset, length)`
index stays on local disk (a chunk lookup cannot be a round trip), and object
storage is a tier behind a local cache, not a substitution — 50–100 ms to
first byte is fine for backup and unusable for a mount.

## Two read paths, both correct

- **A ranged `GET` on the entry**: the server resolves offset → chunk →
  bytes; the client knows nothing about chunking. Right for anything read
  once — a small file, a seek into a large one, a thumbnailer.
- **The manifest, then chunks by id**: right for anything a client means to
  *keep*. A cache keyed by chunk id survives edits (append a byte to a 4 GB
  file and every chunk but the last is still valid) and dedups across files;
  an addressed object can be verified, including a client checking its own
  cache after an unclean shutdown — and verification is what makes a source
  other than the server (a peer, a mirror, a cache of uncertain provenance)
  legal at all.
