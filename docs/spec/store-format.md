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
| Encoding primitives | specified |
| Manifests | specified |
| Content crypto (E2EE) | specified |
| Directory and commit objects | specified |
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

## Encoding primitives

### Varints

Unsigned LEB128: seven bits per byte, little-endian, high bit set on every byte
but the last, at most ten bytes.

**Non-canonical encodings are rejected.** A multi-byte varint whose final byte
is `0x00` is padded and must be refused, as must one that overflows 64 bits.
Go's `encoding/binary` accepts both; a strict reader is required here. Without
this rule two writers produce different bytes for the same object, hence
different ids and different sealing keys for identical content — the silent
divergence the format exists to prevent, arriving through its smallest field.

### AEAD frames

Sealed sections and encrypted chunks are **`ciphertext ‖ tag`**, tag 16 bytes,
with a 96-bit **all-zero nonce** that is never stored. Never CryptoKit's
`combined` form, which prepends the nonce: frames would be twelve bytes longer
and every id would differ.

The cipher is AES-256-GCM everywhere.

### Sealing keys

Wherever a sealed section exists:

```
key = HKDF-SHA256(secret, salt=<domain>, info=SHA-256(AD), L=32)
```

`info` is the hash of **exactly the byte range the AEAD authenticates**, as raw
32 bytes with no length prefix. Domains, pinned and mandatory:

| domain | secret | container |
|---|---|---|
| `silo/manifest/v1` | CK | manifest sealed section |
| `silo/dir/v1` | CK | directory object sealed section |
| `silo/commit/v1` | CK | commit sealed section |
| `silo/chunk/v1` | CK | content chunks — **no AD**, see below |

Deriving the key from the AD is what licenses the zero nonce. Because every
public section carries `seal_hash = SHA-256(sealed plaintext)`, the key commits
to the plaintext as well as to the AD, so **no two distinct (AD, plaintext)
pairs can ever share a key** — the condition GCM requires. Hashing anything
smaller than the full authenticated range would key a strict subset of what the
tag covers, and two objects differing only in `version` or `flags` over
unchanged sealed content would meet one key, one nonce and two ADs.

Content chunks are the one deliberate exception: a chunk frame has no public
section and no AD, and distinct plaintexts already give distinct keys. Do not
invent a chunk public section to make it match the others.

## Manifests

A file is a manifest: a binary, versioned, content-addressed object.

```
manifest
version u8   = 1
flags   u8
file_size varint

chunked (flags & inline == 0):
public:  chunk_count varint
         (chunk_id [32]byte, size varint)*    -- file order; size = PLAINTEXT bytes
         seal_hash [32]byte                   -- E2EE only
sealed:  (H_p [32]byte)*                      -- E2EE only; exactly chunk_count of them

inline (flags & inline != 0):
public:  seal_hash [32]byte                   -- E2EE only
sealed:  the file's bytes                     -- E2EE only
         (in a plain library the bytes follow the header directly, unsealed)
```

### Flags

```
bit 0  (0x01)   E2EE      gates every field marked "E2EE only"
bit 1  (0x02)   inline    the file's bytes replace its chunk list
bits 2–7        reserved  must be zero; parsers REJECT if set
```

Reserved bits reject-if-set, or they are unusable forever: an old parser that
accepted an object carrying a future bit would compute an id over it. This is
also what makes a future extension deployable — a new bit makes old clients
refuse cleanly instead of mis-parsing.

**Bit 0 is a consistency assertion, never a layout selector.** The parser takes
the expected library type as an input, from the catalog, and rejects an object
whose bit 0 disagrees *before parsing any field*. Otherwise a server that
flipped the bit would get a parse under the wrong layout before verification
could run.

### Inlining is determined by size, not chosen by the writer

```
file_size <  65536  =>  inline,  and the chunk list must be absent
file_size >= 65536  =>  chunked, and inline data must be absent
```

Both directions are enforced on read. This is a **vector rule**: if two clients
could disagree about a 30 KB file, they would mint two manifest ids for
identical content, dedup would miss and `changes?since=` would report a
modification that did not happen. A manifest is a function of its file's
content and nothing else.

