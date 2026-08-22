# Silo store format v1 — normative specification

Status: **in progress.** This document is the interop contract between the Go
implementation in [`store/`](../../store) and porter-mac's Swift port. It is
being written section by section as [`plans/store-v2.md`](../plans/store-v2.md)
phase 1 lands; sections marked *(not yet specified)* are pinned in the plan but
not yet restated here as bytes.

| Section | State |
|---|---|
| Ids | specified |
| Chunking | specified |
| Manifests | not yet specified |
| Directory and commit objects | not yet specified |
| Content crypto (E2EE) | not yet specified |
| Key wrapping | not yet specified |

Everything here is normative. Where this document and the plan disagree, this
one is wrong and should be fixed — the plan holds the arguments, this holds the
bytes.

## Conformance

Two kinds of rule, tested two ways.

**Vector rules** determine the bytes. Two implementations that disagree about
one produce different ids for identical content and fail *silently* — each
believes the other's library is corrupt. Field order, mask construction,
endianness, and the two off-by-ones in the cut loop are vector rules. The test
is byte-for-byte agreement on [`store/testdata/vectors`](../../store/testdata/vectors).

**Conformance rules** determine what is refused. An implementation with looser
ones computes identical ids and merely accepts objects another refuses. Bounds
checks, reserved-bit rejection, and unknown-version rejection are conformance
rules. The test is a corpus of malformed objects every implementation must
reject.

Some rules are both: writers must emit the canonical form (vector) and readers
must reject its violation (conformance).

## Ids

An id is 32 bytes: `SHA-256` of the thing it names.

- **Chunk id** — `SHA-256(stored bytes)`. The stored bytes are what the client
  sent, before the storage layer wraps them: plaintext in a plain library, the
  client's ciphertext in an E2EE one. The server hashes only what it stores. It
  never computes an id from plaintext, in either library type.
- **Object id** — `SHA-256(encoded object)` for manifests, directory objects
  and commits, over the exact bytes the encoding produces.
- **H_p** — `SHA-256(chunk plaintext)`. Not an id, never used as one; it is the
  input the per-chunk content key derives from. There is no path from a chunk
  id to its H_p, which is why manifests carry a sealed section.

On the wire and in logs an id is lowercase hex, 64 characters. Uppercase is
rejected on parse: one spelling, so a case-folding hop cannot turn one id into
two cache entries.

SHA-256 rather than BLAKE3 because nothing in the system is hash-bound —
ingest is network-capped, the client's change scan is gated on xxh3, scrub is
disk-capped — and SHA-256 is within 8% of itself on every target with zero
dependencies, where Go BLAKE3 spans 8× between arm64 and x86. The measurements
are in [`store-v2-hash-bench.md`](../plans/store-v2-hash-bench.md).

## Chunking

Algorithm id `fastcdc-gear64/v1`. FastCDC with normalised chunking over a
64-bit gear hash, with a keyed gear table.

### Parameters

Per library, frozen at creation, stored in the catalog and served to clients in
the library listing. Never constants compiled into a client.

| Field | Default | Meaning |
|---|---|---|
| `algorithm` | `fastcdc-gear64/v1` | this specification |
| `seed` | see below | 32 bytes; keys the gear table |
| `min_size` | 262144 (256 KiB) | shortest chunk except the last |
| `target_size` | 1048576 (1 MiB) | where the mask changes |
| `max_size` | 4194304 (4 MiB) | forced cut |
| `normalization` | 2 | bits by which the masks differ from the target |

Readers reject parameters outside these bounds: `min_size` ≥ 64,
`min_size` < `target_size` ≤ `max_size` ≤ 1 GiB, `normalization` in 0..3, and
the derived mask widths (below) within 1..64 bits. A library row that violates
any of them is refused rather than chunked, because a chunker nobody else can
reproduce is worse than no chunker.

### Seeds

```
plain library:  seed = SHA-256("silo/chunker/plain/v1")
                     = 6d4c4d55f494657e45d099563ab3a2f1f9b3381b9c17c65b4ac6b438edce1d68
E2EE library:   seed = HKDF-SHA256(CK, salt="silo/chunker/v1", info="", L=32)
```

The plain seed is a constant so cross-library dedup among plain libraries
survives. It is defined as a hash of its own domain string so a port can derive
it rather than transcribe it.

The E2EE seed is what reconciles content-defined chunking with encryption: cut
points become a function of plaintext *and a secret*, so a server holding only
ciphertext cannot fingerprint known files from their chunk-size sequence. It
carries a second job that is easy to overlook — manifests publish `seal_hash`
over a list of H_p values, which is computable from plaintext alone, so if cut
points were public that field would be a CK-free content-confirmation oracle.
It is not one only because reproducing the H_p list means reproducing the cut
points. **An E2EE library must never chunk under the plain seed.**

### HKDF convention

