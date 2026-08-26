# Silo store format v1 — normative specification

Status: **complete.** This document is the interop contract between the Go
implementation in [`store/`](../../store) and porter-mac's Swift port. Every
byte [`../storage.md`](../storage.md) describes is restated here exactly.

| Section | State |
|---|---|
| Ids | specified |
| Chunking | specified |
| Encoding primitives | specified |
| Manifests | specified |
| Content crypto (E2EE) | specified |
| Names (AES-SIV) | specified |
| Directory and commit objects | specified |
| Key wrapping | specified |

Everything here is normative. Where this document and
[`../storage.md`](../storage.md) disagree, this one is wrong and should be
fixed: that document holds the arguments, this one holds the bytes.

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
are in [`../plans/hash-choice.md`](../plans/hash-choice.md).

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

### Reading without a key — the server's whole view

Every object type has a public section that parses with no content key, and
together they are exactly what a server can see of an end-to-end encrypted
library: **chunk lists, tree edges, roots and parents. Enough to store, serve,
trace and reclaim a library; not enough to read one.**

That is a design commitment, not a leak. A server that could not enumerate an
E2EE library's chunks could never reclaim one, and a server that could not walk
from a commit to those chunks could not enumerate them at all — the walk runs
commit → directory → manifest → chunk, so a key-free reader is needed at every
step or at none.

**Manifest.** The public section is byte-identical in shape across both library
types up to the end of the chunk list:

```
version u8 | flags u8 | file_size varint | chunk_count varint | (id[32], size varint)*
```

Yield: file size and the ordered chunk list. Never the plaintext hashes and
never the inline bytes — those are the sealed section, and H_p in particular is
what a chunk's content key derives from, so a public reader that exposed it
would hand the server the one value it is missing.

**Directory.** Yield: `dir_salt`, and for every entry its child id, its node
type, and its stored name. The child id and the node type have to be public —
a marker that cannot classify an edge cannot follow it, and the type is what
says whether a child id names another directory or a manifest. The stored name
is opaque: SIV ciphertext under E2EE, and the entry order is bytewise over that
ciphertext, so **the order says nothing about the names**. Never the per-entry
mtimes and modes; a key-free read reports them as zero, and a reader that took
those for real values would date every entry in the library to the epoch.

**Commit.** Yield: root, parents, `created_at`. The root has to be public in
both library types — it is the first edge of every server-side walk, and a
sealed root would leave a server unable to trace an E2EE library at all.
**Never the author and never the message**, in *either* library type. They are
sealed under E2EE and plainly readable through the keyed decode in a plain
library, and a public reader that reported them where it could would let a
server path written against it come to depend on a library being unencrypted.

Three rules go with all of them.

**A key-free reader is for servers. A client must not use one.** The public
section of a sealed object *is* covered by the AEAD tag, but a key-free parse
does not check it, because checking it needs the key. A holder of CK reads
through the sealed decode and gets the same fields authenticated; a server
reads them unverified, which is the correct trade for the one party already
assumed hostile to integrity. What a key-free reader produces is the server's
own bookkeeping, never a statement to a client about what an object is.

**A key-free reader still enforces every public-section rule**: the version,
the reserved flag bits, the bounds, and every internal agreement the public
bytes can be checked against on their own — that `chunk_count` matches the
list and the chunk sizes total `file_size`, that a directory's entries are
strictly increasing by stored name, that a commit's parent count is within
bounds. Those are the only fields a server could rewrite whose damage this
layer can catch by itself, so they are checked here rather than left to the
tag.

**One parser per object type, shared with the keyed decode.** The public
section is not a second format; it is a prefix of the one format. Each object's
keyed decoder parses it through the same code and then continues into the seal
hash and the sealed section. Two hand-written parsers of one pinned layout are
two things a port has to implement and keep in step, and they drift.

Go: `DecodeManifestPublic`, `DecodeDirectoryPublic`, `DecodeCommitPublic`.

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

## Names

In an E2EE library each path segment is encrypted with **AES-CMAC-SIV
(RFC 5297), AES-256 halves**, under a key derived per directory.

SIV is used because this job needs a cipher that is **deterministic by
design**: `entries/{path}` routes on the ciphertext, so a client must be able
to compute the same bytes the directory object holds. There is therefore **no
nonce parameter anywhere in the name API**, and there should never be one — a
caller wanting randomness would be asking for a different scheme, not a
different argument. The determinism is Option A's stated and accepted equality
leak; it is confined to one directory by the key derivation below, not by a
nonce.

