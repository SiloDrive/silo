# Compression

What Silo compresses today, why swapping the algorithm is cheaper than it
looks, and where the actual win is. Nothing here is committed.

## What is compressed today

zlib appears in exactly one package: `fsmgr`. Nothing else in the tree
imports `compress/zlib`.

| | Compressed? | Typical size |
|---|---|---|
| Blocks — file content | **no** | 8 MiB each; the bulk of any repo |
| Commit objects | **no** | small JSON |
| fs objects — `SeafDir`, `Seafile` | zlib, default level (`fsmgr.go:492`) | small JSON |

So compression currently covers **metadata only**. The file bytes themselves
travel and rest uncompressed.

That framing matters before reaching for zstd: swapping the algorithm speeds
up directory-tree traversal, not file transfer.

## Why the format is swappable at all

An fs object's id is the SHA-1 of its **uncompressed** JSON, while the stored
and wire forms are compressed. That is why `VerifyObjectID` (`fsmgr.go:609`)
has to inflate before it can check anything, and why the check is behind
`SILO_VERIFY_FS_OBJECT_HASHES` while the block check is unconditional.

The consequence: **the compression format is not part of object identity.**
Changing it rewrites no ids and invalidates no references.

Better still, objects are immutable and the formats are self-identifying — a
zlib stream starts with a CMF byte of `0x78` for the 32 KiB window deflate
uses, and every zstd frame starts with the 4-byte magic `0xFD2FB528`. So
`uncompress` (`fsmgr.go:443`) could sniff the first bytes and handle both,
letting zlib objects written yesterday and zstd objects written tomorrow
coexist in one store **with no migration pass at all**.

### The ingest wrinkle

`recvFSCB` stores the bytes the client sent, verbatim — `WriteRawIngested`
(`fsmgr.go:658`) verifies against the id and hands the same buffer to
`WriteRaw`. So the on-disk format is whatever the client chose, which for a
Seafile client is always zlib.

Getting zstd on disk from a Seafile client therefore means inflating and
recompressing on ingest: spending CPU on the write path to save it on reads.
That is only worth it if reads dominate writes by a wide margin, and it works
against the "one hash, one inflate" tidying already done on that path.

For a native Silo client (see [`native-client.md`](native-client.md)) both
ends are ours, so the client can send zstd and the server can store it as-is.
That is the case where the swap is free.

## The bigger opportunity: compressing blocks

Blocks are uncompressed, and blocks are where the data is.

The enabling fact is the same one as above, one level down. A block id is the
SHA-1 of the plaintext the *client* computed and named the block with. Nothing
in the protocol requires the server's **stored** form to be those same bytes —
the server currently stores them verbatim as an implementation choice, not a
contract. So blocks could be compressed on disk transparently, with clients
unaware, exactly as fs objects already are.

Worth being honest about the payoff:

- **Text, code, documents, VM images, logs** — a large win.
- **Music, photos, video** — approximately nothing, because the content is
  already compressed, and you burn CPU to discover that. This is very likely
  why Seafile left blocks raw: the median sync workload is media.

If built, it wants a cheap incompressibility check — zstd's own early-exit
heuristic, or simply "if the compressed form is not meaningfully smaller,
store raw" with a marker to say which. It also perturbs the verification
invariants: `WriteVerified` currently hashes the stream *as stored*, and would
have to become "hash the plaintext, then compress". `blockmgr.WriteBytes`
(`blockmgr/blockmgr.go:66`), which derives the id from the content, would need
the same split.

**Measure before building.** The measurement is easy and needs no code: run
zstd over a sample of `{data-dir}/storage/blocks/` and look at the ratio for
the workload actually stored there. If it is 1.02, stop.

## Practical notes

- No zstd dependency yet. `github.com/klauspost/compress/zstd` is the standard
  Go implementation, pure Go with no cgo, so it will not break the static
  `CGO_ENABLED=0` release builds.
- zstd decompresses several times faster than zlib at comparable ratios, which
  is the relevant axis here — fs objects are inflated on read far more often
  than they are compressed on write.
- The per-object allocation on the fs read path has already been addressed by
  sharing one reader across a pack (`recvFSCB`); a zstd decoder is likewise
  worth reusing rather than constructing per object.

## Suggested order

1. **fs objects, zlib → zstd, with format sniffing on read.** Safe, no
   migration, mixed formats coexist. Only worth doing alongside a native
   client that can send zstd, otherwise the ingest transcode eats the win.
2. **Expose the compression choice** the way `FixedBlockSize` should be
   exposed — in `/api2/server-info/` — so a native client knows what to send.
3. **Block compression.** Only after measuring, and only with an
   incompressible-content fast path.