Every derivation in this format is `HKDF-SHA256(IKM, salt, info, L)` with all
four slots stated. The ASCII domain string is always `salt`; the variable input
is always `info`, empty where none is named; `L` is 32 unless stated. A port
that swaps salt and info fails every vector at once, by design.

### Gear table

```
raw  = HKDF-SHA256(seed, salt="silo/gear/v1", info="", L=2048)
gear[i] = uint64 little-endian from raw[8i : 8i+8]      for i in 0..255
```

Little-endian is pinned because it is the failure a port cannot see: a
big-endian reader builds a working chunker that cuts in entirely different
places and produces a library nobody else can read.

Gear table vectors — the SHA-256 of the 256 entries rendered as 16-character
lowercase hex and concatenated, plus the first four entries and the last — are
in `chunker.json` under `gear`.

### Masks

```
target_bits = floor(log2(target_size))              -- 20 for the 1 MiB default
mask_s      = the top (target_bits + normalization) bits of a uint64
mask_l      = the top (target_bits - normalization) bits of a uint64
```

"Top n bits" means `^uint64(0) << (64 - n)`; for the defaults, `mask_s` is the
top 22 bits and `mask_l` the top 18.

The masks take the **high** bits because of how the gear hash accumulates. With
`h = (h << 1) + gear[b]`, bit *j* of `h` has been fed by the last *j+1* bytes,
so the low bits are decided by a handful of bytes and the high bits by a window
of sixty-odd. A mask over low bits would cut on the last byte alone and lose
the content-defined property entirely.

This is the one place the format departs from the FastCDC paper, which
publishes hand-picked masks for an 8 KiB average with their set bits spread
through the middle of the word. Those constants do not generalise to another
target size, and a rule that generalises is worth more than matching a table:
"the top n bits" is a sentence, not a number to transcribe.

### The cut

Given `data`, the remaining bytes of the stream (at least `max_size` of them
unless the stream is ending), the length of the next chunk is:

```
n = len(data)
if n <= min_size:            return n            -- the last chunk may be short
if n > max_size:  n = max_size
normal = min(target_size, n)

h = 0
i = min_size - 1
while i < normal:
    h = (h << 1) + gear[data[i]]                 -- wrapping, mod 2^64
    if h & mask_s == 0:  return i + 1
    i = i + 1
while i < n:
    h = (h << 1) + gear[data[i]]
    if h & mask_l == 0:  return i + 1
    i = i + 1
return n                                          -- forced cut at max_size
```

Three things a port gets wrong silently, all pinned above:

1. **Sub-minimum skipping.** Bytes before `min_size - 1` are not hashed at all;
   the hash starts at zero. This is the paper's optimisation, and it changes
   the boundaries, so it is part of the format rather than an implementation
   choice.
2. **Hashing begins at `data[min_size - 1]`**, so the shortest chunk is exactly
   `min_size` — not `min_size + 1`.
3. **A byte that satisfies the mask ends the chunk it belongs to**, so a hit at
   index *i* yields a chunk of *i+1* bytes.

Addition is `uint64` wrapping addition, not XOR. `h << 1` discards the top bit.

### Stream rules

- Chunking is sequential over the whole file: a boundary depends on every byte
  before it. Hashing the resulting chunks is what parallelises.
- Boundaries do not depend on read sizes. An implementation that buffers
  differently must still produce identical chunks.
- **An empty file yields no chunks**, not one empty chunk. Empty files are
  inlined; a zero-length chunk id would otherwise be a real object that every
  empty file in the library referenced.
- Files under **65536 bytes** (`InlineThreshold`) are not chunked at all —
  their bytes live in the manifest. See Manifests.

### Test vectors

[`store/testdata/vectors/chunker.json`](../../store/testdata/vectors/chunker.json).

Inputs are *described, not stored*, so a port regenerates gigabytes from one
rule instead of us committing them:

- `pseudorandom` with label *L* and length *n*: the concatenation of
  `SHA-256(L ‖ uint64le(i))` for i = 0, 1, 2, … truncated to *n* bytes.
- `zeros` with length *n*: *n* zero bytes.

Each case lists every chunk as `(offset, size, id)`. The `zeros` case exists to
pin the forced cut at `max_size`; the `-e2ee` case pins that a different seed
moves every boundary; `empty` and `inline-sized` pin the short-stream edges.

Regenerate with `go test ./store -run TestVectors -update` — deliberately, with
the diff reviewed. A change to this file is a change to every id in every
library.

## Manifests

*(not yet specified — the byte layout is pinned in
[`plans/store-v2.md`](../plans/store-v2.md#manifests) and lands here next.)*

## Directory and commit objects

*(not yet specified — layouts and flag bits are pinned in the plan.)*

## Content crypto

*(not yet specified.)*

## Key wrapping

*(not yet specified.)*
