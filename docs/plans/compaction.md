# Compaction

Status: **all four steps built.** The mark attributed to packs, `silo df
-packs`, the rewrite and its crash rules; `silo gc -compact` with a threshold, a
copy budget and the orphan sweep's two guards; and the cutover, so packs are the
write path with no flag in front of them. Tracked as silo#19 with silo#50,
milestone `compaction`, both closed.

**One thing this plan did not foresee.** Step 3's guards are the sweep's, and
that is right, but a packed store cannot expire history at all: a commit inside
a sealed pack is refused by `Remove`, so the expiry pass leaves it there — and
because it is still on the disk, the mark still reaches it. Every rewrite would
have copied it forward for ever, and step 4's acceptance test would have failed
with no obvious cause. So the cut is handed to `PlanCompaction` as a set of
commits to treat as absent: retention as a decision the disk has not carried
out. See 3b.

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

## Decision 7: a rewrite preserves the physical order of frames

Copying in index order would have been the obvious reading of "walk the pack",
and it is wrong in a way that does not show up in a correctness test. A footer
is sorted by id; ids are SHA-256 and therefore uncorrelated with anything. So
copying in that order scatters a file's chunks uniformly across the rewritten
pack — and because id order is *stable*, every later rewrite preserves the
scattering. One compaction would destroy locality permanently rather than
degrade it gradually.

The physical order is recoverable and costs a sort. Frames were appended in
arrival order and every index record carries its offset, so ordering by offset
reconstructs the layout the pack was written with. The rewrite then keeps
whatever locality the writer achieved, and the survivors end up closer together
than they were, because the dead frames between them are gone. It also turns the
copy into a forward scan of the old pack instead of a random walk over it.

What this does not do is make locality *better*. Chunks of one file split across
two packs when the first sealed stay split, because nothing at this layer knows
which file a chunk belongs to. `storage.md` asks for new chunks of a file to be
written contiguously; that is a property of the *write* path and needs
information from above the seam, and it is the same gap `packs.md` records.

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

**Built.** `silo gc -compact`: for every live library, mark once, measure every sealed
pack against the mark, rewrite the ones past the threshold in the order that
reclaims most per byte copied, and stop when the budget is spent. Dry-run by
default like every other reclaimer here.

**What it stands on is all built.** `PackCensus` is the mark attributed to
packs; `CompactPack` is the rewrite and takes a fresh liveness answer;
`PruneRedundantPacks` is the crash cleanup. Step 3 is the loop that connects
them, and the guards around it. Nothing below opens a frame.

**The two guards are the orphan sweep's, and for the same reasons.** A rewrite
drops every frame the mark did not reach, and a frame nothing reaches is
indistinguishable from one a client is about to commit. So, in the sweep's
order: bump the GC generation *before* the mark, so a head move that read the
old generation is refused (`ErrGCConflict`, a `503` with a retry) and the
client re-uploads rather than publishing a commit whose chunks were rewritten
out from under it; and skip any pack sealed more recently than `-min-age`. The
age guard is on the pack rather than on the frame because a pack's index carries
no times — but every frame in a sealed pack was appended before the footer was
written, so the pack's seal time is a lower bound on every frame's age, which is
the direction the guard needs. The seal time is the file's mtime: the footer is
the last write, and the rename that publishes a rewrite preserves it.

**The liveness answer is history, never head.** `CompactPack` gets `all.has`,
the same set the orphan sweep deletes against. A rewrite that kept only what the
head reaches would expire history without a retention policy having said so.

**`expired` is the parameter this plan was missing.** A commit inside a sealed
pack cannot be deleted where it is, so on a packed store the expiry pass defers
every one of them — and they stay on the disk, and the mark keeps reaching them.
Without naming the cut here, every rewrite copies the expired history forward
and no run ever frees it, which is the whole outcome step 3 is for. So the cut
is a set of commits `reachable` treats as absent: retention as a decision the
disk has not yet carried out. It is computed by the caller from the library's
policy (`retentionCut`), so objmgr is told the answer rather than deciding it,
and `-compact` on its own passes nothing — expiring history is irreversible, and
an operator who typed one flag should not get the other one's consequences.

#### 3a. `PackStat` says which store it came from

`PackCensus` measures both stores and returns one slice, and a `PackStat` does
not say whether its pack is in the chunk store or the object store — which was
fine for a report and is not fine for a caller who has to hand the id back to
the right `ObjectStore`. Add `ObjType` to `PackStat`, set by `PackStats` from
the `ObjectStore`'s own type.

