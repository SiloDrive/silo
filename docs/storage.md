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

## Where the bytes are

```
<data-dir>/storage/chunks/<library-id>/<aa>/<the rest of the id>
<data-dir>/storage/objects/<library-id>/<aa>/<the rest of the id>
```

One object per file, fanned out on the first two hex characters of its id.
Writing is temp-file-then-rename inside the destination directory, so a reader
never sees a partial object and the publish never crosses a filesystem. With
`sync` the data is fsynced before the rename and the directory entry after —
not optional for correctness, because the branch head lives in SQLite, which
fsyncs its own WAL, so a head can survive a power cut that the objects it
references do not. Nothing repairs that afterwards: the client believes it
already uploaded those chunks, so a resync does not send them again.

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

There is no storage-layer encryption and no compression: objects are stored
exactly as they arrive. Both are Part 2.

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
POST   /libraries/{id}/blocks/missing                which of these chunks do you lack?
PUT    /libraries/{id}/blocks/{id}                   a chunk, verified against its id
GET    /libraries/{id}/blocks/{id}                   a chunk, as stored
PUT    /libraries/{id}/objects/{id}                  a manifest, directory or commit
GET    /libraries/{id}/objects/{id}                  the same
PUT    /libraries/{id}/head                          If-Match: <current head commit id>
POST   /libraries/{id}/batch                         many operations, one commit
*      /libraries/{id}/entries/{path}                the path-addressed surface
GET    /account/usage
```

All of it is under `/api/silo/v1`.

**Structure by path, content and writes by id.** `entries/{path}` keeps working
on an E2EE library for everything structural, because AES-SIV is deterministic
precisely so that routing on ciphertext works: a client encrypts each segment,
base64urls it, and sends it, and the server matches ciphertext against
ciphertext without learning what either says. Resolve, `HEAD`, listing, delete
and move all work. `changes?since=` carries ciphertext paths for the same
reason.

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

`server-info` advertises `blocks`, `batch`, `changes`, `entries`,
`entries-copy`, `conditional-writes`, `ranged-reads`, `pagination`,
`library-rename`, `libraries` and `usage`, so clients feature-detect rather
than version-sniff.

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
declares whatever it likes. `PUT blocks/{id}` gets a soft ceiling: it refuses an
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

Three reclaimers, all under `silo gc`, all reporting by default and removing
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

**`-expire-history`** drops the commit objects outside a library's retention
window. It is the only operation in Silo that deletes something a commit
reaches, and it is irreversible once the sweep behind it runs, so everything
about it aims at making the set it chooses obvious: it deletes commit objects
and nothing else, leaving the chunks to the orphan sweep, which already knows
how to decide whether anything still reaches them. Expiry runs before the sweep
in the same invocation, because an operator asking for both meant "reclaim what
retention allows", not "reclaim it next time".

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
```

`retention` and `df` are CLI rather than API for the reason the user commands
are: there is no admin role to gate an endpoint with, and retention is a
server-side setting that bounds the operator's disk rather than something a
library's owner decides. Asking and answering are the same command with and
without a value, so an operator who types the reading form with an argument
gets the change they asked for rather than a usage message.

## What Part 1 does not do

