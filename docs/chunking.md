# Chunking

**Record.** The design is [`storage.md`](storage.md) § Chunking; the binding
definition — masks, cut rules, stream rules, test vectors — is the Chunking
section of [`spec/store-format.md`](spec/store-format.md), and the
implementation is [`store/chunker.go`](../store/chunker.go). This is why it
was decided.

## Why boundaries are content-defined

The inherited chunker read fixed 8 MiB runs from computed offsets, so a
boundary was a function of *where you are*, not *what is there*. Insert or
remove one byte and every boundary downstream lands on different content;
every chunk id after the edit changes; `chunks/missing` honestly reports a
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

- **The hash is SHA-256**, not BLAKE3 — measured in
  [`plans/hash-choice.md`](plans/hash-choice.md). A non-cryptographic hash was
  never eligible: at a billion chunks XXH3-64's accidental-collision
  probability alone is ~2.7%, and a crafted collision in a content-addressed
  store substitutes content.
- **Parameters live on the library**, where the listing reports them per
  library, frozen at creation. Changing them is still a new store — but with
  porter as the only client, and shipped by us, getting them wrong costs a
  migration rather than a break with software we do not control.
- **The file object is a binary manifest** with `(chunk_id, size)` records —
  casync's `.caibx` shape — rather than JSON.
- **Small files inline** below 64 KiB (`store.InlineThreshold`) rather than
  costing a chunk, a manifest and a round trip each.

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

The second path is `GET entries/{path}?type=manifest` (or `GET objects/{id}`,
the same bytes addressed the other way), then `GET chunks/{id}` for one chunk
or `POST chunks/fetch` for many; see
[`protocol.md`](protocol.md#the-chunk-stream--post-chunksfetch).