*Built as `ObjType` rather than the `IsChunk` this plan first called for.* A
boolean invented one layer up would have been a field the owning package could
not fill in — every other caller of `PackStats` would get a stat that says
`false` and means nothing — while `objstore` already holds the answer, as
`ObjectStore.ObjType`. `objmgr.storeFor` is the one place that turns it back
into a choice between the two stores.

Test first: a library with a packed chunk and a packed object reports two stats
that disagree on the store they came from. It fails to compile until the field
exists, which is the failing run.

#### 3b. `objmgr.Store.Compact`: one library, one mark, many rewrites

```go
type CompactionPlan struct {
    Candidates []objstore.PackStat // past the threshold, old enough, in run order
    TooYoung   int                 // past the threshold, sealed too recently
    Held       []objstore.PackStat // past the threshold, over budget this run
    marks      *marks               // the walk it was drawn from
}

func (s *Store) PlanCompaction(head store.ID, expired []store.ID, threshold float64, sealedBefore time.Time, budget int64) (CompactionPlan, error)
func (s *Store) CompactPack(p objstore.PackStat, plan CompactionPlan) (objstore.Compaction, error)
```

Two calls rather than one so the CLI can print the plan without acting on it,
and so the marks from the plan are the marks the rewrite re-verifies against —
one walk per library, which is Decision 1. The plan carries those marks rather
than returning them alongside, so the rule is structural: there is no second
walk a caller could hand to `CompactPack` by mistake. `CompactPack` routes on
`p.ObjType` and passes `plan.marks.has(id, …)` as `live`; that closure is the
whole re-verification, and it is the same set the plan was drawn from.

**One walk, not the census's two.** `PackCensus` takes a second walk to split
head out of history because `df` prints that column; nothing in the plan reads
it, so `PlanCompaction` walks history alone and leaves `HeadBytes` unfilled.

**Order: wholly dead packs first, then by dead fraction descending.** A pack
with no live frames is deleted rather than rewritten, costs no I/O, and does not
count against the budget — and a library whose history has just been expired is
mostly this case. Among the rest, dead fraction is reclaimed-per-byte-copied,
which is what a budget measured in bytes copied should be spent by.
`storage.md`'s locality ordering is about remote packs, which Decision 6 defers.

**The budget is live bytes copied per run**, because that is what a rewrite
reads and writes and the dead frames are seeked past. `0` means no budget. A
pack that would exceed what is left is held rather than started, and held packs
are reported so an operator sees a run that stopped short as "stopped short"
rather than as "done".

`PruneRedundantPacks` runs once per store at the start of the library's pass.
It clears the debris of an interrupted rewrite and applies Decision 3, and it
runs *before* the mark so a redundant pack is not measured, scheduled and
rewritten into a third copy.

Tests, each written before the code it exercises and each on a store with
`objstore.Close()` called to seal, the way
`TestExpireDoesNotReportCommitsItCouldNotRemove` does:

- A pack at 0.4 dead is not a candidate at threshold 0.5; at 0.6 it is.
- A wholly dead pack precedes a half-dead one and costs the budget nothing.
- Two candidates and a budget that fits one: one is a candidate, one is held.
- A pack sealed after `sealedBefore` is counted in `TooYoung`, not compacted,
  and its frames are all still readable.
- The plan's marks and the rewrite's liveness agree: expire history, plan,
  compact, and the census after shows `History` down by what the plan said and
  `Head` unchanged. This is the test that would catch a head-only walk.

#### 3c. `silo gc -compact`

Flags: `-compact`, `-compact-threshold` (default `0.5`), `-compact-budget`
(size, default `0` — no cap, because a cron with no budget set should catch up
rather than fall behind; an operator who wants throttling sets one),
`-min-age` shared with `-orphans` since it guards the same race. `-delete` is
what turns the plan into rewrites.

Order inside one invocation: expiry, then the orphan sweep, then compaction,
then dead libraries. Expiry and the sweep leave `ErrReclaimDeferred` frames in
packs; compaction in the same run is what reclaims them, so a run that asked for
all three frees what retention allows in one pass. The "left in place" line
those two print should now end with "run with -compact".

Output per library in a dry run, one line per candidate: pack, dead fraction,
bytes a rewrite would drop, and whether it is held or too young. With `-delete`:
what each rewrite kept, dropped and reclaimed, and a total that is file bytes
freed — the number `df` will change by — not frame bytes.

