# The store

What a library's bytes are made of, where they go, and who can read them.

**Part 1 describes the server as it runs.** If the code and Part 1 disagree,
one of them is a bug. **Part 2 is designed and not built** — nothing in it
exists unless it says so.

The byte-level contract is not here. Field order, flag bits, key derivations,
parse bounds and the test vectors are
[`spec/store-format.md`](spec/store-format.md), which is normative and
versioned; a second implementation builds against that file and the vectors in
`store/testdata/vectors`, and is conforming when it reproduces them. This
document is the layer above: what the objects are for, where a server puts
them, what it can do with a library it cannot read, and what the numbers mean.

Silo has no deployments yet. Every object in every store can be discarded and
rewritten, which is what licensed replacing the format outright rather than
migrating it. That freedom expires the first time someone else runs this
server.

---

# Part 1 — as built

## Three rules

**Everything is named by the hash of its stored bytes.** A chunk id is
`SHA-256(stored bytes)` — the bytes the client sent, before any server-side
framing. For a plain library that is the plaintext chunk; for an end-to-end
encrypted one it is the client's ciphertext. The server only ever hashes what
it stores, never a plaintext it was shown, so verification and `ETag` work
identically for both library types and no later refactor can quietly acquire
the ability to do otherwise. The id is the only name anything outside the store
uses: dedup key, integrity proof, wire identifier, cache key.

**The server stores and traces; it does not always read.** Every object splits
into a public reference skeleton and, under E2EE, a sealed section. The
skeleton is what the server walks — commits name roots and parents,
directories name children, manifests name chunks — and it is public in both
library types by design, because garbage collection's mark phase and
`changes?since=` are server-side traversals and neither can walk a graph it
cannot read. The alternative, clients publishing what is live, was rejected: an
absent or buggy client could starve reclamation forever or delete live data.

**Anything a client must reproduce is library data, not server configuration.**
The chunker algorithm and its four sizes live in a row per library, frozen at
creation, and are served to clients. A client that chunks differently computes
different ids for the same bytes and dedups against nothing, so these values
are part of the library's identity rather than a setting that may drift under
it.

## Four kinds of object

| object | holds | stored in |
|---|---|---|
| chunk | a run of file content, as the client stored it | `chunks/` |
| manifest | one file: its size, and either its ordered chunk list or its bytes inline | `objects/` |
| directory | ordered entries — child id, type, name, mtime, mode | `objects/` |
| commit | a root id, parent ids, a timestamp, an author and a message | `objects/` |

Two stores, not four, because the split that matters is by access pattern: a
chunk is read as a byte range out of whatever container holds it, and the three
small object types are read whole. Manifests, directories and commits form the
Merkle tree; chunks hang off its leaves.

Byte layouts, flag bits and parse bounds are in the spec. Two shape facts are
worth carrying here because everything else rests on them:

- **A manifest's per-chunk sizes are plaintext sizes, and they are mandatory.**
  Under content-defined chunking a client cannot map a read offset to a chunk
  without them. The wire size is derivable — plus sixteen bytes of AEAD tag
  under E2EE, plus nothing plain.
- **Inlining is decided by size, not chosen by the writer.** Under 64 KiB a
  manifest carries the file's bytes and no chunk list; at or above it carries a
  chunk list and no inline data, both enforced on read. A writer allowed to
  choose would mint two ids for one file, and dedup would miss while
  `changes?since=` reported a modification that never happened. A manifest is a
  function of its file's content and nothing else.

## Chunking

FastCDC with normalised chunking over a 64-bit gear table:
`fastcdc-gear64/v1`, minimum 256 KiB, target 1 MiB, maximum 4 MiB,
normalisation level 2. The cut loop, its two off-by-ones and the mask rule are
pinned in the spec.

**The gear table is keyed.** A plain library seeds it from a published constant
that every implementation derives for itself, so cross-library dedup among
plain libraries survives. An E2EE library seeds it from its content key. That
is what reconciles content-defined chunking with encryption: cut points become
a function of plaintext *and* a secret, so a server cannot fingerprint known
files from the sequence of chunk sizes.

The seed carries a second job it did not start with. Manifests publish
`seal_hash`, the hash of the sealed plaintext, and under E2EE that plaintext is
the list of per-chunk plaintext hashes — which is computable from plaintext
alone. If cut points were public, `seal_hash` would be a key-free
file-confirmation oracle. It is not, only because reproducing the list means
reproducing the cut points, which needs the seed, which needs the content key.
Moving E2EE libraries onto the plain constant would break confidentiality, not
merely fingerprint resistance.

**The seed is never on the wire.** The library listing serves the algorithm and
the four sizes and no seed: a plain library's is a constant, and an E2EE
library's derives from a key the server does not hold and must never be handed.
Publishing one would be the server claiming to know something that, for exactly
the libraries that matter, it does not.

Chunking is sequential, because a boundary depends on the bytes before it;
hashing the chunks it cuts parallelises.

**The numbers came from measurement, not taste.** On the real workload — 6.0 TB
across 114k files of media, 51 GB across 16k documents — 93% of bytes sit in
files of 256 MiB or more, so content-defined dedup pays mainly on remuxes and
resumed downloads, and the 1 MiB target is carried instead by read
amplification: it converges with a client's read window. About 32% of files are
under 64 KiB and hold 0.01% of the bytes, which is the inlining threshold
eliminating a third of all chunk objects for nothing. At least 95% of bytes are
already compressed, which is why compression is per-chunk and automatic if it
ever arrives, rather than a knob at creation.

## What the server can read

A library's store is in one of three states, and which one it is decides what
it can do rather than being a flag somebody checks:

- **plain** — everything is readable and the server does the chunking;
- **E2EE with the content key** — only a client is ever here;
- **E2EE without it** — the server's permanent view of an encrypted library.

The third state is the product, not a degraded mode. In it, bytes go in and out
under verified ids, and every object gives up its public section: a commit its
root and parents, a directory its entries, a manifest its chunk list and
`file_size`. Everything else returns `ErrNoContentKey` rather than
half-working.

**Creating one is a client operation, and it is built.** `POST /libraries` with
`"e2ee": true` is a different request from creating a plain library rather than
a flag on the same one: the initial root directory and the initial commit are
both sealed under a key the server never holds, so they arrive *with* the
request, along with the library id and the content key wrapped to the creator.
The id comes from the client because `store.WrapCK` binds it into the wrap as
associated data — so it has to exist before the key can be wrapped to anybody,
and the alternative of creating the library and publishing its key in two
requests leaves a window holding a library whose key nobody stored.
`client.CreateEncryptedLibrary` is that request in the reference client. It
returns the library's keyring alongside the library, because that is the only
moment the keyring is free: nothing the server holds can reconstruct a content
key, so a caller that drops it has not lost a cache.
`GET /libraries/{id}/key` hands a member their wrap back. Both are behind the
`e2ee-libraries` feature name, and the account key material they depend on is
behind `account-keys`; a client that cannot see those names must not offer the
option, because a `POST` without those fields silently makes a server-readable
library.

| fact | plain | E2EE |
|---|---|---|
| chunk and object ids | public | public |
| file size, chunk sizes, chunk count | public | public |
| tree shape, entry count, child ids, types | public | public |
| commit root id, parent ids, `created_at` | public | public |
| file and directory names | public | AES-SIV ciphertext |
| file bytes | server-readable | sealed |
| entry mtime and mode | public, in the dirent | sealed |
| commit author and message | public | sealed |
| the library's own name and description | public | public |

The last row is a boundary rather than a gap. E2EE covers what is in a library
and what the files inside it are called. It does not cover the library's own
name: the server has to sort it, search it, and put it in a listing for a
client that has not unlocked anything and may not hold that library's key at
all. Sealing it would produce a listing of untitled libraries, or a second name
stored in the clear beside the sealed one, which is the same disclosure with an
extra step. The sentence a client's documentation needs is: **what is in the
library is private; that the library exists and what it is called is not.**

### The threat model, stated once

The server is **honest-but-curious for confidentiality and actively malicious
for integrity**. For an E2EE library it cannot read content or names, and — the
sealed sections binding each object's public bytes as associated data — it
cannot alter, substitute or graft chunks, files or tree entries without a
client detecting it at decrypt time.

It **can**: withhold data; serve a stale but internally consistent state, which
is rollback, accepted here as in every comparable system; delete everything;
lie in the advisory size sidecar of a directory listing, which self-corrects
the moment a client fetches the manifest but is what a user browsing without
opening anything sees; and observe sizes, counts, tree shape, timing and access
patterns.

