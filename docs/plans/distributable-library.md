# Plan: a plain library as a distributable read-only store

**Status: parked, not scheduled.** Recorded because the format already does
most of the work, and it would be a shame to drift away from that by accident.

Moved here from `future-features.md` when that file was replaced by
[`roadmap.md`](../roadmap.md), which holds ordering rather than designs.

## The observation

A plain library is, structurally, what casync was invented to be — a
content-addressed chunk store for distributing large file trees over dumb
transport. Nothing about consuming one read-only needs the Silo server:

- **The store is a static tree**, servable as-is from nginx, an S3 bucket, or a
  torrent. Only writes need Silo; `rsync -a` of the store directory — already
  the documented backup path — is also a mirror.
- **Every object is immutable, so cache lifetime is forever.** The id is the
  ETag and there is no invalidation problem. The only mutable thing in the
  whole system is the branch head: one commit id.
- **Random access needs no server help.** Manifests carry a mandatory
  per-chunk plaintext size beside each chunk id
  ([`spec/store-format.md`](../spec/store-format.md), Manifests), so a lazy
  consumer maps any byte range to the chunks that cover it and fetches only
  those.
- **Updates are deltas for free.** A new version of the tree is a new manifest
  and a few new chunks; mirrors and consumers fetch only what changed. This is
  the property OS-image distribution wants and mostly fakes.
- **The mirror does not need to be trusted.** Everything hashes down to the
  commit id, so a consumer holding one 64-character string verifies the whole
  tree. The plain chunker seed is a public constant precisely so a third party
  can re-chunk source files and reproduce the store independently.

The loss of encryption is not a loss here: content distributed widely is public
by intent, and what distribution actually needs — integrity, and authenticity
of the head — the content addressing already half-provides.

## What would need building, in order of how much it matters

1. **A signed head.** The commit id is the root of all verification, but the
   head pointer is the one thing a mirror can lie about. A detached signature
   over `(library id, branch, commit id)` closes it, and is small enough to
   publish beside the store.
2. **An export command** — `silo export`? — that writes or syncs one library's
   objects into a standalone tree with the head and its signature alongside.
   The store layout is already right, so this is mostly selection and copy.
3. **A read-only consumer**, anything from a fetch-and-verify CLI to a FUSE
   mount. The manifest size fields make the mount cheap in principle.

## Packs change the shape of step 2, not the idea

[`storage.md`](../storage.md) Part 2 replaces the loose layout with packs, at
which point "the store is a static tree of files servable from nginx" stops
being literally true — a consumer needs the per-pack index to find a chunk, and
every pack is ciphertext under `storage.key`.

Neither is fatal and both are worth knowing before this is picked up. The index
is already uploaded beside each pack for exactly this kind of consumer, and an
export is the natural place to decrypt: a distributable copy is public by
intent, so exporting plaintext packs — or loose objects — is a choice the
export command gets to make rather than a property the store has to give up.

## What stays out

Serving this from the Silo API unauthenticated. The wire lane stays
authenticated; distribution happens from an exported copy, on transport built
for it. That split keeps the server's security story one sentence long.

Public *libraries* are a different feature and are not this — they are
authenticated-optional reads from the live server, designed in
[`plans/sharing.md`](sharing.md).