Plain libraries store plaintext names and use none of this.

### Key derivation

```
name_key = HKDF-SHA256(CK, salt="silo/names/v1", info=dir_salt, L=64)
```

**L = 64 is a stated constant, not an implication.** RFC 5297 splits a SIV key
into equal halves — left keys S2V, right keys CTR — so 64 bytes means AES-256
on both. The RFC's own appendix documents only the 128-bit width (a 32-byte
key), so a port that reads the examples and stops will build a 32-byte key that
interoperates with nothing.

Keyed **per directory**, not per library. The job is the equality leak: with
one library-wide key, `Enc("taxes.pdf")` is the same ciphertext everywhere it
appears, and the server learns the shape of a filesystem it cannot read.

The honest cost: path construction is **stateful**. A cold resolve of a depth-N
path is N sequential fetches, because encrypting each segment needs its
parent's salt. Steady state is fine — clients cache the salt map beside the
local index — but the cold start is a tree walk, one step toward Option B's
id-addressed walking rather than away from it. Moving an entry re-encrypts one
name; renaming an ancestor re-encrypts nothing.

### The construction

Standard RFC 5297 with **no associated data** — the directory is already bound
by the key. Three points a hand-written port gets wrong, in the order they bite:

1. **CMAC subkeys.** `dbl` is a left shift with `0x87` folded in when the bit
   shifted off the top was set. A bug here yields a CMAC that is perfectly
   self-consistent, so it survives every round-trip test and every vector
   generated from the same code. Check against RFC 4493's *published
   intermediate values* — `L`, `K1`, `K2` — not only against tags.
2. **Both S2V branches.** A final component of **16 bytes or more** is XORed
   into its own end (`xorend`); a **shorter** one is padded with `0x80` and
   XORed into a doubled accumulator. These produce different output, and the
   short branch is Silo's hot path — most filenames are under sixteen bytes —
   while being the branch the RFC's headline example never exercises. The
   vectors carry lengths 1, 14, 15, 16, 17, 32 and 175.
3. **The CTR counter clears two bits.** Before counting, `Q = V` with
   `Q[8] &= 0x7f` and `Q[12] &= 0x7f`. Omit it and everything round-trips
   against itself while interoperating with nothing.

### Decryption releases nothing unauthenticated

SIV decrypts **before** it can verify: the synthetic IV authenticates the
plaintext, which does not exist until CTR has run. That ordering is inherent to
the mode and is the one place its shape invites a leak that GCM's does not. So
the rule is explicit: recompute S2V, compare with a **constant-time** compare,
and on mismatch **zero the recovered buffer and return nothing** — not even for
the caller to inspect.

### Name rules

- **Maximum plaintext name: 175 bytes** in an E2EE library. The arithmetic
  applies the 255-byte name ceiling twice, and the second application is the
  binding one: the base64url form in `entries/{path}` is itself a path
  segment, so it is held to 255 as well. `ceil(4m/3) ≤ 255` caps the name
  ciphertext at `m ≤ 191`, and SIV prepends a 16-byte synthetic IV, so the
  plaintext caps at `n ≤ 175`. Reading only the first application gives
  `n ≤ 239` and a port that interoperates with nothing. Plain libraries keep
  the full 255, because their names are not wrapped in anything.
- **A name may not be empty, hold `/` or NUL, or be `.` or `..`.** Enforced in
  both directions: writers refuse to produce one, readers refuse to act on one
  whose tag verifies. That is how a directory entry becomes a path traversal on
  whichever client writes it to disk, and in a plain library the server writes
  these names — the party the threat model calls actively malicious for
  integrity. In an E2EE library the server cannot write them, but a buggy
  client could, and the answer is the same.
- **Directory objects carry raw SIV bytes.** base64url unpadded (RFC 4648 §5)
  is the URL encoding for `entries/{path}` and nothing else. A port that
  base64s a name into the object produces different bytes, a different sealing
  key, and a different object id for an identical tree.

### Test vectors

Two layers, because they catch different failures.

**Published vectors, in the Go tests rather than in `testdata`** — they are
other people's, and reproducing them is the point:

- RFC 4493 subkey generation and its four AES-128 CMAC examples, plus NIST
  SP 800-38B's AES-256 examples.
