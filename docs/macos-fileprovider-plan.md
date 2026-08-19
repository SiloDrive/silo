# macOS File Provider Client for Silo

Plan for a native macOS selective-sync ("files on demand") client that mounts Silo
repos into Finder, using Apple's File Provider framework.

Scope: macOS client + the Silo-side endpoints it needs. Nothing about Linux,
Windows, or the existing SeaDrive GUI beyond what we can reuse.

> The server side of this plan has since been built. For the current wire
> contract — request shapes, real responses, and the gaps that are still gaps —
> see [`porter-brief.md`](porter-brief.md). This document remains the design:
> why the File Provider route, how identity works, and what to build in which
> order.

## Why not the alternatives

**WebDAV mounting** — `webdavfs` is a network filesystem, not selective sync.
No local cache with eviction, no offline access, no per-file pin state, no Finder
badges. Every read is a round trip. The badness is architectural, not
configuration, so no amount of server work fixes it. (WebDAV *behind* a File
Provider extension would be fine, but we own both ends, so there is no reason to
add the indirection.)

**Reusing SeaDrive's extension** — `SeaDrive File Provider.appex` is closed
source and signed by Seafile's team. We can re-sign it to test GUI changes, but
we cannot build or modify it.

**Porting seadrive-fuse's engine** — ~43.6k lines of C, of which ~11.4k is
bidirectional merge logic that the File Provider model makes unnecessary (see
below). Wrong tool.

## Verified: the content-addressing model

Checked against both `seadrive-fuse/src/fs-mgr.c` and
`silo/fileserver/fsmgr/fsmgr.go`. Everything is SHA-1, hex-encoded to 40 chars.

| Object | ID is SHA-1 of | Reference |
|---|---|---|
| Block | raw block content | `objstore/backend_fs.go:109` |
| File (`Seafile`) | JSON `{type, version, size, block_ids[]}` | `fsmgr.go:406`, `fs-mgr.c:129` |
| Dir (`SeafDir`) | JSON of its entries | `fsmgr.go:394-401` |
| Commit | JSON of commit fields | `commitmgr.go:74` |

A `SeafDirent` is `{Mode, ID, Name, Mtime, Modifier, Size}` — note there is **no
identity field**. `ID` is the content hash.

Empty dir is the sentinel `EmptySha1` (40 zeros).

### The consequence: content hashes cannot be item identifiers

This is the single most important constraint on the design, and it falls
directly out of content-addressing being *correct*:

1. **Not unique.** Two files with identical content have the same file object ID
   — that is the dedup property, working as intended. But
   `NSFileProviderItemIdentifier` must be unique per item. Two copies of the same
   file in a repo would collide and Finder would treat them as one item.

2. **Not stable.** Editing a file changes its ID. File Provider identifiers must
   survive content changes; that is what `NSFileProviderItemVersion` is for.

3. **Cascading churn.** Because a dir's ID is the hash of its entries (including
   each child's ID *and name*), touching any file rewrites its parent's ID, its
   grandparent's, up to root, and produces a new commit. Directory IDs are the
   least stable thing in the system.

4. **Paths are not stable either.** Rename and move change the path, but File
   Provider requires the identifier to persist across a rename *it initiated* —
   you return the same `itemIdentifier` with a new `filename`. If identity were
   path-derived, every rename would look like delete + create: Finder churn, lost
   download state, and re-downloading of already-materialised files.

**Therefore something must maintain a synthetic, persistent identifier layer.**
The open question is which side of the wire it lives on — see "Identifier
design" below. It is *not* Silo: SeaDrive proves it can be done entirely in the
client, which is why unmodified Silo works with SeaDrive today.

### Where content-addressing pays off instead

Content hashes are exactly right for `NSFileProviderItemVersion`:

```swift
NSFileProviderItemVersion(
    contentVersion:  fileObjectID.data,   // SHA-1 of the Seafile object
    metadataVersion: metadataHash.data    // hash of (name, parent, mode, mtime)
)
```

The system uses `contentVersion` to decide whether a materialised file is stale.
We get a perfect, cheap, already-computed answer for free. Content-addressing is
a gift for *versioning* — just not for *identity*.

It also helps with rename detection (see reconciliation below): a file whose
content hash is unchanged appearing at a new path is almost certainly a rename.

