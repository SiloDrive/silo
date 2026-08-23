# Plan: store v2 — content-defined chunks, packs, end-to-end encryption

Date: 2026-08-22
Status: **proposed** — greenfield on a branch. Silo has no installed base and
porter-fuse / porter-mac are the only clients, so there is no migration: the
branch replaces the store format outright and the old format dies with the
Seafile lane.

This is the execution plan for the direction argued in
[`chunking.md`](../chunking.md) and [`encryption.md`](../encryption.md). Where
those documents disagree with each other, this plan resolves the conflict and
says so; the amendments are listed in [Doc changes](#doc-changes) at the end so
neither doc silently drifts out of date.

## Decisions

| # | Decision | Status |
|---|---|---|
| 1 | Chunking is keyed FastCDC, ~256 KiB / 1 MiB / 4 MiB min/target/max | decided — G1 confirms, see Gate results |
| 2 | Chunker parameters are **per-library data** in the catalog, not constants | decided |
| 3 | No "mostly media / mostly files" knob at creation | decided |
| 4 | Address hash is SHA-256 | **confirmed** — G2 x86 run 2026-08-22, see [`store-v2-hash-bench.md`](store-v2-hash-bench.md) |
| 5 | File objects become binary manifests: ordered `(chunk id, size)` + inline data under 64 KiB | decided |
| 6 | Packs, borg/restic-style: ~512 MB, append-only, sealed, compacted by rewrite | decided |
| 7 | Pack indexes are per-pack local files, mmap'd — never SQLite, never remote | decided |
| 8 | Storage encryption is **universal**: every pack on every backend, including the server's own disk, under a server-generated `storage.key` | decided |
| 9 | E2EE is per-library, chosen at creation, **default on** | decided |
| 10 | E2EE content: convergent per-chunk AEAD under the library key; chunk ids are hashes of ciphertext | decided |
| 11 | AEAD is AES-256-GCM (content and storage layers) | decided — see rationale |
| 12 | Names in E2EE libraries: Option A (deterministic per-segment encryption), B kept reachable | decided (carried from encryption.md) |
| 13 | One user password, split client-side into authKey + wrapKey via argon2id | **specified** — phase 1, `store/kdf.go`; endpoints couple to auth.md |
| 14 | Backends: local fs (hot tier), NAS fs and S3 (durable tiers), same pack format byte-for-byte | decided |

### Gates — measure before freezing

- **G1 — workload.** What the libraries actually hold: file-size distribution,
  compressibility, edit-in-place vs append-only. Decides whether 1 MiB target
  is right and whether a second chunk profile is ever worth exposing.
  `find` + a small histogram script over a real library; the discipline
  [`compression.md`](../compression.md) asks for.
- **G2 — hash on the x86 server.** [`store-v2-hash-bench.md`](store-v2-hash-bench.md) settled
  arm64: hardware SHA-256 at ~2.35 GB/s beats every Go BLAKE3 binding 3.5×,
  because neither Go binding has NEON assembly. The bench is self-contained;
  run it on the deployment box. `grep sha_ni /proc/cpuinfo` is the decisive
  variable: SHA-NI present → SHA-256 confirmed; absent (older Xeon) →
  zeebo/blake3 becomes the cross-platform choice. **Nothing in the system is
  hash-bound** (ingest is network-capped, scan is xxh3-gated, scrub is
  disk-capped), so this is about picking the hash that is boring and
  hardware-fast on every target, not about winning a benchmark.

Everything below is written assuming SHA-256; if G2 flips it, only the
identifier length and the library imports change.

### Gate results — 2026-08-22

**G2 — closed, SHA-256 confirmed.** Run on the author's Ryzen 5 5600 (Zen 3,
SHA-NI): SHA-256 at 2.19 GB/s, zeebo/blake3 (AVX2) at 5.15 GB/s. BLAKE3 wins
this x86 outright — but in Go it spans 0.66 GB/s (M1, no NEON asm) to
5.15 GB/s (Zen 3), an 8× spread, while SHA-256 is within 8% of itself on both
machines with zero dependencies and CryptoKit parity. The selection rule —
uniformly hardware-fast and boring everywhere, since nothing is hash-bound —
picks SHA-256. Full table appended to [`store-v2-hash-bench.md`](store-v2-hash-bench.md). Rerun
only if the deployment server lacks both SHA-NI and ARMv8 crypto extensions.

**G1 — closed, parameters confirmed.** Measured on the real workload
(`~/Media`, 6.0 TB / 114k files; `~/Documents`, 51 GB / 16k files):