- RFC 5297 A.1 and A.2, at the **128-bit** width the RFC documents.
- Two **256-bit-subkey** examples from miscreant's cross-implementation vector
  set — the production width, cross-checked against an implementation that is
  not this one.

**This format's own vectors**:
[`store/testdata/vectors/names.json`](../../store/testdata/vectors/names.json),
carrying the derived `name_key` for two directory salts and, for each, every
name at the branch-boundary lengths with its ciphertext and URL form. The two
directories share every name, so no ciphertext may appear in both — that
assertion is the per-directory keying, tested.

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

Three secrets, in the order one opens the next:

```
password ──argon2id──▶ master ──HKDF──▶ authKey   (goes to the server)
                              └─HKDF──▶ wrapKey   (never leaves the device)
                                           │
                                           ▼ opens
                              identity private key (X25519, one per user)
                                           │
                                           ▼ opens
                              CK, one per library ──▶ chunks, manifests, names
```

The server holds two opaque blobs per account — the identity key wrapped under
`wrapKey`, and one wrapped under each recovery code — plus one CK wrap per
(library, member) pair. It can read none of them.

### The password split

```
master  = argon2id(password, salt, params)
authKey = HKDF-SHA256(master, salt="silo/auth/v1", info="", L=32)
wrapKey = HKDF-SHA256(master, salt="silo/wrap/v1", info="", L=32)
```

All three steps run **on the client**. `authKey` is sent to the server as the
password and hashed there like any other; `wrapKey` never leaves the device —
not to the server, not into a log, not into a crash report.

The trap this closes: Silo's login sends the raw password to the server. If
that same password also wrapped the identity key, the server would see the
wrapping secret at every login and the end-to-end encryption would be
decoration.

One argon2id call feeding two HKDFs, not two argon2id calls: two would double
the cost of every login for no gain, and turning one strong secret into several
independent ones is precisely HKDF's job. The two halves are independent in the
sense that matters — recovering `wrapKey` from `authKey` means inverting HKDF —
but not independent *of the password*. A password weak enough to guess yields
both, which is what argon2id and the floor below are for.

### Parameters are data, with a floor

Parameters are **stored, not fixed**: a constant can never be raised, and this
format will outlive the hardware its default was chosen on.

| | memory (KiB) | passes | lanes |
|---|---|---|---|
| floor | 19456 | 2 | 1 |
| **default** | **65536** | **3** | **4** |
| ceiling | 1048576 | 16 | 16 |

The floor is OWASP's published minimum for argon2id, chosen because a bound
wants a citation behind it rather than an opinion. The default is Bitwarden's
client-side parameters — the closest published prior art to what this does.

The **ceiling** is the same attack from the other end and is easy to forget: a
server answering with `m=4 GiB` weakens nothing and takes the device down
trying.

**Where the bounds are enforced, and why it is both places.** Parameters are
attacker-influenced — whoever writes a blob picks the cost at which the key
that opens it was derived — so the check runs on **every derivation**: when a
client asks the server for parameters pre-login, *and* when it opens a stored
blob on a new device. A port that checks only the first still derives under
whatever a blob says at bootstrap, which is the one flow that matters.

**The attack shape, stated honestly.** Weak parameters do not unlock blobs that
already exist: different parameters give a different `wrapKey`, and the wrap
simply fails to open. What they do is weaken blobs *created* under them — an
attacker who controls the parameters at enrolment or at password change gets a
`wrapKey` that is cheap to brute-force from the resulting blob. That is why the
floor applies at derivation time, every time, rather than being validated once
when a blob is stored.

Parsing and validating are **separate operations**. `ParseKDFParams` reads
out-of-bounds parameters without complaint, because finding out what is wrong
with a blob has to be possible; only the derivation paths refuse them.

### The parameter string

An argon2 encoded string, minus the hash — self-describing, like every other
stretched secret in this system, and parsed already by every argon2 library on
every platform:

```
$argon2id$v=19$m=65536,t=3,p=4$euXxiPNB85FEQSnDUgO5Dw
```

`v=19` is argon2's own version (0x13), not this format's. Salt is exactly
**16 bytes**, rendered **unpadded standard base64** — standard, not URL: the
one base64url in this format is `NameToURL`.

