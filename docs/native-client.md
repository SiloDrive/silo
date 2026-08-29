# A Native Silo Client

> **Partly stale as of `5d4baa0` and the current store format.** Two things below no longer hold.
> The `/repo/*` sync-protocol table in "What the CLI does today" describes
> routes deleted with the legacy lane in `5d4baa0` — nothing to cross to
> any more. And "Tier 2" describes the block surface as it worked under fixed
> 8 MiB SHA-1 chunking (`fileop.go`, `blockmgr/blockmgr.go` — both deleted in
> the same commit); the current format replaced it with content-defined SHA-256
> chunking, so the mechanism `blocks/missing` uses today is not the one
> described here. Kept because the *reasoning* — ask before you send, dedup
> is a network problem not a storage one — is what tier 2 actually shipped on,
> and still holds under the new chunker. See
> [`storage.md`](storage.md) for what's current and
> [`porter-brief.md`](porter-brief.md) for the live wire contract.

Silo ships a CLI and a TUI, but neither of them syncs on its own — they drive
the management API (`/api/silo/v1/`) one file at a time. Early revisions of
this document compared that to what the upstream desktop and virtual-drive
clients did over the legacy sync protocol; that protocol no longer exists
(`5d4baa0`), so the comparison is now historical.

This document is about closing the sync gap, in three tiers that can be built
independently and in order. Nothing here is committed.

## What the CLI does today, and why it is not sync

`silo put` maps to `client.UploadFile`: one `PUT libraries/{id}/entries/{path}`
with the whole file as the request body for a small file, or the block surface
for anything over one block. (It used to mint an upload access token and POST a
multipart body; that was the pre-0.4.0 shape and this document described it for
longer than it was true.) `cmdPut` (`internal/cli/cli.go:148`) takes exactly one
local path and passes it straight through, so a directory argument fails at
`os.Open` with `read <dir>: is a directory`. There is no walk and no incremental
comparison of a tree.

Tier 2 below is largely built, and on the Silo lane rather than through a
crossing to the sync lane — see [`protocol.md`](protocol.md) for the endpoints
and `client/blocks.go` for a working client. What remains is described here as
it was reasoned about, because the reasoning is what the remaining tiers rest
on.

The sync protocol is a different shape. Its write path is a negotiation:

| Endpoint | Purpose | Registered |
|---|---|---|
| `POST /repo/{id}/check-blocks` | which of these block ids do you *not* have? | `server.go:665` |
| `POST /repo/{id}/check-fs` | same question for fs objects | `server.go:663` |
| `POST /repo/{id}/recv-fs` | upload the fs objects it asked for | `server.go:667` |
| `PUT /repo/{id}/block/{id}` | upload one block | see `protocol.md` |
| `PUT /repo/{id}/commit/HEAD` | advance the branch head | see `protocol.md` |

The difference that matters is `check-blocks`. A sync client asks before it
sends; the management API has no way to ask, so it always sends everything.

Note this is a *network* distinction, not a storage one. The server chunks
uploads through `writeChunk` (`fileop.go:2731`) and `blockmgr.WriteBytes`
(`blockmgr/blockmgr.go:66`) skips any block already present, so uploading the
same album twice costs disk once either way. It costs bandwidth twice.

## Tier 1 — recursive put

`silo put -r <library-id> <local-dir> [remote-dir]`. **Landed**, and not in the
shape this section proposed.

The plan here was a `filepath.WalkDir` over `client.Mkdir` and
`client.UploadFile` — no protocol work, and nothing incremental, so a re-run
would re-upload everything. By the time it was built, tier 2 and the batch
surface were already there, so it went straight to composing them
(`client/tree.go`): hash every file in the tree, ask `blocks/missing` about all
of it at once, send only what is new, then create every directory and file in
one `batch`. A folder of five hundred photos is one commit rather than five
hundred, and a re-run transfers nothing.

Dedup is across the tree rather than per file, so a directory holding the same
export twice sends it once.

What it still does not buy is anything incremental in the *tree* sense: a
re-run sends no content, but it does name every file again, so the server
writes a new dirent for each and mints a commit. Skipping files whose content
the library already has at that path means comparing against the remote tree,
which is tier 3's problem.