Names get one further defence worth naming here because it shapes the API. Name
encryption is keyed per directory, from a random per-directory salt, so the
same filename in two directories yields unrelated ciphertexts and
`Enc("taxes.pdf")` stops being a library-wide constant. The cost is that path
construction is stateful: a cold resolve of a depth-N path is N sequential
fetches, because encrypting each segment needs its parent's salt. Steady state
is fine — a client caches the salt map beside its local index — but the cold
start is a tree walk. Moving an entry re-encrypts one name; renaming an
ancestor re-encrypts nothing.

## Key material

The account model E2EE needs is built, and [`auth.md`](auth.md#the-accounts-key-material)
is normative for it. It is four schema items and one endpoint:

- the published X25519 public key;
- the wrapped identity private key, one blob;
- recovery blobs as **individually deletable rows** — that granularity is
  forced by the redemption rule below rather than chosen;
- the client's KDF parameters;
- a pre-login endpoint serving a user's KDF salt.

The endpoint is the one with teeth. It is unauthenticated, so it is an
account-enumeration oracle in a new place, and its answer is
attacker-influenced input to the client's KDF — which is what the parameter
ceiling in `store/` exists to bound. Asking for an unknown address returns
deterministic plausible parameters, indistinguishable from a real account's,
exactly as login does. The wrap construction itself is
[`spec/store-format.md`](spec/store-format.md) § Key wrapping.

**Keys.** An X25519 identity keypair per user, its private half wrapped under a
key derived from the user's password and stored server-side as an opaque blob.
A content key **CK** per library: 32 random bytes, generated client-side at
creation, never password-derived, wrapped to each member's public key as one
blob per member. Sharing is a wrap for one more key; a password change re-wraps
only the user's own private key.

**The library id has to exist before its CK can be wrapped**, because the wrap
binds the id as associated data. That is why an encrypted library's id arrives
with the create request rather than being minted by the server — see
[`protocol.md`](protocol.md). The alternative is creating the library and
publishing its key in two requests, which leaves a window holding a library
whose key nobody stored.

**The wrap construction is owned, HPKE-shaped, and does not claim to be HPKE** —
ephemeral X25519, HKDF-SHA256 over a context carrying both public keys, then
AES-256-GCM. RFC 9180 ships in neither Go's standard library nor CryptoKit, so
conformance would mean hand-porting its whole negotiation into Swift: a larger
correctness surface than the forty lines it replaces, for a wire nobody outside
Silo reads.

**Every wrap binds who and what it is for** — holder and library as associated
data, both immutable UUID text — so a server can neither move a blob between
accounts nor replay a content-key wrap into another library.

**What the wraps do not vouch for.** Binding the recipient's public key stops
blob-swapping; it does not vouch that the key is the person's. A server
substituting a public key at share time gets a wrap the sharer built correctly
for the wrong recipient. The resolution is tamper-evidence, not prevention: the
member's public key travels in the audit payload, so chain-head pinning
([`plans/events.md`](plans/events.md)) makes a substitution evident after the
fact. Out-of-band fingerprint comparison is what closes it outright.

**Recovery codes are a required feature, not an afterthought**: 160 bits,
Crockford base32, 32 characters, ten to a set, single-use, printed once.
Redemption deletes one blob and the rest of the set stands — regenerating the
whole set on every use punishes the person who just proved they lost something.
No KDF stretch: at 160 bits the secret goes straight into HKDF.

**argon2id parameters are data with a floor and a ceiling**, not constants: a
constant can never be raised, and a served parameter is a downgrade lever. The
bound is enforced in the shared package on *every* derivation — at enrolment
and at open — because a client that checks only when it asks the server still
derives under whatever a blob says at new-device bootstrap.

**`storage.key` is the server's own key, and is not account key material.**
32 random bytes, **generated** rather than derived — there is no secret worth
deriving from — at first start, `0600` in the data directory, and it is what
every object on disk is sealed under (§ At rest). It is the **first item in the
must-not-lose set**, and it is effectively **unrotatable**: every copy of the
store holds byte-identical ciphertext under it, so rotation is a rewrite of
everything everywhere. Cannot-lose and cannot-rotate are two halves of one
fact. A one-time "back this up now" warning is printed when it is generated,
and [`backup.md`](backup.md) is where an operator is told what to do about it.

A missing key over a store that already holds objects is a **refusal**, not a
fresh key. Silently generating one would look exactly like a clean first start
— the server comes up, prints the warning, serves requests — and the loss would
only surface at the first read of an old object, by which time more has been
written under the new key. Cannot-lose is enforced at the moment of loss rather
than described after it.

The refusal is a **refusal to start**, and deliberately not a refusal at the
first read. Startup checks the key before it opens a database or a library, on
both the paths that reach a store, so a server whose key has gone missing
never comes up to accept logins and serve listings it cannot honour. It is
also where the key is generated on a first start, after the log destination is
settled, so the once-ever warning lands wherever the operator is reading.

The other way `objstore` can fail to open — a store directory it cannot create
— is still raised lazily, at the first object that asks. Making that one eager
too is silo#28.

**Default on.** E2EE is per-library and chosen at creation, and the common
library is meant to be the one where server-side preview, search, thumbnails
and inline rendering do not exist. Server-readable is the deliberate exception,
chosen for the libraries that want those features. The costs are real and stay
listed in [`encryption.md`](encryption.md): no server-side preview or search,
browser access needs client-side crypto or does not exist, key loss is data
loss, revocation is not retroactive. Features needing plaintext get scoped
explicitly to plain libraries, so the boundary lives in code.

## Where the bytes are

```
<data-dir>/storage/chunks/<library-id>/<aa>/<the rest of the id>
<data-dir>/storage/objects/<library-id>/<aa>/<the rest of the id>
```

That is the **loose** layout: one object per file, fanned out on the first two
hex characters of its id. Writing is temp-file-then-rename inside the
destination directory, so a reader never sees a partial object and the publish
never crosses a filesystem. With `sync` the data is fsynced before the rename
and the directory entry after — not optional for correctness, because the
branch head lives in SQLite, which fsyncs its own WAL, so a head can survive a
power cut that the objects it references do not. Nothing repairs that
afterwards: the client believes it already uploaded those chunks, so a resync
does not send them again.

**It is no longer the write path.** New objects go into packs — see § Packs —
under `<data-dir>/storage/<type>/<library-id>/packs/`, beside the fan-out
rather than in it. The loose layout stays a **permanent read path**: there is no
ingest, so a store written before the cutover carries its old objects forward
where they are and writes its new ones packed, and the lookup asks the open
pack, then the sealed packs, then the loose file.

**Every id is sixty-four lowercase hex characters.** One width, one parser
(`store.ParseID`), one digest. A store that accepted a second width would be a
second id parser waiting to disagree with the first.

**The backend interface is pack-shaped already** — write-and-seal, ranged read,
whole read, stat, list, remove, remove-library — and the filesystem backend
implements it degenerately: one object per "pack", sealing is the atomic
publish it already does, and a ranged read is a seek. Real packs and durable
tiers are Part 2, and they arrive as another implementation of this same
interface, so the seam changes shape once rather than once now and again later.
Absence is normalised to one `ErrNotFound` at the seam, so tiering logic gets
written once instead of per backend.

**The catalog never holds object locations.** SQLite keeps the jobs it is good
at — libraries, branches, membership, chunker parameters, totals — and nothing
that would stop the store being reconstructible from its own bytes. That is
what keeps [`backup.md`](backup.md)'s "databases first, objects second"
ordering safe.

**Every object on disk is a sealed frame**, in both library types, under
`storage.key`. See § At rest. There is no compression: within the frame, an
object is exactly the bytes that arrived. That one is still Part 2.

## At rest

Every object is stored as a **sealed frame**: AES-256-GCM under `storage.key`,
with a fresh 96-bit nonce per write. Uniformly — both library types, and the
local disk is not an exception. The rule is one sentence, *what is on the disk
is ciphertext, everywhere, always*, and its cost is invisible against hardware
AES at multiple GB/s. Part 2's § Storage encryption, universal has the
reasoning; what follows is the shape.

```
magic       4    "SILF"
version     1
id         32    the object's id — SHA-256 of the bytes sealed here
ct_len      8    length of ciphertext ‖ tag, little-endian
nonce      12
ct ‖ tag    n
```

73 bytes of overhead, every width fixed. Fixed is forced rather than tidy: a
`Content-Length` is the object's length, not the file's, and deriving it as
`file size − 73` is what keeps answering that question a single `stat`.

**The header is the AEAD's associated data.** Without that the id in the header
would be decoration — nothing would stop a frame being relabelled, or moved to
another object's path, and opening happily under the same key.

**A frame is not convergent**: the same bytes sealed twice give different
files. That is the opposite of the rule the content crypto follows, and right
for the opposite reason — an id is minted over the bytes handed to the store,
*before* this runs, so dedup, ETags and `changes?since=` never see what happens
underneath them.

**The frame is applied above the backend seam**, in `objstore`, not inside the
filesystem backend. Every backend stores identical bytes; that is what will
make replication a file copy, and a backend that sealed for itself would be a
second place for the answer to differ.

The byte layout is here rather than in [`spec/store-format.md`](spec/store-format.md)
because that file is what a second implementation reproduces, and there will
never be a second implementation of this: no client holds `storage.key`. It is
pinned by vectors in `fileserver/objstore/testdata/frame.json` instead.

**There is nothing else on disk.** Bytes that are not a frame are corruption
and are reported as such; the store does not read unsealed objects. A store
written before framing existed is not migrated — it is discarded and rebuilt,
which this server is still allowed to say (see the top of this document), and
a missing key over a non-empty store is a refusal to start that offers exactly
those two answers: restore the key, or delete the store.

For a plain library this is the only encryption; for an E2EE library it is the
outer of two, and the inner one is the client's. The server gains nothing by
it that it did not already have, and the difference between the library types
remains exactly one sentence: *whether the server holds a key that can read the
content.*

## What the catalog holds

| table | what it is for |
|---|---|
| `Library` | the chunker — algorithm, four sizes, normalisation — and the `e2ee` flag. No `DEFAULT` on any column, so a creation path that forgets them fails at the `INSERT` rather than quietly minting a library whose parameters came from the schema |
| `Branch` | the head commit id and the root id it names, one row read in one query |
| `LibraryInfo` | display name, `update_time`, `last_modifier` |
| `LibraryUsage` | `(size, file_count, root_id)` — the totals and the root they are true at |
| `ObjectSize` | `manifest id → file_size`, the listing sidecar |
| `LibraryRetention` | `keep_days` per library, over `DefaultKeepDays` |
| `GCID` | the generation stamp an orphan sweep is guarded by |

Parameters are validated on every read, not only on write. The row is data, and
a row whose target size is below its minimum is either a bug upstream or a
database somebody edited; in both cases the answer is to refuse rather than to
chunk something no other client will reproduce.

**Every catalog column here is a server-observed fact.** `last_modifier` is the
account that was authenticated when the head moved, and `update_time` is the
server's clock at that moment — not values copied out of a commit. On an E2EE
library they may differ from the author and `created_at` sealed inside it,
because a client may seal any attribution it likes. That is the intended
relationship rather than a discrepancy to reconcile: the sealed pair is what a
library's members say happened, and the catalog is what the server witnessed.
Where they disagree, the catalog is the one that is not a claim.

A rename is therefore an `UPDATE` and moves no head. Minting a commit whose
only change was the name it carried meant every client saw a new head, fetched
it, diffed two identical roots and found nothing.

## The wire

```
GET    /libraries                                    chunker params, head, root, size, file_count
POST   /libraries                                    create
PATCH  /libraries/{id}                               rename
DELETE /libraries/{id}
GET    /libraries/{id}/changes?since=                the diff, paginated
GET    /libraries/{id}/commits                       history
GET    /libraries/{id}/key                           the caller's wrapped content key (E2EE)
POST   /libraries/{id}/chunks/missing                which of these chunks do you lack?
POST   /libraries/{id}/chunks                        many chunks, one framed request
POST   /libraries/{id}/chunks/fetch                  many chunks, one framed response
PUT    /libraries/{id}/chunks/{id}                   a chunk, verified against its id
GET    /libraries/{id}/chunks/{id}                   a chunk, as stored (HEAD for existence)
PUT    /libraries/{id}/objects/{id}                  a manifest, directory or commit
GET    /libraries/{id}/objects/{id}                  the same (HEAD for existence)
PUT    /libraries/{id}/head                          If-Match: <current head commit id>
POST   /libraries/{id}/batch                         many operations, one commit
*      /libraries/{id}/entries/{path}                the path-addressed surface
GET    /account/usage
```

All of it is under `/api/silo/v1`.

**Structure by path is designed, not built.** AES-SIV is deterministic
precisely so that `entries/{path}` can route on ciphertext: a client encrypts
each segment, base64urls it, and sends it, and the server matches ciphertext
against ciphertext without learning what either says. Today `objmgr.Resolve`
refuses a keyless E2EE store outright, so every `entries/{path}` call on an
encrypted library is `403` and structure is read by walking objects from the
head commit; only `changes?since=` carries ciphertext paths. Building the
keyless lookup is issue #30.

What the server cannot do on such a library is anything with the *bytes*. A
byte range of the plaintext is not a byte range of a per-chunk sealed object,
so content reads go through the manifest and the chunks; and it cannot chunk,
cannot build a manifest and cannot name an entry, so every write is by id.
`entries/{path}` writes survive as the convenience for a web UI and for `curl`,
on plain libraries only.

**The id-addressed surface is deliberately the same for both library types.** A
plain library could be written either way, and a client that implements this
one does not need a second implementation for the encrypted case — which is the
point of building one interface with two backends rather than branching on
`e2ee` at every call site.

**What the server verifies on a `PUT`, and it is a short list because it is
everything it can:** that the id is the SHA-256 of the bytes offered under it,
and that the bytes decode through their public reader. It cannot check that a
manifest describes a real file, that a directory's names are well formed, or
that a commit says anything true — those need the key. What the decode check
buys is a refusal to store something that would break the server's own walks
later, where it would fail as a mystery instead of here as a 400.

**`PUT head` requires `If-Match`.** It names the head being replaced, and the
server checks that the new commit's parent is the head it was handed. There is
no server-side three-way merge and there will not be one: merging trees means
reading names, and an E2EE library has none the server can read. A writer that
loses re-reads rather than having its edit merged for it. The retry loop behind
`entries` and `batch` writes is jittered and bounded.

**A `since` the server cannot resolve is 410 Gone**, carrying the current head,
so the client's next move rides in the response. History expiry is one cause;
anything that starts a new history root is the other. One answer, both causes —
and `entries/{path}?at=` answers the same way for the same reason.

**A batch is all-or-nothing.** Every operation applies to one root and produces
one commit, so a refusal halfway would refuse operations carrying no bytes at
all. It is weighed against quota once, before the tree is touched.

`server-info` advertises `libraries`, `entries`, `entries-copy`,
`conditional-writes`, `ranged-reads`, `changes`, `library-rename`, `chunks`,
`objects`, `chunks-fetch`, `entries-manifest`, `chunks-upload`, `pagination`,
`batch`, `usage`, `logout`, `password-change`, `account-keys`,
`e2ee-libraries` and `setup` (plus `notifications` when built with it), so
clients feature-detect rather than version-sniff. The list is `features` in
`fileserver/api/api.go`.

## What the numbers mean

Three numbers, three owners, never added together.

**Quota is logical size at head** — the sum of `file_size` over the files the
head commit reaches. Not stored bytes, not disk. Under content-defined
chunking, dedup and deferred compaction those three diverge by multiples in
both directions, and only this one is predictable (delete a 2 GB file, get 2 GB
back) and only this one holds still while the server works. A background job
must never change what somebody is charged. It costs no key to compute in
either library type, because `file_size` is public by design; and it is not
usefully forgeable, because a chunked manifest's chunk sizes must sum to
`file_size` and the count check rejects a mismatch. Overstating is possible and
self-defeating: it charges the liar.

**Dedup is not a discount, and a stored-bytes quota is refused.** Cross-library
dedup exists only among plain libraries — a different content key mints a
different frame — so charging stored bytes would bill two users differently for
the same file depending on who uploaded it first, and bill E2EE users more for
choosing E2EE. Nor is there machinery that could split a shared chunk fairly:
liveness comes from a tracing mark and must never be refcounted, and without
refcounts there is no principled attribution.

**`LibraryUsage` repairs itself, and there is no hook on the write path.** The
row records the root its totals are true at. A reader finding a different root
computes the delta itself and publishes it with a conditional update on the
root it read, so two readers racing to bring the same stale row forward cannot
add their deltas on top of each other, and the loser's skipped write is the
correct outcome rather than an error. This is the shape rather than a hook at
head-move because there is more than one head-mover, and a number kept right by
everyone remembering to update it is a number that eventually is not. A total
never written, or lost to a crash between the commit and the update, costs the
next reader one delta and is then right again.

The delta is what makes it affordable: a subtree whose id is unchanged
contributes nothing and is skipped unread, so bringing a row forward across one
commit reads the directories along one path. A library that has not moved costs
one row read and no store access at all. When the old root has been collected
out from under the row, the walk falls back to measuring the current tree
outright — a full walk on a request path, which is the price of the cheap
answer having expired, paid once.

The quota check repairs before it compares, for the same reason: reading the
row raw would under-charge a burst of writes for the length of the burst.

**Where the check goes.** `PUT entries/{path}` asks twice — once on
`Content-Length` before the body is read, because receiving forty gigabytes and
then declining them wastes the transfer on both ends, and again on the size the
bytes actually were, because a chunked request declares nothing and a lying one
declares whatever it likes. `PUT chunks/{id}` gets a soft ceiling: it refuses an
account already over, and any single chunk that would alone carry one over. It
does not bound an account just under its ceiling pushing chunks no head names —
that needs a per-account tally of unreferenced bytes, which is the mark phase's
number and does not exist. Until it does, unreferenced bytes are what
reclamation is for.

Quota is therefore exceeded by at most one request, everywhere: the moment a
head moves the tree is measured exactly and the next write is refused. An
overwrite is charged as an addition, because the size it replaces is not known
without resolving the path first — which refuses slightly early at the
boundary, never late, and does not accumulate. The refusal is **507 Insufficient
Storage**, not 403: a 403 tells a client the request was not allowed and to
stop, where the truth is that it should free some space and try again.

**Unlimited is the absence of the field, never a sentinel.** A widget rendering
"-2 bytes" is the predictable end of putting an internal sentinel on the wire.
The same rule makes the listing's `size` and `file_count` optional: zero is an
empty library, and absent is a library whose size the server could not work
out. And a quota nobody configured is no quota — a server told nothing about
ceilings must not refuse every write on the grounds that zero bytes are
allowed.

**Every figure is labelled by kind** (`logical-at-head`), because the first
support question any of this generates is a mismatch against `du`, which is not
a bug but the three-numbers rule working as designed.

**Per-library `size` and `file_count` ride the libraries listing, not
`account/usage`.** They are library facts from the same row as `update_time`,
and sharing settles it: a library shared with you appears in your listing but
is charged to its owner's quota, so per-library sizes under an account report
would either leak libraries you do not own into a total they must not sum to,
or omit them and leave the widget with rows it cannot size.

**The listing sidecar is filled on read.** A directory object names its
children and their types and says nothing about how big any of them are,
because size is a property of the file rather than of the directory that
mentions it — which is right for the format, and leaves the listing endpoint
holding N ids and no numbers. `ObjectSize` is that number written down once,
keyed by object id alone with no library column, because content-addressing
makes the size a function of the id. It is populated by the listing itself —
read the manifests a page is missing, answer, write the numbers down — rather
than at commit time, because there is more than one way for a manifest to reach
a store and a table depending on every one of them calling it is a table with
holes nobody notices: a missing size is indistinguishable from one not asked
for yet. Reading it back is the only thing that has to be right, and reading it
back repairs it. The row cannot go stale, since an id names fixed bytes
forever.

**Disk is a second number with a different owner**, and `silo df` reports it
per library, split three ways by what reaches each object:

- **head** — what the library holds now. Reclaimed by deleting files.
- **history** — reachable from an older commit but not from head. Reclaimed
  only by retention; this is the number that says what a shorter window buys.
- **unreferenced** — reachable from no commit at all. Interrupted uploads,
  commits that lost their race. Reclaimable now with no policy attached, which
  is why it is counted apart from history rather than folded into it.

The three partition the store — every object lands in exactly one and the
totals equal what the filesystem holds — asserted by a test, because three
overlapping estimates look the same from outside as three correct ones. The
walk is key-free, so a server holding an E2EE library it cannot read can still
say what that library is costing it; a census that needed the content key would
be one that never ran on the libraries most likely to be large. It is a set
union over ids and deliberately not a sum of per-commit measurements:
successive commits share nearly all their objects, so adding up what each
reaches would report a history several times the size of the disk and grow
every time somebody touched an unrelated file. An object the walk cannot read
is an error rather than a smaller number — a census is what somebody reaches
for when they suspect a store is wrong, and one that silently reported the
readable fraction would answer the question with a number that means something
else.

## Reclaiming

Four reclaimers, all under `silo gc`, all reporting by default and removing
only with `-delete`.

**Deleted libraries.** `DeleteLibrary` removes a library's rows and records its
id in `GarbageLibraries`; the default pass removes those libraries' store
directories. It is deliberately not a mark and sweep over live libraries: it
never inspects or deletes anything belonging to a library that still exists,
which is what lets it run without reasoning about concurrent writes. The
directory names come from the store package rather than being spelled again, so
a rename cannot turn `gc -delete` into a silent no-op that still reports
success.

**`-orphans`** sweeps objects inside live libraries that no commit reaches.
Two guards, because an upload that stopped halfway and an upload still in
progress leave the same trace and only elapsed time separates them: an object
must have sat unreferenced for at least `-min-age` (a day by default — orders
of magnitude longer than any client takes to go from its first chunk to its
head move, and short enough that a server does not carry a week of dead
uploads), and the `GCID` generation stamp guards writes in flight.

The stamp is a marker for the duration of a collection and a fresh value on
either side of it, because one bump before the mark was not enough. Age
covers a fresh object; it does not cover an object a commit found by dedup,
which may have sat unreferenced for a month — a resumed upload is the common
case — and a client that read the generation *after* the bump, while the mark
was walking a head that did not include it, passed the check and published a
commit whose chunks the sweep then took. So `updateBranch` refuses every head
move while the marker is set (`503`, retry), and the collection bumps again on
its way out, so a check-blocks answered mid-collection is stale by the time it
can be committed against. A collection that dies leaves the marker, and head
moves stay refused until the next `silo gc` clears it; that is the safe
direction. The head-move handler reads the stamp before it checks anything
exists, for the same reason.

**`-expire-history`** drops the commit objects outside a library's retention
window. It is the only operation in Silo that deletes something a commit
reaches, and it is irreversible once the sweep behind it runs, so everything
about it aims at making the set it chooses obvious: it deletes commit objects
and nothing else, leaving the chunks to the orphan sweep, which already knows
how to decide whether anything still reaches them. Expiry runs before the sweep
in the same invocation, because an operator asking for both meant "reclaim what
retention allows", not "reclaim it next time".

**`-compact`** rewrites a sealed pack without the frames nothing reaches, and
it is the only way space comes back from a pack at all. A sealed pack is
immutable — that is what lets a tier replicate one as a file copy, and what
makes "reclaim by rewrite, never by hole reuse" a rule rather than a preference
— so `Remove` refuses an object inside one with `ErrReclaimDeferred`, and both
the sweep and expiry report honestly what they had to leave behind. Compaction
runs last in an invocation for exactly that reason: a run that asked for all
four frees what retention allows in one pass instead of the next.

A pack is a candidate when its dead fraction is past `-compact-threshold`
(`0.5` by default: at a half, the bytes copied and the bytes reclaimed are the
same number). Wholly dead packs go first — they are deleted rather than
rewritten, so they cost no I/O — and the rest are ordered by dead fraction,
which is reclaimed per byte copied. `-compact-budget` caps the live bytes one
run copies; unset, a run catches up rather than falling behind. The guards are
the orphan sweep's, for the same reasons: the `GCID` generation is marked
before the head is read and bumped after the last rewrite, and a pack sealed
inside `-min-age` is left alone. The guard
is on the pack rather than on the frame because an index record carries no
time — but every frame was appended before the footer was written, so a pack's
seal time is a lower bound on every frame's age, which is the direction the
guard needs.

**Retention that could not be carried out is carried by the mark.** On a packed
store `-expire-history` cannot delete a single commit — they are inside sealed
packs — so it defers, and a compaction that only believed the disk would copy
those commits forward for ever. `-compact` run alongside `-expire-history`
therefore takes the same cut as a set of commits to treat as absent, and drops
them and everything only they reached. `-compact` on its own reclaims garbage
and never history: expiring history is irreversible, and an operator who typed
one flag should not get the other one's consequences.

**`-compact -delete` is offline, and that is a rule rather than advice.** The
server holds its pack set in memory and opens a sealed pack by path, so a
rewrite from a second process renames a new pack into place the server does not
know about and deletes the one it does; the next read of a frame that moved is
a `404` to a client that stored it. Nothing locks the data directory, so this
cannot be detected. Compaction inside the server's own process is the
in-process scheduler, which is unwritten and wants a kill switch before it
wants code.

**Retention is the definition of a live commit, not a second collector.**
Without a limit every commit ever written is live and nothing short of deleting
the library reclaims a byte. Under one, the live set is the head plus every
commit younger than the cutoff; the head is always live, however old. The
promise is snapshot-shaped: anything deleted or overwritten inside the window
is still recoverable.

It works without the key — truncation needs a commit's parent and its age, and
both are public — so the server can cut an encrypted library's history without
reading a byte of it.

The knob is `silo retention`: per-library `keep_days` over one global default,
where `0` means head-only, for a library that is a mirror rather than an
archive. **Absent means keep everything, and that is the default**, because
reclamation nobody configured is the one kind this system must never perform.

**Accounting is untouched by all of it, by design.** Quota is logical size at
head and truncation moves no head: cutting history reclaims disk and never
quota. The two numbers diverging here is the system working.

`gc -delete` warns to stop the server first. There is no lock on the data
directory, so it cannot detect a running instance; the default pass only ever
touches already-deleted libraries, which a running server will not write to,
but a server midway through `DeleteLibrary` is a genuine race.

## Operating it

```
silo df [-q] [library-id]                  where the disk went, per library
silo retention [library-id [days|keep-all|default]]
silo gc [-delete] [-q] [-orphans] [-min-age D] [-expire-history] [-expire-window D]
       [-compact] [-compact-threshold F] [-compact-budget SIZE]
```

`retention` and `df` are CLI rather than API for the reason the user commands
are: there is no admin role to gate an endpoint with, and retention is a
server-side setting that bounds the operator's disk rather than something a
library's owner decides. Asking and answering are the same command with and
without a value, so an operator who types the reading form with an argument
gets the change they asked for rather than a usage message.

## One password, split client-side

**Both halves are built.** A password that wraps the identity key and is also
sent at every login is a password the server sees the wrapping secret of, and
E2EE is then theatre. So the client splits it and sends only the half that
unwraps nothing:

```
master  = argon2id(password, user_salt)     -- client-side
authKey = HKDF(master, "silo/auth/v1")      -- sent to the server AS the password
wrapKey = HKDF(master, "silo/wrap/v1")      -- never leaves the device
```

The server hashes whatever arrives, so it needs almost nothing beyond the salt
endpoint — and the raw password stops crossing the wire, which is a win
independent of E2EE. It also permanently ends `curl -u user:password` against
the API: account passwords stop being wire credentials at all, and programmatic
callers hold tokens, which they already should.

**What a client keeps.** The password is an enrolment credential — presented
once, exchanged, forgotten — and this does not reopen that. At enrolment the
client derives `wrapKey`, unwraps the identity private key, stores **the
unwrapped identity key** in the platform key store, and discards both the
password and `wrapKey`. The wrapped blob on the server exists for new-device
bootstrap, the one moment the password is typed again.

Why the identity key and not `wrapKey`, which is the more obvious thing to
cache: `wrapKey` is a deterministic function of the password and the salt, so
`wrapKey` at rest is an offline password oracle. Whoever reads it off a stolen
device tests guesses against it directly, and what they recover is the password
— which yields `authKey`, the account, and whatever else that password opens.
The identity key discloses nothing about the password, is independently random,
and stealing it costs exactly the libraries it unwraps. Same convenience,
strictly smaller blast radius, and the one that fails without escalating.

**Server-side hashing of `authKey` collapses to a fast hash.** A 256-bit
credential does not need a memory-hard KDF, by the rule already stated in
[`auth.md`](auth.md); argon2id remains sized for enrolment and link redemption
only.

What this server does about all of it — the `AUTHKEY-SHA256$` hash that names
its own format and is the crossover flag, the one-statement write of hash and
parameters, the `409` on a change that would undo a crossover by omission, and
what an operator reset does and does not touch — is
[`auth.md`](auth.md) § Split-derivation login, on this side.

An account crosses over when a client enrols it or changes its password, and
never on the server's own initiative: crossing over needs `master`, which is the
one value the server does not have. Until a client enrols it, an account logs in
with its password — which is every account at the moment it is created, since
`POST auth/setup` mints one before there is a client to enrol it. Both shapes
have to keep working, and the client tries the derived one first.

## What Part 1 does not do

- **There is no cache in front of the store.** A read is a read.
- **Objects are not compressed.** They are encrypted — see § At rest — but
  what a frame holds is exactly the bytes that arrived.
- **Nothing creates a `VirtualLibrary` row**, and several queries still join
  the table.

---

# Part 2 — designed, not built

## Packs

Loose objects do not survive the measured workload: ~6 TB at a 1 MiB target is
~6 million chunks, and a filesystem with six million files in it is a different
kind of object than one with six thousand.

- **~512 MB target**, sized by compaction-rewrite granularity. Append-only,
  sealed when full, immutable thereafter. Reclaim is rewrite-and-swap, never
  in-place hole reuse — no free-list allocator inside a pack, ever.
- **Sealed on age as well as size**, and at clean shutdown. Size alone leaves
  the single-copy window unbounded *in time*: an open pack cannot be uploaded,
  so the day's last chunks sit on one disk until unrelated future traffic
  happens to fill the pack, which on a quiet server is never. A pack seals at
  target size, or when its oldest frame exceeds a maximum age (~5 minutes), or
  at clean shutdown. What that buys is a durability story with a number in it —
  the single-copy window becomes seal age plus upload-and-verify time, which an
  operator can reason about, rather than "until enough data arrives", which is
  not a quantity. What it costs is occasional undersized packs, and compaction
  merges those.
- The alternative — opportunistically pushing an open pack and overwriting it
  as it grows — was rejected. It re-uploads a growing prefix (around 5.5× the
  bytes for a pack filled in ten increments), makes verification a moving
  target, and breaks the write-once argument the conditional-write-free feature
  floor stands on. **Sealed and immutable stays the only remote state.**
- New chunks of a file are written contiguously, in file order, spilling into
  the next pack as packs seal. **Where locality and dedup conflict, dedup
  wins**: a chunk already stored anywhere is referenced, never written twice for
  locality's sake.
- Crash safety is append → fsync → atomic index update; recovery truncates a
  pack to its last indexed frame.
- **A pack never crosses the wire** — not an id, not an offset, not a count.
  Compaction rewrites packs, so anything a client held about one goes stale.

Each chunk sits in a pack as a self-describing frame (`chunk_id`,
`ciphertext_len`, nonce, `ciphertext+tag`) — the same `SILF` frame a loose
object already is, copied rather than re-sealed. So the store stays
reconstructible from packs plus the storage key alone, with no database
required.

### A sealed pack carries its own index, in a footer

Magic, frames, index, bloom filter, two fixed-width lengths, magic again. A
reader seeks to the end, reads the lengths out of a known offset, and seeks
back — one ranged GET on a tier that has no local copy, or two if it does not
guess the tail size generously enough.

Two lengths rather than one because the reader has to split the footer, and
neither section can say where it ends on its own: the index is a run of
fixed-width records with no count, and the filter's size is a function of a
parameter stored at *its* start. Putting both in the tail is what keeps the
index section byte-identical to a sidecar.

The footer is not a stylistic choice; it is the only place the index can go. A
pack does not know its own frame offsets until the frames are written, so a
header would need either a second pass or reserved space seeked back into.
Parquet reaches the same layout from the same constraint, and the magic at both
ends is worth copying with it: a truncated pack is the *normal* crash case
here, not an exotic one, and a missing tail magic says so in four bytes.

One object per pack rather than a pack and a sidecar is what this buys on a
durable tier: half the PUTs, half the LIST entries, and no way for a pack and
its index to be separated, to upload out of order, or for one to survive the
other. Recovery still fetches indexes and never packs — it is a ranged read of
the tail, and ranged GET is in the four-verb floor.

### While a pack is open, its index is a sidecar

An open pack is appended to, so its index cannot be in its footer yet. It
accumulates in a sidecar file beside the pack, in **the same format the footer
uses** — so there is one index writer, one parser, and one thing to get right,
used for the live index, for the footer, and for recovery.

Sealing sorts the sidecar's records, appends them to the pack as its footer,
fsyncs, and removes the sidecar. It is removed rather than emptied because a
sidecar is named after its pack and pack ids are random, so there is no next
pack to hand an emptied one to — and "a sidecar exists" is then an exact answer
to "was this pack open", with no second piece of state anywhere to disagree
with it.

A crash part-way through is the same rule as any other: truncate to the last
offset the sidecar indexes and re-do the step. That is idempotent because the
sidecar is still the authority on what is in the pack, and it is *exact*,
because sorting and the filter are both deterministic — the second attempt
writes the bytes the first one would have.

The ordering is load-bearing, for the reason the loose store's already is:
append → fsync the pack → append the sidecar → fsync the sidecar → acknowledge.
The branch head lives in SQLite, which fsyncs its own WAL, so a head commit can
otherwise outlive the objects it references — and nothing repairs that
afterwards, because the client believes it has already uploaded them. In that
order, the worst a crash leaves is a pack tail no sidecar entry points at.

**Concurrent writers serialise on the append**, which is new: writes to
distinct loose paths needed no coordination and an append to a shared file
needs an allocated offset. The batch surface is what makes this cheap rather
than costly — `POST chunks` carries up to 256 frames, so the lock is taken once
per request, and 256 appends followed by one fsync replaces 256 temp files,
fsyncs and renames.

### Finding the pack a chunk is in

Per-pack indexes are sorted by id, so a lookup within a known pack is a binary
search over raw records and never a round trip. Whether those records are held
in memory or mmap'd is a resident-set decision rather than a format one — the
search is the same either way. What names the pack is a **bloom
filter per sealed pack**, held in memory: "no" is certain and ends the search,
"yes" is a probability and costs one binary search to confirm.

Ids are SHA-256 and therefore uniformly random, which decides two things. It
rules out min/max statistics — every pack's id range is the whole id space, so
they partition nothing, which is the same dead end that put optional bloom
filters into Parquet for high-cardinality equality. And it makes the filter's
hash free: take `k` disjoint bit-ranges out of the 256 bits and use them as
indices, rather than hashing an already-uniform value again.

**The false-positive rate is set against the number of packs, not per pack.**
At a 512 MB target a pack holds ~500 chunks of 1 MiB, so 6 TB is ~12,000 packs
— and a textbook 1% rate would mean ~120 false hits, and 120 confirming
searches, on *every* lookup. At 1e-5 it is ~24 bits per chunk, ~1.5 KB per
pack, ~18 MB across the whole store, and about one false hit in ten lookups.
The filter's parameters are written into the footer beside it rather than
compiled in, so a later pack can choose differently without invalidating an
earlier one.

**The open pack is asked first, and it is asked differently.** Its contents are
in the sidecar rather than in any filter — it is one file, it is small, and its
index is already in memory because the writer is holding it. So a lookup checks
the open pack, then the filters. A loose object, in a store that has not
finished ingesting, is the third and last place to look, and that lane exists
only until ingest completes.

**There is no ingest of a loose store, and there is deliberately no migration.**
The earlier plan here was to write one anyway, on the reasoning that the first
install would be running the loose store. That holds only if packs are still
off when it arrives, and the answer to that is to turn them on first rather
than to write a migration for a population of zero. A development store written
before the switch is deleted, not converted.

What remains is the *read* lane: a lookup asks the packs and then falls through
to a loose object if there is one. It costs nothing to keep, it is what let
packs land beside a working store rather than in place of one, and it is what
makes a half-converted directory readable rather than a puzzle.

## Storage encryption, universal

**Built, for loose objects.** The frame codec, `storage.key` and its refusals
are Part 1 § At rest and Part 1 § Key material; what is still designed here is
the *pack* holding many frames rather than one. The reasoning below is kept
because it is the argument, not the description.

`storage.key`: 32 random bytes, **generated** rather than derived — there is no
secret worth deriving from — at first start, stored `0600` in the data dir,
first item in the must-not-lose set, with a one-time "back this up now" warning
printed at generation. Every chunk frame in every pack is AES-256-GCM under it
with a random 96-bit nonce stored in the frame.

**Uniformly**: both library types, and all backends including local disk. The
"encrypt only at the trust boundary" framing died on the NAS question — a NAS
is neither trusted-local nor remote, and a per-backend trust taxonomy is an
invariant that decays. The uniform rule is one sentence — *packs are
ciphertext, everywhere, always* — and its costs are invisible against hardware
AES at multiple GB/s. E2EE chunks get a second wrap, which is harmless and
keeps the backend ignorant of libraries.

What it buys: at-rest encryption on the primary tier for free, `rsync -a
storage/` backups safe to point anywhere, and — because every backend stores
identical bytes — replication as plain file copy of sealed packs.

Losing the key loses every pack on every tier, and it is effectively
**unrotatable**: every tier holds byte-identical ciphertext under it, so
rotation is a rewrite of everything everywhere. Cannot-lose and cannot-rotate
are two halves of one fact and the ops documentation states them together.

For a plain library this is the only encryption; for an E2EE library it is the
outer of two. The difference between the library types remains exactly one
sentence: *whether the server holds a key that can read the content.* Nothing
about storage layout differs.

**The frame is the unit, and it did not wait for packs.** The nonce lives in
the frame header, which is why the two are separable: a frame is
self-describing whether the file holding it contains one or a thousand. So the
loose store adopted the frame first — a loose object is one sealed frame at its
existing path, which is Part 1 § At rest — and packs arrive later as a
container of frames plus an index, with no change to the frame format or the
key. What that bought is at-rest encryption against the store an install is
actually running, and a loose store that is already made of the thing packs
will contain — so the pack ingest moves frames rather than making them. The
recovery scan that walks frames is the same code over a directory of
single-frame files or a pack. Nothing about
the decision above changes: cannot-lose and cannot-rotate hold from the first
frame written.

Two things the frame carries that this section did not originally give it: a
magic and a version byte, and the header bound as associated data. Both are in
Part 1 § At rest, and both are permanent from the first frame written — which
is what an unrotatable key means for the format under it as well as for the
key itself.

## Durable tiers

The same pack format byte-for-byte on every backend: server-local disk as the
hot tier, a filesystem on a NAS mount, and S3 — one PUT per sealed pack, since
request count is what object storage bills, and a chunk read is one ranged GET.

**A NAS and an S3 bucket are the same object logically**, and the interface
already says so: write-and-seal, ranged read, whole read, stat, list, remove,
remove-library, with absence normalised to one `ErrNotFound` at the seam.
Everything that differs between them is operational rather than semantic — S3
gives all-or-nothing PUT for free where a filesystem needs write-temp-then-
rename or a crash leaves a half pack that LIST reports; S3 bills requests and
egress where a NAS bills nothing, which is why reading back to verify is the
NAS's cheap option and the checksum header is S3's; a NAS fills up and object
storage effectively does not; and the latency difference decides read
preference and nothing about correctness. Trust is deliberately not on that
list. Packs are ciphertext on every tier including local disk, so no backend is
trusted and there is no per-backend trust taxonomy to keep true.

One consequence worth stating plainly, because it is the deployment people
reach for first: **running Silo on the NAS is not this feature.** Then the NAS
*is* local disk, there is one copy, and every property below that starts "the
other copy" does not exist. A tier is the two-copy arrangement — write locally,
seal, copy, verify, and only then let the local copy become evictable.

### A tier is a row, not a special case

Local disk is not a different kind of thing from a durable backend; it is the
same interface with different attributes. Naming the attributes collapses two
special cases into one table, and makes a multi-tier deployment expressible
without a policy matrix:

| attribute | what it decides |
|---|---|
| **backend** | which implementation; the four-verb floor below is the contract |
| **capacity** | a byte target, or unbounded. Bounded means it evicts |
| **counts toward copies** | whether a verified copy here satisfies the durability requirement, and so whether it may gate eviction elsewhere |
| **writable** | whether this server writes it, or only reads it |

**Bounded does not force uncountable — autonomous eviction does.** A bounded
tier evicted *by the writer that knows the global copy count* can still count,
because eviction and compaction-delete become the same operation gated by the
same check: never drop below the required number of copies. It is a tier that
manages its own disk on its own schedule — a replica trimming its cache — that
must be excluded, because the accounting and the deletion would be in two
places that can disagree. So the rule is about control rather than about size:

> **Only a tier whose eviction the copy count controls may gate eviction
> elsewhere.**

That gives two roles. An **authoritative** tier counts toward required copies.
A **cache** tier never does, is lossy at any moment, and falls through on a
miss — which means a read chain must always terminate in a reachable
authoritative tier, or a miss is a hard failure rather than a slow read.

Local disk is then a row like any other: bounded, cache, not counting. What
stays specific to it is the never-evictable floor — the open pack,
sealed-but-unverified packs, pack indexes, the catalog and `storage.key` —
because that floor is about what a running server needs, not about durability.
A cache tier should hold *all* pack indexes and evict only pack bodies, which
is the same rule local disk already follows, generalised for free: a few
hundred KB against 512 MB, and losing it means re-downloading packs to rebuild
by scan.

**Resist adding attributes.** The uniform-encryption rule exists because a
per-backend trust taxonomy decays, and a per-tier policy matrix decays the same
way for the same reason. Four attributes and one sentence of rule, or this
becomes the thing it was written to avoid.

### More than one durable tier

N sinks is structurally the easy case, and that is a payoff of write-once
rather than luck: packs are immutable once sealed, byte-identical everywhere,
named by ids that never collide, with one writer. Fan-out needs no
coordination, no ordering between tiers, and no consensus.

Four things generalise, and only the last is new work.

- **`verified` becomes per (pack, tier)** rather than a boolean — a set of
  tiers each pack has been verified on. Still derivable by scan, which is what
  keeps it safe to lose.
- **The upload queue becomes per tier.** An S3 outage and a NAS outage are
  independent, and the queue-depth alert has to name which one is behind.
- **Reads get a preference order** — nearest and cheapest first, falling
  through on a miss or an outage.
- **Deletions have to fan out, and that is the genuinely new piece.**
  Compaction deletes packs. With one tier a failed delete is a retry; with N,
  a tier that is offline when a delete happens accumulates garbage no mark
  phase will ever revisit, because the pack is gone from the catalog and
  nothing knows to look. So the upload queue grows a delete counterpart, and a
  periodic LIST-against-catalog reconciliation per tier is what catches the
  drift. Design this before building fan-out, not after.

The reason to be sparing is cost rather than architecture: N tiers is N times
the storage bill and N times the PUTs, and two buckets in one provider's region
buy much less failure independence than they look like they do.

**The one-writer invariant now has to hold per tier.** Exactly one Silo server
writes a given bucket-prefix or NAS path, nothing in the feature floor can
enforce it, and a read-only tier on a replica is how that stays true while more
than one server reads the same bytes.

**The S3 feature floor is PUT, ranged GET, DELETE, LIST. Nothing else.** Every
additional API a backend is assumed to have is a backend that stops working,
and the clones worth supporting diverge exactly where the API surface widens.
Single PUT per pack, no multipart: 512 MB is under every backend's single-PUT
ceiling, and multipart is precisely where clone behaviour diverges.

**Conditional writes are not required**, and that is a consequence of three
guarantees rather than an omission: one writer per bucket-prefix, pack ids that
never collide, and packs immutable once sealed. Where `If-None-Match: *`
exists, send it as an **assertion rather than a mechanism** — a precondition
failure then means split-brain or an id collision, and it should log loudly
rather than be retried, because a retry overwrites the evidence of what it just
detected.

**Ops documentation gains the one-writer invariant.** Exactly one Silo server
writes a given bucket-prefix. Nothing in the feature floor can enforce it. It
belongs beside the backup ordering rule, which is the other thing that is
correct only because a human read it.

### What "verified" means

Eviction deletes the only fast copy of data, so the word has to name a check:

1. **HEAD**, confirming the object exists and its length is exactly the pack's.
2. **The pack's SHA-256, computed by us**, checked against
   `x-amz-checksum-sha256` where the backend supports it.
2. **Where it does not**, a ranged read-back of the frame headers.

**ETag is not the check.** It is MD5 *sometimes* — not with SSE-KMS, not on
multipart, and not at all on several clones. A check that means "MD5 of one
part" on some backends and "an opaque string" on others is not a check, and the
failure it misses is the one that matters: a truncated object whose length
happens to match.

The corollary belongs in the durability story rather than being left implicit:
**a sealed-but-unuploaded pack is single-copy data.** Between sealing and
verification there is one copy on one disk, and the window is as long as the
upload queue is deep. So the queue is a first-class thing — packs without a
verified remote copy — derivable by scan, which is the recovery path and what
makes the column safe to lose, and persisted as a catalog column so the
ordinary case does not scan and an operator can ask how much of this server
exists once.

### The cache has a size, and zero is one of them

One knob per bounded tier: **a byte target for evictable pack data.** Bytes,
not a fraction of the disk — disks are shared, and a fraction of a disk
something else is also filling means something different every day. Over
target, evict verified packs least-recently-read first.

Local disk is the tier that always has this knob; a remote tier has it when it
is configured as a cache rather than as authoritative, and the mechanism is the
same code against a different backend.

- **Zero is the limit case, not a mode.** It means seal → upload → verify →
  evict immediately, and needs no separate code path. What stops it deleting
  the server is the floor of things never evictable at any target: the open
  pack, sealed-but-unverified packs, pack indexes, the catalog, and
  `storage.key`.
- **The bound is soft against that floor.** A durable-tier outage grows the
  unverified backlog past any target. The answer is the queue-depth alert,
  never refusing writes and never evicting something unverified to get back
  under a number.
- **With no durable tier configured the knob is inert by construction.**
  Nothing is verified remotely, so nothing qualifies as evictable, so a
  local-only deployment cannot misconfigure its way into deleting its only
  copy. That is a property of the eviction predicate rather than a check, which
  is why there is **no force-evict flag, ever** — one would be the only way to
  reach that outcome, and its existence is the entire risk.

### Evicting history before live data

Pure LRU already approximates this, since history-only chunks are not read. The
explicit signal earns its keep after a large history read — a restore, a
`.snapshot` browse — pollutes the cache, which is exactly when a naive LRU
evicts the working set instead.

It costs one more field in a mark that is already running:
**`head_bytes` beside `live_bytes` in `PackStats`** — reachable from the
current head, rather than reachable at all. That is a second bit in the same
walk, not a second walk. Eviction then orders by
`(head_bytes / total_bytes, last_read)` rather than by recency alone.

The precise version falls out of compaction rather than needing anything of its
own: compaction is already rewriting packs, so it can sort live frames into
head-reachable and history-only packs, at which point eviction is exact instead
of statistical. Treat that as a scheduling input like locality and undersize —
one more reason to rewrite a pack, not a second mechanism.

### Migrating between tiers

Adding a tier and demoting one are ordinary operations, and the order matters
because one direction is safe and the other is not. The worked case: a 1 TB NAS
that fills, an S3 bucket added beside it, and the NAS kept afterwards as a
cache.

1. Add S3 as authoritative and unbounded, and put it in the required set.
2. Backfill every sealed pack not yet there, verifying each as it lands. This
   is the existing upload-and-verify queue pointed at already-sealed packs; it
   is restartable at any point, because packs are immutable and verification is
   idempotent.
3. **Wait for a predicate, not an event.** Not "replication finished" but *the
   count of packs not verified on S3 is zero*, evaluated against the per-tier
   verified set — derivable by scan, and therefore checkable rather than
   trusted.
4. Flip the NAS to cache with a byte target. Nothing moves and nothing is
   deleted at the flip; it is a metadata change, and the NAS's existing
   contents become a warm working set rather than a cold cache.

Three things to get right:

- **The capacity bound must be inert until the gate passes.** A NAS that starts
  evicting while it still holds the only durable copy of some pack races the
  backfill and deletes it. Same shape as the rule that the local knob is inert
  with no durable tier configured.
- **The backfill must tolerate packs vanishing.** Compaction running alongside
  it can delete a pack mid-upload. That is safe, since ids are never reused,
  but "no longer in the catalog" has to be a normal skip rather than an error
  that stalls the queue.
- **Demotion is cheap and promotion is not.** A cache has holes by definition,
  so turning one back into an authoritative tier means proving completeness,
  which means a full backfill anyway. The flag is easy to lower and expensive
  to raise.

None of this re-encrypts, re-chunks, or changes an id. Every tier holds
byte-identical ciphertext under `storage.key`, so a tier migration is `cp` —
which is the payoff of the uniform-encryption decision, and the same property
that makes a replica's copy of a pack legal.

**The state before the new tier is added is the one to alert on.** A full
authoritative tier stalls uploads, the unverified backlog grows, and nothing
unverified is ever evictable — so local disk grows until it fills and writes
stop. The design's answer is deliberately "alert on queue depth, never refuse
writes, never evict unverified", which makes the queue-depth alert the only
thing standing between a full NAS and a wedged server. The sequence above is
the recovery path; it has to start before local disk fills.

## The tracing mark, and compaction

The three CLI reclaimers in Part 1 are the shape of this without packs. With
packs it becomes one mark phase feeding a scheduler.

- **Mark is a tracing collector.** Liveness is global reachability from live
  commits; it cannot be maintained incrementally and **must never be
  refcounted**.
- Mark output is `PackStats(pack_id, total_bytes, live_bytes, head_bytes,
  gc_id)` in the catalog — a scheduling input, stale by construction,
  re-verified before any sweep. `live_bytes` drives compaction and `head_bytes`
  drives eviction order, and the two are one walk: the difference between them
  is what history is keeping alive in that pack.
  **The catalog row arrives with the in-process scheduler, not with the CLI.**
  `silo gc -compact` draws the plan and acts on it in one process, seconds
  apart, with the marks in memory: a row is for a *later* process to schedule
  from, and `gc_id` on it is what would make its staleness detectable. A table
  nothing reads is speculative code, so it lands when there is a reader.
- Compact a pack when its dead fraction crosses a threshold (default 0.5),
  rate-limited autovacuum-style: a threshold and an I/O budget, not "quiet
  hours". Rewrite live frames into a new pack, fsync, swap index entries
  atomically, delete the old pack locally and remotely. Idempotent and
  interruptible at every step.
- **Gate on dead fraction, order by locality, starve nothing.** Compacting a
  remote-only pack means downloading it first. The economics settle the
  ordering rather than taste: that egress costs roughly the pack's size
  **once**, about four months of storing those same bytes; leaving a mostly
  dead pack on paid storage costs its dead fraction **every month, forever**. A
  one-time cost against a perpetual one always eventually pays for itself, so
  deprioritised-but-not-starved is the only consistent ordering. Pure
  locality-first leaves paid storage full of garbage, and operator-only remote
  compaction fails the same way with extra steps: it converts a decision the
  system can price precisely into a chore a human defers indefinitely.
- **Two budgets, because they are two bills.** Local rewrites spend disk I/O.
  Remote compactions spend disk I/O *and* egress, and those have different
  owners — one shows up as a slow server, the other as a line item somebody has
  to explain. So the limiter carries a separate egress budget: bytes downloaded
  per interval, config-visible, generous default. That is also the honest knob
  to hand an operator — not "may the system touch remote packs", but "how fast
  may it spend money doing so".
- **Compaction may also merge undersized packs**, at the lowest priority there
  is. Age-sealing produces small packs on a quiet server, and smallness is a
  nuisance rather than a leak. Same machinery, one more scheduling input, not a
  second mechanism.

The mark also produces the number retention trades against: **the bytes only
this library's history keeps alive**, because a chunk another library still
reaches is reclaimed by nobody's truncation. That is computable inside a global
mark without refcounts and no other way. It reaches the wire as
`history_bytes` with a `measured_at` — per-library because the knob is, and
dated because it is stale by construction, where visible staleness beats silent
drift.

## Compression

Client-side, before encryption, because ciphertext does not compress.
Attempt-and-store-raw per chunk, so the 95% of measured bytes that are already
compressed cost nothing. No per-library knob: the tuning media actually wants
is "skip it", which is what per-chunk attempt-and-discard does by itself.

## Conversion is a named operation

Switching a library between E2EE and server-readable — including "make this
library public", which [`plans/sharing.md`](plans/sharing.md) needs — is `silo
convert`: a client holding CK reads every file, re-chunks under the target seed
(the seeds differ, so boundaries move), re-uploads, and commits new manifests.
O(library), client-side, resumable, deliberate. Nothing should be designed as
if the toggle were a server-side flag flip.

Conversion **starts a new history root by default**: old commits reference
chunks only CK can read, and a library whose own access mode cannot read its
history is a trap. E2EE → server-readable may keep the dark tail behind an
explicit flag; conversion to public never does, because `public_read ⇒
server-readable` must hold for everything an anonymous client can reach,
`changes?since=` included.

## Observability for the background workers

The upload queue, the eviction verifier and the compaction worker are this
server's first long-running goroutines: no request to hang a trace on, no
middleware in the call stack to catch a panic. Each gets its own
recover-that-reports, in the pattern [`error-reporting.md`](error-reporting.md)
already establishes. Without it a panic in the upload queue takes the process
down with no event and the operator learns about it from the restart.

**Three conditions are pinned at error level**, which is what makes them
reportable at all — the hook's threshold is the whole mechanism:

- **A durable-copy verification mismatch.** The pack on the remote tier is not
  the pack that was written. The check runs *before* the local copy is deleted,
  so this warns about a bad remote copy rather than reporting data already
  lost.
- **An `If-None-Match: *` precondition failure**, which signals one of the two
  things the one-writer invariant forbids.
- **The upload queue failing to drain past a depth-or-age threshold.** Not any
  single failure: the *shape* of a queue that is not moving, which means the
  single-copy window is growing.

**Retryable object-storage failures stay below error level** — a warn, seen in
logs, sent nowhere. A retryable blip recurring several times a minute would
drown the three above. A retry that then succeeds is not a failure; a retry
budget exhausted is, and the queue-depth condition catches that.

**`store/` stays log-free and error-reporting-free, permanently.** It ships
inside clients and is reimplemented in Swift, so it has no business holding an
opinion about where a server sends its errors. Errors cross that boundary as
return values, and every sentinel in the package exists so a caller can decide
what to do with one.

## Deliberately not built

- **No pack identifiers on the wire, ever** — compaction makes them lies.
- **No skip-encryption branch in any backend.** Uniform or nothing; the first
  "this chunk is already ciphertext so skip the wrap" flag is the beginning of
  the taxonomy the uniform rule exists to avoid.
- **No chunk-parameter renegotiation.** Parameters are frozen per library;
  changing them is a rechunk, done deliberately or not at all.
- **No refcounts in the store.** Liveness comes from the mark, only ever.
- **The catalog never holds chunk locations.** The store stays reconstructible
  from packs plus `storage.key`.
- **No server-side key cache and no plaintext-derived ids.** Carried from
  [`encryption.md`](encryption.md), and the reason the third store state exists
  as a first-class thing rather than an error path.
- **No server-side three-way merge.** Merging trees means reading names.
- **No plaintext-requiring feature that is not explicitly scoped to plain
  libraries.**

Clients carry one rule of their own that belongs here because the format is why
it is needed: **a FUSE client mounts `nosuid,nodev` by default.** The format
keeps all twelve mode bits, and in a plain library mode is public and
server-writable — so a hostile server can set `04755` on any file and a
faithful client restores it, which hands a local privilege-escalation path to
exactly the party the threat model calls actively malicious for integrity.
`nosuid` makes the restored bit inert. Under E2EE the same server cannot touch
mode at all, since it is sealed: an asymmetry worth knowing when reasoning about
the two library types.

## What is left, in order

1. **The split-derivation login — the client half.** The server half is built:
   the atomic switch-over write landed, so an account can no longer be left
   with parameters describing a password the stored hash was not made from.
   What is left is a client that derives `authKey` and sends it, which is
   silo#13 and where this was always mostly going to live.
2. **Packs** — the container format, per-pack indexes,
   seal-on-size-or-age-or-shutdown, the recovery scan, and the loose-store
   ingest that scan doubles as. Loose objects are already frames, so the
   ingest is a rewrite of where they live rather than of what they are.
3. **The tracing mark and compaction** — `PackStats`, threshold and throttled
   rewrite, locality and undersize as scheduling inputs, two budgets. Built
   together with per-library GC, which is the same mark.
4. **Durable backends** — NAS and S3 against the four-verb floor, async upload
   of sealed packs, verified-then-evictable local cache, the cache-size knob,
   the unverified-packs column and the scan that rebuilds it, replication as
   pack copy. The background workers arrive here and bring their panic recovery
   and the three error-level conditions with them.
5. **Compression**, measured before it is written.
6. **`silo convert`.**

One piece of debris to clear on the way past, not load-bearing: nothing creates
a `VirtualLibrary` row while several queries still join the table.
