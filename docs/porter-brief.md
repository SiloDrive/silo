# Porter — the Silo server contract

Brief for the macOS File Provider client. The design lives in
[`macos-fileprovider-plan.md`](macos-fileprovider-plan.md) and the reasoning
behind the storage model in [`sync-design.md`](sync-design.md); this is the
*wire contract* — what to call, what comes back, and where the gaps are.

Every response below was captured from a running server, not written from
memory. Where something is not implemented it says so.

## What changed, and why now

`/api/silo/v1` grew endpoint by endpoint and was not internally consistent:
verbs in paths (`mkdir`, `rename`, `move`, `download`) mixed with nouns; the
same resource under two names (`file` for DELETE, `download` for GET); the path
passed as `?path=`, in a JSON body, or not at all.

It has been replaced with **one addressable noun and the HTTP methods as its
verbs**. This happened *before* Porter exists rather than after, deliberately: a
version number protects consumers you cannot upgrade atomically, and until now
v1 had exactly one consumer shipping in the same binary as the server. That
window closes the moment a separately-installed client pins the old shape.

The older endpoints still work and are still routed. Nothing outside the Silo
repository has ever used them, so they remain deletable — **do not build against
them.**

## Authentication

`POST /api/silo/v1/auth/login` with `{"email":…,"password":…}` returns
`{"token": "<JWT>"}`. Send it as `Authorization: Bearer <token>` on every
`/api/silo/v1/…` request.