Parsing is strict: exact field order, no whitespace, no unknown cost fields, no
leading zeros or `+` on a number, one algorithm. Two spellings of one number is
one too many for a string that has to round-trip byte for byte, and a parser
that shrugs at a field it does not recognise is a parser that derives the wrong
key and reports success.

### Identity keys

One **X25519** keypair per user, not per device. The private key is 32 bytes;
the public key is published as a resource and is what a CK is wrapped to.

A device gets the *unwrapped* private key at enrolment and stores it in the
platform key store — Keychain, keyring, or a `0600` file on a headless host
with no key store, stated plainly rather than pretended otherwise. The wrapped
blob on the server exists for one moment: bootstrapping a new device, which is
the one time the password is typed again.

**Cache the identity key, never `wrapKey`.** Caching `wrapKey` avoids the same
re-typing and is the more obvious thing to reach for, and it is the wrong one:
`wrapKey` is a deterministic function of the password and salt, so `wrapKey` at
rest is an offline password oracle — whoever reads it off a stolen device tests
guesses against it directly and recovers the *password*, which also yields
`authKey` and the account itself. The identity key discloses nothing about the
password. Same convenience, strictly smaller blast radius.

Any 32 bytes are a valid private key. X25519 clamping forces bit 254 set and
the low three bits clear, and no scalar of that shape is a multiple of the
group order, so even an all-zero input yields an ordinary public key rather
than the identity point. There is nothing to refuse on this side; the
degenerate case is a hostile **public** key, and it is refused below.

### The two identifiers a wrap binds to

Every wrap binds itself to who and what it is for, as associated data.

- **holder** — the account's id in **canonical lowercase hyphenated UUID text**
  (`3f2a1c58-9b0d-4e77-8a61-5c2d0e4f9ab3`). It must be **immutable for the life
  of the account**: changing it makes every blob the account holds unopenable.
  An email address must not be used for this, for exactly that reason — an
  address is mutable and an account may hold several.
- **library** — the library's UUID, same spelling.

Neither may be empty: a wrap that binds to nothing binds nothing. They are
refused rather than truncated — a truncated holder still binds, just to the
wrong person.

**The spelling is enforced on both, not merely requested**, on the way in and on
the way out. Each is exactly 36 characters: lower-case hexadecimal digits with
hyphens at offsets 8, 13, 18 and 23, and nothing else. Not the 32-character
unhyphenated form, not `{braced}`, not `urn:uuid:`-prefixed, not upper case — a
permissive UUID parser accepts every one of those, and they are all different
byte strings. Since both are associated data, two clients that disagree about
the spelling seal blobs neither can open, and the only error either sees is that
the key was wrong.

One rule for both identifiers, applied by one function. There is no version of
this format in which an account id is checked and a library id is not: the
asymmetry buys nothing and costs the reader a rule they have to remember applies
to only half the fields.

So this is a *spelling* rule rather than a parse: handing the text to a
platform UUID type and reading it back is not the check, because a permissive
parser reports success on all six spellings above. The version and variant
nibbles are deliberately not examined — Silo mints UUIDv7 for accounts and takes
a library id from whoever creates the library, but which flavour of UUID either
is drawn from is the server's business, not something a blob should refuse to
open over. The **nil UUID** (`00000000-0000-0000-0000-000000000000`) is refused
for the reason the empty string is: it is well formed and names nothing.
`keys.json`'s `identifiers_refused` commits every case a port has to reject, and
each has to be rejected in both positions.

**A wrap that has no holder is a different kind, not a holder-bearing blob with
the field left out.** `sharing.md`'s *compatible* link flavor wraps a share key
`SK` to the server, and the server is not an account — it has no UUID, and
inventing one for it would mean either an exception carved into the rule above
or a sentinel that binds to nothing. Whenever that wrap is specified it takes
its own kind byte and its own AD domain (`silo/wrap/sk/...`), and it does not
carry a holder field at all. The rule above stays total.

### Wrapped identity blob

Written under `wrapKey` (kind 1) or under a recovery code (kind 2). The two
kinds are not interchangeable in either direction, even though both seal the
same 32 bytes.

```
 0        1        2                18
+--------+--------+------------------+
| version| kind   |   wrap_salt[16]  |
+--------+--------+------------------+
| params_len varint | params bytes   |   <- empty for kind 2
+-------------------+----------------+
| holder_len varint | holder bytes   |
+-------------------+----------------+
| ciphertext (32) ‖ tag (16)         |
+------------------------------------+
```