### Field rules

- `size` is the chunk's **plaintext** length, because mapping a read offset to
  a chunk needs it. The client-visible wire size is derivable: +16 under E2EE
  for the content tag, +0 plain. Storage framing and the pack frame header are
  server-internal and never enter client arithmetic.
- Per-chunk sizes are mandatory. Under content-defined chunking a client cannot
  map an offset to a chunk without them.
- **The reference skeleton is public even in E2EE libraries.** Chunk ids and
  sizes stay plaintext; only secrets seal. This is what lets the server
  function: GC's mark phase traces commits → directories → manifests → chunk
  ids, and `changes?since=` diffs by id. Neither can walk a graph it cannot
  read.
- **The sealed section exists because ids alone cannot decrypt.** A reader
  holds `chunk_id = SHA-256(ciphertext)` and needs a key derived from
  `H_p = SHA-256(plaintext)`. There is no path from one to the other, so the
  sealed section carries H_p per entry — which also lets the reader verify the
  plaintext after decrypting it.
- `chunk_count` ≥ 1 for a chunked manifest, each `size` ≥ 1, and the sizes must
  total `file_size`. All three reject on mismatch: they are redundant with each
  other by design, so that two parsers cannot disagree about a malformed object
  with only one of them refusing it.
- **The sealed section carries no length of its own.** Its plaintext length is
  fixed by the public section — `32 × chunk_count` chunked, `file_size` inline
  — so the encoded sealed section is exactly that plus the 16-byte tag, and
  trailing bytes have nowhere to hide. A length field would be a second
  encoding of a fact already in the object.
- After opening, the reader checks `SHA-256(sealed plaintext) == seal_hash`.
  This is redundant against the AEAD, which authenticates `seal_hash` as part
  of the AD — but it catches a broken *writer*, which the tag never can.

### The authenticated range

**AD is a byte range, not a concept**: byte 0 through the end of the public
section, header included. `flags` selects inline-vs-chunked parsing and
`file_size` sizes reads; leaving either unauthenticated hands the attacker the
parser.

Because the key derives from the AD, editing a chunk id, a size, or the header
does not merely fail the tag — it fails to produce a key that could ever have
made that ciphertext. Ordering and membership integrity live here; chunks
themselves stay position-free.

### Bounds

An encoded manifest is at most **1 GiB**, checkable from `Content-Length`
before a byte is parsed or allocated. `file_size` is at most 2^48. A
`chunk_count` larger than the object could hold is refused before anything is
sized from it.

The 1 GiB figure is a **format** ceiling — what a file may be — not a memory
promise. Manifests do not segment and their size is linear in the file's
(~67 bytes per chunk, ~67 MB per TB at the 1 MiB target), so a very large file
is a large immutable object rewritten in full on every edit. The answer for
those is a streaming parse, not a smaller constant. In practice manifests get
uncomfortable in the low tens of GB per file; segmentation is the named fix if
that ever matters.

## Content crypto

Per-library, chosen at creation, default on. A plain library's chunks are
stored as plaintext (under the server's storage-layer encryption, which is
invisible to clients); an E2EE library's are encrypted client-side first.

### Chunks

```
H_p   = SHA-256(plaintext_chunk)
K_c   = HKDF-SHA256(CK, salt="silo/chunk/v1", info=H_p, L=32)
frame = AES-256-GCM(K_c, zero nonce, plaintext_chunk)     -- no associated data
id    = SHA-256(frame)
```

Convergent on purpose: identical plaintext under the same content key yields an
identical frame and a stable id, so delta sync and within-library dedup work
exactly as they do in a plain library. Nonce reuse is safe for the reason it is
safe nowhere else — `K_c` is a function of the plaintext, so each key encrypts
exactly one plaintext, ever.

What this costs is a content-confirmation oracle, extending only to holders of
CK — the library's own members. Accepted deliberately. Cross-library and
cross-user dedup end for E2EE libraries, since a different CK gives a different
frame; that was always the price of E2EE.

