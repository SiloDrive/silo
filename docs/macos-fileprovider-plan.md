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
   │ SiloFileProvider     │  Swift .appex, sandboxed
   │  .appex              │  stateless-ish, HTTP client
   └──────────┬───────────┘
              │ HTTPS
              ▼
   ┌──────────────────────┐
   │ Silo                 │
   └──────────────────────┘

   ┌──────────────────────┐
   │ Silo.app             │  container app: login, add/remove domains,
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
  per-subtree rather than per-file.

**This lives entirely in the client.** Silo never sees an item identifier. That
is why unmodified Silo already works with SeaDrive today.

### Our design

Mirror it. Same two tables in the extension's container, keyed by domain. Repo
IDs are stable UUIDs so a repo's identifier can be its repo ID; root is
`NSFileProviderItemIdentifier.rootContainer`.

There is no reason to invent something different — this schema has survived
production contact, and matching it keeps the option of running both clients
against the same server without surprises.

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
| 412 / version mismatch | `.versionOutOfDate` | re-fetch item, let system re-drive — **not implemented server-side; `If-Match` is ignored and writes are last-writer-wins** |
| 413 / 507 | `.insufficientQuota` | surface to user |
| 429 | `.serverUnreachable` | honour `Retry-After`, back off |
| 5xx | `.serverUnreachable` | exponential backoff, retriable |
| offline / DNS | `.serverUnreachable` | retriable |
| anchor too old | `.syncAnchorExpired` | system falls back to full enumeration |
| page token stale | `.pageExpired` | restart enumeration |

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

4. `If-Match` → 412 for optimistic concurrency, which is what makes the
   `.versionOutOfDate` row in the error table above real rather than
   aspirational.
5. Resumable upload. A `PUT` that dies partway starts over.

## macOS-side work

Swift, not Go. The extension must be an ObjC/Swift bundle; Apple's sample code is
Swift; the API is completion-handler-heavy and `async/await` tames it. Linking Go
via `c-archive` is possible but not worth the FFI for a REST client — `URLSession`
is less trouble.

- `SiloFileProviderExtension: NSFileProviderReplicatedExtension`
- `SiloEnumerator: NSFileProviderEnumerator`
- `SiloItem: NSFileProviderItem`
- `SiloAPI` — thin `URLSession` client, mirrors `client/client.go`
- `Silo.app` — menu-bar container: login, keychain, add/remove domains

Estimate: 2–4k lines. This is where the risk lives.

## Milestones

**M0 — signing harness.** Empty appex + container app, registers a domain, shows
in the Finder sidebar. No network. Proves the signing and provisioning story
before any real code exists. Needs an Apple developer account; a free personal
team is likely enough for local development, paid for distribution — confirm
this early, not at the end.

**M1 — read-only enumeration.** Root lists repos, directories enumerate. No
downloads. Every file shows as dataless. First point at which the Sentry request
inventory can be checked against reality.

**M2 — `fetchContents`.** On-demand download. This is the milestone where it
visibly becomes selective sync in Finder.

**M3 — sync anchor + `enumerateChanges`.** Remote changes appear without a
restart. First milestone needing `IdMap` reconciliation, and the first with any
Silo work behind it (the `/changes` endpoint). Anchor is the repo HEAD commit ID.

**M4 — writes.** create / modify / delete / rename / move. Full error mapping.

**M5 — polish.** Eviction, "keep downloaded" pinning, conflict presentation,
`NSFileProviderItemDecorating` badges, offline behaviour.

M0–M2 is the honest go/no-go point: it is where we find out whether the File
Provider API is going to fight us, and it is reachable in weeks rather than
months.

## Risks

- **`NSFileProviderReplicatedExtension` is a difficult API.** Poor error
  messages, aggressive caching by `fileproviderd`, and a debug loop that often
  requires tearing down and re-registering the domain. Budget for slow iteration.
  This risk is unavoidable — it exists on every route to selective sync on macOS.
- **Signing / team ID.** Unresolved until M0. Everything else is downstream of it.
- **Rename reconciliation** for commits authored by other clients. Benign failure
  mode, but needs tests with SeaDrive writing concurrently. Mitigated if Silo
  emits `old_path` rather than leaving the client to infer renames from hashes.
- **`IdMap` is now local state we own**, which means it can drift from the
  server. Needs a rebuild path — dropping the tables and re-enumerating must be
  safe and must not lose pin (`policy`) settings. Design that escape hatch early;
  it is also the debug tool we will use constantly during M1–M4.
- **Large files.** Whole-file fetch in v1; no resume until range GETs land. A
  10 GB file over a flaky link will be unpleasant until then.
- **Encrypted repos.** Out of scope for v1. Client-side crypto inside a sandboxed
  appex is its own project.

## Explicitly out of scope

Block-level dedup on upload, the Seafile sync protocol, encrypted repos, Linux
and Windows clients, and any change to `seadrive-gui`.
