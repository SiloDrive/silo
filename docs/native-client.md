# A Native Silo Client

Silo ships a CLI and a TUI, but neither of them syncs. They drive the
management API (`/api/silo/v1/`) one file at a time, which is a different thing
from what SeaDrive and Seafile Desktop do over the sync protocol.

This document is about closing that gap, in three tiers that can be built
independently and in order. Nothing here is committed.

## What the CLI does today, and why it is not sync

`silo put` maps to `client.UploadFile`: one `PUT repos/{id}/entries/{path}`
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
| `POST /repo/{id}/check-blocks` | which of these block ids do you *not* have? | `server.go:668` |
| `POST /repo/{id}/check-fs` | same question for fs objects | `server.go:666` |
| `POST /repo/{id}/recv-fs` | upload the fs objects it asked for | `server.go:670` |
| `PUT /repo/{id}/block/{id}` | upload one block | see `protocol.md` |
| `PUT /repo/{id}/commit/HEAD` | advance the branch head | see `protocol.md` |

The difference that matters is `check-blocks`. A sync client asks before it
sends; the management API has no way to ask, so it always sends everything.

Note this is a *network* distinction, not a storage one. The server chunks
uploads through `writeChunk` (`fileop.go:2722`) and `blockmgr.WriteBytes`
(`blockmgr/blockmgr.go:66`) skips any block already present, so uploading the
same album twice costs disk once either way. It costs bandwidth twice.

## Tier 1 — recursive put

`silo put -r <repo-id> <local-dir> [remote-dir]`.

A `filepath.WalkDir` in `internal/cli` over the existing `client.Mkdir` and
`client.UploadFile`. No protocol work, no new server endpoints.

What it buys: a supported command instead of a `find -exec` one-liner, with
real error handling and a progress count. Good for one-shot loads and for
scripting (push a generated report into a library from cron).

What it does not buy: anything incremental. Re-running it re-uploads
everything.

## Tier 2 — dedup-aware upload

The interesting tier, and cheaper than it sounds, because of one fact:

**Silo chunks at fixed offsets, not content-defined boundaries.**

`chunkFile` (`fileop.go:2675`) seeks to an offset and reads exactly
`FixedBlockSize` bytes; the caller steps offsets by the same amount
(`fileop.go:2545-2551`). The default is `1 << 23`, 8 MiB
(`option/option.go:221`). So a block id is:

    blockID = sha1(file[offset : offset+8MiB])

There is no rolling hash to replicate and no parameters to match. Any client
can compute the same ids the server would with stdlib SHA-1 and a loop.

That makes this flow available:

1. Chunk locally at the server's `block_size`, SHA-1 each block.
2. `POST /api/silo/v1/repos/{id}/blocks/missing` — the server replies with the
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
- **Encrypted repos need the repo key.** `writeChunk` encrypts and then hashes
  (`fileop.go:2722`), so the block id is the SHA-1 of the ciphertext. Without
  the key a client cannot compute matching ids. Moot in practice: Silo cannot
  create encrypted repos and will not support Seafile's format — see
  `docs/encryption.md`. Worth noting that hashing *after* encrypting is the
  right order and a constraint any future scheme keeps.
- **Fixed chunking is weak against insertions.** Insert one byte at the front
  of a file and every subsequent boundary shifts, so nothing dedups. Fixed
  wins on unchanged and append-only files, loses on edits in the middle. This
  is inherited from Seafile; content-defined chunking would fix it and would
  also change every block id in existence, so it is not a change to make
  casually. The cheaper answer is a delta on the wire rather than new
  boundaries in the store — see the rsync note in
  [`protocol-gaps.md`](protocol-gaps.md).

## Tier 3 — a headless sync agent

`silo sync <repo-id> <local-dir>`.

The full write path: build `Seafile` and `SeafDir` objects, `recv-fs` them,
mint a commit, advance HEAD, and handle the read direction and conflicts.

This is real work, but it is not speculative — the server side is complete and
documented in `protocol.md`, and the tier-2 pieces are prerequisites of it.
What it would give a NAS deployment is a sync agent with no GUI dependency and
no virtual filesystem, which neither SeaDrive nor Seafile Desktop offers.

The hard parts are the ones every sync client has: tree diffing against the
last known commit, deletion and rename detection, conflict resolution when the
remote head has moved, and deciding what to do about files that change while
being read. `fastForwardOrMerge` (`fileop.go:1982`) already implements the
server half of the merge story.

## Recommendation

Tier 2 is built and was the one with the interesting payoff-to-effort ratio;
its prerequisite — reporting the block size — went in with it. Tier 1 is now
the contained afternoon that is left, and it is worth more than it was: a
recursive `put` over the block surface skips everything the server already
holds, so re-running it over a mostly-unchanged tree is cheap rather than a
full re-upload. Tier 3 should not start until someone actually wants a headless
agent badly enough to maintain it — until then, Seafile Desktop is the answer
for ongoing sync.