`params_len` is bounded at **128 bytes** and `holder_len` is exactly **36** —
the width of a canonical UUID, since that is the only thing a holder may be; a
reader enforces both before it allocates, and then applies the spelling rule
above to the 36 bytes it read. The longest parameter string this format can
write is 57 bytes — `$argon2id$v=19$` plus `m=1048576,t=16,p=16` at the
ceilings, a separator, and a 16-byte salt in unpadded base64 — so 128 is that
with room for a longer future parameter set. A reader that takes `params_len`
on trust allocates whatever a hostile blob asks for.

```
AD = bytes 0 .. start of ciphertext
K  = HKDF-SHA256(secret, salt=<domain>, info=SHA-256(AD), L=32)
     domain = "silo/wrap/idkey/v1"     (kind 1, secret = wrapKey)
              "silo/wrap/recovery/v1"  (kind 2, secret = the 20 code bytes)
blob = AD ‖ AES-256-GCM(K, zero nonce, private_key, AD)
```

**`wrap_salt` is 16 fresh random bytes per wrap, and it is what makes the zero
nonce correct here.** This differs from the sealed containers in Manifests
above, and the difference is worth being precise about: there, the public
section carries `seal_hash`, so the key commits to the plaintext and no two
plaintexts can meet one key. Here there is **no `seal_hash`** — publishing
SHA-256 of a private key would hand out a confirmation oracle for it — and
freshness comes from the salt instead.

Without it, one live case reuses a nonce: re-wrapping a *different* identity
key under an *unchanged* password gives the same `wrapKey`, the same key, the
same zero nonce and two plaintexts. The two ciphertexts XOR to the two private
keys.

### Wrapped content key blob

```
 0        1                    33                   65
+--------+---------------------+--------------------+
| version|  ephemeral_pk[32]   |  recipient_pk[32]  |
+--------+---------------------+--------------------+
| library_len varint | library bytes                |
+--------------------+------------------------------+
| ciphertext (32) ‖ tag (16)                        |
+---------------------------------------------------+
```

`library_len` is exactly **36**, and the 36 bytes are held to the same spelling
rule as `holder` above — a reader enforces the length before it allocates and
the spelling before it compares.

```
(esk, epk) = X25519 keygen                  -- fresh, every wrap
ss         = X25519(esk, recipient_pk)      -- refused if all-zero
AD         = bytes 0 .. start of ciphertext
K          = HKDF-SHA256(ss, salt="silo/wrap/ck/v1", info=SHA-256(AD), L=32)
blob       = AD ‖ AES-256-GCM(K, zero nonce, CK, AD)
```

CK is **32 random bytes generated client-side at library creation**, never
password-derived: a derived key could not be shared without sharing the
password, and could not survive a password change.

This is **HPKE's shape — ephemeral DH, then a KDF over a context that includes
both public keys — without claiming to be HPKE.** RFC 9180 ships in neither
Go's standard library nor CryptoKit, so conformance would mean hand-porting its
whole KEM/KDF/AEAD negotiation into Swift: a far larger correctness surface
than these forty lines, for a wire nobody outside Silo will ever read. What the
shape is borrowed *for* is the property — binding both public keys into the
derivation is what stops a wrap being replayed at a different recipient.

**The all-zero shared secret must be refused.** Go's `crypto/ecdh` rejects the
low-order points that produce it, so the check never fires there and exists for
the ports. Where it is not free: every wrap made against a low-order "public
key" derives the same key, and whoever published that key reads all of them.

**Freshness.** A new ephemeral keypair per wrap, always — including two wraps
of one CK to one member, and one CK to two members. Reusing an ephemeral reuses
a (key, nonce) pair, the one thing the zero nonce cannot survive.

Sharing a library is one more call. A password change re-wraps only the
member's own identity key and touches **no** content key at all.

### The zero nonce, once, for the whole format

**This format never varies a nonce.** Every AEAD invocation in it — chunks,
sealed containers, all three wraps — uses the all-zero 96-bit nonce, and
freshness comes from the *key* every time. What supplies it differs by
construction and is stated with each: the plaintext hash for chunks, `seal_hash`
in the AD for sealed containers, `wrap_salt` for the identity wraps, the
ephemeral key for content-key wraps.

State it as one rule because it is one conformance check: a port that finds
itself writing nonce-handling code has misread something.