- **93% of Media bytes sit in files ≥ 256 MiB** — mkv 4.2 TB, mp4 0.9 TB;
  video alone is ~85% of all bytes. Append-only media dominates, so CDC's
  dedup pays mainly on the faststart/remux cases — and the read-amplification
  argument (1 MiB chunk ≈ porter's window) carries the 1 MiB target on its
  own, exactly as the chunking doc predicted.
- **~32% of files are under 64 KiB but hold 0.01% of bytes** — the inlining
  threshold eliminates a third of all chunk objects for free. Confirmed.
- **≥ 95% of bytes are already compressed** (video/flac/jpg/mp3), confirming
  automatic per-chunk try-compress-store-raw over any per-library knob.
- Scale check: ~6 TB → ~6M chunks at 1 MiB → packs are mandatory (≈12k packs
  at 512 MB), and the largest file (a 110 GB zim) yields a ~4.4 MB binary
  manifest — acceptable, amortised across reads and cacheable.
- 45 GB of `.part` files: in-progress downloads that get renamed on
  completion — content addressing dedups across the rename for free, a small
  bonus the fixed-block store could never see.

## The store

### Identity

A chunk id is `SHA-256(stored bytes)` — the bytes the client sent, before the
storage layer wraps them. For a plain library that is the plaintext chunk; for
an E2EE library it is the client's ciphertext. Either way the guardrail from
encryption.md holds: **the server only ever hashes what it stores**, ids are
never computed from plaintext server-side, and `ETag`/verification work
identically for both library types.

The id remains the only name anything outside the store uses: dedup key,
integrity proof, wire identifier, cache key. Pack offsets are lookup results,
never identities.

### Chunking

FastCDC with normalised chunking, 64-bit gear, min 256 KiB / target 1 MiB /
max 4 MiB (pending G1). The ~1 MiB target is pinned by two independent
arguments from chunking.md: porter's 2 MiB read windows converge with it,
killing the 8× read amplification in `doFileRange`, and it is fine dedup
granularity without manifest bloat.

**The gear table is keyed.** Every library's chunker is seeded:

- plain library: a published constant seed (same for every plain library, so
  cross-library dedup among plain libraries survives)
- E2EE library: seed = `HKDF-SHA256(CK, salt="silo/chunker/v1", info="",
  L=32)`, expanded into the gear table by the pinned construction in
  Vector-tightness (after Manifests)

Keying the chunker is what reconciles CDC with E2EE: cut points become a
function of plaintext *and a secret*, so the server cannot fingerprint known
files from chunk-size sequences. This is borg's per-repo buzhash seed and
restic's per-repo Rabin polynomial — well-trodden. It supersedes
encryption.md's "fixed 1 MiB chunks" instruction, which would have quietly
destroyed delta sync for encrypted libraries (see amendments).

The keyed seed also carries a **second job** it did not start with:
manifests publish `seal_hash` — the hash of the H_p list — and H_p is
computable from plaintext alone, so if cut points were public, seal_hash
would be a CK-free file-confirmation oracle against E2EE libraries. It is
not, only because reproducing the H_p list means reproducing the cut
points, which needs the gear seed, which needs CK. Anyone later tempted to
"simplify" E2EE libraries onto the plain constant seed would be breaking
confidentiality, not just fingerprint resistance.

**Parameters are per-library rows in the catalog** — algorithm id, seed or
seed derivation, min/target/max, normalisation level — served to clients in
the library listing, frozen at creation. E2EE forces per-library seeds anyway,
so plain libraries storing theirs costs one row and buys the ability to add a
tuned profile later without a format change. No creation-time persona knob:
users guess wrong, the choice is sticky, mixed libraries are the norm, and the
tuning media actually wants (skip compression) is automatic and per-chunk.

Chunking is sequential (boundaries depend on preceding bytes); hashing of cut
chunks parallelises. Two passes if profiling ever demands it.

### Manifests

The Seafile `Seafile` JSON object dies. A file is a **manifest**: a binary,
versioned object, content-addressed like everything else, split into a public
reference skeleton and (E2EE only) a sealed secrets section:

```
version u8 | flags u8 | file_size varint
public:  chunk_count varint                  -- chunked only
         (chunk_id [32]byte, size varint)*   -- ordered; size = PLAINTEXT bytes
         seal_hash [32]byte                  -- E2EE only; SHA-256 of sealed plaintext
sealed:  (H_p [32]byte)*                     -- E2EE only; exactly chunk_count of them
   or:   the file's bytes                    -- files under 64 KiB (sealed when E2EE;
                                             --   in a plain library they follow the
                                             --   header directly, unsealed)
```

Three details this diagram did not originally carry, resolved by the phase-1
spec and recorded here so the two do not drift:

- **`chunk_count` is explicit.** A parser could have found the end of the
  chunk list by summing sizes to `file_size`, but the count-rejects-on-mismatch
  rule below exists precisely so two parsers cannot disagree about a malformed
  object — and it applies to this list for the same reason it applies to
  `entry_count`. Cost: one to five bytes per manifest.
- **Inline data carries no length of its own**, because `file_size` already
  states it. More generally: *the sealed plaintext's length is always fixed by
  the public section* — `32 × chunk_count` chunked, `file_size` inline — so no
  sealed section carries a length field and trailing bytes have nowhere to
  hide.
- **Inlining is determined by size, not chosen by the writer**: under 64 KiB a
  manifest inlines and must carry no chunk list; at or above, it is chunked and
  must carry no inline data, both enforced on read. The bullet below reads as
  though a writer may inline when it likes, which would let two clients mint
  two manifest ids for one file — dedup missing and `changes?since=` reporting
  a modification that did not happen. A manifest is a function of its file's
  content and nothing else.

- `size` is pinned as the **plaintext** size — offset→chunk mapping needs
  it. The client-visible wire size is derivable: +16 (content-layer tag)
  under E2EE, +0 plain. Storage-layer framing (+28) and the pack frame
  header (~40 more) are server-internal and never enter client arithmetic.
- Per-chunk sizes are mandatory — under CDC a client cannot map a read offset
  to a chunk without them. ~40 B/entry plain, ~72 B E2EE: 20 / 40 KB for a
  500 MB video, 400 / 800 KB for a 10 GB archive, cached against the file id.
- **The reference skeleton is public even in E2EE libraries.** Chunk ids and
  sizes stay plaintext; only secrets seal. This is not a concession — it is
  what lets the server function at all: GC's mark phase is server-side
  tracing (commits → dirs → manifests → chunk ids) and `changes?since=`
  diffs by id, and neither can walk a graph it cannot read. The alternative
  — client-published reachability sets, with GC trusting clients to say what
  lives — was considered and rejected: an absent or buggy client could
  starve GC forever or delete live data. The choice is taken, not forced.
  What it reveals —
  file→chunk composition and counts — sits inside Option A's accepted
  structure leakage; keyed cut points already prevent size-sequence
  fingerprinting, and without CK no confirmation attack works.
- **The sealed section exists because ids alone cannot decrypt.** A reader
  holds `chunk_id = SHA-256(ciphertext)` and needs
  `K_c = HKDF(CK, info=H_p)` where `H_p = SHA-256(plaintext)` — there is no
  path from one to the other. The sealed section carries `H_p` per entry,
  which also lets the reader verify plaintext after decryption.
- **Sealing is convergent, like everything else** — and the key derives
  from the exact bytes the tag authenticates: key =
  `HKDF-SHA256(CK, salt="silo/manifest/v1", info=SHA-256(AD), L=32)`, zero
  nonce, where **AD is the pinned byte range** — byte 0 through the end of
  the public section, header included (Vector-tightness) — and the public
  section carries `seal_hash = SHA-256(sealed plaintext)` as a field. So
  reordering or substituting public entries fails decryption, which is
  where ordering and membership integrity live (chunks stay
  position-free). Deriving from the AD does two jobs. First, the reader
  can actually compute the key — it holds those bytes; an
  `info=SHA-256(sealed plaintext)` form would be circular (hashing the
  sealed plaintext requires decrypting it requires the key). Second, since
  seal_hash sits inside the AD, the key **commits to every byte the tag
  covers and to the plaintext behind seal_hash**: no two distinct (AD,
  plaintext) pairs can ever share a key, which is exactly the condition
  that licenses the zero nonce under GCM. Manifests would have satisfied it only
  incidentally (their public section is a function of their sealed
  section); directories would not — child ids change while names hold
  still, so editing one file would have put one (key, zero-nonce) pair over
  two different ADs, the forbidden attack — so the property is structural,
  pinned in Vector-tightness for every sealed container. Convergence
  survives intact: identical content yields an identical public section,
  key, ciphertext, and id. A random nonce here would give the same content
  a new manifest id on every write, turning copies, re-uploads after index
  loss, and identical files from two members into spurious `changes?since=`
  traffic — the exact positional-anchoring mistake the chunk scheme exists
  to avoid.
- **Inlining is in the format from day one** — mandatory below the threshold
  rather than optional, per the note above. A 4 KB file must not cost
  manifest + dirent + chunk — three round trips over a remote backend. Inline
  data sits in the sealed section under E2EE.

Directory objects and commits keep their Merkle shape and take **the same
split, sealed sections included** — because per-object AD only defends the
inside of one object, and a tree is attacked *between* objects. Without
this, Option A names are deterministic library-wide: `Enc("taxes.pdf")` is
the same ciphertext in every directory, and a malicious server can swap the
child id beside a name, or graft a name/id pair from one directory into
another, and every object still verifies internally. So:

- A directory object's public section is its ordered entries — child id,
  type, and the SIV-encrypted name, which the server cannot read but must
  see to route `entries/{path}` — plus a random per-directory **name
  salt** and its `seal_hash`; the sealed section carries the **per-entry
  mtimes and modes** — `(mtime varint, mode varint)*`, one pair per
  entry, in entry order — under
  `HKDF-SHA256(CK, salt="silo/dir/v1", info=SHA-256(AD), L=32)`, zero
  nonce, with child ids, types, names, and salt bound as AD: an in-place
  swap of a child id changes the AD *and* the key, so it fails
  structurally. (An empty directory carries zero pairs, so its sealed
  section is empty and `seal_hash` returns to `SHA-256("")` — safe, per
  the commit bullet's empty-sealed-section reasoning, and distinct from
  every other empty directory by `dir_salt`.) The SIV ciphertexts are the
  **single source of truth for names**. A sealed plaintext-name list was considered and dropped: two
  encodings of one fact under one writer key must either be verified
  against each other — N SIV operations per listing, exactly the cost the
  list would exist to avoid — or documented as permanently divergent
  sources of truth. Mtimes pass the test the name list failed, and the
  principle behind both calls is: **store what cannot be derived, advise
  what can.** The name list duplicated a fact already in the object;
  mtimes are derivable from nowhere, and they are sync *state*, not
  display garnish — porter-fuse must answer `getattr` with a real
  `st_mtime` (epoch zero and shared commit-time are both visibly broken),
  the second device to pull a file must set something on disk or the two
  devices disagree forever about a user-visible fact, and the local
  index's `(size, mtime)` gate rests on this value. **Mode is the same
  argument one field over**: a chmod that doesn't propagate and a shell
  script arriving non-executable on the second machine are the same
  silent divergence, and a symlink with nowhere to live is worse — a
  client must either follow it (duplicating bytes, looping on cycles) or
  silently drop it, neither of which a filesystem may do. So symlinks are
  `type = 2`, target bytes stored as the child manifest's content —
  Seafile's own representation, making its absence here a regression
  rather than a feature request. Nor could any of this wait for a later
  phase: `seal_hash` moving off `SHA-256("")` would change every
  directory's public section, hence its id, hence every ancestor and
  commit root — a whole-tree re-commit of every E2EE library and a
  `changes?since=` storm, against ~7 bytes per entry now with zero added
  rewrite frequency (any child change already rewrites the directory).
  Sealed rather than public because per-file activity timing is a leak
  the threat model does not concede (mode rides in the same tuple —
  splitting the pair across sections would buy nothing); in plain
  libraries the pair sits in the dirent instead — the same flags-gated
  placement already pinned for commit author/message. Sizes are the mirror image: already public in
  manifest headers, hence derivable, hence advised rather than stored
  (listing bullet below). Readers list a directory with N cheap SIV
  opens; E2EE clients read the directory **object**, never a
  server-synthesized view of its public section — the view would bypass
  the tag. (The size sidecar in the listing bullet *accompanies* the
  object, never replaces it — no tension.) What stops a **graft**
  (an entry lifted whole from another directory) is this whole-object
  seal: inserting or altering an entry means re-computing the tag, which
  needs CK. Note that well before anyone "optimises" toward per-entry
  sealing — the per-directory name keys below are not the graft defense
  and would not survive that change alone.
- **`dir_salt` is generated once, at directory creation, and carried
  forward on every rewrite** — the writer reads the current directory
  object first (it must anyway) and copies the salt. A fresh salt per
  write would change the directory's id on every write, hence every
  ancestor's id, hence the root: `changes?since=` would report the entire
  tree modified on every commit — the manifest section's random-nonce
  mistake one level up, with a wider blast radius. Consequence: two
  clients independently creating the same path produce different salts, so
  directory merge picks a winner and re-encrypts the loser's names.
- Name encryption is keyed **per directory**:
  `HKDF-SHA256(CK, salt="silo/names/v1", info=dir_salt, L=64)`. Its job is
  the equality leak: the same filename in two directories yields unrelated
  ciphertexts, so `Enc("taxes.pdf")` stops being a library-wide constant.
  `entries/{path}` routing survives, because a client walking down holds
  each directory's salt by the time it needs the next segment — but the
  honest cost is that path construction is now **stateful**: a cold
  resolve of a depth-N path is N sequential fetches (encrypting each
  segment needs the parent's salt), where a library-wide name key could
  compute the whole path offline. Steady state is fine — clients cache the
  salt map alongside the local index — but the cold start is a tree walk,
  one step *toward* Option B's id-addressed walking, not away from it.
  Moving an entry re-encrypts one name; renaming an ancestor re-encrypts
  nothing.
- **What a phase-1 listing shows, and from where.** The listing endpoint
  returns the directory **object** — names, types, child ids, the
  integrity-bearing facts — plus sizes as an advisory
  `(child_id → file_size)` sidecar the server fills from public manifest
  headers, populated from its object index when the commit is processed,
  never N manifest reads per listing. `file_size` is public in both
  library types, so the sidecar reveals nothing new; clients may display
  it immediately and verify it for free whenever they fetch the manifest —
  advisory means it is never trusted for sync or allocation decisions
  until then. Two boundaries keep it honest: it covers **type-0 and
  type-2 entries** — both name manifests, and a symlink's size is its
  target length, exactly as `ls -l` reports it; directory objects have no
  `file_size`, so listings show nothing for folders — and it is
  **scoped to the directory being listed** —
  never exposed as a bulk index, or it becomes `GET /sizes` for the whole
  library the first time someone wants it elsewhere. It accompanies the
  directory object rather than replacing it, so the no-synthesized-view
  rule above holds. Dates come from the **object**, not the server:
  public dirent mtimes in plain libraries, sealed mtimes the client
  decrypts under E2EE.
- A commit takes the same split — and its root id is **public**, beside
  the parent ids, never sealed. It must be: the root is the first edge of
  every server-side walk — GC's mark (commits → dirs → manifests),
  `diff.DiffCommitRoots`, `changes?since=` — and a sealed root would leave
  the server unable to trace any E2EE library at all. That is the
  public-skeleton argument from the Manifests bullet above, applied at the
  one edge that starts the walk. The anti-splice property survives the
  move because the seal **binds** the root rather than hides it: the
  sealed section (author and message under E2EE — or nothing) seals under
  `HKDF-SHA256(CK, salt="silo/commit/v1", info=SHA-256(AD), L=32)` with
  root id and parent chain inside the one authenticated range. The splice
  worked only while the sealed blob was portable between commits; one tag
  over root and ancestry ends it. The commit also answers the one
  question no dirent can: **the root directory's own metadata**. Every
  node's mtime and mode live in its parent's dirent, and the root has no
  parent — so root mtime = the commit's `created_at` (public, already
  under the timestamp range rule) and root mode = `0o755`, pinned here
  with no format change. Left unpinned, Go returns zero, Swift returns
  "now", and `ls -ld` on the mount point disagrees with itself across
  platforms. An empty sealed section is safe on its
  own terms — the uniform derivation gives every commit a distinct key
  from its distinct public bytes, a hazard ("every empty sealed section
  shares one key") the seal_hash change quietly fixed, said here so nobody
  re-derives it. What remains — the server serving an older but internally
  consistent commit (rollback), or refusing to serve at all — is accepted,
  as in every comparable system, and stated in the threat model below.

Directory and commit byte layouts are pinned **here**, because field order
*is* the AD: two implementations that order the public section differently
produce different keys and different object ids for identical trees, and
the failure surfaces as "the Swift client cannot read anything the Go
client wrote".

```
directory object
version u8 | flags u8 | entry_count varint
public:  dir_salt [16]byte                       -- E2EE only (flags)
         (child_id [32]byte, type u8,            -- type: 0 = file, 2 = symlink (child id
          name_ct_len varint, name_ct bytes,     --   names a manifest; the symlink's
          mtime varint, mode varint)*            --   target is the manifest's content),
                                                 --   1 = directory; others REJECTED
                                                 -- mtime: unix seconds, UTC (range rule
                                                 --   below); mode: permission bits only
                                                 -- mtime, mode: PLAIN libraries only
                                                 --   (flags) — under E2EE they move to
                                                 --   the sealed section
                                                 -- strictly increasing bytewise by
                                                 --   name_ct; duplicates unrepresentable
                                                 -- raw AES-SIV bytes (E2EE),
                                                 --   plaintext name (plain); never base64
         seal_hash [32]byte                      -- E2EE only; SHA-256 of sealed plaintext
sealed:  (mtime varint, mode varint)*            -- E2EE only; one pair per entry, in
                                                 --   entry order; pair count must equal
                                                 --   entry_count; trailing bytes REJECTED

commit object
version u8 | flags u8
public:  root_id [32]byte
         parent_count varint, (parent_id [32]byte)*
         created_at varint                       -- unix seconds, UTC (range rule below)
         author_len varint, author bytes,
         msg_len varint, msg bytes               -- PLAIN libraries only (flags)
         seal_hash [32]byte                      -- E2EE only
sealed:  author_len varint, author bytes,
         msg_len varint, msg bytes               -- E2EE only; may be empty
```

`flags` is one byte with **pinned bit assignments** — it sits inside the
AD, so two ports that pick different bits for the same fact produce
different keys and different object ids for identical trees: the exact
failure the layouts above exist to prevent, arriving through the one byte
a layout diagram doesn't specify.

```
bit 0  (0x01)   E2EE      all objects      gates every field marked "E2EE only"
bit 1  (0x02)   inline    manifests only   inline_data replaces the chunk list;
                                           reserved on directories and commits
bits 2–7        reserved  all objects      must be zero; parsers REJECT if set
```

Rules that travel with the layouts:

- **E2EE is one bit, not a family.** On commits, bit 0 alone drives all
  three placements — `seal_hash` present, author/message sealed,
  author/message absent from the public section. On directories it
  likewise alone drives `dir_salt`, `seal_hash`, name encryption, and
  mtime/mode placement (public dirent vs sealed list). Spelled as independent
  flags, a port could encode an impossible object (E2EE set, author
  public) that still parses and still hashes.
- **Bit 0 is a consistency assertion, never a layout selector.** Locating
  the tag requires parsing the object; parsing requires reading `flags`;
  and for E2EE objects the tag is what protects `flags` — so a hostile
  server flipping bit 0 gets a parse under the wrong layout before any
  verification runs (16 bytes of `dir_salt` read as the head of a
  `child_id`). The client already knows which kind of library it opened —
  the catalog declares it — so the parser takes the expected type as an
  input, and an object whose bit 0 disagrees is **rejected before field
  parsing**. That closes the window entirely; the entry-count mismatch
  rule catching most flipped parses is an accident, not a defence.
- **Reserved bits reject-if-set**, or they are unusable forever — old
  parsers would have accepted objects carrying a future bit and computed
  ids over them. This rule is also what makes any future sealed-payload
  extension deployable: a new bit makes old clients reject cleanly
  instead of mis-parsing. Named candidate occupant, so the mechanism
  reads as a plan rather than ceremony: per-entry xattrs, if a client
  platform ever proves to need them carried.
- **`type` values are pinned — 0 = file, 1 = directory, 2 = symlink
  (target bytes are the child manifest's content); anything else
  rejects.** The field is server-consumed, not client metadata: GC's mark
  must know whether a child id names a manifest or a directory object to
  continue the walk — 0 and 2 name manifests, 1 names a directory, still
  a total classification of every edge — so it can never be sealed or
  demoted later. Reject-on-unknown is the walk's correctness condition,
  not parser hygiene — a marker that cannot classify an edge cannot mark
  it.
- **`mode` carries the low permission bits only** — values above
  `0o7777` reject. File *type* lives in `type`, never in mode, or two
  encodings of one fact reappear one field over — the name-list mistake
  in miniature. Type-2 entries carry `mode = 0o777`, both sides stated
  per the split below: writers emit it — vector — and readers **reject**
  a type-2 entry with any other mode — conformance. Symlink permissions
  are noise the OSes disagree on, and a varying value would mint
  divergent ids for identical trees. A symlink's target length rides the
  manifest's existing bounds; nothing new to bound here.
- **A name may not be empty, hold `/` or NUL, or be `.` or `..`** — refused
  by writers and by readers, in plain libraries at parse time and in E2EE
  libraries at decrypt time (SIV ciphertext may legitimately contain any
  byte, so the rule applies to the plaintext, wherever that becomes
  available). This is the one rule the layouts above did not carry and
  needed: it is how a directory entry becomes a path traversal on whichever
  client writes it to disk, and in a plain library the server writes these
  names — the party this document calls actively malicious for integrity.
  Refusing beats escaping, because escaping is a per-client decision and
  there are two clients.
- **Entry order is strictly increasing** bytewise by `name_ct` — not
  merely sorted — so a duplicate name is unrepresentable, rejected by the
  same single pass that validates canonical order, at no cost. Otherwise
  duplicate handling is an unstated per-implementation choice inside an
  object the client trusts after one tag check.
- **Counts reject on mismatch.** `entry_count` and `parent_count` are
  redundant with the lists they prefix — the same discipline as
  non-canonical varints; otherwise two parsers disagree about a malformed
  object and only one refuses it. The sealed payload obeys the same rule
  from inside the tag: the decoded `(mtime, mode)` pair count must equal
  `entry_count`, and trailing bytes in the sealed plaintext reject. No
  server can forge the mismatch — it lives under the seal — but a buggy
  writer can emit it, and two ports must refuse it identically rather
  than one indexing past the end and one silently truncating.
- **Timestamps are canonicalized at both ends, with one rule for the
  whole format.** `mtime` and `created_at` are unix seconds, UTC, in
  `[0, 2^34)` — through year ~2514. Writers clamp below to 0 (pre-epoch
  mtimes exist on real disks) and above to `2^34 − 1`; parsers reject
  values outside the range. An unbounded, server-writable (in plain
  libraries) timestamp is otherwise a reliable way to learn which client
  formats dates defensively — and two clamping stories for one data type
  in one block is a port trap, so there is exactly one.
- **Parse bounds are pinned**, because these objects arrive from a server
  the threat model calls actively malicious. The memory bound is a **byte
  ceiling on the encoded object, per type** — directory ≤ 256 MiB,
  manifest ≤ 1 GiB, commit ≤ 128 KiB — checkable from `Content-Length`
  before a byte is parsed or allocated, which is what actually bounds
  allocation. An entry-count cap would not (a million entries spans
  ~41–225 MB depending on name lengths) and would cap a legitimate
  structure — million-entry directories exist (maildirs, generated data
  sets) and lifting a format ceiling is a version bump by the rule below;
  `entry_count`'s value is purely the consistency check above. Field
  bounds: `name_ct_len ≤ 255` (plain names cap at 255 bytes; SIV
  ciphertexts at 16 + 175 = 191), `parent_count ≤ 16`, `author_len ≤
  255`, `msg_len ≤ 65,536`; timestamps by the range rule above. All
  enforced during parse, not discovered during fuzzing. One caution about
  the manifest ceiling: it is a **format** ceiling — "what may a file
  be" — not a memory promise, and the two questions must not be
  conflated. Manifests do not segment, and manifest size is linear in
  file size (~67 B per chunk ≈ 67 MB per TB at the 1 MiB target), so a
  1 TB image is a ~67 MB immutable object rewritten and re-uploaded in
  full on every edit, however small — a fact G1 will never surface (its
  files average 52 MB, ~52 chunks per manifest); one large file will.
  The memory answer is a **streaming parse** for large manifests, not a
  smaller constant. Practically, manifests get uncomfortable somewhere in
  the low tens of GB per file, and segmentation is the named fix if that
  ever matters — the 1 GiB constant means "nobody has looked past a few
  GB", not "comfortable at 15 TB".
- **Unknown `version` rejects, and the counter is per object type** —
  manifest, directory, and commit formats version independently, as the
  domain strings already imply (`silo/manifest/v1` vs `silo/dir/v1`). A
  port assuming a shared counter would bump directory objects when the
  manifest format changes and diverge on every id.
- **Author/message and mtime/mode placement are flags-gated**, one
  field group each: public in plain libraries, sealed under E2EE — so
  plain-library history does not regress to anonymous commits, and
  E2EE listings do not leak per-file activity timing.

These rules are of two kinds, and a conformance suite tests them
differently. Field order, flag-bit assignments, `type` values, canonical
varints, and strictly-increasing entry order are **vector rules**: they
determine the bytes, so two ports that disagree diverge *silently*,
minting different keys and ids for identical trees — the test is
byte-for-byte agreement on shared vectors. The byte ceilings, field
bounds, count-mismatch, reserved-bit, and unknown-version rules are
**conformance rules**: a port with looser ones still computes identical
ids — it merely accepts objects another port refuses — and the test is a
corpus of malformed objects every implementation must reject. (Strictly
increasing order and timestamp clamping are both: writers must emit the
canonical form — vector — and readers must reject its violation —
conformance.)

The hand-rolled C-compatible JSON encoder in `fsmgr.go` dies with the lane.
Server-side tree diff (`diff.DiffCommitRoots`, the `changes?since=`
endpoint) keeps working on both library types because the edges it compares
are public.

### Vector-tightness — pinned now, encoded by the phase-1 spec

Two implementations must agree byte-for-byte, so the following are
decisions, not implementation details. The phase-1 spec restates them as
exact byte layouts with vectors; none is left to a port's judgment.

- **HKDF convention, everywhere.** Every derivation in this plan is
  `HKDF-SHA256(IKM, salt, info, L)` with all four slots stated: the ASCII
  domain string is always `salt`, the variable input is always `info` (empty
  where none is named), `L = 32` unless stated otherwise. A port that swaps
  salt and info fails every vector at once, by design.
- **Gear table PRF.** Gear = `HKDF-SHA256(seed, salt="silo/gear/v1",
  info="", L=2048)`, read as 256 little-endian uint64. Plain libraries use
  the published constant seed; E2EE seeds derive per the Chunking section.
- **FastCDC, fully.** Normalisation level 2; boundary test
  `(h & mask) == 0`; forced cut at max; the trailing partial chunk is a
  chunk. All constants land in the spec and in the per-library params.
  The masks are the **top `target_bits ± normalisation` bits of the 64-bit
  gear hash** — a departure from the paper, which publishes hand-picked
  masks for an 8 KiB average with their set bits spread through the middle
  of the word. Those constants do not generalise to another target size,
  and what two ports need is a rule rather than a table to transcribe. High
  bits rather than low because with `h = (h<<1) + gear[b]` bit *j* has been
  fed by the last *j+1* bytes: a low-bit mask would cut on the last byte
  alone and lose the content-defined property outright. Pinned with the
  rest of the cut loop — including its two off-by-ones — in
  [`spec/store-format.md`](../spec/store-format.md#chunking).
- **Canonical encoding.** Varints are shortest-form and parsers reject
  non-canonical — without this, two encoders produce different AD bytes for
  identical content.
- **AD is a byte range, not a concept.** The manifest AD is the encoded
  object from byte 0 through the end of the public section — header
  (`version | flags | file_size`) included, since `flags` selects
  inline-vs-chunked parsing and `file_size` sizes reads; leaving either
  unauthenticated hands the attacker the parser. Directory and commit
  objects bind their AD the same way, over the byte layouts pinned in
  Manifests.
- **One sealing-key derivation, wherever a sealed section exists.** The
  key is `HKDF-SHA256(secret, salt=<domain>, info=SHA-256(AD), L=32)` —
  `info` is the hash of **the same byte range the AEAD authenticates**,
  raw 32 bytes, no length prefix. One range, pinned once above, used
  twice; the two uses cannot drift apart in later edits because there is
  only one range. Hashing anything smaller — say the public *section*,
  leaving out the header — would key a strict subset of what the tag
  covers: two objects identical below the header but differing in
  `version` or `flags` over unchanged sealed content (an ordinary format
  revision; two clients at different releases) would meet one key, one
  zero nonce, and two ADs — the forbidden attack, reintroduced. Because
  the public section carries `seal_hash = SHA-256(sealed plaintext)`, the
  key hashes both the AD and the plaintext, making the invariant
  structural rather than argued per container: **no two distinct (AD,
  plaintext) pairs ever share a key**, which is what licenses the zero
  nonce. Domains, pinned: `silo/manifest/v1`, `silo/dir/v1`,
  `silo/commit/v1` under CK; `silo/share/v1` under SK (sharing.md) —
  distinct per container type, mandatory, or the CK containers share one
  key space. **The one deliberate exception is content chunks**:
  `K_c = HKDF-SHA256(CK, salt="silo/chunk/v1", info=H_p)` has no public
  section and no seal_hash, correctly — a chunk frame carries no AD, and
  distinct plaintexts already give distinct keys, the same invariant
  reached by a shorter path. Do not invent a chunk public section to
  satisfy this rule.
- **AEAD frame layout.** Convergent frames — chunks and sealed sections —
  are stored and transmitted as `ciphertext ‖ tag(16)` with the 96-bit zero
  nonce implicit. Never CryptoKit's `combined` form, which prepends the
  nonce and would produce frames 12 bytes longer and ids that don't match.
  Storage-layer frames carry their random 12-byte nonce explicitly in the
  pack frame header.
- **AES-SIV for names** is RFC 5297 AES-CMAC-SIV with AES-256 — a 64-byte
  key, hence `L = 64` in the name-key derivation. Stated as a constant in
  the spec rather than left as an implication, because RFC 5297's own
  appendix documents only the 128-bit width: a port that reads the examples
  and stops builds a 32-byte key that interoperates with nothing. The three
  further traps — CMAC subkey doubling, S2V's two branches either side of
  sixteen bytes, and the two bits the CTR counter clears — are pinned with
  their vectors in the spec's Names section. base64url **unpadded**
  (RFC 4648 §5) is the **URL encoding only** — what travels in
  `entries/{path}`. Directory objects carry raw SIV bytes; a port that
  base64s `name_ct` into the object produces different AD bytes, a
  different key, and a different id for an identical tree — the exact
  failure the pinned layouts exist to prevent, one layer up.

### Packs

Adopted as argued in chunking.md — summary of the load-bearing rules, which
that document owns:

- ~512 MB target, sized by compaction-rewrite granularity (borg's end of the
  spectrum, not restic's), sealed when full, then immutable.
- **Sealed on age as well as size**, and at clean shutdown. Size alone leaves
  the single-copy window unbounded *in time*: an open pack cannot be uploaded,
  so it cannot be verified, so the day's last chunks sit on one disk until
  unrelated future traffic happens to fill the pack. On a quiet server that is
  never. So a pack seals when it reaches target size, **or when its oldest
  frame exceeds a maximum age (default ~5 minutes)**, or when the server shuts
  down cleanly.

  What that buys is a durability story with a number in it: the single-copy
  window becomes **seal age + upload and verify time**, which an operator can
  reason about, rather than "until enough data arrives", which is not a
  quantity. What it costs is occasional undersized packs, and that is not a
  cost — see GC and compaction, which merges them.

  **The alternative was considered and rejected**: opportunistically pushing
  the open pack to the durable tier and overwriting it as it grows. It
  re-uploads a growing prefix — around 5.5× the bytes for a pack filled in ten
  increments — makes verification a moving target, since what was checked is
  already stale, and breaks the write-once argument the conditional-write-free
  feature floor stands on. **Sealed and immutable stays the only remote
  state.**
- Append-only. Reclaim is rewrite-and-swap, never in-place hole reuse — no
  free-list allocator inside packs, ever.
- New chunks of a file are written contiguously, in file order, into the
  current pack, spilling into the next as packs seal — a 10 GB file spans
  ~20 packs by construction, as contiguous runs. **Where locality and dedup
  conflict, dedup wins**: a chunk already stored anywhere is referenced,
  never written twice for locality's sake — the same call borg and restic
  made. On the measured workload (G1: append-only media, near-zero cross-file
  dedup) the conflict is rare enough that sequential reads stay effectively
  contiguous without needing it as an invariant.
- Crash safety: append → fsync → atomic index update; recovery truncates a
  pack to its last indexed frame.
- **A pack never crosses the wire** — not an id, not an offset, not a count.
  Compaction rewrites packs; anything a client holds about one goes stale.

Each chunk sits in the pack as a self-describing frame
(`chunk_id, ciphertext_len, nonce, ciphertext+tag`), so the store is
reconstructible from packs plus `storage.key` alone — no database required.
That property is what keeps [`backup.md`](../backup.md)'s "databases first,
objects second" ordering safe, and it is the reason pack indexes stay out of
SQLite.

### Pack indexes

Per-pack local files mapping `chunk_id → (offset, len)`, sorted by id, mmap'd,
with an in-memory bloom/summary layer across packs. Always on the server's
local disk regardless of where the pack lives — a chunk lookup is never a
round trip. Rebuildable by scanning packs (the frames carry ids), so index
loss is an inconvenience, not data loss — **while every pack is local**. Once
the local tier is an evictable cache, rebuild-by-scan means re-downloading
every evicted pack: a full-store egress bill. So each sealed pack's index
file is uploaded beside it on durable tiers — a few hundred KB next to
512 MB — and recovery fetches indexes, never packs. Lookups still only ever
run against the local copies.

SQLite keeps the catalog jobs it is good at: repos, commits, membership, chunk
parameters, GC statistics. It never holds chunk locations.

### Storage encryption — universal

`storage.key`: 32 random bytes, **generated** (not derived — there is no
secret worth deriving from) at first start, stored `0600` in the data dir,
first item in backup.md's must-not-lose set, with a one-time "back this up
now" warning printed at generation, restic-style. Losing it loses every pack
on every tier; say so plainly. And it is effectively **unrotatable**: every
tier holds byte-identical ciphertext under it, so rotation is a rewrite of
every pack everywhere — expressible with the compaction machinery, but a
full-store egress on remote tiers. Cannot-lose and cannot-rotate are two
halves of one fact; the ops documentation states them together.

Every chunk frame in every pack is AES-256-GCM under `storage.key` with a
random 96-bit nonce stored in the frame. This applies **uniformly**: both
library types (E2EE chunks get a second wrap — harmless, and it keeps the
backend ignorant of libraries), and **all backends including local disk**. The
earlier "encrypt only at the trust boundary" framing died on the NAS question:
a NAS is neither trusted-local nor remote, and a per-backend trust taxonomy is
an invariant that decays. The uniform rule is one sentence — *packs are
ciphertext, everywhere, always* — and its costs are invisible (hardware AES at
multiple GB/s against disks doing a fraction of that; scrub and GC decrypt to
verify and still have 5–10× headroom).

What it buys: at-rest encryption on the primary tier for free (a stolen or
RMA'd disk is noise), `rsync -a storage/` backups safe to point anywhere, and
— because every backend stores identical bytes — **replication is plain file
copy of sealed packs**.

For a plain library the storage layer is the only encryption; for an E2EE
library it is the outer of two. The difference between the two library types
is exactly one sentence: *whether the server holds a key that can read the
content*. Nothing about storage layout differs.

### Backends and tiering

`storageBackend` (`objstore.go:62`) is the seam. Its shape changes from
per-object ops to pack ops: write/seal pack, ranged read, list, delete.
Implementations:

- **fs** — server-local disk. The hot tier: packs land here first.
- **fs on a NAS mount** — same code, different root, durable tier.
- **s3** — durable tier. One PUT per sealed pack (request count is what object
  storage bills); a chunk read is one ranged GET, same as a loose object would
  cost. Immutability and rewrite-compaction are exactly what S3 objects
  permit.

Tier policy: sealed packs (and their index files, per Pack indexes above)
upload to durable backends asynchronously; local acts as cache and may evict
only packs whose durable copy is **verified**, as defined below. S3's
50–100 ms first-byte is fine behind the cache and unusable in front of it —
object storage is a tier, not a substitution. Lookups never leave local disk;
the remote index copies exist only for recovery.

#### What "verified" means

Eviction deletes the only fast copy of data, so the word has to name a check
rather than a feeling:

1. **HEAD**, confirming the object exists and its length is exactly the pack's.
2. **The pack's SHA-256, computed by us**, checked against
   `x-amz-checksum-sha256` where the backend supports it.
3. **Where it does not**, a ranged read-back of the frame headers.

**ETag is not the check.** It is MD5 *sometimes* — not with SSE-KMS, not on
multipart, and not at all on several clones. A check that silently means
"MD5 of one part" on some backends and "an opaque string" on others is not a
check, and the failure it misses is the one that matters: a truncated or
partially-written object whose length happens to match.

The corollary belongs in the durability story rather than being left implicit:
**a sealed-but-unuploaded pack is single-copy data.** Between sealing and
verification there is exactly one copy of those bytes, on one disk, and the
window is as long as the upload queue is deep. So the queue is a first-class
thing: **packs without a verified remote copy**. It is derivable by scan —
that is the recovery path and the thing that makes the column safe to lose —
and persisted as a catalog column so the ordinary case does not scan, and so
an operator can ask how much of this server exists once.

#### The cache has a size, and zero is one of them

One knob: **a byte target for evictable local pack data.** Bytes, not a
fraction of the disk — disks are shared, and a fraction of a disk something
else is also filling is a number that means something different every day.
Over target, evict verified packs least-recently-read first; a coarse
per-pack last-read stamp is enough resolution, and a precise one would cost a
write on every read.

Three consequences, stated because each one is somewhere an implementation
would otherwise invent its own answer:

- **Zero is the limit case, not a mode.** A target of zero means seal →
  upload → verify → evict immediately, and it needs no separate code path.
  What stops it deleting the server is the floor of things that are never
  evictable at any target: the open pack, sealed-but-unverified packs, pack
  indexes, the catalog, and `storage.key`.
- **The bound is soft against that floor.** A durable-tier outage grows the
  unverified backlog past any target, because unverified packs cannot be
  evicted and new writes keep arriving. The answer is the queue-depth alert
  below — never refusing writes, and never evicting something unverified to
  get back under a number.
- **With no durable tier configured the knob is inert by construction.**
  Nothing is ever verified remotely, so nothing ever qualifies as evictable,
  so a local-only deployment cannot misconfigure its way into deleting its
  only copy. This is a property of the eviction predicate rather than a check,
  which is why there is **no force-evict flag, ever** — one would be the only
  way to reach that outcome, and its existence is the entire risk.

#### The S3 feature floor

**PUT, ranged GET, DELETE, LIST. Nothing else.** Every additional API a
backend is assumed to have is a backend that stops working, and the clones
worth supporting — MinIO, Ceph RGW, Backblaze B2, Cloudflare R2, Wasabi —
diverge exactly where the API surface widens.

Conditional writes are **not required**, and that is a consequence of three
things the design already guarantees rather than an omission: one writer per
bucket-prefix (an invariant, stated in ops docs, not enforced by the store),
pack ids that never collide, and packs that are immutable once sealed.

Where `If-None-Match: *` exists — AWS, R2, current MinIO — send it, as an
**assertion rather than a mechanism**. A precondition failure then means one
of the two things that invariant forbids, split-brain or an id collision, and
it should log loudly rather than be retried. Feature-detect it and never
depend on it: Ceph RGW, B2 and older clones do not have it, and a store that
needs it does not run there.

**Single PUT per pack, no multipart.** 512 MB is under every backend's
single-PUT ceiling, and multipart is precisely where clone behaviour diverges
— part sizing, completion semantics, and the point at which ETag stops meaning
anything at all.

Pack sizing stands on fsync amortisation and compaction-rewrite granularity
with no S3 in the room; object-storage economics corroborate the number
rather than decide it. [`target.md`](../target.md) currently rules that S3
"does not get a vote in any decision" — this plan promotes it to a planned
tier, so target.md gets amended rather than quietly contradicted (see Doc
changes).

### GC and compaction

Carried from chunking.md; built once, alongside per-repo GC — they are the
same mark phase.

- **Mark is a tracing collector.** Liveness is global reachability from live
  commits; it cannot be maintained incrementally and must never be refcounted.
- Mark output: `PackStats(pack_id, total_bytes, live_bytes, gc_id)` in the
  catalog — a scheduling input, stale by construction, re-verified before any
  sweep. `GCID`/`LastGCID` (`dbutil/schema.go:125`) become the generation
  stamp protecting in-flight uploads.
- Compact a pack when its dead fraction crosses a threshold (default 0.5),
  rate-limited postgres-autovacuum-style — a threshold and an I/O budget, not
  "quiet hours". Rewrite live frames into a new pack, fsync, swap index
  entries atomically, delete the old pack (locally and on remote tiers).
  Idempotent and interruptible at every step.
- **Compaction prefers packs that are locally present.** Compacting a
  remote-only pack means downloading all 512 MB of it first, so the same
  rewrite costs nothing on a cached pack and costs egress on an evicted one —
  a real bill the mark phase should price in rather than discover. `PackStats`
  already carries the dead fraction; **local presence joins it as a scheduling
  input**, so the autovacuum-style limiter spends its I/O budget on the free
  rewrites before the paid ones.

  **Gate on dead fraction, order by locality, starve nothing.** Dead fraction
  decides *whether* a pack wants compacting; locality decides *which* of the
  wanting packs goes first. The economics settle it rather than taste:
  compacting a remote-only pack costs egress **once**, roughly the pack's size,
  which at S3 rates is about four months of storing those same bytes. Leaving a
  mostly-dead pack on paid storage costs its dead fraction **every month,
  forever**. A one-time cost against a perpetual one always eventually pays for
  itself, so deprioritised-but-not-starved is the only ordering consistent with
  what this bullet is for.

  Neither alternative survives that. Pure locality-first leaves paid storage
  full of garbage. **Operator-only remote compaction fails the same way with
  extra steps**: it converts a scheduling decision the system can price
  precisely into a chore a human will defer indefinitely, and the default
  outcome of a deferred chore is the garbage-filled bucket again.

- **Two budgets, because they are two bills.** The autovacuum analogy implies
  one limiter; this needs two. Local rewrites spend disk I/O. Remote
  compactions spend disk I/O *and* egress, and those have different owners —
  one shows up as a slow server, the other as a line item somebody has to
  explain. A single budget cannot express "rewrite freely, spend carefully".

  So the limiter carries a **separate egress budget: bytes downloaded per
  interval, config-visible, generous default.** A backlog of mostly-dead remote
  packs then drains at a bounded, predictable rate instead of arriving as one
  surprise invoice.

  That is also the honest knob to hand an operator. Not "may the system touch
  remote packs" — it may, and the economics above say it must — but **"how fast
  may it spend money doing so"**, which is a policy question that is correctly
  theirs, expressed as a number rather than a ritual.

- **Compaction may also merge undersized packs**, at the lowest priority there
  is. Age-sealing (see Packs) produces small packs on a quiet server, and
  smallness is a nuisance rather than a leak: one extra object, one extra small
  index, one more bloom slot. Nothing is lost by leaving them, so merging is
  what the rewrite machinery does with leftover I/O budget, or does not do at
  all.

  It is the same machinery — read live frames, write a new pack, swap index
  entries, delete the old — so this is **one more scheduling input, not a
  second mechanism**. Dead fraction and locality already order the queue;
  undersize joins them at the bottom.

### History retention

The "delete file history after N days" knob, and it is a GC input rather than
a feature of its own. `RepoHistoryLimit (repo_id, days)` has sat in the schema
since the Seafile era (`dbutil/schema.go:158`) with no reader but `DeleteRepo`'s
cleanup — the storage survives, and the mark phase is where it grows teeth.

- **Retention is the definition of "live commit", not a second collector.**
  Mark traces from live commits, and without a limit every commit ever written
  is live — nothing short of deleting the library ever reclaims a byte. Under a
  limit the live set is the head plus every commit younger than the cutoff; the
  head is always live, however old. Everything reachable from that set stays,
  and commits outside it — with the objects only they reference — are what the
  sweep exists for. The user-facing promise is snapshot-shaped: anything
  deleted or overwritten less than N days ago is still recoverable.
- **It works without the key.** Truncation needs a commit's parent and its
  age, and `DecodeCommitPublic` serves both in an E2EE library. The server can
  cut an encrypted library's history without reading a byte of it — the
  key-free readers were named the server's view of the format, and this is
  that view doing its job.
- **The knob: per-library override over one global default, unlimited when
  unset.** `days = 0` is valid and means head-only — a library that is a
  mirror rather than an archive. Absent means keep everything, and unlimited
  is the default, because reclamation nobody configured is the one kind this
  system must never perform.
- **`changes?since=` must answer truncation before the diff is written.** A
  client asking `since=` a commit retention has reclaimed gets a defined
  answer — history was cut, resync from the head — not an object-not-found
  dressed as a 500. Pinned as **410 Gone carrying the current head**, so the
  client's next move rides in the response. Conversion already needs the
  identical answer: a converted library **starts a new history root**
  (Conversion is a named operation), and a `since=` from behind that root is
  the same "your baseline no longer exists" case. One response, two causes —
  and it goes in before the diff bakes in the assumption that every `since`
  resolves.
- **Disk usage decomposes into three numbers, and the middle one is this
  knob's.** Chunks the head reaches; chunks only retained history reaches; and
  what the directory actually holds — packs with their dead fraction, indexes,
  overhead. The mark walks the first two sets anyway and can tell them apart
  in the same trace; `PackStats` already totals the third. The middle number
  is what retention trades against, so it is the one to show next to the knob
  — with one honesty rule under cross-library dedup: the figure worth showing
  is the bytes *only* this library's history keeps alive, because a chunk
  another library still reaches is reclaimed by nobody's truncation. That is
  computable inside the global mark without refcounts, and no other way.
- **Accounting is untouched by design.** Quota is logical size at head
  (below), and truncation moves no head: cutting history reclaims disk, never
  quota. The two numbers diverging here is the system working — `PackStats`
  sees the reclaim and the user's number never flinches.
- Enforcement lands with phase 5's mark, where liveness is computed anyway.
  What phase 2 must carry is only the wire answer above and the row that
  already exists.

### Size accounting and quota

Accounting today is built entirely on what phase 2 deletes, and the lane
store-v2 builds on never had it.

- **What dies.** `size_sched.go` computes `RepoSize`/`RepoFileCount` through
  `commitmgr.Load`, `diff.DiffCommits` and an `fsmgr.GetFileCountInfoByPath`
  fallback — all three doomed managers — and is queued only from
  `sync_api.go:1170` and `fileop.go:2962`, both frozen lanes. Its
  `notifyRepoSizeChange` needs Redis, which a single-binary local Silo has no
  reason to run. At the deletion nothing computes a size and nothing asks for
  one.
- **Quota is already absent from the silo lane.** Every `checkQuota` call site
  sits in `doUpload`, `doUpdate`, `parseUploadHeaders`, `commitFileBlocks` or
  `sync_api` — Seafile lanes without exception. `entries/{path}` has never
  checked it. Note the failure mode precisely, because it is quiet: usage stays
  0, `usage >= quota` never trips, so quota does not break loudly at the
  deletion, it stops existing.
- **Quota is logical size at head** — the sum of `file_size` over the files the
  head commit reaches. Not stored bytes, not disk. Under content-defined
  chunking, dedup and deferred compaction those three numbers diverge by
  multiples in both directions, and only the first is one a user can predict or
  act on: delete a 2 GB file, get 2 GB back. It is also the only one that holds
  still while the server works — a compaction run must never change what
  somebody is charged.
- **It is computable without a key, in both library types.** `file_size` is
  public by design (Manifests), so one path covers plain and E2EE with no
  branch. Nor is it usefully forgeable: a chunked manifest's chunk sizes must
  sum to `file_size` and the count check rejects a mismatch, and those chunks
  are bytes the client actually uploaded. Overstating is possible and
  self-defeating — it charges the liar.
- **The number comes from the object index, not a walk.** The
  `(child_id → file_size)` map the server already populates at commit time for
  the listing sidecar is the same table accounting reads. One population pass,
  two consumers; a second walker built for sizes is the thing to not do.
- **Steady state is incremental off the store-v2 diff** — the same diff
  `changes?since=` needs, so build them together. Delta over added, modified
  and deleted, applied to the stored total. The full walk survives as a repair
  path and as phase 4's recovery scan's natural companion, never as the
  routine.
- **Dedup is not a discount, and a stored-bytes quota is refused.**
  Cross-library dedup exists only for plain libraries — a different CK mints a
  different frame (Content crypto) — so charging stored bytes would bill two
  users differently for the same file depending on who uploaded it first, and
  bill E2EE users more for choosing E2EE. There is also no machinery that could
  split a shared chunk fairly: mark **must never be refcounted** (GC and
  compaction, above), and without refcounts there is no principled attribution.
  Logical-at-head is the only rule that is both computable and explicable.
- **Disk is a second number with a different owner.**
  `PackStats(pack_id, total_bytes, live_bytes, gc_id)` already answers it, and
  the mark phase already produces it. Sum it for what the disk holds; its gap
  against the logical total *is* the dead fraction waiting on compaction. Two
  numbers, two purposes, never added together — and an operator number rather
  than a per-user one, because packs are not per-library and per-library disk
  attribution does not exist to be reported.
- **Open — where the check goes on the id-addressed lane.** `PUT entries/{path}`
  checks `Content-Length` as today. `PUT objects/{id}` cannot: objects are
  admitted before the head that references them moves, so charging at admission
  over-charges a client that abandons an upload, and charging only at head-move
  lets one push unbounded bytes first. The shape that fits is a soft ceiling at
  admission to bound the damage and the real accounting at head-move, leaving
  unreferenced bytes to GC — which is what GC is for. Decide this **with** the
  surface, not after it.
- File count rides along unchanged in shape: same diff, same index. Its only
  reader today is `size_sched`'s own bookkeeping query, so whether
  `RepoFileCount` survives turns on whether anything outside wants it.

### Observability for the background workers

Error reporting today rides two things, and both are request-shaped: the
logrus hook, which sends everything at error level and above, and the recover
middleware that turns a panicking handler into an event instead of a dead
process. See [`error-reporting.md`](../error-reporting.md).

The upload queue, the eviction verifier and the compaction worker are this
server's **first long-running goroutines** — no request to hang a trace on, no
middleware in the call stack to catch a panic. So each gets its own
recover-that-reports, in the established pattern. Without it a panic in the
upload queue takes the process down with no event, and the operator learns
about it from the restart.

**Three conditions are pinned at error level**, which is what makes them
reportable at all — the hook's threshold is the whole mechanism:

- **A durable-copy verification mismatch.** The pack on the remote tier is not
  the pack that was written. This is an integrity signal rather than an
  operational one, and it is the moment the eviction gate earns its keep: the
  check ran *before* the local copy was deleted, so this is a warning about a
  bad remote copy rather than a report of data already lost.
- **An `If-None-Match: *` precondition failure.** "Log loudly" means error
  level; the hook is what makes loud actually loud. It signals one of the two
  things the one-writer invariant forbids — a second writer on the prefix, or
  an id collision — and it must never be retried, because a retry overwrites
  the evidence of the condition it just detected.
- **The upload queue failing to drain past a depth-or-age threshold.** Not any
  single failure: the *shape* of a queue that is not moving. What it means is
  the single-copy window is growing, which is the number the cache-size knob
  above deliberately refuses to fix by evicting.

**Retryable object-storage failures stay below error level** — a warn, seen in
logs, sent nowhere. That is not timidity, it is error-reporting.md's own rule:
the threshold exists because everything at error goes, and a retryable blip
recurring several times a minute would drown the three conditions above. A
retry that then succeeds is not a failure; a retry budget exhausted is, and
that is what the queue-depth condition catches.

**`store/` stays log-free and sentry-free, permanently.** It ships inside
porter-fuse and is reimplemented in porter-mac, so it has no business holding
an opinion about where a server sends its errors. Errors cross that boundary
as return values, and every sentinel in the package exists so a caller can
decide what to do with one.

## End-to-end encryption

Per-library, chosen at creation, **default on**. The costs are real and stay
listed in encryption.md — no server thumbnails/preview/search/zip for E2EE
libraries, browser access needs client-side crypto or doesn't exist, key loss
is data loss, revocation is not retroactive — and features that need plaintext
get scoped explicitly to plain libraries so the boundary lives in code.

Said once, as a product decision rather than the accident of two separate
defaults: **the common library is the one where server-side preview, search,
thumbnails, and inline rendering do not exist.** Server-readable is the
deliberate exception, chosen at creation for the libraries that want those
features.

### The threat model, stated once

No document in this set said it plainly before. The server is treated as
**honest-but-curious for confidentiality and actively malicious for
integrity**. For an E2EE library a malicious server cannot read content or
names, and — with the sealed bindings in Manifests — cannot alter,
substitute, or graft chunks, files, or tree entries without a client
detecting it at decrypt time. It **can**: withhold data; serve a stale but
internally consistent state (rollback — accepted, as in every comparable
system); delete everything; lie in the **advisory size sidecar** of a
directory listing (accepted — sizes there are display hints that
self-correct on manifest fetch and are never sync inputs, but a directory
the user browses without opening its files shows server-chosen numbers
indefinitely; the third residual, beside rollback and withholding); and
observe sizes, counts, tree shape, timing, and access patterns. Browser recipients of share pages additionally trust
the JS the server serves, as sharing.md states. encryption.md receives this
paragraph via Doc changes, so the founding indictment and its answer live
in one place.

### Keys (carried from encryption.md, unchanged)

- X25519 identity keypair per user; private key wrapped under the user's
  wrapKey (argon2id-derived, below) and stored server-side as an opaque blob;
  public key published as a resource.
- Content key **CK** per library: 32 random bytes, generated client-side at
  creation, never password-derived. Wrapped to each member's public key — one
  sealed blob per member. Sharing = wrap for one more key; password change
  re-wraps only the user's own private key.
- Recovery codes are a required feature, not an afterthought: a second wrap of
  the private key under a generated high-entropy code, printed once.

**Pinned in phase 1**, with the bytes and vectors in
[`spec/store-format.md`](../spec/store-format.md) § Key wrapping:

- **The wrap construction is owned, HPKE-shaped, and does not claim to be
  HPKE.** Ephemeral X25519, then HKDF-SHA256 over a context carrying both
  public keys, then AES-256-GCM. RFC 9180 ships in neither Go's standard
  library nor CryptoKit, so conformance would mean hand-porting its whole
  KEM/KDF/AEAD negotiation into Swift — a larger correctness surface than the
  forty lines it replaces, for a wire nobody outside Silo reads. Same reasoning
  that put SIV in the package.
- **argon2id parameters are data with a floor and a ceiling**, not constants: a
  constant can never be raised, and a served parameter is a downgrade lever.
  Floor is OWASP's published minimum (m=19456 KiB, t=2, p=1), default is
  Bitwarden's client-side set (m=65536, t=3, p=4), ceiling exists because
  `m=4 GiB` takes a device down without weakening anything. The parameters ride
  **inside** the wrapped blob, argon2-encoded-string style, and the bound is
  enforced in the shared package on **every derivation** — at enrolment and at
  open — because a client that checks only when it asks the server still
  derives under whatever a blob says at new-device bootstrap. Committed
  rejection vectors, because a port that omits the guard passes every other
  vector in the file.
- **Recovery codes: 160 bits, Crockford base32, 32 characters, ten to a set,
  single-use.** Redemption deletes one blob and the rest of the set stands —
  regenerating the whole set on every use punishes the person who just proved
  they lost something. No KDF stretch: at 160 bits the secret goes straight
  into HKDF. Normalization is pinned to the byte (case-folded, separators
  stripped, `I`/`L`→`1`, `O`→`0`, `U` refused rather than guessed at) with
  unnormalized-input vectors, because forgiveness only interoperates if two
  clients forgive identically.
- **Every wrap binds who and what it is for** — holder and library as
  associated data, both immutable UUID text — so a server can neither move a
  blob between accounts nor replay a content-key wrap into another library.
  Enforced as a spelling rule on read and on write since 2026-08-23, by one
  function over both: this decision was stated here from the start and the
  code bounded the two at 255 free-form bytes for a while, which is the drift a
  rule nobody checks always has.
- **What the wraps do not vouch for, stated rather than left to be
  rediscovered.** Binding the recipient's public key stops blob-swapping; it
  does not vouch that the key is the person's. A server substituting a public
  key at share time gets a wrap the sharer built correctly for the wrong
  recipient. The resolution is **tamper-evidence, not prevention**: the
  member's public key travels in the `member.granted` audit payload, so
  chain-head pinning ([`events.md`](events.md) phase 3) makes a substitution
  evident after the fact. Costs one payload field, wires up when the audit
  chain lands, and matches how this system already treats rollback — the chain
  does not stop a malicious server, it makes what it did visible. Out-of-band
  fingerprint comparison remains what closes it outright.

### Content — amendments to the sketch

Two parts of encryption.md's content section defeat the sync design and are
replaced:

1. *"Fixed 1 MiB chunks, not CDC"* → **keyed CDC** (see Chunking above). The
   leak the fixed-chunk rule feared is closed by the secret gear seed; what
   still leaks is **exact** plaintext sizes and chunk counts — by design,
   they are the public offset map — together with tree shape. Padding would
   have to happen before chunking and is not planned; the trade is argued in
   Manifests.
2. *Nonce `file_nonce ‖ chunk_index`, AD binding index and count* →
   **convergent per-chunk encryption**. Index-bound nonces re-anchor identity
   to position: insert bytes at the front and every chunk's index shifts,
   every ciphertext changes, and the client re-uploads the file CDC just
   saved. Instead:

```
H_p   = SHA-256(plaintext_chunk)
K_c   = HKDF-SHA256(CK, salt="silo/chunk/v1", info=H_p)
nonce = zeros                     -- K_c encrypts exactly one plaintext, ever
frame = AES-256-GCM(K_c, nonce, plaintext_chunk)
id    = SHA-256(frame)
```

Identical plaintext under the same CK yields identical ciphertext and a stable
id — delta sync and within-library dedup work exactly as in plain libraries.
The confirmation-of-content oracle this creates extends only to holders of CK,
i.e. the library's own members; encryption.md already leaned toward accepting
that trade, and this plan accepts it deliberately. Chunks are
position-independent; reordering/truncation integrity is the encrypted
manifest's job, and the manifest is authenticated.

Cross-user and cross-library dedup end for E2EE libraries (different CK ⇒
different ciphertext). Accepted; it was already the price of E2EE.

**AEAD choice: AES-256-GCM.** encryption.md's XChaCha20-Poly1305 is not in
CryptoKit, and its extended nonce exists to make random nonces safe — a
non-problem here, where every key is single-use (content layer) or the nonce
is stored per-frame (storage layer). AES-GCM is native in Go and CryptoKit and
hardware-accelerated on every target. ChaCha20-Poly1305 is the fallback if
implementation experience turns up a reason; either interops.

Compression, where it happens at all, happens client-side before encryption
(ciphertext doesn't compress), attempt-and-store-raw per chunk so media costs
nothing.

### Names and metadata

Option A, as encryption.md recommends — with the per-directory keying and
sealed directory bindings specified in Manifests, which close the graft and
equality leaks a library-wide name key would have: each path segment
AES-CMAC-SIV (RFC 5297, AES-256) under the directory's derived key,
base64url unpadded. `entries/{path}` keeps routing on ciphertext it cannot
read; the plaintext name cap is **~175 characters**, not the older ~180 —
SIV prepends a 16-byte synthetic IV before base64url, so the 255-byte
budget gives `ceil(4(16+n)/3) ≤ 255 → n ≤ 175` — accepted and documented. Option B (opaque directory objects, id-addressed
walking) is costed honestly rather than called "reachable": the public
skeleton means B is now a **GC redesign**, not an addressing change —
opaque directories break commits → dirs → manifests, so reachability would
have to come from client-published liveness sets, rejected above. What
survives of the old rule is only this: the API never hard-wires a path
where an id could serve.

E2EE read path: fetch manifest (one object), map offset → chunks, fetch chunk
ciphertext, decrypt, trim. The server serves stored bytes and ranges over them
and never decrypts anything — `serveFile`'s refuse-ranges-when-encrypted
branch disappears rather than grows.

### One password, split client-side

The trap: silo's login currently sends the raw password to the server. If that
password also wraps the identity key, the server sees the wrapping secret at
every login and E2EE is theatre. The fix is the Bitwarden/ProtonMail shape:

```
master  = argon2id(password, user_salt)          -- client-side
authKey = HKDF(master, "silo/auth/v1")           -- sent to server AS the password
wrapKey = HKDF(master, "silo/wrap/v1")           -- never leaves the device
```

The server PBKDF2s whatever arrives, so it needs almost nothing: one new
endpoint serving the per-user KDF salt pre-login, and the raw password never
crosses the wire again — a general win independent of E2EE. This couples to
[`auth.md`](../auth.md)'s credential replacement; land that rewrite first or
in the same branch, not after.

Three consequences the plan owns explicitly rather than leaving to be built
the obvious way:

- **The salt endpoint must not become an enumeration oracle.** Asking for an
  unknown address returns a deterministic fake salt —
  `HMAC(server_secret, address)` — indistinguishable from a real one: the
  same closure auth.md's finding 7 applies to login, extended to the one new
  pre-login surface this plan adds.
- **What a client keeps.** auth.md establishes the password as an enrolment
  credential — presented once, exchanged, forgotten — and E2EE does not
  reopen that. At enrolment the client derives wrapKey, unwraps the identity
  private key, stores the **unwrapped identity key in the platform key
  store** (Keychain, keyring; a 0600 file on key-store-less headless hosts,
  stated plainly), and discards both the password and wrapKey. The wrapped
  blob on the server exists for new-device bootstrap — the one moment the
  password is typed again. Nothing persists the password; the identity key
  sits exactly where auth.md already puts the device key.

  **Why the identity key and not wrapKey**, since caching wrapKey would also
  avoid re-typing and is the more obvious thing to reach for: wrapKey is a
  deterministic function of the password and the salt, so wrapKey at rest is an
  **offline password oracle**. Whoever reads it off a stolen device tests
  guesses against it directly, and what they recover is the password itself —
  which yields authKey, the account on the server, and whatever else that
  password opens, not merely this library. The identity key discloses nothing
  about the password: it is independently random, and stealing it costs exactly
  the libraries it unwraps. Same convenience, strictly smaller blast radius,
  and the one that fails without escalating.
- **Server-side hashing of authKey collapses to a fast hash.** auth.md sized
  64 MiB argon2id behind a 4-wide semaphore against low-entropy passwords; a
  256-bit authKey needs none of it, by auth.md's own rule that a 256-bit
  credential does not. One SHA-256 lookup, no semaphore contention on login.
  And client-side derivation permanently ends `curl -u user:password`
  against the API — account passwords stop being wire credentials at all;
  programmatic callers hold tokens, which they already should.

### Conversion is a named operation

Switching a library between E2EE and server-readable — including "make this
library public", which [`sharing.md`](sharing.md) needs — is **`silo
convert`**: a client holding CK reads every file, re-chunks under the target
seed (the chunker seed differs, so boundaries move), re-uploads, and commits
new manifests. O(library), client-side, resumable, deliberate.
Implementation is deferred past phase 6, but the operation is defined here
so nothing designs as if the toggle were a server-side flag flip.

Conversion **starts a new history root by default**: old commits reference
chunks only CK can read, and a library whose own access mode cannot read its
history is a trap. E2EE → server-readable may keep the dark tail behind an
explicit flag; conversion to public never does — `public_read ⇒
server-readable` must hold for everything an anonymous client can reach,
`changes?since=` included.

## Clients

**One Go package implements the whole scheme** — chunker, ids, manifest
codec, content crypto, key wrapping — exported from the silo module and
imported by the TUI/CLI and porter-fuse. porter-mac implements it in Swift
against **a shared test-vector file that is part of the spec**: chunk
boundaries for known inputs under known seeds, ids, sealed frames, wrapped
keys. The vectors are the interop contract; no release without both
implementations green against them.

Primitive availability, verified: X25519, HKDF-SHA256, AES-GCM, SHA-256 —
native in Go x/crypto+stdlib and Swift CryptoKit. argon2id — x/crypto in Go; a
small C binding in Swift (login-frequency only). FastCDC and xxh3 — the shared
Go package; Swift ports against the vectors (BLAKE3's C library with NEON is
available to Swift if G2 flips the hash).

The TUI/CLI is a full crypto client — it reads content, so it carries CK
unwrapping like any other client. The server-side `keycache`/`parseCryptKey`
path is deleted, not completed (guardrail carried from encryption.md).

**porter-fuse mounts `nosuid,nodev` by default** — a stated client rule,
not whatever happens to be passed to `fuse.Mount`. The format keeps all
12 mode bits, and this rule is why that is safe: in a plain library mode
is public and server-writable, so a hostile server can set `04755` on
any file and a faithful client restores it — a local
privilege-escalation path handed to exactly the party the threat model
calls actively malicious for integrity. `nosuid` makes the restored bit
inert on the mount. Under E2EE the same server cannot touch mode at all
— it is sealed — an asymmetry worth knowing when reasoning about the
two library types.

Client change detection stays as sync-design.md has it: local index
`path → (size, mtime, xxh64, chunk-ids)`, xxh3 as the cheap gate, chunking and
content hashing only for what the gate says changed. The gate never crosses
the wire.

One asymmetry worth naming: in an E2EE library a chunk cannot be *named*
without being encrypted first — ids hash ciphertext — so `blocks/missing`
skips uploads but never crypto. The xxh3 gate bounds that work to changed
files and hardware AES keeps it off the critical path, but the scan-cost
model is not identical to a plain library's.

## Wire protocol

Mostly unchanged in shape — that was the point of content addressing:

- `blocks/missing` + `PUT blocks/{id}` negotiation survives verbatim with new
  ids. The client names chunks; the server says which it lacks.
- **`pack-blocks` batching lands with this work**: N chunks per
  request/response stream, both directions. At 1 MiB chunks,
  one-round-trip-per-chunk is 8× more painful than it was at 8 MiB; this is
  the highest-value wire change available (sync-design.md) and the branch is
  the moment.
- The library listing serves per-library chunk parameters and `head_commit_id`
  (the client must chunk identically for ids to match; `server-info`'s single
  `block_size` integer dies).
- `changes?since=` is untouched — it compares ids.
- `entries/{path}` ranged GETs keep working for plain libraries (server
  unwraps the storage layer); E2EE libraries read via manifest + chunks.
- Client-supplied library UUIDs at creation (key wrapping binds to the id).

## Build order

Phases are sequential on the branch; each leaves the tree working.

0. **Gates G1, G2.** — **done, 2026-08-22.** Plus the two paper decisions:
   confirm default-on E2EE and AES-GCM. Cheap, do first, everything
   downstream hardens.
1. **Spec + shared package + vectors.** — **done, 2026-08-23.** The whole
   format, as a document in [`spec/store-format.md`](../spec/store-format.md)
   and as Go in [`store/`](../../store), with committed vectors for every
   piece: ids, chunker parameters, the keyed gear table and the cut loop; the
   manifest, directory and commit codecs in both library types; convergent
   chunk encryption and the sealed-container key derivation; AES-CMAC-SIV
   names with an owned CMAC (neither Go nor CryptoKit ships one), checked
   against RFC 4493, RFC 5297 and an independent implementation's published
   256-bit-subkey vectors; and key wrapping — the argon2id password split,
   X25519 content-key wraps, recovery codes.
   No server changes, by design. This is the artifact porter-mac builds
   against, so it lands first and alone.
2. **Server store cutover, loose chunks — and E2EE with it** (phase 3 folded
   in; see below). New ids, manifests, per-library
   params in the catalog, keyed chunker — on the existing fs backend,
   write-temp-rename, no packs yet. Delete the Seafile object formats, the
   JSON encoder, `keycache`, `parseCryptKey`, the `IsEncrypted` branches in
   `serveFile`/`putEntryFile`, and the frozen lanes. `pack-blocks` batching
   and manifest inlining land here (they are format/wire, not storage).
   The `storageBackend` seam takes its **pack-shaped interface here** —
   write/seal, ranged read, list, delete — with the loose-object store as a
   degenerate implementation (one chunk per "pack", seal a no-op), so phase
   4 swaps implementations, not interfaces, and the seam changes shape once.

   **Step 1 landed 2026-08-23**: the catalog carries the chunker. `Repo` gains
   the algorithm, sizes, normalisation and the e2ee flag, with no DEFAULT — a
   creation path that forgets them fails at the INSERT. The seed is not stored,
   because a plain library and an E2EE one derive it differently from the same
   row. Parameters are validated on every read, so a row describing no chunker
   makes the library corrupted rather than merely unusual. `CreateRepo` refuses
   an E2EE library outright: its initial commit is sealed under a key the
   server never holds, which is the first thing the fold above has to build.

   **Step 2 landed 2026-08-23**: the seam is pack-shaped. `storageBackend`
   goes from four per-object verbs to write-and-seal, ranged read, whole read,
   stat, list, remove and remove-repo, with the fs backend implementing it one
   object per pack. Sealing turned out to be a name for what temp-file-and-
   rename already did. Absence is normalised to one `ErrNotFound` so the
   tiering logic is written once rather than per backend, and verification
   takes a `hash.Hash` rather than a bool so one path covers SHA-1 block ids
   and SHA-256 chunk ids — as do the path validators, since the new ids arrive
   while the old ones are still here. `list`, `remove` and `readAt` have no
   caller until phases 4 and 5; pinning the shape now is the point of doing
   this step here.

   **Step 3 landed 2026-08-23**: [`objmgr`](../../fileserver/objmgr), the join
   between the format and the seam — chunk, manifest, directory and commit
   stores, plus `WriteFile`/`ReadFile`. It sits beside `fsmgr`/`blockmgr`/
   `commitmgr` rather than replacing them yet. A `Store` has **three** states,
   not two: plain, E2EE-with-key, and E2EE-without — the server's permanent
   view of an encrypted library, where bytes go in and out under verified ids
   and manifests give up their public chunk list, and everything else returns
   `ErrNoContentKey`. That third state is the product, not a degraded mode.
   `New` refuses a seed that disagrees with the library type, since an E2EE
   library on the plain seed would make `seal_hash` reproducible without the
   key. It also needed `store.DecodeManifestPublic`, which did not exist —
   without it a server cannot enumerate an E2EE library's chunks at all, and a
   server that cannot enumerate them can never reclaim one.

   **Step 4 landed 2026-08-23**: the tree layer — resolve, list, read-by-path,
   and `Walk` for reachability, plus `PutDir`/`NewDirSalt` as their inverse.
   Name encryption is where the two library types stop being one code path
   with a flag: the cipher is built once per directory, and lookup encrypts
   the segment to match on ciphertext rather than decrypting every entry.
   Resolving a depth-N path under E2EE is now demonstrably N sequential
   fetches — encrypting a segment needs its parent's salt — which is the cost
   the Names section predicted, arriving on schedule. `.` and `..` are refused
   rather than resolved. The server's view can enumerate chunks but cannot
   walk an encrypted tree at all, and there is no version of this where it
   can. Still wired to nothing; the handlers come next.

   **Step 5 landed 2026-08-23**: the inverse — put, mkdir, `mkdir -p`, remove,
   rename, each returning a new root id. A mutation rewrites the spine from the
   changed directory up and carries every untouched subtree by id. Three rules
   about what the rewrite *carries forward* rather than what it writes: the
   salt comes from the object being replaced (a fresh one would remint every
   ancestor's id and make `changes?since=` report the whole tree); only the
   directory whose entry list changed gets a new mtime, and that mtime lives
   one level up in its parent's entry, so exactly one ancestor is touched
   beyond having its child id swapped; and the mutation's own timestamp is a
   separate argument from the node's mtime, because a client preserves a file's
   mtime, which may be years old, while the directory it lands in changed just
   now. Rewrites operate on stored entries, so only the one name involved is
   encrypted or decrypted. Refusals are refusals rather than policy: a put over
   a directory drops a subtree as a side effect of a write, a rename onto an
   occupied name is a collision only the caller can decide, and a directory
   moved into its own subtree detaches from the root. Remove takes the subtree
   without complaint — older commits still reach every object, and the
   collector reclaims what nothing can.

   **The key-free readers, completed 2026-08-23.** Step 3 found that a server
   could not enumerate an E2EE manifest's chunks and added
   `DecodeManifestPublic`. Scoping the handlers found the same hole in the
   other two object types, and the commit was the bad one: the root id lives in
   the head commit, so a server that cannot open a sealed commit cannot find
   the tree at all. `DecodeDirectoryPublic` and `DecodeCommitPublic` close it,
   each sharing its parser with the keyed decoder. The set is now what it
   should always have been described as — not a manifest feature but **the
   server's view of the format**: chunk lists, tree edges, roots and parents,
   which is exactly enough to store, serve, trace and reclaim an encrypted
   library and not enough to read one.

   **Step 6 — the cutover, scoped 2026-08-23.** Five decisions, written down
   before anything is cut, because each one was found by reading the code
   rather than by reading this plan:

   *The frozen lanes go first, and take auto-merge with them.* `fileop.go`,
   `sync_api.go`, `merge.go`, `diff/`, `virtual_repo.go`, the SeaDrive
   handlers and their routes — roughly 7000 lines, and
   [`target.md`](../target.md) already gave up Seafile wire compatibility and
   named `seafile-compat-end` as the tag to revert to. Server-side three-way
   merge goes with them, and that is forced rather than chosen:
   `GenNewCommit`'s contention path merges trees, merging trees means reading
   names, and an E2EE library has none the server can read. `target.md`
   already pins the replacement — `If-Match` compare-and-swap on the branch
   head, and a writer that loses re-reads rather than having its edit merged
   for it.

   *`entries/{path}` keeps structure and loses content under E2EE* —
   **corrected 2026-08-23**, having first been written down as "refuses
   encrypted libraries outright". That was wrong, and the Names section says
   so in as many words: SIV is deterministic *because* `entries/{path}` routes
   on the ciphertext. A client encrypts each segment, base64urls it, and sends
   it; the server matches ciphertext against ciphertext and never learns what
   either says. Resolve, `HEAD`, listing, delete and move therefore all work on
   an E2EE library, and a listing returns ciphertext names for the client to
   decrypt.

   What the server genuinely cannot do is anything with the *bytes*. A byte
   range of the plaintext is not a byte range of a per-chunk sealed object, so
   content reads go through the manifest and the chunks; and it cannot chunk
   (the seed is CK's), cannot build a manifest and cannot build a directory
   object, so every write is by id. **Structure by path, content and writes by
   id.** The blunter version of this rule would have cost `changes?since=` its
   paths for no reason — the delta can carry ciphertext paths just as the
   entries route can.

   *So the write shape for an E2EE library is by id, and it is the one both
   types share.* `POST blocks/missing`, `PUT blocks/{id}` and batched
   `pack-blocks` for content; `PUT objects/{id}` for manifests, directories
   and commits, where the server verifies that the id is the SHA-256 of the
   bytes and that the object decodes through its public reader, and verifies
   nothing else because it can verify nothing else; then `PUT head` with
   `If-Match: <current head commit id>`, where the server checks that the new
   commit's parent is the head it was handed. `entries/{path}` writes survive
   as the server-side convenience for the web UI and for curl, on plain
   libraries only.

   *Library metadata moves out of commits and into the catalog.* Seafile keeps
   a library's name, description, last modifier and last modification time
   inside every commit, and `repomgr.GetWithReason` reads the head commit to
   fill them in. Under E2EE the author and message are sealed and the name has
   nowhere to live at all, so the server would be reading fields it cannot
   read. `Repo` gains those columns; `store.Commit` carries none of them. The
   head commit keeps exactly one server-facing job — supplying the root id,
   through `DecodeCommitPublic`. Storing the last modifier server-side is not
   a concession: the server authenticated the writer, so it already knows, and
   sealing a fact the server established would be theatre.

   *`utils.IsObjectIDValid` narrows to 64 characters at the deletion, not
   before.* It is 40-only today and has ten callers. Widening it to accept
   both would create a window in which a SHA-1 id is accepted on a route that
   can no longer serve one; cutting straight from 40 to 64 alongside the
   deletion has no such window.

   **The order reversed, 2026-08-23** — recorded because the first decision
   above says the lanes go first, and the build now runs the other way:
   creation, reads, writes, the id-addressed surface, and the deletion last.
   "Each phase leaves the tree working" won. The kept `entries/{path}` write
   path commits through `postFilesAndGenCommit` in `fileop.go`, so deleting
   the lanes first breaks the one route being kept before its replacement
   exists; deleting last means every replacement lands against a tree that
   builds and serves throughout. What made lanes-first look forced was
   auto-merge, and that concern does not survive inspection: the CAS write
   path never routes into `GenNewCommit`'s contention merge, and a fresh
   instance has no Seafile client to drive the old lanes into it. Two things
   the reversal does not touch: the lanes stay frozen — nothing new lands in
   them while they wait — and the 40→64 narrowing still moves with the
   deletion as one motion, because the window rule above is about the two id
   widths coexisting, not about when the cut happens.

   **Step 6a landed 2026-08-23: the catalog is the authority.** A library's
   name, last modifier and modification time were fields inside every commit,
   mirrored into `RepoInfo` on each head move, with the commit as the source of
   truth. That is inverted: the commit carries none of them, `RepoInfo` is
   where they live, and `Branch` gains `root_id` so the head and the root it
   names are one row read in one query — which also takes an object-store read
   off the path every request goes down.

   Two things fell out that are worth more than the move itself.

   *Every catalog column here is a server-observed fact.* `last_modifier` is
   the account that was authenticated when the head moved and `update_time` is
   the server's clock at that moment — not values copied from a commit. On an
   E2EE library they may differ from the author and `created_at` sealed inside
   it, because a client may seal any attribution it likes, and that is the
   intended relationship rather than a discrepancy to reconcile later. They
   answer different questions: the sealed pair is what the library's members
   say happened, and the catalog is what the server witnessed. Where they
   disagree, the catalog is the one that is not a claim.

   *A rename is an UPDATE and moves no head.* It used to mint a commit whose
   only change was the name it carried, so every client saw a new head, fetched
   it, diffed two identical roots and found nothing — and under E2EE the server
   could not write that commit at all.

   Recording the head move is now **inside the same transaction as the head
   move**, which the write-contention tests found the hard way: done afterwards,
   a failure arrives when the head has already moved, and a caller reading that
   error as lost contention retries a commit that already landed. One event,
   one transaction.

   What remains reading a commit is one function, `loadSeafileCrypto`, filling
   the vestigial Seafile key-ceremony fields for libraries in the old format
   only — chosen by the head id's width, forty characters being SHA-1, exactly
   as the object store picks a digest. It is a bridge with a scheduled end: it
   goes with the frozen lanes, taking those fields with it.
3. **E2EE.** — **folded into phase 2, 2026-08-23.** Identity keys, salt
   endpoint, split-derivation login (with or after auth.md's rewrite), CK
   wrapping, library creation with client UUIDs, Option A names, convergent
   content crypto in the shared package, TUI and porter-fuse reading/writing
   encrypted libraries.

   **The account side of this has no schema and no route**, found 2026-08-23
   and written up as
   [auth.md § The client's KDF is not this one](../auth.md#the-clients-kdf-is-not-this-one-and-it-needs-four-columns)
   rather than left to be discovered mid-phase, the way `CreateRepo` refusing
   E2EE libraries was. Phase 1 pinned the wire format and auth.md pinned the
   account model, and nothing connects them: four schema items — the published
   X25519 public key, the kind-1 wrapped identity blob, the kind-2 recovery
   blobs as individually deletable rows (that granularity is forced by the
   redemption rule, so it is a constraint rather than a choice), and the
   client's `KDFParams` — plus the "salt endpoint" above, which today is a name
   with no route and no backing column. The endpoint is the one with teeth: it
   is unauthenticated, so it is finding 7's enumeration oracle in a new place,
   and its answer is attacker-influenced input to the client's KDF, which is
   what `store`'s parameter ceiling was written to survive.

   The split existed to keep each step small, and it was written assuming the
   phases would ship in order to somebody. There is nobody: no installs, no
   upgrade path, and decision 9 makes E2EE the default, so the plain-only case
   is not even the common one. Building `serveFile` and `putEntryFile` against
   plain libraries and then rewriting them for sealed manifests and encrypted
   names is work that exists only because of a phase boundary. Both library
   types are live from the start of the cutover instead; what phase 2 gains is
   the client-side CK plumbing and an earlier coupling to auth.md's login
   rewrite, and what it loses is one full rewrite of the read and write paths.
4. **Packs.** Pack format, per-pack indexes, seal-on-size-or-age-or-shutdown,
   `storage.key` generation + backup-set wiring + init warning, recovery scan.
   The fs backend becomes the pack store; step 2's loose store was the
   scaffold.

   **The recovery scan also ingests a phase-2 loose store**, pinned 2026-08-23.
   The folds above are all justified by "there are no installs", and that is
   true right up until the first one — which will be running the loose store,
   because phase 2 ships before phase 4. Leaving that unwritten would make the
   one migration this project claims not to need the one it discovers in
   production. It is cheap when planned and awkward when not: ids do not
   change across the boundary, a pack is a container rather than a new naming
   scheme, and the scan that rebuilds an index from pack bytes is most of the
   machinery already. Ingest is that scan pointed at loose objects: read each,
   append its frame to an open pack, index it, delete the loose copy once the
   index is durable — restartable at every step, because it is the same
   append → fsync → index update the writer uses.
5. **GC + compaction.** Tracing mark, `PackStats`, threshold + throttled
   rewrite, with locality and undersize as scheduling inputs and **two
   budgets** — disk I/O for every rewrite, egress for the ones that have to
   download the pack first.
   Built together with per-repo GC from
   [`future-features.md`](../future-features.md) — same mark, build it once.
6. **Durable backends.** NAS fs root and S3 against the four-verb feature
   floor, async upload of sealed packs, verified-then-evictable local cache
   (with verification meaning what Backends and tiering says it means, not
   ETag), the cache-size knob, the "no verified remote copy" catalog column and
   the scan that rebuilds it, replication as pack copy. The background workers
   arrive here and bring their own panic recovery and the three error-level
   conditions with them.

porter-mac proceeds against the vectors from the end of phase 1 and the wire
from the end of phase 2 — it does not wait for packs, which it can never
observe anyway.

## What not to build

Encryption.md's guardrails all carry forward (no server key cache, no
plaintext-derived ids, names opaque, no plaintext-requiring features except
explicitly scoped to plain libraries, room for per-user key material). New
ones from this plan:

- **No pack identifiers on the wire, ever** — compaction makes them lies.
- **No skip-encryption branch in any backend.** Uniform or nothing; the first
  "this chunk is already ciphertext so skip the wrap" flag is the beginning of
  the taxonomy this plan exists to avoid.
- **No chunk-parameter renegotiation.** Params are frozen per library;
  changing them is a rechunk, done deliberately or not at all.
- **No refcounts in the store.** Liveness comes from the mark, only ever.
- **The catalog never holds chunk locations.** The store stays reconstructible
  from packs + `storage.key`.
- **A library's display name and description are server-plaintext, and that is
  a boundary rather than a gap.** E2EE covers a library's content and the names
  of the files inside it. It does not cover the library's own name: the server
  has to sort it, search it, and put it in a listing for a client that has not
  unlocked anything — a client that may not hold that library's key at all.
  Sealing it would mean a listing of untitled libraries, or a second name
  stored in the clear beside the sealed one, which is the same disclosure with
  an extra step. The line is drawn here on purpose and is the sentence the
  client docs need: **what is in the library is private; that the library
  exists and what it is called is not.**

## Doc changes

- `encryption.md` — amend the content section: keyed CDC replaces fixed
  chunks; convergent per-chunk AEAD replaces index-bound nonces (integrity
  moves to the manifest); AES-256-GCM replaces XChaCha20-Poly1305 with the
  CryptoKit rationale. The keys section, names options, costs, and guardrails
  stand. Two additions: the founding metadata indictment ("filenames,
  directory structure, file sizes and block boundaries are all plaintext")
  gets its honest answer — names sealed and bound, structure, sizes, and
  boundaries public **by design** for GC and diff, per the Manifests
  argument — and the threat-model paragraph moves in beside it.
- `chunking.md` — status note at top: adopted, executed by this plan; the
  suggested-order section is superseded by Build order here.
- `sync-design.md` — mark the two-lane and SHA-1 sections historical (the
  frozen lanes die in phase 2); the xxh3-gate and batching arguments stand and
  are cited here.
- `backup.md` — add `storage.key` and `link.key` (sharing.md) as the first
  items in the must-not-lose set. Also the single-copy window: a sealed pack
  with no verified remote copy exists once, so "how many packs are awaiting
  verification" is a number an operator taking a backup wants in front of them.
- `error-reporting.md` — the "what gets sent" section is written entirely
  around request-shaped reporting. It gains the background workers: their own
  recover-that-reports, the three error-level conditions, and the rule that
  retryable object-storage failures stay at warn so they do not drown them.
- **Ops documentation gains the one-writer invariant.** Exactly one Silo
  server writes a given bucket-prefix. It is not enforced by the store —
  nothing in the S3 feature floor can enforce it — and two servers pointed at
  one prefix is the failure the `If-None-Match: *` assertion exists to make
  loud rather than to prevent. It belongs beside the backup ordering rule,
  which is the other thing that is correct only because a human read it.
- `store-v2-hash-bench.md` — the G2 x86 run is appended (2026-08-22); the file
  moved here from the repo root, where the plan cited it but git did not have it.
- `target.md` — S3 is promoted from "does not get a vote in any decision" to
  a planned tier of this plan; the ruling sentence gets amended to point
  here.
- `auth.md` — three amendments from the split derivation: the salt endpoint
  joins finding 7's enumeration closure (deterministic dummy salts);
  server-side hashing of the 256-bit authKey drops to a fast hash, with the
  argon2id semaphore remaining sized for enrolment and link redemption only;
  and the unwrapped identity key joins the device key in the platform key
  store. Record that `curl -u user:password` against the API is permanently
  gone.