- **E2EE libraries cannot be created.** `CreateLibrary` refuses one outright,
  and the refusal is honest rather than a stub: a content key is wrapped to
  each member's X25519 public key, that key is an account column that does not
  exist, and the wrap has no table. Creating one now would produce a library
  whose key dies with the device that made it, which is data loss wearing a
  feature's clothes. Everything else about E2EE — the codecs, the key-free
  readers, the three store states, the name encryption, the wrap primitives and
  their vectors — is built and tested. What is missing is the account side. See
  Part 2, and [`auth.md`](auth.md#the-accounts-key-material).
- **There is no cache in front of the store.** A read is a read.
- **Objects are stored raw**, unencrypted and uncompressed.
- **The id-addressed surface has no `server-info` feature name.** `blocks`
  covers the chunk half; `objects/{id}` and `PUT head` are undiscoverable.
- **Nothing creates a `VirtualLibrary` row**, and several queries still join
  the table.

---

# Part 2 — designed, not built

## End-to-end encryption, end to end

The format is done and the account model it needs is **built** — see
[`auth.md`](auth.md#the-accounts-key-material), which is now the normative
description of all five items below. What follows is the reasoning that chose
them, and it holds. The gap it described was four schema items and one
endpoint:

- the published X25519 public key;
- the wrapped identity private key, one blob;
- recovery blobs as **individually deletable rows** — that granularity is
  forced by the redemption rule below rather than chosen;
- the client's KDF parameters;
- a pre-login endpoint serving a user's KDF salt.

The endpoint is the one with teeth. It is unauthenticated, so it is an
account-enumeration oracle in a new place, and its answer is
attacker-influenced input to the client's KDF — which is what the parameter
ceiling in `store/` was written to survive. Asking for an unknown address must
return a deterministic fake salt, indistinguishable from a real one, exactly as
login does. The account half of this is written up in
[`auth.md`](auth.md#the-accounts-key-material).

**Keys.** An X25519 identity keypair per user, its private half wrapped under a
key derived from the user's password and stored server-side as an opaque blob.
A content key **CK** per library: 32 random bytes, generated client-side at
creation, never password-derived, wrapped to each member's public key as one
blob per member. Sharing is a wrap for one more key; a password change re-wraps
only the user's own private key.

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

**Default on.** E2EE is per-library and chosen at creation, and the common
library is meant to be the one where server-side preview, search, thumbnails
and inline rendering do not exist. Server-readable is the deliberate exception,
chosen for the libraries that want those features. The costs are real and stay
listed in [`encryption.md`](encryption.md): no server-side preview or search,
browser access needs client-side crypto or does not exist, key loss is data
loss, revocation is not retroactive. Features needing plaintext get scoped
explicitly to plain libraries, so the boundary lives in code.

### One password, split client-side

Silo's login sends the password to the server. If that password also wraps the
identity key, the server sees the wrapping secret at every login and E2EE is
theatre. The fix:

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
`ciphertext_len`, nonce, `ciphertext+tag`), so the store stays reconstructible
from packs plus the storage key alone, with no database required.

**Pack indexes** are per-pack local files mapping `chunk_id → (offset, len)`,
sorted by id, mmap'd, with an in-memory summary layer across packs — always on
local disk regardless of where the pack lives, so a chunk lookup is never a
round trip. They are rebuildable by scanning packs, which makes index loss an
inconvenience rather than data loss *while every pack is local*. Once the local
tier is an evictable cache, rebuild-by-scan means re-downloading every evicted
pack: a full-store egress bill. So each sealed pack's index is uploaded beside
it — a few hundred KB next to 512 MB — and recovery fetches indexes, never
packs.

**The recovery scan also has to ingest a loose store.** Everything above is
justified by there being no installs, and that is true right up until the first
one, which will be running the loose store. Leaving it unwritten would make the
one migration this project claims not to need the one it discovers in
production. It is cheap when planned: ids do not change across the boundary, a
pack is a container rather than a new naming scheme, and the scan that rebuilds
an index from pack bytes is most of the machinery. Ingest is that scan pointed
at loose objects — read each, append its frame to an open pack, index it,
delete the loose copy once the index is durable — restartable at every step,
because it is the same append → fsync → index update the writer uses.

## Storage encryption, universal

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

## Durable tiers

The same pack format byte-for-byte on three backends: server-local disk as the
hot tier, a filesystem on a NAS mount as a durable tier, and S3 as a durable
tier — one PUT per sealed pack, since request count is what object storage
bills, and a chunk read is one ranged GET.

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
3. **Where it does not**, a ranged read-back of the frame headers.

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

One knob: **a byte target for evictable local pack data.** Bytes, not a
fraction of the disk — disks are shared, and a fraction of a disk something
else is also filling means something different every day. Over target, evict
verified packs least-recently-read first.

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

## The tracing mark, and compaction

The three CLI reclaimers in Part 1 are the shape of this without packs. With
packs it becomes one mark phase feeding a scheduler.

- **Mark is a tracing collector.** Liveness is global reachability from live
  commits; it cannot be maintained incrementally and **must never be
  refcounted**.
- Mark output is `PackStats(pack_id, total_bytes, live_bytes, gc_id)` in the
  catalog — a scheduling input, stale by construction, re-verified before any
  sweep.
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

1. **The account side of E2EE.** The four schema items and the salt endpoint
   with its dummy-salt closure are **built** — see
   [`auth.md`](auth.md#the-accounts-key-material) for the routes and the
   reasoning. What is left of this item is **creation of an encrypted
   library**: the content key is wrapped to each member's public key, that key
   now exists, and the wrap blob still has no table. Everything below is
   storage; this is the one item that unblocks a product decision already
   taken.
2. **The split-derivation login**, which lands with or after
   [`auth.md`](auth.md)'s credential work and cannot land before it.
3. **A `server-info` feature name for the id-addressed surface**, so a client
   can detect `objects/{id}` and `PUT head` rather than assume them.
4. **Packs** — the format, per-pack indexes, seal-on-size-or-age-or-shutdown,
   `storage.key` and its backup wiring, the recovery scan, and the loose-store
   ingest that scan doubles as.
5. **The tracing mark and compaction** — `PackStats`, threshold and throttled
   rewrite, locality and undersize as scheduling inputs, two budgets. Built
   together with per-library GC, which is the same mark.
6. **Durable backends** — NAS and S3 against the four-verb floor, async upload
   of sealed packs, verified-then-evictable local cache, the cache-size knob,
   the unverified-packs column and the scan that rebuilds it, replication as
   pack copy. The background workers arrive here and bring their panic recovery
   and the three error-level conditions with them.
7. **Compression**, measured before it is written.
8. **`silo convert`.**

One piece of debris to clear on the way past, not load-bearing: nothing creates
a `VirtualLibrary` row while several queries still join the table.