### Recovery codes

**160 bits, Crockford base32, 32 characters, displayed in four groups of eight:**

```
7ZQD-8M4X-0RKB-VN3W-PGHJ-0123-4567-89AB
```

160 bits is the number that lets the wrap **skip a password KDF entirely**. A
code this size is not guessable, so the secret goes straight into HKDF;
stretching it would cost the user seconds of argon2id at the worst possible
moment — the one where they have already lost something — and buy nothing.
32 characters is exactly 160 bits at five bits each, so **no padding case
exists and none is defined**.

Crockford's alphabet is `0123456789ABCDEFGHJKMNPQRSTVWXYZ` — no I, L, O or U.

**A set holds 10 codes**, generated together and shown once. The server stores
one wrapped blob per code and never sees a code.

**Redemption is single-use, and the rest of the set stands.** Using a code
deletes its blob and nothing else; the remaining codes keep working. The format
makes that possible by wrapping once per code — the blobs are independent.
Regenerating the whole set on every use is the tidier-looking rule and the
worse one: it invalidates the codes a person is still holding at the exact
moment they have proved they lost something.

**Normalization, pinned, because forgiveness only works if two clients forgive
identically.** Applied before decoding, in this order:

1. ASCII space, tab and `-` are separators and are **removed**, wherever they
   appear — not only where the display groups put them.
2. Letters are **upper-cased**.
3. `I` and `L` become `1`; `O` becomes `0`. After upper-casing, so a lower-case
   `l` is covered.
4. What remains must be **32 characters**, all in the alphabet.

Nothing else is forgiven. **`U` is not remapped to `V`** — it is not in the
alphabet, so a code containing one was mistyped, and a decoder that guesses
turns a typo into a wrong key and an authentication failure the user cannot
tell from a wrong code.

### What the wraps do not vouch for

A CK wrap binds the recipient's public key, which stops blob-swapping and
cross-library replay. It does **not** vouch that the public key is the person's.

If the server hands Alice a substituted public key for Bob at share time, Alice
faithfully binds the wrap to the wrong key and the binding defends the attack
perfectly. The server then holds a CK for a library it was never a member of.
Nothing in the crypto above closes this, and no amount of binding can: it is
key distribution, not key wrapping.

**The resolution: the member's public key travels in the `member.granted` audit
payload**, so chain-head pinning ([`events.md`](../plans/events.md) phase 3)
makes a substitution evident after the fact — a client that later sees a
different key for the same member in a chain it has pinned knows the server
rewrote history. This costs one payload field and this paragraph now, and wires
up when the audit chain lands.

It is **tamper-evident, not tamper-proof**, and the distinction is the honest
part: detection is after the fact, and a member who never re-checks the chain
never detects it. Out-of-band fingerprint verification is what closes it
outright, and remains available to anyone who wants it. This matches how the
rest of the system already treats rollback — the chain does not prevent a
malicious server, it makes what the server did visible.

### Test vectors

[`store/testdata/vectors/keys.json`](../../store/testdata/vectors/keys.json):
the bounds as numbers; four `authKey`/`wrapKey` derivations including an empty
and a non-ASCII password; identity keypairs; identity, content-key and recovery
wraps with the salt and ephemeral key fixed so the blobs are reproducible; and
every spelling of one recovery code with its normalized form.

Three things in that file are not like the others:

- **`kdf_refused` is the half a port passes every other vector without.** Twelve
  parameter strings that must be refused, each labelled with *where* — at the
  parser or at the bounds — including `m=19455`, one KiB under the floor. A port
  that omits the floor check reproduces every derivation and every blob in this
  file and is still broken.
- **`identifiers_refused` is the same shape for the two UUIDs.** Ten spellings
  that must not be wrapped to, including the upper-case, unhyphenated, braced
  and `urn:uuid:` forms a permissive UUID parser accepts. One list rather than
  two, because it is one rule: every entry has to be refused as a holder *and*
  as a library, and a port that enforces it in one position only passes half of
  this. Both arrays are described rather than stored — what must not happen is a
  blob existing at all, so there is no blob to commit.
- **The argon2id argument order is checked against the reference
  implementation**, in the Go tests rather than in `testdata`, because
  reproducing somebody else's numbers is the point. `IDKey` takes *time* before
  *memory*; swapping them derives 32 bytes that agree with nothing, and every
  vector here would still be internally consistent.
