# Packs

Status: **all five steps built, and the cutover is behind a flag that is off.**
The index, the sidecar, recovery, sealing, reading a sealed pack, the lookup,
the writer, the three sealing rules and `gc` through the seam are all in.
`option.PackWrites` is `false`, so every install still writes loose objects; see
§ The cutover for why, which is compaction (silo#19) rather than anything in
this plan. Tracked as silo#17, milestone `packs`.

Owned by [`../storage.md`](../storage.md) § Packs, which is normative for the
format and the sealing rules. This document owns the *build*: what order, what
each step can be tested against on its own, and which decisions were made here
rather than there.

## Why, in one paragraph

Not space. Dedup already removed the duplicate bytes and compression is a
separate deferred item; a pack of a million chunks holds the same bytes the
million files held. What changes is the *count*: ~6 TB at a 1 MiB target is ~6
million chunks, and `storage.md` says what that means — "a filesystem with six
million files in it is a different kind of object than one with six thousand."
Two consequences follow and neither is about the local disk. A durable tier
cannot take six million PUTs, and the S3 feature floor — PUT, ranged GET,
DELETE, LIST, no append, no multipart — can only accept sealed immutable
objects, so a container has to exist before a tier can. And compaction reclaims
by rewriting a whole pack and swapping it, never by reusing holes inside one,
so without a container there is nothing for silo#19 to rewrite.

## The invariant this plan is built to keep

**`objstore`'s exported API stays object-addressed, and nothing above it learns
what a pack is.** `Read`, `ReadInto`, `ReadAt`, `Write`, `WriteVerified`,
`Stat`, `Exists`, `List`, `Remove`, `RemoveLibrary` take a library id and an
object id today and take exactly those after packs. `Stat` still answers a
plaintext length, because it becomes a `Content-Length` through
`ChunkStoredSize`/`ObjectSize`.

This is the same invariant at-rest encryption was built to
([`at-rest-encryption.md`](at-rest-encryption.md)), and it held: a frame never
escaped the package. A pack must not either — `storage.md` already forbids it
on the wire, "not an id, not an offset, not a count", because compaction moves
chunks and anything a client cached about a pack goes stale.

## What is already true, and worth not re-deciding

- **Every stored object is already a self-describing frame.** `SILF` v1:
  magic, version, the 32-byte id, an 8-byte plaintext length, a 96-bit nonce,
  then ciphertext+tag, with the header bound as AAD. **The frame does not
  change** — a pack is a container for frames that already exist in exactly
  that form, which is what makes ingest a copy rather than a re-seal.
- **The fixed-width header exists for this.** It was made fixed-width so `Stat`
  could stay one `os.Stat`; the same property lets an index record an offset
  and a reader parse a header there without scanning.
- **`validPackID` and the fan-out already speak "pack".** The vocabulary went
  into `id.go` deliberately, and `packPath` already builds `<aa>/<rest>` under
  a library directory.

## Decision 1: an open pack is appended in place, with its index in a sidecar

Three shapes were considered and the third is the one being built.

*Open pack, index inside it.* Rejected: data and index interleave in one file
while it is open, and the recovery rule has to reason about both.

*No open pack at all — accumulate loose objects and seal a batch of them once
there are enough.* Attractive because it deletes the crash-recovery problem
outright: today's write path is untouched, and a pack is written in one pass
and immutable from birth. Rejected on I/O and on the batch surface. It writes
every byte twice and reads all 6 TB back to do it, and it cannot improve
`POST chunks`, which carries up to 256 frames and today pays 256 temp files,
fsyncs and renames for them.

*Open pack, index in a sidecar beside it.* **Built.** Frames are appended to
the pack. The index accumulates in a sidecar **in the same format the footer
will use**, so there is one index writer, one parser and one thing to get
right, serving the live index, the sealed footer, and recovery. Sealing sorts the
sidecar's records, appends them to the pack as its footer, fsyncs, and removes
the sidecar — removes rather than empties, because a sidecar is named after its
pack and pack ids are random, so there is no next pack to hand an emptied one
to.

Each byte is written once. The batch surface becomes 256 appends and one fsync.
And `storage.md`'s existing model — "crash safety is append → fsync → atomic
index update; recovery truncates a pack to its last indexed frame" — stays
literally true, with the sidecar being what makes "its last indexed frame" a
cheap question rather than a scan.

What it costs is the one genuinely new concurrency concern in this plan:
**writers serialise to allocate an append offset**, where writes to distinct
loose paths needed no coordination at all. The batch surface is what keeps that
cheap — the lock is taken once per request rather than once per chunk.

## Decision 2: `storageBackend` stays a tier interface

The seam looks pack-shaped and is not. `write(libraryID, packID, r, sync)`
takes a whole pack and publishes it atomically, and `backend_fs.go` is explicit
that this is deliberate: "a pack enters this interface only once it is sealed,
and arrives whole."

That is exactly right for a tier and says nothing about an open pack, which
never crosses it. So the seam is left as it is and becomes the tier interface —
local cache today, NAS and S3 later. The open pack, the sidecar and the lookup
live above it, and `ObjectStore` keeps its object-addressed API and delegates
to them instead of mapping one object to one degenerate "pack".

## Decision 3: the footer, and why it cannot be a header

```
SILP  <version> <reserved×3>        magic, opening
<frame> <frame> …                   SILF frames, appended in arrival order
SILX  <version> <records>           the index, sorted by id, fixed-width
SILB  <params> <bits>               the bloom filter
<footer length> <index length>      fixed width, at a known offset from the end
SILP                                magic, closing
```

Two lengths, because the reader has to split the footer and neither section
can say where it ends on its own: the index is a run of records with no count,
and the filter's size is a function of a parameter stored at its start. Both
go in the tail rather than in a section header, which is what keeps the index
section byte-identical to a sidecar — the property the one-format argument
rests on.

A pack does not know its own frame offsets until the frames are written, so a
header would need a second pass or reserved space to seek back into. Parquet
reaches this layout from the same constraint. Two details are copied with it:
the fixed-width lengths at a known offset from the end, so the footer is found
in one seek; and **magic at both ends**, because a truncated pack is the normal
crash case here and a missing tail magic says so in four bytes.

One object per pack rather than a pack plus a sidecar is what this buys on a
tier: half the PUTs, half the LIST entries, and no way for the two to be
separated or to upload out of order.

Not copied from Parquet: Thrift, which is a dependency and fights byte-exact
test vectors, and the schema machinery, for a format with one record type.

## Decision 4: bloom filters, sized against the number of packs

A lookup has to name the pack before it can search one. Ids are SHA-256 and
therefore uniformly random, which kills the obvious answer: min/max per pack
partitions nothing, because every pack's id range is the whole space. That is
the same dead end that put optional bloom filters into Parquet for
high-cardinality equality.

So: one filter per sealed pack, in memory. "No" is certain; "yes" costs one
binary search over that pack's index to confirm.

**The rate is set against the pack count, and this is the part that is easy to
get wrong.** A 512 MB pack holds ~500 chunks at a 1 MiB target, so 6 TB is
~12,000 packs — and a textbook 1% per-pack rate would mean ~120 false hits and
120 confirming searches on *every* lookup. At 1e-5: ~24 bits per chunk, ~1.5 KB
per pack, ~18 MB for the whole store, ~0.1 false hits per lookup.

The hash is free. A uniformly random id needs no hashing to index a filter:
take `k` disjoint bit-ranges out of the 256 bits. Parameters go in the footer
rather than being compiled in, so a later pack can choose differently without
invalidating an earlier one.

## Decision 5: packs are per store, not per library

`removeLibrary` is the reason. It is a `RemoveAll` today and wants to stay
cheap; packs shared across stores would make deleting one a compaction of every
pack it touched — rewriting live frames and swapping them — so the cheapest
operation here becomes one of the most expensive, at exactly the moment someone
is trying to reclaim space.

Two things fall out of it that would justify it on their own. **Dedup already
does not cross stores**: objects live under `storage/<type>/<store-id>/`, so a
chunk in two libraries is already two copies and a shared pack would not
recover a byte. And **the lookup stays scoped** — a read names a store, so only
that store's filters are asked rather than every filter in the installation.

*Store* rather than *library* is the precise word, and the difference is real:
`objmgr` passes `StoreID`, which for a virtual library is its origin's id. So a
virtual library shares its origin's packs, which is right — it already shares
that origin's dedup and its GC.

The cost is undersized packs on small stores, which is what compaction merges —
and what seal-on-age already accepts.

## Decision 6: the pack format is not in the spec

[`../spec/store-format.md`](../spec/store-format.md) is the contract a second
implementation reproduces, and a pack never crosses the wire. porter-macos
never sees one. The frame inside a pack is the storage frame, which is
server-side and already deliberately absent from the spec for the same reason.
So the pack format is documented in [`../storage.md`](../storage.md) and given
vectors in the package, not promoted to the spec.

## Work order

Each step is separately testable and leaves the tree working. The loose store
stays the write path until step 4.

### 1. The index format, the sidecar, and recovery

**Built.** First, because the sidecar is the centre of the design rather than an
implementation detail: it is the live index, it becomes the footer, and it is
what recovery reads. Getting its format settled first means steps 2 and 3 have
nothing to invent.

Append frames to a pack, record them in the sidecar, and recover: truncate the
pack to the last offset the sidecar indexes. Tested by interrupting between
every pair of steps — after the append, after the pack fsync, after the sidecar
append, after the sidecar fsync — and asserting the same store every time.

### 2. Sealing, the footer, and reading a sealed pack

**Built.** Sort the sidecar's records, append them as the footer, fsync, remove
the sidecar. Then read back: the footer is found from the tail, the index is
binary-searched in place, every frame is where the index says. A crash mid-seal
truncates and re-seals, which is idempotent because the sidecar is still the
authority — and *exact*, because sorting and the filter are deterministic, so
the test compares a re-sealed pack byte-for-byte against an uninterrupted one.

The filter is written here rather than in step 3, because it lives *in* the
footer: writing the format now means step 3 adds a registry of filters rather
than a second footer version.

The index is read into memory rather than mmap'd. The search runs over raw
bytes either way, so an mmap is one line when it is wanted — and what wants it
is the resident-set question across twelve thousand packs, which is step 3's to
answer with numbers rather than step 2's to guess at.

Vectors for the sealed format land here, on the `-update` protocol
`store/vectors_test.go` established: `objstore/testdata/packfooter.json`.

### 3. Lookup, and reads through the pack layer

**Built.** Three places are asked in order: the open pack, from the index its
writer already holds in memory; then every sealed pack, filter first; then the
loose store. `Read`, `ReadInto`, `ReadAt`, `Stat` and `Exists` all go through
it, and `Stat` answers out of the index, so it stays one lookup and no read.

**The pack is asked before the loose store, and that ordering is load-bearing
rather than an optimisation.** Ingest appends a frame to a pack and deletes the
loose copy once the index is durable, so an object exists in both places at
once, and the pack is the copy that will still be there afterwards. The test
damages the loose copy so that reading it would fail, which is what stops the
assertion passing by luck.

**The invariant held**: the existing `objstore` suite passes unchanged.

Three things this step settled that the plan had not:

- **A sealed pack holds no file descriptor.** 6 TB is ~12,000 packs and the
  default descriptor limit is about a thousand, so a pack is its footer in
  memory plus a path, and a read opens, reads its range, and closes. The two
  extra syscalls are nothing against the read, and it is the shape a ranged GET
  against a tier has anyway, where there is no handle to hold.
- **`List`, `Remove` and `RemoveLibrary` stay loose-only.** `Remove` of a
  packed object is not a deletion at all — a sealed pack is immutable, so
  reclaiming a frame inside one is compaction (silo#19). Those verbs move with
  the write path in steps 4 and 5, not before it.
- **Sealed packs are not behind the `storageBackend` seam.** They live in
  `<library>/packs/`, beside the fan-out rather than in it, which is what keeps
  the loose walk working untouched. Moving them into the fan-out — where a
  sealed pack simply *is* what the backend stores, and a frame read becomes
  `backend.readAt` — is **not** step 5's, and this originally said it was; see
  § The layout is implemented twice.

### 4. Seal on age and at shutdown, and packs become the write path

**Built, behind `option.PackWrites`, which is off.** The writer, the three
sealing rules and the process-wide registry are in and tested; what the flag
gates is only which container a write lands in.

Three rules close a pack: **target size**; **age**, so the single-copy window
is a number rather than "until enough data arrives" — an open pack cannot be
uploaded, so on a quiet server the day's last chunks would otherwise sit on one
disk until unrelated future traffic happened to fill the pack; and **clean
shutdown**, so a stop leaves nothing half-open. A fourth case falls out of
recovery: a pack found open at startup is sealed rather than appended to, since
its frames have been outside any sealed pack for at least as long as the
process was down.

An open pack that never held a frame is **discarded rather than sealed**.
Sealing it would leave a footer and a filter that every later lookup asks and
every listing walks, permanently, for a pack that held nothing.

**The open pack is process-global per store directory, and that is forced.**
`objstore.New` is called once per loaded library, so an open pack held on an
`ObjectStore` would mean two instances appending to one library through two
handles at two offsets, each believing it owned the end of the file. Step 3's
"two open packs" refusal catches that state on disk; the registry is what stops
it being created. `objstore.Close()` is package-level for the same reason, and
the server calls it once on the way down.

Two prerequisites landed before any of this, because they are what a write path
moving would otherwise break:

- **`Remove` refuses a packed object** with `ErrInPack` instead of succeeding.
  The loose path is gone, and `os.Remove` on a missing file is deliberately not
  an error here, so the old code would have reported bytes reclaimed that are
  still on the disk. `gc` counts those separately and says so.
- **`List` reports packed objects**, and reports an object that is in a pack
  and loose exactly once.

### The cutover

`PackWrites` defaults to **off**, and this is the one decision in this plan
made against the plan's own ordering.

A sealed pack is immutable, so an unreferenced object inside one cannot be
deleted; the bytes come back only when compaction rewrites the pack without
them, and compaction is silo#19 and is not built. Turning packs on as the write
path therefore means `silo gc -delete` finds orphans, reports them honestly as
left in place, and frees nothing — a store that churns grows without bound.
Neither step 4 nor step 5 restores that; only silo#19 does.

So the write path lands, is exercised and can be reviewed against a server that
still reclaims space, and the default flips when compaction makes it safe. The
flag is not a tuning knob and no deployment should set it; it is deleted rather
than documented.

### 5. `gc` stops walking the layout

**Built** (silo#29). `gc.go` walked the fan-out with `objstore.LibraryDir` +
`filepath.WalkDir` and deleted with `os.RemoveAll` — the two things
`ObjectStore` does behind the interface. Both now go through the seam, and
`gc.go` no longer imports `os`, `io/fs` or `path/filepath` at all, which is the
check that it stopped.

**Measure needed a new question rather than an existing one.** `List` answers
about *objects* — well-formed ids, plaintext sizes, one entry each — and
`measure` has to report what the delete pass will actually free, which includes
frame overhead, a pack's footer and filter, and the debris of an interrupted
write that a listing skips on purpose. So the seam gained `libraryUsage`, the
pair of `removeLibrary`: on a tier it is a LIST with sizes, which is inside the
four-verb floor. Using the listing's number would have made `silo gc` quote a
figure smaller than the space that came back, every time.

**A regression this uncovered, which had nothing to do with packs.** Routing
`gc` through `ObjectStore` made reclaiming disk require `storage.key`, because
`New` recorded one init error for both the backend and the key. But the key
opens and seals frames; it has no bearing on how many bytes a library occupies
or on deleting them. So the two errors are now separate, and sizes, listings
and deletions are gated on the backend alone. A server that lost its key cannot
read its objects — true and unavoidable — but can still free the disk they sit
on, which is what an operator in that position is trying to do.

**Ingest is dropped.** It was here to migrate a store that already held loose
objects, and there are no installs to migrate: the write path flips before the
first one exists, so no store ever accumulates loose objects that need moving.
What that removes is the writer half — reading each loose object and appending
its frame to a pack. It does *not* remove the loose read lane, which step 3
built and which costs nothing to keep; a development store written before the
flip is deleted rather than converted.

This reverses the argument `storage.md` made for ingest, which was that the
first install would be running the loose store. That is only true if the flag
is still off when it arrives, and the answer to that is to flip the flag, not
to write a migration for a population of zero.

`backup.md` needs a pass too: the object store stops being "an immutable
content-addressed file tree; any ordinary copy tool handles it" and becomes
sealed immutable packs plus an open one — still copyable by any ordinary tool,
but for a different reason and with a different answer about what an
interrupted copy leaves.

## The layout is implemented twice, and that is a debt

`objstore` now knows the on-disk layout in two places: `fsBackend.packPath` /
`libraryPath`, and `pack.go`'s `packDir` with the `os` and `filepath` calls in
`pack.go`, `packseal.go` and `packstore.go`. That is the same fault this plan's
step 5 congratulates itself on removing from `gc.go`, one level further down.

It is already load-bearing rather than merely untidy:

- `libraryUsage` satisfies its contract — every byte `removeLibrary` frees,
  including a pack's footer and filter — only because its `WalkDir` happens to
  descend into `<library>/packs/`. The backend counts bytes it cannot name.
- `removeLibrary` deletes packs for the same reason: a `RemoveAll` over a
  directory the pack layer chose to nest inside.
- Packs are invisible to `fsBackend.list` because that walk skips any entry
  whose name is not exactly two characters. A cross-seam invariant enforced by
  a string length.

The cost lands when a second backend exists — which is what packs are *for*.
Every `os` call in the pack files has to be rewritten, and `libraryUsage` and
`removeLibrary` stop agreeing on anything that is not a directory tree.

**The fix is a narrow file interface handed to the pack layer at construction**
— open, create, append, truncate, readAt, list, remove, scoped to a library —
supplied by the backend, so the layout lives in one place and `libraryUsage`
becomes backend usage plus pack usage rather than one walk that silently covers
both. That belongs with the first real tier, not before it: it is the tier that
supplies the second implementation, and writing the interface without one would
be guessing at its shape.

Recorded here rather than left implicit, because step 3 promised this to step 5
and step 5 did not do it.

## What this plan does not do

- **No tiers.** NAS and S3 follow compaction in
  [`../roadmap.md`](../roadmap.md); the seam Decision 2 leaves untouched is
  what they arrive at.
- **No compaction.** silo#19. This plan produces packs that accumulate dead
  frames and never shrink.
- **No compression.** Measured first, separately.
- **No file-order locality.** `storage.md` asks for new chunks of a file to be
  written contiguously. `objstore` is chunk-level and does not know which file
  a chunk belongs to, so arrival order is what a pack gets, and closing that
  gap needs information from above the seam.
- **No change to the frame**, the id, or anything a client can observe.
