# Plan: replication, and parity

**Status: not built, and parked deliberately.** Nothing here is scheduled, and
the first step is worth more than everything under it combined.

Moved here from `future-features.md` when that file was replaced by
[`roadmap.md`](../roadmap.md), which holds ordering rather than designs.
[`target.md`](../target.md) names replication with a single authority as a genuine
stretch goal rather than a non-goal, and asks that nothing in the target make
it harder later; this is what that would look like.

## The engine is already running

The store is git-shaped. `commitmgr.Commit` carries both `ParentID` and
`SecondParentID`; `mergeTrees` (`merge.go:21`) is a real three-way merge over
base, head and remote roots; `fastForwardOrMerge` (`fileop.go:1991`) mints
merge commits with a second parent and CASes the branch forward. That engine
already runs on every concurrent client write. Pointing it at a peer rather
than at a desktop client is a smaller change than "multi-server sync" sounds.

The one genuinely missing piece is **the merge base**. Today the client hands
in `base`, because it knows what it last saw. Two servers have to compute their
common ancestor themselves, by walking the commit DAG — a `git merge-base` walk
over parent pointers.

## Two products, not one

- **Primary plus N secondaries is durability.** Secondaries pull the DAG and
  the objects and never mint commits. A replica that cannot write cannot
  conflict: no merge base to find, no conflict files, no clock skew. Failover
  is permission to write. **This is the piece actually worth building, and it
  needs none of the merge machinery above.**
- **Two people mirroring each other is collaboration.** That one is genuinely
  bidirectional, and Seafile's answer is already implemented and is the right
  one: never block. Both edits survive and one is renamed
  `foo (SFConflict user time)` (`merge.go:348`) — convergence by making the
  conflict visible rather than by choosing a winner.

Bundled, they give a system that is good at neither. The replica is also the
transport for the mirror, so build it first and stop there if nothing else is
wanted.

## What a secondary needs, and it is not only bytes

A read-only secondary needs three things, and only one of them is solved by
[`storage.md`](../storage.md)'s tiering work:

1. **The packs.** Solved, and cheaply: packs are immutable and byte-identical
   on every tier under `storage.key`, so replication is plain file copy. A
   secondary configured with a nearby cache tier and a shared authoritative
   tier gets locality with no new mechanism — see storage.md § Durable tiers.
   Per-pack indexes are uploaded beside their packs, so bootstrap fetches a few
   hundred KB per pack rather than the pack.
2. **The catalog.** Not solved and not designed. Libraries, branches,
   membership, chunker parameters and totals live in SQLite, and a secondary
   that cannot read those cannot serve anything, however many packs it holds.
   This is the actual open problem, and it is why "point a replica at the same
   bucket" is not a replication design.
3. **Credentials.** A secondary that answers requests has to resolve tokens,
   which means the credential tables travel too — which also settles the
   redirect question below.

## Writes go to the primary, and the client is told so

A secondary refuses writes. Two ways to route them, and the difference is
whether the secondary is on the write path:

**Redirect.** `307`, which preserves method and body. The client caches the
answer and writes to the right place thereafter.

**Proxy.** The secondary forwards the write. Simpler for the client, and it
puts a read-only replica in the middle of every write, which is the thing the
split existed to avoid.

Redirect is preferred, with one rule: **the cached answer must be temporary,
never permanent.** Failover is permission to write moving, so the write
location is mutable by design, and a `308` cached at a client is a stale write
target the day the secondary is promoted. The shape that holds is
`server-info` naming the write endpoint — it is already the discovery surface,
with a `features` array — the client caching *that*, and `307` as the
correction when its cached answer has gone stale.

Either way the credential has to be valid where the write lands, which is
requirement 3 above. There is no routing trick that avoids replicating the
credential tables.

## Parity fits here better than it fits SnapRAID

SnapRAID's parity is a snapshot: change a file and the parity is stale until
the next `snapraid sync`. That is the whole reason it is only recommended for
media libraries — the tool is confined to static data by its own design.

Content addressing removes that constraint. Chunks are immutable, so an edit
writes new chunks and leaves the old ones untouched, and parity computed over
chunks is never invalidated by a write. Only deletion disturbs it, which makes
GC rather than editing the thing parity has to be designed around.

Which lands it on the compactor. [`chunking.md`](../chunking.md) already
concludes that packing and the mark-phase GC are one project; parity stripes
are the same unit at the same layer. **Parity over packs rather than over loose
chunks** makes a sealed pack the stripe and compaction the only event that
recomputes anything.

## What stays out

A group where everyone holds a fraction plus parity and still reads locally is
two incompatible wishes. Holding 1/N of the chunks means fetching the rest from
peers, which means being online: a cache with an exotic backing store, and a
worse experience for a media library than simply storing it. Full
peer-to-peer parity also needs agreement on which chunks sit in which stripe,
and shared mutable state across untrusting peers is consensus — at which point
this is no longer a binary anyone can explain.

The cheap version keeps an authority. The primary owns the stripe map and hands
secondaries their assignments: subset replicas, no consensus, and the
durability that matters — a member's disk can die without every member holding
everything.

Erasure coding is a separate refusal and a settled one: [`target.md`](../target.md)
drops it on the grounds that replication buys the same durability at household
scale for the price of disk, which is the cheapest thing in the system.