The whole-file fallback survives for a server that advertises neither surface,
and for an encrypted library, which cannot be assembled from blocks
server-side at all.

## Tier 2 — dedup-aware upload

The interesting tier, and cheaper than it sounds, because of one fact:

**Silo chunks at fixed offsets, not content-defined boundaries.**

`chunkFile` (`fileop.go:2684`) seeks to an offset and reads exactly
`FixedBlockSize` bytes; the caller steps offsets by the same amount
(`fileop.go:2565-2569`). The default is `1 << 23`, 8 MiB
(`option/option.go:221`). So a block id is:

    blockID = sha1(file[offset : offset+8MiB])

There is no rolling hash to replicate and no parameters to match. Any client
can compute the same ids the server would with stdlib SHA-1 and a loop.

That makes this flow available:

1. Chunk locally at the server's `block_size`, SHA-1 each block.
2. `POST /api/silo/v1/libraries/{id}/blocks/missing` — the server replies with the
   ids it needs.
3. Upload only those, then name the whole list in one call.

This is what shipped, and it needs no sync token: the crossing described in
earlier drafts of this document — mint a sync token, cross to
`POST /repo/{id}/check-blocks` on the frozen lane — is gone. A re-run over a
mostly-unchanged tree sends almost nothing.

### Caveats

- **Block size does not have to match for correctness.** A file's fs object is
  a list of block ids, so any chunking produces a valid file. Matching the
  server's `FixedBlockSize` only maximises how much existing server data
  dedups. A mismatch degrades to "upload everything", not to corruption.
  `FixedBlockSize` is configurable (`option/option.go:449`), and `GET
  /server-info` now reports it as `block_size` beside the `features` array —
  that response is already where a client asks what this server will accept,
  and a block size a client has to guess is the one input that silently
  degrades a dedup-aware upload to "upload everything".
- **Encrypted libraries need the library key.** `writeChunk` encrypts and then hashes
  (`fileop.go:2731`), so the block id is the SHA-1 of the ciphertext. Without
  the key a client cannot compute matching ids. That was written when Silo could
  not create an encrypted library at all; it can now, and the constraint became
  the design — see `docs/encryption.md`. Hashing *after* encrypting is the right
  order and the current scheme keeps it.
- **Fixed chunking is weak against insertions.** Insert one byte at the front
  of a file and every subsequent boundary shifts, so nothing dedups. Fixed
  wins on unchanged and append-only files, loses on edits in the middle. This
  is inherited from upstream; content-defined chunking would fix it and would
  also change every block id in existence, so it is not a change to make
  casually. It is nonetheless the change now being argued for — a wire delta
  saves bandwidth and stores the file twice regardless. See
  [`chunking.md`](chunking.md), which supersedes the rsync note in
  [`protocol-gaps.md`](protocol-gaps.md).

## Tier 3 — a headless sync agent

`silo sync <library-id> <local-dir>`.

The full write path: build the manifest and directory objects, upload them,
mint a commit, advance HEAD, and handle the read direction and conflicts.

This is real work, but it is not speculative — the server side is complete and
documented in `protocol.md`, and the tier-2 pieces are prerequisites of it.
What it would give a NAS deployment is a sync agent with no GUI dependency and
no virtual filesystem, which none of the upstream clients offered.

The hard parts are the ones every sync client has: tree diffing against the
last known commit, deletion and rename detection, conflict resolution when the
remote head has moved, and deciding what to do about files that change while
being read. `fastForwardOrMerge` (`fileop.go:1991`) already implements the
server half of the merge story.

## Recommendation

Tier 2 is built and was the one with the interesting payoff-to-effort ratio;
its prerequisite — reporting the block size — went in with it. Tier 1 is now
the contained afternoon that is left, and it is worth more than it was: a
recursive `put` over the block surface skips everything the server already
holds, so re-running it over a mostly-unchanged tree is cheap rather than a
full re-upload. Tier 3 should not start until someone actually wants a headless
agent badly enough to maintain it. No upstream client is a fallback for ongoing
sync any more, since none of them can reach a current Silo server at all;
porter-fuse and Porter (the File Provider client) are the answer today, and
neither offers headless, GUI-free sync to a NAS.