Note the asymmetry it creates for clients: in an E2EE library a chunk cannot be
*named* without being encrypted first, so `blocks/missing` skips uploads but
never crypto.

### Chunker seed

An E2EE library's gear seed is `HKDF-SHA256(CK, salt="silo/chunker/v1",
info="", L=32)`. See Chunking; an E2EE library must never chunk under the plain
seed.

### Test vectors

[`store/testdata/vectors/objects.json`](../../store/testdata/vectors/objects.json),
covering chunk frames and encoded manifests in both library types. Inputs are
described exactly as in `chunker.json`; the content key is the literal ASCII
string in `content_key_utf8`. Objects small enough to compare byte for byte
carry `encoded_hex`; the rest are pinned by `object_id`.

## Directory and commit objects

Both take the same public/sealed split as manifests, for a reason that is not
obvious: per-object associated data defends the *inside* of one object, and a
tree is attacked *between* objects. Without a whole-object seal, Option A names
are deterministic library-wide — `Enc("taxes.pdf")` is the same ciphertext in
every directory — and a malicious server could swap the child id beside a name,
or graft a name/id pair from one directory into another, with every object
still verifying internally.

### Byte layouts

```
directory object
version u8 = 1 | flags u8 | entry_count varint
public:  dir_salt [16]byte                       -- E2EE only
         (child_id [32]byte, type u8,
          name_len varint, name bytes,
          mtime varint, mode varint)*            -- mtime, mode: PLAIN only
         seal_hash [32]byte                      -- E2EE only
sealed:  (mtime varint, mode varint)*            -- E2EE only; exactly entry_count pairs

commit object
version u8 = 1 | flags u8
public:  root_id [32]byte
         parent_count varint, (parent_id [32]byte)*
         created_at varint
         author_len varint, author bytes,
         msg_len varint, msg bytes               -- PLAIN only
         seal_hash [32]byte                      -- E2EE only
sealed:  author_len varint, author bytes,
         msg_len varint, msg bytes               -- E2EE only; always present,
                                                 --   both fields may be empty
