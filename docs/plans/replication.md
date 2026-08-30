# Plan: replication, and parity

**Status: not built, and parked deliberately.** Nothing here is scheduled, and
the first step is worth more than everything under it combined.

[`target.md`](../target.md) names replication with a single authority as a genuine
stretch goal rather than a non-goal, and asks that nothing in the target make
it harder later; this is what that would look like.

## There is no merge engine, and a secondary does not need one

The store is git-shaped: commits point at trees, and a branch is one head
pointer. The server does not merge. `PUT head` is a compare-and-swap on the
branch head — a writer whose `If-Match` is stale gets a refusal and rebuilds
its change on the new root — because merging trees means reading names, which
an E2EE server cannot do
([`porter-brief.md`](../porter-brief.md) § Server-side merge is gone,
[`protocol.md`](../protocol.md)). Every merge happens in a client that holds
the keys.

That settles the shape of replication before it starts. A secondary that
never mints commits has nothing to merge: it copies a DAG whose only mutable
point is the head, and the head moves by the same compare-and-swap it moves by
today. Anything bidirectional would need a merge base computed by walking the
DAG — a `git merge-base` over parent pointers — *and* a merge, which the
server cannot perform. That is a client's job, and it is already the client's
job.

## Two products, not one

- **Primary plus N secondaries is durability.** Secondaries pull the DAG and
  the objects and never mint commits. A replica that cannot write cannot
  conflict: no merge base to find, no conflict files, no clock skew. Failover
  is permission to write. **This is the piece actually worth building, and it
  needs none of the merge machinery above.**
- **Two people mirroring each other is collaboration.** That one is genuinely
  bidirectional, and the only place it can be resolved is a client holding the
  keys. The right rule there is upstream's: never block. Both edits survive and
  one is renamed to a visible conflict file — convergence by making the
  conflict visible rather than by choosing a winner. Nothing server-side
  implements that, and this plan does not ask it to.

Bundled, they give a system that is good at neither. The replica is also the
transport for the mirror, so build it first and stop there if nothing else is
wanted.

## What a secondary needs, and it is not only bytes

A read-only secondary needs three things, and only one of them is answered by
[`storage.md`](../storage.md)'s tiering design — which is designed, not built:

1. **The packs.** Answered on paper, and cheaply: packs are immutable and
   byte-identical on every tier under `storage.key`, so replication is plain
   file copy. A secondary configured with a nearby cache tier and a shared
   authoritative tier gets locality with no new mechanism — see storage.md §
   Durable tiers. Per-pack indexes are uploaded beside their packs, so bootstrap
   fetches a few hundred KB per pack rather than the pack. None of this exists
   yet: packs, `storage.key` and durable tiers are designed, not built, and the
   store today is loose objects.
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
recomputes anything. Packs are designed, not built, so parity waits on them.

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