**Stop the server first, and this time it is a rule and not a warning.** A
sealed pack is opened by path on every read, and the server holds its pack set
in memory: a rewrite from a second process renames a new pack into place the
server does not know about and deletes the one it does, and the next read of a
frame that moved is a `404` to a client that stored it. The lock design in
step 2 — compaction takes its own lock, off the write path — is for compaction
*in the server's process*, which is the scheduler the roadmap lists as unwritten
and which wants a kill switch before it wants code. Until it exists, `-compact
-delete` is an offline operation, said so in the usage text and refused when a
server is running: a deleting pass takes the data directory's `flock`, which
the server holds for its lifetime. That is `data-safety.md` item 6, and it is
what turned the warning `gc -delete` used to print into a check.

Tests: `-compact` without `-delete` reports and leaves every pack file where it
was; with `-delete`, `LibraryUsage` afterwards is smaller by what was reported.
Extend `TestSweepAndExpiryAreIdempotent` to a third mechanism: two consecutive
`-compact -delete` runs, the second does nothing.

#### 3d. The catalog row, and why it waits

`storage.md` puts the mark's output in a `PackStats` catalog row with a `gc_id`.
Step 1 deferred it because nothing read it, and step 3 as planned here still has
no reader: the plan is drawn and acted on in one process, seconds apart, with
the marks in memory. A row is for a *later* process to schedule from, and the
later process is the in-process scheduler. It lands with that scheduler, where
`gc_id` on the row is what makes its staleness detectable. `storage.md` gets a
sentence saying the row arrives with the scheduler rather than with the CLI.

#### Docs step 3 changes

`storage.md` § Reclaiming: the `silo gc` usage line and a paragraph for
`-compact`. `quota.md`'s table of reclaimers gains a row. `df -packs`'s footer
stops saying nothing reclaims dead bytes and names the flag. `cmd/silo` usage.

### 4. Flip the cutover

**Built, silo#50.** `PackWrites` became true by default and then stopped
existing:
`SILO_PACK_WRITES`, the `option.go` warning and the field go, and the tests
that set the option to reach the pack path set nothing. A loose store keeps
reading — the three-place lookup asks the open pack, the sealed packs, then the
loose file — so an existing install carries its old objects forward unpacked and
writes new ones packed, which is what `packs.md` § 5 settled on instead of an
ingest.

**The check is that space comes back**, not that the code runs. The test is the
one `storage.md` has been promising since packs landed: write a tree, overwrite
it, expire history, run `gc -orphans -expire-history -compact -delete`, and the
store's `LibraryUsage` afterwards is within a footer of the head's census — on
a store built by a server that was never told to pack. Written before the flag
flips, it fails because `gc` finds one file per object and no pack to compact;
that failing run is what the flip is answering.

Then the same against a real server, the way the pack work was verified: a
library written through the HTTP surface, `silo df -packs` showing dead bytes,
the server stopped, `silo gc -compact -delete`, `df` showing them gone, the
server started, and every file readable through a client.

**Docs step 4 changes.** `packs.md`'s status line and § The cutover; the
roadmap's storage chain items 2 and 3; `storage.md` § Where the bytes are, if it
still describes loose objects as the write path; this document's status line.
Close silo#19 and silo#50 together — the second is one flag, and it is the
first's acceptance test.

### Out of step 3 and 4, and where each waits

- **The in-process scheduler.** Roadmap § Independent work names it and the
  kill switch it wants first. It is also where the lock design in step 2 starts
  to matter, and where the catalog row (3d) arrives.
- **Everything in Decision 6**: egress budget, remote ordering, eviction,
  undersize merging. Tiers first.
- **Per-frame age.** The pack-level guard is coarser than the sweep's
  per-object one, in the safe direction. A finer guard needs a time in the
  index record, which is a format change for no case anyone has hit.

## What this plan does not do

- **No tiers, no egress budget, no eviction.** Decision 6.
- **No undersize merging.** Decision 6.
- **No refcounts**, now or later. The invariant.
- **No change to the frame, the pack format, or anything a client can observe.**
  Compaction produces packs in exactly the format `packs.md` defined, and moves
  frames between them without opening one.
- **No compaction of the open pack.** Decision 5.
- **No *improvement* of locality.** See below — a rewrite preserves the frame
  order it found, and cannot do better without knowing which file a chunk
  belongs to.