## Architecture

```
   Finder / any app
         │
         ▼
   ┌──────────────────────┐
   │ fileproviderd        │  macOS, owns the local replica
   └──────────┬───────────┘
              │ NSFileProviderReplicatedExtension
              ▼
   ┌──────────────────────┐
   │ PorterFileProvider   │  Swift .appex, sandboxed
   │  .appex              │  stateless-ish, HTTP client
   └──────────┬───────────┘
              │ HTTPS
              ▼
   ┌──────────────────────┐
   │ Silo                 │
   └──────────────────────┘

   ┌──────────────────────┐
   │ Porter.app           │  container app: login, add/remove domains,
   │  (menu bar)          │  registers NSFileProviderDomain per account
   └──────────────────────┘
```

**One domain per account**, with repos as the top-level entries under
`.rootContainer`. (SeaDrive does the same; it keeps the Finder sidebar to one
entry per server.)

Crucially, **macOS owns the local replica**. It tracks local edits, decides what
to materialise and evict, and calls us with discrete item operations. We do not
implement a sync loop, a merge algorithm, or conflict naming. That is why this is
a few thousand lines rather than forty.

We use Silo's **file-level REST API**, not the Seafile block sync protocol. Block
sync (`/repo/{id}/check-fs`, `/recv-fs`, `/block/{id}`, …) exists to keep two
independent replicas convergent — a problem File Provider has already solved.

## Identifier design

### Prior art: how SeaDrive already solves this

Recovered from the shipped `SeaDrive File Provider.appex` binary (`strings`, so
the schema is verbatim; the surrounding logic is inferred):

```sql
CREATE TABLE IdMap (
  id         INTEGER PRIMARY KEY,
  domain_id  TEXT,
  identifier TEXT,     -- the NSFileProviderItemIdentifier
  parent     TEXT,     -- parent's identifier
  repo_id    TEXT,
  path       TEXT
);                     -- later migrations add: policy INTEGER, rename_path TEXT

CREATE TABLE WorkingSet (
  id             INTEGER PRIMARY KEY,
  identifier     TEXT,
  mtime          INTEGER,
  size           INTEGER,
  is_dir         INTEGER,
  commit_version TEXT,
  obj_id         TEXT   -- content hash
);                      -- later migration adds: mode INTEGER
```

The load-bearing statement is:

```sql
UPDATE IdMap SET parent=?, repo_id=?, path=? WHERE identifier=?
```

Rename and move rewrite the path and keep the identifier — exactly what File
Provider requires. Deletes cascade by `parent`, `repo_id` and `domain_id`, which
is how removing a repo or an account tears down its subtree.

Two details worth stealing:

- `WorkingSet.obj_id` is the content hash, used as `contentVersion`. Confirms
  the versioning approach above.
- `IdMap.policy`, queried with `path LIKE`, is the "keep downloaded" pin stored
  per-subtree rather than per-file. **Do not steal this one** — see below.

**This lives entirely in the client.** Silo never sees an item identifier. That
is why unmodified Silo already works with SeaDrive today.

### Our design

Mirror it. Same two tables in the extension's container, keyed by domain. Repo
IDs are stable UUIDs so a repo's identifier can be its repo ID; root is
`NSFileProviderItemIdentifier.rootContainer`.

There is no reason to invent something different — this schema has survived
production contact, and matching it keeps the option of running both clients
against the same server without surprises.

**Except `policy`. Drop that column.** SeaDrive is a `NSFileProviderExtension`
of the older generation and had to build pinning itself; a replicated extension
does not. `NSFileProviderItem.contentPolicy` (macOS 13.0, comfortably under our
14.0 floor) is a declarative property on the item:

| `NSFileProviderContentPolicy` | Meaning |
|---|---|
| `.inherited` | take the parent's policy — the default on everything but the root |
| `.downloadLazily` | fetch on read, evict under disk pressure — the macOS root default |
| `.downloadLazilyAndEvictOnRemoteUpdate` | as above, plus drop the local copy when the server's changes |
| `.downloadEagerlyAndKeepDownloaded` | pre-fetch, keep fetching updates, refuse eviction |