This is a different credential from the `Authorization: Token …` that
`/api2/…` and `/api/v2.1/…` use (SeaDrive's lane) and from the
`Seafile-Repo-Token` on `/repo/…` (the sync lane). Three surfaces, three
credentials; do not mix them.

Missing or bad token → **401**. No permission on the library → **403**.

## The entries endpoint

```
GET    /api/silo/v1/repos/{repo}/entries/{path}   dir → listing, file → bytes
HEAD   /api/silo/v1/repos/{repo}/entries/{path}   headers only
PUT    /api/silo/v1/repos/{repo}/entries/{path}   create a directory (?type=dir)
DELETE /api/silo/v1/repos/{repo}/entries/{path}
POST   /api/silo/v1/repos/{repo}/entries/{path}   {"op":"move","to":"/x/y"}
```

Files and directories share a noun because in a content-addressed tree they are
the same kind of thing — a client shouldn't have to know which before it asks.
There is no rename: **renaming is moving**, and a client that needs to tell them
apart can compare the parent directories itself.

`entries/` with nothing after it is the library root. An unsupported method
returns **405** with an `Allow` header, not a 404 — the path was fine, the verb
wasn't.

### Escaping

The path is part of the URL, not a query parameter. Escape it **per segment**:
separators must survive as separators or the route stops matching, and
everything else must be escaped or a file named `awkward name?.txt` truncates
the request at the `?`.

In Swift, build it with `URLComponents` and set `path` (not `string`), or
percent-encode each component with a character set that excludes `/`. Do not
use a query-string escaper: it encodes a space as `+`, which is wrong in a path.

### Reading a directory

```
GET /api/silo/v1/repos/{repo}/entries/
```
```json
[
  {"name":"sub",   "type":"dir",  "id":"60db574ab801e71078e8dc55dc5764d9ab60adf6","mtime":1786965293},
  {"name":"empty", "type":"dir",  "id":"0000000000000000000000000000000000000000","mtime":1786965305},
  {"name":"a.txt", "type":"file", "id":"164d2dec219b7dad29247649cf8d20aff5d7e225",
   "size":2,"mtime":1786965292,"modifier":"d@nmilne.com"}
]
```

The all-zeros id is the empty-directory sentinel, not an error.

### Reading a file

`GET entries/{path}` on a file answers **302** to `/files/{token}/{name}`, which
needs no auth and streams the bytes. `URLSession` follows this automatically.

```
HTTP/1.1 302 Found
Etag: "v1-164d2dec219b7dad29247649cf8d20aff5d7e225"
Last-Modified: Mon, 17 Aug 2026 11:14:52 GMT
Location: /files/b2cce1e9-76a9-4b7c-a9d0-b24b326312da/a.txt
```

### ETags — the part worth building around

Every GET and HEAD sets `ETag: "v1-{id}"`, where `{id}` is the object's content
hash. Send it back as `If-None-Match` and an unchanged object answers **304**
having read no blocks at all — the check costs one dirent lookup in the parent
directory. This is the cheapest question in the API; ask it often.

```
GET entries/ + If-None-Match: "v1-fb5eb777…"  →  304
```

The `v1-` prefix versions the *representation*, not the object. Treat the whole
quoted string as opaque and never parse the id out of it: if the listing JSON
ever changes shape the prefix changes with it, which is exactly what stops a
client validating a cache entry against a body format that no longer exists.

`Last-Modified` is present when the entry has an mtime. The library root has no
dirent of its own, so it carries an ETag (the commit's root id) but no
`Last-Modified`.

Map the id straight into `NSFileProviderItemVersion.contentVersion` — as the
plan notes, content-addressing is a gift for versioning even though it is
useless for identity.

### Writing

| | |
|---|---|
| create a directory | `PUT entries/{path}?type=dir` → **201** |
| move or rename | `POST entries/{path}` with `{"op":"move","to":"/new/path"}` |
| delete | `DELETE entries/{path}` |
| **upload a file** | **not on this endpoint yet — see below** |

A trailing slash on the path also means "directory", but prefer the explicit
`?type=dir`: a bare `PUT` is a file upload, and that is the request that fails.

The library root cannot be moved (**400**) or deleted (**400**), and creating it
is a **409**.

## Gaps — read this before planning M4

**`PUT` of file content returns 501.** Uploads index blocks straight out of a
multipart part (`indexBlocks` takes a `*multipart.FileHeader`), so a raw request
body needs a reader-shaped variant of that function before the endpoint can
honestly claim to store bytes. Returning 501 with the alternative in the body
beats accepting an upload and dropping it.

Until it lands, **writes use the existing two-step flow**, which is unchanged
and fully working:

1. `POST /api/silo/v1/access-tokens` with `{"repo_id":…, "op":"upload",
   "obj_id":"{\"parent_dir\":\"/some/dir\"}"}` → `{"token":…}`
2. `POST /upload-api/{token}` as multipart form data

`op` is `update` rather than `upload` when replacing an existing file. The
access token authorizes the upload on its own, so step 2 carries no bearer
token.

**Conditional writes are not implemented.** `If-Match` is ignored — nothing
returns **412** today, and writes are last-writer-wins. The error table in the
plan lists `412 → .versionOutOfDate`; that row is aspirational. If Porter needs
optimistic concurrency, say so and it goes in server-side: the machinery is
already there, since `getEntry` resolves the same id that would be compared.

**Range requests work, but the URL they work on is single-use.** This is the
sharp edge in the whole contract, and it bites sequential-read clients hardest.

`/files/{token}/{name}` honours `Range` on unencrypted libraries — verified,
byte-for-byte against the source file:

```
Range: bytes=100-149  →  206 Partial Content
                         Accept-Ranges: bytes
                         Content-Range: bytes 100-149/240000
```

But the token in that URL is minted one-time (`api_handlers.go`, `CreateToken(…,
true)`), so the *second* request against the same URL is a **403 "Access token
not found"**. Resuming or seeking therefore costs a fresh
`GET entries/{path}` → 302 → ranged GET: **two round trips per read**.

That is deliberate, not an oversight: `/files/{token}/{name}` takes no auth
header, so the URL *is* the capability, and spending it once bounds what a
leaked one can do. The fix is not to loosen the token but to serve ranges from
the authenticated endpoint instead — see below.

For whole-file fetches that is one extra round trip and nobody notices. For a
client that reads at offsets it is the difference between a usable filesystem
and an unusable one — see "If you are building a FUSE client" below.

## The delta endpoint

```
GET /api/silo/v1/repos/{repo}/changes?since={commit}
```
```json
{
  "anchor": "9f016e4b3e5c4ffb99776ab6e155b10bce15e654",
  "changes": [
    {"op":"create","path":"/deep/nested/z.txt",
     "id":"cb01668407c71277b010774ffdceb5fe2a85a72a","size":2,"is_dir":false}
  ]
}
```

`op` is one of `create`, `delete`, `modify`, `move`. A `move` carries `old_path`
(where it was) alongside `path` (where it is now); everything else omits it.
`id` is the content hash and is absent on deletes. `changes` is always an array,
never null.

**`anchor` is the commit these changes bring you up to.** Hand it back as
`since` next time. It is returned even when nothing changed, so a polling client
advances without special-casing the empty answer.

### Getting the first anchor

`GET /api/silo/v1/repos` now returns `head_commit_id` per library:

```json
[{"id":"bf3230f1-…","name":"CLI test","update_time":1786965306,
  "encrypted":false,"head_commit_id":"9f016e4b3e5c4ffb99776ab6e155b10bce15e654"}]
```

This exists because listing libraries is the first thing a sync client does.
Without it the opening move is to enumerate a library with no way to name the
state just enumerated, so the first delta call has nothing to pass as `since`.

Use it for `currentSyncAnchor` too — it is cheaper and more direct than
`GET /repo/{id}/commit/HEAD`, which the plan's table currently names.

### Two contract details that are not visible in the response

**Apply with `mkdir -p` semantics.** A directory is reported in its own right
only when it is **empty**. One that arrives with content appears solely as the
paths inside it, because the diff walks to where the trees actually differ and a
directory present in only one tree differs at its contents. Verified:

```
mkdir /empty                        → create d /empty
mkdir /deep, /deep/nested, put z.txt → create f /deep/nested/z.txt      (only)
```

Create the parents of every path you are told about. The same holds in reverse
for deleting a non-empty directory.

**Renames are emitted server-side**, not inferred client-side, because the
server holds both trees. The inference is imperfect either way — deleting one
file and creating another with identical bytes looks like a rename — but the
failure mode is benign: an identifier follows the "wrong" copy of byte-identical
content.

### Status codes

| Status | Meaning | What to do |
|---|---|---|
| **400** | `since` missing | pass an anchor, or enumerate |
| **410** | `since` unreachable — GC'd, or the library was reset | `.syncAnchorExpired`; full enumeration |
| **403** | no permission on the library | surface; do not retry |

**410 is not an error.** The commit isn't wrong, it is merely no longer
reachable, and the recovery is to enumerate from scratch — a different
instruction from "retry", so it gets a different status. It is the natural
source for `NSFileProviderError.syncAnchorExpired`.

## Updated request inventory

Supersedes the table in `macos-fileprovider-plan.md`. Every row here has been
exercised against a running server.

| Callback | Method + path |
|---|---|
| domain setup | `POST /api/silo/v1/auth/login` |
| — | `GET /api/silo/v1/server-info` |
| `enumerateItems` (root) | `GET /api/silo/v1/repos` |
| `enumerateItems` (dir) | `GET /api/silo/v1/repos/{id}/entries/{path}` |
| `currentSyncAnchor` | `head_commit_id` from `GET /api/silo/v1/repos` |
| `enumerateChanges` | `GET /api/silo/v1/repos/{id}/changes?since={commit}` |
| `item(for:)` | *none* — local `IdMap` ⋈ `WorkingSet` |
| `fetchContents` | `GET /api/silo/v1/repos/{id}/entries/{path}` → 302 → `/files/{token}/{name}` |
| revalidate a cached item | the same GET with `If-None-Match` → 304 |
| `createItem` (dir) | `PUT /api/silo/v1/repos/{id}/entries/{path}?type=dir` |
| `createItem` (file) | `POST /api/silo/v1/access-tokens` then `POST /upload-api/{token}` |
| `modifyItem` (contents) | access token with `op=update`, then `POST /update-api/{token}` |
| `modifyItem` (rename) | `POST /api/silo/v1/repos/{id}/entries/{path}` `{"op":"move",…}` |
| `modifyItem` (reparent) | the same call — a move is a move |
| `deleteItem` | `DELETE /api/silo/v1/repos/{id}/entries/{path}` |
| push invalidation | `WS /notification` |

Identifiers never cross the wire: every request is `(repo_id, path)`, resolved
client-side from `IdMap`. Silo's logs and Sentry traces therefore look like
SeaDrive's, which makes SeaDrive a working reference for what correct traffic
looks like.

## Checking your work against the server

The Go CLI speaks exactly this surface, so it is a reference implementation you
can diff against — `client/client.go` is ~100 lines of it.

```bash
export SILO_URL=http://server:8082 SILO_EMAIL=… SILO_PASSWORD=…
silo repos --json                      # libraries, with head_commit_id
silo ls   <repo> /some/dir             # GET entries/
silo mkdir <repo> /new                 # PUT entries/…?type=dir
silo mv   <repo> /a.txt /sub/a.txt     # POST entries/… {"op":"move"}
silo rm   <repo> /sub/a.txt            # DELETE entries/…
silo changes <repo> <since> --json     # the delta endpoint
```

Server-side Sentry will show anything Porter sends that Silo does not expect —
malformed paths, missing auth headers, requests against tombstoned items — with
the route template as the transaction name, so `entries/{path}` groups rather
than fragmenting per file. Client-side Sentry in an appex is unreliable for
crashes; lean on breadcrumbs and explicit `captureMessage` on the error
branches.

## If you are building a FUSE client

The File Provider model and the FUSE model want different things from this
server, and the difference is not cosmetic.

File Provider fetches **whole files** — `fetchContents` hands back a complete
file and macOS owns the local replica. One request per file, redirect included,
and the single-use token costs nothing.

FUSE serves **`read(fd, buf, len, offset)`**. If each of those becomes
`GET entries/` → 302 → ranged GET, every read is two round trips and a
sequential scan of a large file is thousands of them. Do not build that.

Three options, in the order I would try them:

1. **Use the block lane for content.** This is what `seadrive-fuse` does, and
   it is the reason the block API exists. Blocks are content-addressed, so they
   are immutable and cacheable forever, fetched by id:

   ```
   GET /repo/{repoid}/block/{blockid}      Seafile-Repo-Token
   GET /repo/{repoid}/block-map/{fileid}   block boundaries for offset → block
   ```

   The repo token is long-lived, so there is no per-read token dance. You need
   the file's block list, which means reading its `Seafile` fs object — that is
   the cost of this route, and it is a real one: you are partway to replicating
   the object store. Blocks also default to 8MB, so a 4KB read pulls a whole
   block unless block fetches themselves accept `Range` (I have not verified
   that they do — check before relying on it).

2. **Cache whole files locally** and serve reads from the cache, fetching once
   via `entries/`. Simplest correct thing; fine if libraries are documents and
   not 10GB videos. `entries/` + ETags gives you cheap revalidation, which is
   most of what a cache needs.

3. **Ask for the server change.** Honouring `Range` directly on
   `GET entries/{path}` — streaming rather than redirecting when a `Range`
   header is present — collapses this to one round trip per read with no token
   involved. The machinery already exists (`doFileRange` in `fileop.go`); it is
   not wired to this endpoint. This is a small, additive change and it is the
   right one if a FUSE client is going to exist. **Ask for it rather than
   working around it.**

Everything else in this document applies unchanged: `entries/` for metadata and
enumeration, `/changes` for invalidation, ETags for revalidating cached attrs,
and the two-step flow for writes.

Note also that a FUSE client on Linux has the `Seafile-Repo-Token` lane fully
available to it — that is the lane unmodified Seafile clients use, and it is
frozen, which means it is stable. `../seafile/seadrive-fuse` is a working
reference for it.

## Asking for server changes

The server is ours and additive endpoints are cheap. Three are already
identified and none are blocking M0–M3:

1. `Range` honoured directly on `GET entries/{path}`, so a ranged read is one
   round trip and needs no single-use token (see the FUSE section)
2. `PUT entries/{path}` accepting file content (removes the two-step upload)
3. `If-Match` → 412 for optimistic concurrency

If Porter wants something else, ask rather than working around it in Swift.
Reimplementing server logic client-side is what makes sync clients enormous —
SeaDrive carries tens of thousands of lines to answer questions we can answer
with a GET.