```

Field order **is** the AD. Two implementations that order the public section
differently produce different keys and different object ids for identical
trees, and the failure surfaces as "the Swift client cannot read anything the
Go client wrote".

### Flags

Bit 0 (E2EE) as for manifests. **Bit 1 is the manifest's inline flag and is
reserved here**; bits 1–7 must be zero and parsers reject if set.

**E2EE is one bit, not a family.** On a commit, bit 0 alone drives all three
placements — `seal_hash` present, author/message sealed, author/message absent
from the public section. On a directory it alone drives `dir_salt`,
`seal_hash`, name encryption, and mtime/mode placement. Spelled as independent
flags, a port could encode an impossible object — E2EE set, author public —
that still parses and still hashes.

### Entries

- **`type` is pinned: 0 = file, 1 = directory, 2 = symlink. Anything else
  rejects.** The field is server-consumed, not client metadata: GC's mark must
  know whether a child id names a manifest or a directory object to continue
  the walk — 0 and 2 name manifests, 1 names a directory, a total
  classification of every edge — so it can never be sealed or demoted later.
  Reject-on-unknown is the walk's correctness condition, not parser hygiene.
- A symlink's target bytes are the child manifest's content.
- `name` is the **raw** bytes: the plaintext name in a plain library, the raw
  AES-SIV ciphertext in an E2EE one. **Never base64.** base64url unpadded is
  the URL encoding for `entries/{path}` only; a port that base64s the name into
  the object produces different bytes, a different key, and a different id for
  an identical tree.
- `name_len` ≤ 255, and a zero-length name rejects.
- **Entry order is strictly increasing bytewise by `name`** — not merely
  sorted. A duplicate name is therefore unrepresentable, rejected by the same
  single pass that validates canonical order, at no extra cost. Writers must
  emit this order (vector); readers must reject its violation (conformance).
- `mode` carries **permission bits only**; values above `0o7777` reject. File
  type lives in `type`, never in mode, or two encodings of one fact reappear
  one field over. **A type-2 entry carries `mode = 0o777`**: writers emit it,
  readers reject anything else. Symlink permissions are noise the operating
  systems disagree about, and a varying value would mint divergent ids for
  identical trees.
- `mtime` and `created_at` are **unix seconds, UTC, in [0, 2^34)** — through
  roughly the year 2514. **Writers clamp** into range (pre-epoch mtimes exist
  on real disks); **parsers reject** anything outside it. One rule for the
  whole format: two clamping stories for one data type would be a port trap.

### Sealed sections

- A directory's sealed plaintext is `(mtime, mode)` pairs in entry order,
  **exactly `entry_count` of them**, with trailing bytes rejected. No server
  can forge that mismatch — it lives under the seal — but a buggy writer can,
  and two ports must refuse it identically rather than one indexing past the
  end and one silently truncating.
- Mtimes and modes are sealed under E2EE because per-file activity timing is a
  leak the threat model does not concede. They are sync *state*, not display
  garnish: a client must answer `getattr` with a real `st_mtime`, and a chmod
  that does not propagate is the same silent divergence one field over.
- **An empty directory has an empty sealed section**, so its `seal_hash` is
  `SHA-256("")`. This is safe because the key still derives from the public
  bytes, and `dir_salt` makes every empty directory distinct from every other.
- **A commit's sealed section is always present** and always carries both
  fields, even when both are empty — one shape rather than two, so no port has
  to decide what an absent sealed section would mean.
- **The root id is public in both library types, never sealed.** It has to be:
  the root is the first edge of every server-side walk — GC's mark,
  `diff.DiffCommitRoots`, `changes?since=` — and a sealed root would leave the
  server unable to trace an E2EE library at all. The anti-splice property
  survives because the seal *binds* the root rather than hiding it: root and
  parent chain sit inside the one authenticated range. The splice worked only
  while a sealed blob was portable between commits.

### `dir_salt`

16 bytes, generated once at directory creation and **carried forward on every
rewrite** — the writer reads the current object first, which it must anyway,
and copies the salt.

A fresh salt per write would change the directory's id on every write, hence
every ancestor's id, hence the root: `changes?since=` would report the entire
tree modified on every commit. Consequence to accept: two clients independently
creating the same path produce different salts, so directory merge picks a
winner and re-encrypts the loser's names.

### Bounds

Encoded directory ≤ **256 MiB**, encoded commit ≤ **128 KiB**, both checkable
from `Content-Length`. `parent_count` ≤ 16, `author_len` ≤ 255, `msg_len` ≤
65536.

An entry-count cap is deliberately *not* a bound: a million entries spans 41 to
225 MB depending on name lengths, so it would not bound allocation, and
million-entry directories are legitimate (maildirs, generated data sets).
`entry_count`'s only job is the consistency check.

### The root directory's own metadata

Every node's mtime and mode live in its parent's dirent, and the root has no
parent. So: **root mtime = the commit's `created_at`**, and **root mode =
`0o755`**. Left unpinned, Go returns zero, Swift returns "now", and `ls -ld` on
a mount point disagrees with itself across platforms.

### Test vectors

`directory` and `commit` in
[`objects.json`](../../store/testdata/vectors/objects.json), each with full
`encoded_hex`. The conformance check is round-trip: decode the committed bytes,
re-encode, and require the same bytes back — which exercises the decoder
against bytes the build did not just write.

## Client rules that ride on the format

- **porter-fuse mounts `nosuid,nodev` by default.** The format keeps all twelve
  mode bits, and this is why that is safe: in a plain library mode is public
  and server-writable, so a hostile server can set `04755` on any file and a
  faithful client would restore it — a local privilege-escalation path handed
  to exactly the party the threat model calls actively malicious for integrity.
  `nosuid` makes the restored bit inert. Under E2EE the same server cannot
  touch mode at all, since it is sealed.
- **A client never reads a server-synthesized view of an object.** E2EE clients
  read the directory *object*; a rendered listing would bypass the tag. The
  advisory size sidecar accompanies the object, never replaces it, and is never
  trusted for sync or allocation decisions.

## Key wrapping

*(not yet specified.)*