`.inherited` is what makes the `path LIKE` query unnecessary: a pin on a folder
already applies to everything beneath it, because the system walks the tree for
us. Moving an `.inherited` item into a `.downloadEagerlyAndKeepDownloaded`
folder even schedules its download automatically. All we store is the pin the
user actually set, on the item they set it on — a small set of explicit
choices, not a policy on every row of `IdMap`.

The counterpart for the other direction is
`NSFileProviderManager.evictItem(identifier:)`, and the old
`NSFileProviderItemCapabilities.allowsEvicting` is formally deprecated in
favour of `contentPolicy` as of macOS 13.

The practical consequence is that the M5 "escape hatch" below gets easier: pins
are a handful of user choices worth persisting separately, not a column that
makes dropping and rebuilding `IdMap` dangerous.

### The real fork: where reconciliation gets its input

Whichever side holds the map, it has to be maintained when *someone else*
commits — SeaDrive and Seafile Desktop still write to Silo, and they commit whole
trees. Given a new commit, we must diff against its parent and classify:

- path in both, same content hash → unchanged
- path only in new → create
- path only in old → delete
- **same content hash at a different path → rename/move**, update `path`, keep
  `identifier`

That last rule is where content-addressing pays off. It is imperfect — deleting
one file and creating another with identical bytes looks like a rename — but the
failure mode is benign: an identifier follows the "wrong" copy of byte-identical
content.

The genuine architectural choice is **how the client learns what changed**:

| | **A. Local tree state** (SeaDrive's way) | **B. Server delta endpoint** |
|---|---|---|
| Client walks commits/fs objects itself | yes — needs a local object store | no |
| Silo changes | none | one new endpoint |
| Client complexity | must replicate commit/fs/block objects and diff trees | issue a GET, apply the result |
| Offline enumeration | works | degraded |
| Bandwidth | fetches fs objects | one small response |

SeaDrive takes **A**, which is precisely why the appex embeds all 43k lines of
the engine and why its container holds `commits/`, `fs/`, `storage/` and
`deleted_store/` directories. It is self-sufficient, and it is a lot of machinery.

**Recommendation: B.** We own the server. Reimplementing tree diffing in Swift to
avoid one Go endpoint is a bad trade. A shows what is possible without server
cooperation; we have server cooperation.

This is now a considered choice rather than a necessity, and it is reversible —
starting with B does not preclude adding local object caching later for offline
enumeration.

## HTTP request inventory

> **Superseded.** The table below described `/api/silo/v1` as it was when this
> plan was written. That surface has since been replaced by the `entries`
> endpoint, and the delta endpoint has been built. The current, verified
> contract is in [`porter-brief.md`](porter-brief.md) — build against that.
> This section is kept because the callback-by-callback breakdown is still the
> right way to think about the work.

This is the contract to verify against Sentry. Every File Provider callback and
the request it makes.

| Callback | Method + path | Status |
|---|---|---|
| domain setup | `POST /api2/auth-token/` | superseded — use `/api/silo/v1/auth/login` |
| — | `GET /api/silo/v1/server-info` | exists |
| `enumerateItems` (root) | `GET /api/silo/v1/repos` | exists, now returns `head_commit_id` |
| `enumerateItems` (dir) | `GET /api/silo/v1/repos/{id}/dir/?p={path}` | superseded — `entries/{path}` |
| `currentSyncAnchor` | `GET /repo/{id}/commit/HEAD` | superseded — `head_commit_id` |
| `enumerateChanges` | `GET /api/silo/v1/repos/{id}/changes?since={commit}` | **built** |
| `item(for:)` | *none* — local `IdMap` ⋈ `WorkingSet` | — |
| `fetchContents` | `GET /api/silo/v1/repos/{id}/file?p={path}` → redirect | superseded — `entries/{path}`, no redirect |
| — | `GET /files/{token}/{name}` | exists |
| `createItem` (file) | `GET .../upload-link` then `POST /upload-api/{token}` | superseded — `PUT entries/{path}` |
| `createItem` (dir) | `POST /api/silo/v1/repos/{id}/mkdir` | superseded — `PUT entries/{path}?type=dir` |
| `modifyItem` (contents) | `POST /update-api/{token}` | superseded — the same `PUT` |
| `modifyItem` (rename) | `POST /api/silo/v1/repos/{id}/rename` | superseded — `POST entries/…` `{"op":"move"}` |
| `modifyItem` (reparent) | `POST /api/silo/v1/repos/{id}/move` | superseded — the same move call |
| `deleteItem` | `DELETE /api/silo/v1/repos/{id}/file?p={path}` | superseded — `DELETE entries/{path}` |
| push invalidation | `WS /notification` | exists |

The prediction here held: almost the entire surface already existed, and the
delta endpoint was the only genuinely new one. What changed is its *spelling* —
the operations are the same, addressed consistently, and `client/client.go` was
moved onto them without any change to its method signatures.

One capability was added that this plan did not anticipate: **GET carries an
`ETag` of the content hash and honours `If-None-Match` with a 304**, so
revalidating a materialised item reads no blocks. See the brief.

Note that identifiers never cross the wire: every request is expressed in
`(repo_id, path)`, resolved client-side from `IdMap`. Silo's request logs and
Sentry traces will therefore look the same as SeaDrive's — a useful property,
since it means SeaDrive itself is a working reference for what correct traffic
looks like.

### Error mapping

"Handling all the HTTP requests correctly" is mostly this table. Getting it wrong
produces the classic sync-client failure modes: silent data loss, infinite retry
loops, or Finder showing stale state forever.

| HTTP | `NSFileProviderError` | Behaviour |
|---|---|---|
| 401 | `.notAuthenticated` | prompt for re-login in container app; do not retry |
| 403 | `.cannotSynchronize` | surface; do not retry |
| 404 | `.noSuchItem` | treat as deleted; reconcile |
| 409 | `.filenameCollision` | return the existing item so the system renames |
| 410 | `.syncAnchorExpired` | anchor too old to diff from; full enumeration |
| 412 / version mismatch | **no error** — see below | inside `modifyItem`, return the server's item on the success path with `shouldFetchContent: true`. Send `If-Match` built from `baseVersion.contentVersion` on every write |
| 413 / 507 | `.insufficientQuota` | surface to user |
| 429 | `.serverUnreachable` | honour `Retry-After`, back off |
| 500 | `.cannotSynchronize` | **do not retry** — may have been partly applied |
| 502 / 503 / 504 | `.serverUnreachable` | transient, nothing applied; back off and retry |
| offline / DNS | `.serverUnreachable` | retriable |
| page token stale | `.pageExpired` | restart enumeration |

Two rows in that block are finer than the draft they replace, and both came from
reading `silo/docs/responses.md` rather than from anything failing.

**5xx is not one row.** `503` is transient, carries `Retry-After`, and the
identical request will work; `500` means the request *may have been partly
applied*, and it is also how a library with damaged storage reports itself.
Retrying a `500` blind is how a client turns a server fault into its own. `502`
and `504` come from a proxy rather than Silo and are transient by nature.

**`409` means one thing, but only since 0.4.4.** It briefly also carried
`GC conflict; retry`, where renaming — the correct response to a collision — is
precisely wrong. That is the reason the floor is a version comparison and not a
feature check: no feature name appeared or disappeared when the meaning moved.

Two corrections to an earlier draft of this table, both verified against the
macOS 26.5 SDK headers:

**`.versionOutOfDate` does not exist.** The enum is `-1000…-1007` and
`-2001…-2015`; nothing in it is that. The nearest names are
`.versionNoLongerAvailable` (-2009, macOS 12.3), which belongs to
`fetchPartialContents` under `strictVersioning` and not to writes at all, and
`.localVersionConflictingWithServer` (-2015), which *is* this conflict but is
macOS 26.0+ and only under the opt-in `failUploadOnConflict` policy. On our
14.0 deployment target a 412 is reported by *succeeding* with the server's
item, not by failing. The brief has the full sequence.

**`.pageExpired` and `.syncAnchorExpired` are the same value.** `PageExpired`
is a literal alias for `SyncAnchorExpired` (-1002) in the header, so the last
two rows are one row wearing two names. Returning one and expecting the system
to distinguish it from the other will not work — the fallback is a full
re-enumeration either way.

Server-side Sentry should show us anything the extension sends that Silo does not
expect — malformed paths, unescaped `?p=` values, missing auth headers, requests
against tombstoned items. Client-side Sentry in an appex works but is limited;
extension crash reports are unreliable, so lean on breadcrumbs and explicit
`captureMessage` for the error branches above rather than trusting crash capture.

## Silo-side work

**Done.** Item 1 below shipped, along with the `entries` surface and the ETag
support the brief describes. M0–M3 need no further server work.

1. ~~`GET /api/silo/v1/repos/{id}/changes?since={commit}`~~ — built. Returns
   `{op, path, old_path, id, size, is_dir}` plus the new anchor, `410 Gone` when
   `since` is unreachable. Renames are emitted server-side, as argued. Note it
   returns `id` (the content hash) rather than `content_hash`, and no `mtime` —
   the hash is what versioning needs, and mtime comes from the listing.

2. ~~`PUT entries/{path}` accepting file content~~ — built. One request, body is
   the file, and it replaces rather than renaming the collision.
3. ~~Range GETs on file download~~ — built, and without the single-use
   capability URL, so a ranged read is one authenticated round trip and repeats
   on the same URL.

Remaining, in order of value, none blocking:

4. ~~`If-Match` → 412 for optimistic concurrency~~ — built, on PUT, DELETE and
   move, which makes the 412 row in the error table above real.
5. Resumable upload. A `PUT` that dies partway starts over.

## macOS-side work

Swift, not Go. The extension must be an ObjC/Swift bundle; Apple's sample code is
Swift; the API is completion-handler-heavy and `async/await` tames it. Linking Go
via `c-archive` is possible but not worth the FFI for a REST client — `URLSession`
is less trouble.

As built, in the `Porter` repo:

- `FileProviderExtension: NSFileProviderReplicatedExtension`
- `FileProviderEnumerator: NSFileProviderEnumerator`
- `FileProviderItem: NSFileProviderItem`
- `ItemID` — the `identifier ⇄ (repo_id, path)` mapping, ahead of `IdMap`
- `SiloAPI` — thin `URLSession` client, mirrors `client/client.go`
- `Porter.app` — container: login, keychain, add/remove domains

Estimate: 2–4k lines. This is where the risk lives.

## Field notes — what the system actually does

Everything below was established by measurement against a live 0.4.2 server on
macOS 26.5, not read from documentation. Several of these contradict what the
headers imply, and two of them contradict earlier drafts of this plan.

### Capabilities decide the POSIX flags, and the immutable flag breaks Quick Look

An item whose `capabilities` omit `.allowsWriting` is marked **user-immutable**
(`uchg`) by the system. This is undocumented — the FileProvider headers never
mention immutability — and it is not something the provider opts into.

`uchg` breaks Quick Look's video player. Playback restarts every few seconds and
scrubbing shows frames from the wrong point before snapping back. Quick Look
writes to files it plays (resume position, cached metadata, extended attributes)
and every one of those writes fails against an immutable file.

This is **not** a File Provider bug. Two byte-identical copies in an ordinary
folder, same `0600` mode, differing only in `chflags uchg`, reproduce it exactly:
the locked one is broken, the plain one is perfect. During playback of a
materialised file the extension is asked for nothing at all — `fetchContents` is
never called — so nothing about the sync path is involved.

**There is no way to shed the flag while read-only.** `NSFileProviderFileSystemFlags`
has no immutability member; its whole vocabulary is `userExecutable`,
`userReadable`, `userWritable`, `hidden`, and `pathExtensionHidden`. Reporting
`.userWritable` moves the mode from `0400` to `0600` and leaves `uchg` untouched.
The flag tracks `capabilities` alone.

The Finder padlock has the same single cause. **M4 clears both for free.** Do not
add `.allowsWriting` early to dodge it: that lets an edit appear to succeed in
the Finder and then fail to upload, which is a worse failure than a stuttering
preview.

The general rule this establishes: `capabilities` and `fileSystemFlags` are
*not* independent. Capabilities are the stronger statement and win where they
overlap. `fileSystemFlags` can only decorate within what capabilities already
permit.

### `reimportItems(below:)` is not a refresh button

It looks like the light half of the escape hatch — re-read every item without
discarding the replica. It is neither.

`reimportItems` does not ask the provider to re-state its items. It scans the
**on-disk replica** and pushes what it finds back *up* to the extension as
`createItem` calls:

```
create-item(… n:"Music" dir …) why:itemChangedRemotely|diskImport
```

A read-only extension has no `createItem` to receive them, so the
`com.apple.fileproviderd.disk-import` background task can never complete. Called
once, it relaunched under one unchanging UUID every hour for four hours. While it
was parked `fileproviderd` forwarded **nothing** to the extension — no
enumeration, no `fetchContents` — so every dataless item became permanently
unavailable and Quick Look spun forever on all of them. It also discarded every
existing download on the way in.

Tearing the domain down and re-adding it is the only honest escape hatch before
M4. Revisit `reimportItems` when `createItem` exists to receive the push.

### Special containers arrive unannounced

The system enumerates `NSFileProviderTrashContainerItemIdentifier` and
`NSFileProviderWorkingSetContainerItemIdentifier` without being asked, at mount.
Both must be parsed **before** any repo-ID case, or they reach the server as
library names and earn a 403 apiece, once per mount. Returning an empty list is
correct for both today: nothing is deletable before M4, and change tracking
arrives at M3.

This is the same bug twice. The trash was found at M1 and the working set at M2 —
worth assuming there is a third.

### Item metadata was frozen at first enumeration until M3

~~The sync anchor is a constant and `enumerateChanges` reports nothing, so once
the system has cached an item's metadata it has no reason to ask again.~~ Fixed
in M3, but the debugging lesson outlives the cause and the trap returns whenever
the anchor stops advancing. **Any change to what an item reports is invisible on
everything already enumerated.**

This wastes an enormous amount of debugging time if you do not know it: a fix to
`capabilities`, `fileSystemFlags`, or dates appears to have no effect, and the
natural conclusion — that the fix was wrong — is usually false. It was right and
was never read. Verify such a change against a domain torn down and re-registered
*after* the change, or you are testing the old build's metadata.

`reimportItems` is the wrong tool for this, per above.

### A `nil` sync anchor is an answer, not a failure

`currentSyncAnchor` may hand back `nil`, and it means "this enumerator does not
track changes; re-enumerate me instead of asking what changed". That is the
correct answer for a per-directory enumerator here and the only workable one:
Porter's anchor names the state of every library at once, so a directory
enumerator that returned it would be claiming to answer `enumerateChanges` for
changes in libraries it has nothing to do with.

Only `.workingSet` returns a real anchor. Everything else returns `nil` and is
re-enumerated, which costs a listing and buys the guarantee that no enumerator
ever reports a change it cannot actually see.

### One `changes` batch can carry two operations for the same path

`/changes` is a net diff between two trees, not a replay. Deleting a directory
`/Z` and creating a file `Z` between two anchors arrives as a create and a
delete **for the same path**, separable only by `is_dir`, in no guaranteed
order. A client keyed on path alone will apply them in whichever order they
arrive and can delete the file it just created.

Porter's answer is structural rather than procedural: `is_dir` is part of the
`IdMap` location key, so the two are different rows and cannot collide no matter
what order they are seen in. Deletes are applied before creates within a batch
for the same reason. This is the same trap that
`silo/docs/bugs/fixed/move-onto-directory-destroys-it.md` found on the server,
and it is now documented in `silo/docs/protocol.md` under "Reading `changes`".

### SQLite and Swift disagree about what a character is

Renaming a directory has to rewrite every descendant path, which is a
`substr` over the old prefix's length. SQLite's `substr` counts **code points**;
Swift's `String.count` counts **grapheme clusters**. They agree on ASCII and
diverge exactly where macOS is most likely to hand you a filename that differs —
macOS returns decomposed (NFD) unicode, so `é` is one grapheme and two code
points, and an offset computed in Swift would slice one byte into the middle of
every descendant path under it.

The offset is therefore computed as `oldPrefix.unicodeScalars.count + 1`, which
is what SQLite is counting. There is no test that would have caught this from
the outside; it had to be known.

### `fetchContents` — the staged file contract, as built

- Return a **regular file on the replica's volume**, from
  `NSFileProviderManager.temporaryDirectoryURL()`. The system **clones and
  unlinks** it; after a successful fetch the staging directory is empty again,
  as documented.
- `URLSession` unlinks its own temp file when the completion handler returns, so
  the move out of it must happen **inside** that handler, not after.
- A crash strands staged files and **the extension owns deleting them**. Sweep
  once per launch, and only files older than an hour — a live download is
  indistinguishable from an orphan.
- Cancellation must report `NSUserCancelledError`. Any error the system does not
  recognise is treated as transient and retried.
- Resolve the item from the server *before* downloading. It 404s if the item is
  gone, and it is the authority on the version being reported back.
- `requestedVersion` is always nil in practice.
- Stream socket→disk rather than buffering. This is about not holding a 10 GB
  file in a sandboxed appex's memory; the file is still **fully downloaded before
  handoff**. Porter does not stream to the reader, and should not — buffered
  local files are the entire point of this class of product.

Measured at M2: 3.9 MB cold in 1.045 s including extension launch and login;
375 KB warm in 0.68 s. `dataless` clears on completion.

### Diagnostics that earned their keep

- **Log a version digest at every enumeration.** If it changes between two
  enumerations of an unchanged directory, the system is seeing phantom
  modifications. Use **FNV-1a, not `Hasher`** — `Hasher` is seeded per process,
  so it cannot answer "did this change between two runs", which is the only
  question it exists to answer.
- **`log show --predicate` against `fileproviderd`** is where the real story is.
  Our own subsystem going silent for hours was the single strongest signal all
  session: it proved the extension was not being asked, which relocated the bug
  entirely.
- **Never `kill` `qlmanage`.** SIGTERM makes it abort and raise a crash dialog;
  repeated SIGKILLs wedge Quick Look's agents system-wide, and recovering needs
  `qlmanage -r`, `qlmanage -r cache`, and restarting `quicklookd`,
  `QuickLookUIService` and `ThumbnailsAgent`. Drive Quick Look from the Finder
  and read the logs instead.

### A methodology note

Three plausible theories were pursued and all three were wrong: a `moov`-at-end
atom layout (a real correlation across four files that dissolved on the fifth),
backward-seek stalls on the provider volume (measured identical to a local file,
7 µs median), and a coordination-claim storm (real, but self-inflicted by the
`reimportItems` call, and gone once the domain was re-registered).

What worked was two byte-identical files in an ordinary directory differing in
exactly one attribute. When a File Provider bug seems to depend on the domain,
**try to reproduce it outside the domain first.** It is much cheaper to be
wrong there.

## Milestones

**M0 — signing harness.** ~~Empty appex + container app, registers a domain,
shows in the Finder sidebar.~~ **Done.** A free personal team was enough,
including App Groups and Keychain Sharing. Two things the plan did not
anticipate: the app group identifier on macOS is `TEAMID.bundle.id` with **no**
`group.` prefix and must match in both `.entitlements` files *and* the
extension's `NSExtensionFileProviderDocumentGroup`; and `com.apple.security
.application-groups` alone does not buy keychain access — sharing an item
between app and extension needs a matching `keychain-access-groups` entitlement
on both targets, or `SecItemAdd` fails with `errSecMissingEntitlement`
(-34018).

**M1 — read-only enumeration.** ~~Root lists repos, directories enumerate. No
downloads. Every file shows as dataless.~~ **Done**, against a live 0.4.1
server. Two more surprises worth recording: `fileproviderd` launches the
extension the moment the domain is registered, which races the container app
writing the account — resolve the account lazily and retry, because an
extension that caches the failure at `init` stays broken with the credentials
sitting right there. And the system enumerates
`NSFileProviderTrashContainerItemIdentifier` unprompted; parse it before the
repo-ID case or it goes to the server as a library name and earns a 403 per
attempt.

**M2 — `fetchContents`.** ~~On-demand download. This is the milestone where it
visibly becomes selective sync in Finder.~~ **Done**, against 0.4.2. It is
selective sync in the Finder: the cloud-with-arrow badge is `dataless`, a plain
cloud is materialised, and the flag clears on completion. Three things the plan
did not anticipate, all in the field notes above — the working set arrives
unannounced exactly as the trash did; item metadata is frozen at first
enumeration until M3, which makes a correct fix look like a failed one; and
read-only `capabilities` force `uchg`, which breaks Quick Look's video player.
That last one is the first place the read-only posture has cost visible
functionality rather than just convenience.

~~**M3 — sync anchor + `enumerateChanges`.** Remote changes appear without a
restart. First milestone needing `IdMap` reconciliation, and the first with any
Silo work behind it (the `/changes` endpoint). Anchor is the repo HEAD commit
ID.~~ **Done**, against 0.4.4. A commit on the server reaches the Finder in
about **one second**, and a rename keeps its identifier across the move rather
than presenting as a delete and a create. Both were the point of the milestone.

The anchor is the repo HEAD commit ID as planned, but per-repo: `SyncAnchor` is
a `[repo: head]` map, because the working set spans every library and one commit
id cannot name the state of several. Three things the plan did not anticipate
are in the field notes below — `nil` from `currentSyncAnchor` is a *supported*
answer and the right one for a per-repo enumerator, one `changes` batch can
carry two operations for the same path, and SQLite's `substr` and Swift's
`String.count` disagree about what a character is.

The socket that makes it prompt lives in the container app, not the extension —
`ChangeNotifier` documents why. Nothing breaks when the app is not running; the
system still enumerates on its own schedule, so it is a latency feature rather
than a correctness one. Making Porter a background agent so that holds without
a window open is its own change, not part of this.

**M4 — writes.** create / modify / delete / rename / move. Full error mapping.
Now carries three things beyond its own scope: the Finder padlock, Quick Look
video playback, and `reimportItems` becoming usable — all of which are blocked
on `.allowsWriting` and `createItem` existing, not on anything of their own.

**M5 — polish.** Eviction and "keep downloaded" pinning — both via
`contentPolicy` and `evictItem(identifier:)` rather than a policy table, as
above — conflict presentation, `NSFileProviderItemDecorating` badges, offline
behaviour.

~~M0–M2 is the honest go/no-go point: it is where we find out whether the File
Provider API is going to fight us, and it is reachable in weeks rather than
months.~~ **Passed.** The API does fight, but on iteration speed rather than on
capability: nothing in M0–M2 turned out to be impossible or to need a paid
developer account, and the whole read path works against a real server. The tax
is real and should be budgeted for — most of the cost is discovering undocumented
behaviour, and most of *that* cost is not knowing whether a fix failed or was
simply never read back.

## Risks

- **`NSFileProviderReplicatedExtension` is a difficult API.** Poor error
  messages, aggressive caching by `fileproviderd`, and a debug loop that often
  requires tearing down and re-registering the domain. Budget for slow iteration.
  This risk is unavoidable — it exists on every route to selective sync on macOS.
  Confirmed through M2, with the shape now clearer than "slow": the expensive
  failure is a *silent* one, where the system stops asking the extension anything
  at all and every symptom appears in the Finder instead. Check whether the
  extension is being called before debugging what it returns.
- **Read-only is not a safe subset.** It looked like a posture that could only
  cost features; it also costs correctness. Omitting `.allowsWriting` forces
  `uchg`, which breaks Quick Look video, and the absence of `createItem` is what
  makes `reimportItems` wedge the domain permanently. Weight M4 accordingly: it
  is not only the writes milestone, it is where several unrelated-looking defects
  resolve.
- ~~**Signing / team ID.** Unresolved until M0.~~ **Retired.** A free personal
  team signs the appex, the App Groups and the Keychain Sharing entitlements
  Porter needs. Paid membership is still required for distribution, but nothing
  in M1–M5 is blocked on it.
- **Rename reconciliation** for commits authored by other clients. Benign failure
  mode, but needs tests with SeaDrive writing concurrently. Mitigated if Silo
  emits `old_path` rather than leaving the client to infer renames from hashes.
- **`IdMap` is now local state we own**, which means it can drift from the
  server. Needs a rebuild path — dropping the tables and re-enumerating must be
  safe. Cheaper than it looks now that pins live in `contentPolicy` rather than
  in an `IdMap` column: the rebuild only has to preserve the user's explicit
  pins, which are few and stored apart. Design that escape hatch early; it is
  also the debug tool we will use constantly during M1–M4.
- **Large files.** Whole-file fetch in v1; no resume until range GETs land. A
  10 GB file over a flaky link will be unpleasant until then.
- **Encrypted repos.** Out of scope for v1. Client-side crypto inside a sandboxed
  appex is its own project.

## Explicitly out of scope

Block-level dedup on upload, the Seafile sync protocol, encrypted repos, Linux
and Windows clients, and any change to `seadrive-gui`.
