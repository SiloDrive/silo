# Compaction

Status: **steps 1 and 2 built.** The mark attributed to packs, `silo df -packs`,
the rewrite, and the crash rules. Step 3 (threshold, budget, `gc` wiring) and
step 4 (the cutover, silo#50) are ahead. Tracked as silo#19, milestone
`compaction`.

Owned by [`../storage.md`](../storage.md) § The tracing mark, and compaction,
which is normative for the mark's rules and the scheduler's economics. This
document owns the *build*: what order, what each step can be tested against on
its own, and which decisions were made here rather than there.

## Why, in one paragraph

Because [`packs.md`](packs.md) produced a store that cannot free space. A sealed
pack is immutable — that is what lets a tier replicate it as a file copy and
what makes "reclaim by rewrite, never by hole reuse" a rule rather than a
preference — so an unreferenced frame inside one cannot be deleted. `Remove`
says so honestly with `ErrReclaimDeferred` and `silo gc` counts it apart from
what it actually freed, but honest is not the same as reclaimed. Until
compaction exists, packs cannot become the write path (silo#50), which means
every install still writes one file per object and the whole reason packs were
built goes unrealised.

## The invariant this plan is built to keep

**Liveness is a tracing mark and is never refcounted.** `storage.md` states it
twice, and it is the one thing here that cannot be softened later: a refcount is
wrong the first time two libraries share a chunk, wrong again the first time a
commit is expired, and wrong silently. Every number this plan produces is
derived from a walk of what commits actually reach.

The corollary is that **`PackStats` is a cache and is stale by construction.**
It is a scheduling input — good enough to decide which pack is worth rewriting —
and never the authority for deleting anything. A sweep re-verifies before it
acts, for the same reason `gc -orphans` has an age guard: an object nobody
reaches is indistinguishable from one that is about to be committed.

## What is already true, and worth not re-deciding

- **The mark exists.** `objmgr.Store.reachable` walks commits, trees, manifests
  and chunks; it short-circuits on any id already seen, so a hundred commits of
  a large library cost little more than one. It is **key-free**, so it runs on
  an E2EE library the server cannot read — which is exactly the library most
  likely to be large.
- **`marks` already keeps chunks and objects apart**, because an id is only
  unique within its store.
- **A sealed pack can enumerate itself without being read.** `sealedPack.entries()`
  decodes the footer index; ids, offsets and lengths, no frame content, no
  decryption. That is what makes attributing the mark to packs cheap.
- **A frame is copied, never re-sealed.** The same property that made ingest a
  copy makes a rewrite one: `readFrameAt` gives bytes that `append` takes.
  Compaction never needs `storage.key`.
- **`gc` already owns the vocabulary** — generations (`GCID`), the age guard,
  dry-run-by-default, and per-library sweeps that do not strand each other.

## Decision 1: the mark is per store, and packs are an attribution of it

Not a walk per pack. One reachability walk gives a set of live ids; a pack's
index gives the ids it holds; the intersection gives `live_bytes`. So the cost
is one mark per store plus one index walk per pack, and no pack's *contents* are
read at all.

That ordering also makes the two numbers `storage.md` asks for fall out of one
walk rather than two. `reachable(head, withParents:false)` is head; `true` is
all of history. A pack's `head_bytes` and `live_bytes` differ by exactly what
history is keeping alive in that pack, which is the number retention trades
against.

**`head_bytes` is computed now even though eviction is not built.** It is the
same walk, the marks are already in memory, and computing it later would mean
walking twice for a column that could have been filled the first time.

## Decision 2: compaction rewrites into a temporary name and publishes by rename

The naive swap — write the new pack, delete the old — has a failure that is not
corruption but is unbounded: a crash between the two leaves both, holding the
same frames, and *both still look live*, because liveness is a property of an id
and not of a pack. Re-running compaction then produces a third copy.

Worse, a new pack written the ordinary way is an **open** pack, with a sidecar.
`loadPackSet` refuses a library with two open packs — correctly, since that is
the signature of two writers — so a compaction sharing the writer's naming would
collide with the write path rather than merely waste space.

So a compaction writes `<id>.pack.tmp` and `<id>.idx.tmp`, which `loadPackSet`
ignores, and publishes by renaming both into place once the pack is sealed. The
old pack is the authority until that rename. An interrupted compaction leaves
debris under a name nothing loads, and the next run deletes it — **roll back,
never roll forward**, which is available here precisely because the old pack was
never touched.

## Decision 3: a pack whose every id lives elsewhere is redundant, and that is the crash rule

After the rename there is a window where both packs exist and hold the same
frames. Deleting the old one closes it; a crash inside it does not.

The cleanup rule needs no journal and no marker, because the packs can answer
the question themselves: **a pack every one of whose ids is present in another
pack is redundant and can be deleted outright.** That is decidable from the
indexes alone — no frame is read, no key is needed — and it is true whether the
duplication came from an interrupted compaction, from two writers racing past
dedup, or from anything else.

Stating it as a property of the store rather than as "finish what compaction
started" is what keeps it from being a second mechanism with its own failure
modes.

## Decision 4: the pack set is updated add-then-remove

A lookup asks the open pack, then every sealed pack. If the old pack left the
set before the new one entered it, a concurrent read of a live frame would miss
in the window between — and a miss is not a retry, it is a 404 to a client that
believes it has that chunk.

So: publish the new pack, add it to the set, remove the old from the set, delete
the old from disk. Every step leaves at least one pack holding every live frame.

## Decision 5: only sealed packs are compacted, ever

The open pack is the writer's, and it is the one file in the store with an
append offset somebody is holding. Compaction never reads it, never rewrites it
and never waits on it. A frame that ought to be compacted out of the open pack
becomes eligible when the pack seals, which is at most `packMaxAge` away.

This is what keeps compaction off the write path's locks entirely.

## Decision 6: what is deferred, and why it cannot be built yet

`storage.md` describes a scheduler with two budgets and a locality ordering.
Most of that is about **tiers**, and there are none:

- **The egress budget and remote-pack ordering.** The economics argument —
  one-time fetch against a perpetual monthly cost, so deprioritised but never
  starved — is right, and it prices a decision that does not exist until a pack
  can live somewhere that charges for reads. Building the budget now would mean
  testing it against a cost model with no backend behind it.
- **Eviction order.** `head_bytes` is recorded (Decision 1) so the input is
  there, but what it orders is which packs leave the local cache, and there is
  no second tier for them to leave *to*.
- **Merging undersized packs.** `storage.md` puts it "at the lowest priority
  there is", and it is a nuisance rather than a leak. It is the same machinery
  with one more scheduling input, and it should arrive when there is a real
  store with real undersized packs to measure, not before.

Deferring these is the same judgement that made `packs.md` tractable: build what
a running server can exercise, and record the rest as a named waiting item
rather than as speculative code with no test.

## Work order

Each step is separately testable and leaves the tree working. Nothing here
changes what a client can observe.

### 1. `PackStats`: attribute the mark to packs

**Built.** `objstore.PackStats` intersects a caller's liveness answer with each
sealed pack's index; `objmgr.PackCensus` supplies that answer from the same walk
`Census` uses, so the two are two attributions of one mark rather than two
estimates. Surfaced as `silo df -packs`.

**No catalog table yet.** The plan called for one, and a table nothing reads is
the speculative code this plan set out to avoid: `PackStats` is a cache for a
*scheduler*, and the scheduler is step 3. It lands there, with the `gc_id` that
makes a row's staleness detectable.

Testable on its own against a store with known contents: write files, expire
some history, assert the three numbers partition the pack the way `Census`
already asserts they partition the store.

### 2. Rewrite one pack

**Built.** `CompactPack` re-verifies liveness, copies the live frames into a
temporary name, seals, renames, swaps the set and deletes the old pack.
`PruneRedundantPacks` is the crash cleanup: it clears temporary debris and
applies Decision 3.

Two cases turned out to be worth special-casing rather than falling out:

- **A wholly dead pack is deleted, not rewritten** into an empty one. A library
  whose history has just been expired produces exactly that, so it is the common
  case rather than a corner.
- **A fully live pack is left alone.** Rewriting it would copy the whole thing
  to produce an identical pack under a new name.

**The swap is one lock hold, which is stronger than Decision 4 asked for.** The
new pack is appended and the old removed under a single `mu.Lock`, so no reader
sees a moment with only one of them in either direction — not merely never a
moment with neither.

**Compaction takes its own lock, not the write path's.** A rewrite copies up to
a whole pack; holding the lock uploads take would stall every write for that
long. It touches only sealed packs, which the writer never writes to, so the two
need no mutual exclusion — only rewrites need excluding from each other.

The redundancy rule's failure mode is the one worth naming: two packs holding
exactly the same ids are *both* redundant by the plain reading, and deleting
both destroys the data. A pack gives up its claim on its ids before it is
removed, so the second is no longer redundant by the time it is considered. The
test for it deletes both when that line is taken out, and reports the objects as
missing.

### 3. Threshold, budget, and wiring into `gc`

Compact a pack when its dead fraction crosses 0.5, rate-limited by an I/O budget
per interval. `silo gc -compact` to run it by hand, dry-run by default like every
other reclaimer here.

### 4. Flip the cutover

silo#50: `PackWrites` defaults true, `SILO_PACK_WRITES` and the flag are
deleted. The check is not that the code runs — it is that `silo gc -delete` on a
pack-backed store frees space, verified against a real server the way the pack
work was.

## What this plan does not do

- **No tiers, no egress budget, no eviction.** Decision 6.
- **No undersize merging.** Decision 6.
- **No refcounts**, now or later. The invariant.
- **No change to the frame, the pack format, or anything a client can observe.**
  Compaction produces packs in exactly the format `packs.md` defined, and moves
  frames between them without opening one.
- **No compaction of the open pack.** Decision 5.
