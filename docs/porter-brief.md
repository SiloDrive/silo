# Porter — the Silo server contract

Brief for the native clients — the macOS File Provider extension and the FUSE
client. Both talk to the same surface; where they need different things, it is
called out. The macOS design lives in
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

**The session token lasts 24 hours.** There is no refresh endpoint: on a 401,
log in again with the stored credentials and retry the request once. Keep the
password in the Keychain, not the token — the token is the short-lived thing.

If several requests are in flight when it expires they will all 401 at once.
Collapse that into one re-login rather than a stampede; `client/client.go` does
it by recording which token a caller observed and only re-logging in if it has
not already been replaced.

## Server info and version

```
GET /api/silo/v1/server-info        (no auth)
```
```json
{"version":"0.4.1"}
```

Check it at domain setup. **This surface changed materially in 0.4.0** — reads
stopped redirecting, `PUT` started accepting file content — and **0.4.1** added
conditional writes. A client built against this document talking to an older
server will fail in confusing ways: against 0.3.x the reads and writes break
outright, and against 0.4.0 the `If-Match` headers are silently ignored, which
is worse, because losing an edit looks like success. Require **0.4.1** and say
why, rather than discovering it one broken callback at a time.

There is no capability list yet, only a version. If you would rather
feature-detect than compare version strings, ask — it is a small addition and
the argument for it is exactly the paragraph above.

## Libraries

```
GET    /api/silo/v1/repos              list (see the delta endpoint for head_commit_id)
POST   /api/silo/v1/repos              {"name":"..."} → the new library
DELETE /api/silo/v1/repos/{repoid}     delete a library
```

The container app needs create and delete; the extension itself only enumerates.
Deleting a library is not a `DELETE entries/` on its root — the root is not
deletable (**400**), because removing a library is a different operation from
emptying one.

## The entries endpoint

```
GET    /api/silo/v1/repos/{repo}/entries/{path}   dir → listing, file → bytes
HEAD   /api/silo/v1/repos/{repo}/entries/{path}   headers only
PUT    /api/silo/v1/repos/{repo}/entries/{path}   body → file, or ?type=dir
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

`GET entries/{path}` on a file streams the bytes back on that response. One
request, authenticated by the bearer header on it. No redirect.

```
HTTP/1.1 200 OK
Accept-Ranges: bytes
Content-Length: 14
Content-Type: text/plain
Etag: "v1-b8d0fa06b4d2412e9695f1f3561ecee173a5ae4b"
Last-Modified: Mon, 17 Aug 2026 11:38:12 GMT
```

**`Range` is supported, on this URL, as many times as you like.** Verified
byte-for-byte against the source across five sequential ranged reads at
different offsets on one URL:

```
Range: bytes=200000-200049  →  206 Partial Content
                               Content-Range: bytes 200000-200049/240000
```

This used to be a **302** to `/files/{token}/{name}`, whose token was spent on
first use — so a second ranged read against the same URL was a 403 and seeking
cost two round trips. That is gone from this lane. See
[`capability-urls.md`](capability-urls.md) for why the redirect existed and why
it does not belong here.

No charset is declared on text files. The server does not know how a file it is
handing back is encoded, and the Seafile lane's inherited `charset=gbk` is a
guess that mangles anything else. Do not trust a charset you did not put there.

### HEAD, and how to get attributes cheaply

`HEAD entries/{path}` on a **file** gives you everything `getattr` needs:

```
Content-Length: 240000                          size
Last-Modified:  Mon, 17 Aug 2026 11:38:51 GMT   mtime
Etag:           "v1-bfa1b1a4…"                  content hash
Accept-Ranges:  bytes
```

On a **directory** it gives a correct `ETag` and `Last-Modified` — but
**`Content-Length` is the size of the listing JSON, not of the directory**:

```
HEAD entries/adir  →  Content-Length: 3     ← that is "[]\n"
HEAD entries/      →  Content-Length: 653   ← the root's listing
```

Never take a size from a directory HEAD. The library root has no dirent of its
own, so it carries an ETag but no `Last-Modified`.

Two things that matter more than HEAD itself:

- **HEAD on a directory is not cheaper than GET.** The listing is still built;
  the body is just discarded. What makes it cheap is `If-None-Match`, which
  answers 304 before any of that work happens.
- **Do not HEAD each child to populate attributes.** The parent's listing
  already carries `name`, `type`, `id`, `size` and `mtime` for every entry, so
  one readdir fills the whole attribute cache. Revalidate the parent with
  `If-None-Match` and N HEADs collapse into one conditional GET that usually
  304s.

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
| upload a file | `PUT entries/{path}` — the body **is** the file → **201** |
| create a directory | `PUT entries/{path}?type=dir` → **201** |
| move or rename | `POST entries/{path}` with `{"op":"move","to":"/new/path"}` |
| delete | `DELETE entries/{path}` |

**`PUT` replaces.** That is what PUT means, and it is deliberately unlike the
Seafile lane's upload, which is modelled on a person dragging files into a
folder and so renames a collision to `notes (1).txt`. PUT the same path three
times here and you get one file with the last body:

```
put #1 → 201    put #2 → 201    put #3 → 201
GET → "version 3";  the listing has one notes.txt
```

Set `Content-Length` if you can. It is checked against the server's upload limit
before the body is read, so an oversize upload is refused up front rather than
after you have pushed it. A chunked body works too — the limit is then enforced
on the way through.

The response carries the new content's `ETag`, so a client can record the
version without a follow-up GET:

```json
{"id":"b8d0fa06…","name":"greeting.txt","size":14,"type":"file"}
```

**The parent directory must exist** — a PUT into a missing directory is a
**404**, not an implicit `mkdir -p`. A typo in a path should not silently build
a directory tree.

A trailing slash also means "directory", but prefer the explicit `?type=dir`.
The distinction is load-bearing now: a bare `PUT` stores the body, so dropping
the parameter creates an empty file where a directory was meant.

The library root cannot be moved (**400**) or deleted (**400**), and creating it
is a **409**.

## Conditional writes

Every mutating request honours `If-Match` and `If-None-Match`, so a client can
write without losing someone else's edit. This is the same pair S3 added in
2024, and it maps onto `NSFileProviderError.versionOutOfDate`.

| Header | Means | On failure |
|---|---|---|
| `If-Match: "v1-<id>"` | replace only if this is still the content I read | **412** |
| `If-None-Match: *` | create only if nothing is there | **412** |

Applies to `PUT` (file and `?type=dir`), `DELETE`, and `POST` move — where the
precondition is about the **source**, the thing you looked at before deciding to
move it.

The lost-update race, run against a real server:

```
A and B both read race.txt   ETag "v1-02294300…"
A: PUT If-Match "v1-02294300…"  → 201
B: PUT If-Match "v1-02294300…"  → 412     ← B's stale write is refused
content is "A edit"                        ← A's edit survived
```

On a 412: re-read the entry, reapply your change, and write again. That is the
`.versionOutOfDate` recovery — hand it back to the system and let it re-drive.

**Both are opt-in.** A request with neither header behaves exactly as before, so
last-writer-wins is still available — it just has to be chosen now rather than
arrived at by accident. For a File Provider extension, send `If-Match` on every
`modifyItem`: the version you were handed is precisely the tag to send.

Note the header changes meaning with the method. On `GET`, `If-None-Match` asks
"skip the body if unchanged" and yields **304**. On a write it asks "fail if it
exists" and yields **412**. Same header, different question, as RFC 9110
specifies.

## Gaps — read this before planning M4

Short list, and shorter than it was.

**No resumable upload.** A PUT that dies partway has to start over. Whole-file
uploads only; there is no chunk/offset protocol on this endpoint.

**Encrypted libraries** serve whole files but not ranges — the stored blocks are
ciphertext, so a byte range of the plaintext is not a byte range of what is
stored. Out of scope for a v1 client either way.

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
| `fetchContents` | `GET /api/silo/v1/repos/{id}/entries/{path}` — bytes on the response |
| revalidate a cached item | the same GET with `If-None-Match` → 304 |
| read at an offset | the same GET with `Range` → 206, repeatable |
| `createItem` (dir) | `PUT /api/silo/v1/repos/{id}/entries/{path}?type=dir` |
| `createItem` (file) | `PUT /api/silo/v1/repos/{id}/entries/{path}` — body is the file |
| `modifyItem` (contents) | the same `PUT` — it replaces |
| `modifyItem` (rename) | `POST /api/silo/v1/repos/{id}/entries/{path}` `{"op":"move",…}` |
| `modifyItem` (reparent) | the same call — a move is a move |
| `deleteItem` | `DELETE /api/silo/v1/repos/{id}/entries/{path}` |
| push invalidation | `WS /notification` |

Identifiers never cross the wire: every request is `(repo_id, path)`, resolved
client-side from `IdMap`. Silo's logs and Sentry traces therefore look like
SeaDrive's, which makes SeaDrive a working reference for what correct traffic
looks like.

## Push invalidation

Polling `/changes` is correct but latent. `WS /notification` tells you when a
library moves, so you can call `/changes` immediately instead of on a timer.

Getting subscribed takes two credentials you do not already have, because this
endpoint predates the Silo lane and speaks the Seafile lane's auth:

1. `POST /api/silo/v1/repos/{repoid}/sync-token` — **Bearer** → `{"token":…}`
   A long-lived repo token. Mint one per library and keep it.
2. `GET /repo/{repoid}/jwt-token` — header `Seafile-Repo-Token: <that token>`
   → `{"jwt_token":…}`. Valid **72 hours**, and per library.
3. Connect to `WS /notification` and send one frame:

```json
{"type":"subscribe","content":{"repos":[{"id":"<repo-id>","jwt_token":"<jwt>"}]}}
```

`"unsubscribe"` takes the same shape. Events come back in the same envelope:

```json
{"type":"repo-update","content":{"repo_id":"…","commit_id":"…"}}
{"type":"jwt-expired","content":{…}}
```

`commit_id` is exactly the anchor `/changes` wants, so a `repo-update` translates
directly into `GET /changes?since=<your last anchor>`. Do not treat the pushed
`commit_id` as your new anchor without fetching — you may have missed events.

On `jwt-expired`, re-mint at step 2 and re-subscribe. The 72-hour lifetime means
this will happen to any long-running mount, so implement it before you ship
rather than after the first mysterious silence.

The server pings every **30 seconds** and hangs up if it has not seen a pong in
**90**. Most WebSocket libraries answer pings automatically — confirm yours
does, because the failure mode is a connection that looks alive and delivers
nothing.

If notifications are disabled server-side, step 2 returns **404**. Treat that as
"fall back to polling", not as an error.

## Checking your work against the server

The Go CLI speaks exactly this surface, so it is a reference implementation you
can diff against — `client/client.go` is ~100 lines of it.

```bash
export SILO_URL=http://server:8082 SILO_EMAIL=… SILO_PASSWORD=…
silo repos --json                      # libraries, with head_commit_id
silo ls   <repo> /some/dir             # GET entries/
silo put  <repo> ./local.txt /dir      # PUT entries/… (body is the file)
silo get  <repo> /dir/local.txt out    # GET entries/…
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

FUSE serves **`read(fd, buf, len, offset)`**, which is the access pattern this
lane was just changed to support. **`GET entries/{path}` with a `Range` header
is one authenticated round trip, repeatable on the same URL.** Build the read
path directly on it.

That is worth stating plainly because it was not true a day ago, and the
workarounds it used to require are no longer worth their cost:

- **You do not need the block lane** (`/repo/{id}/block/{id}` with a
  `Seafile-Repo-Token`). That is what `seadrive-fuse` does, and the reason is
  that it had no better option: reaching bytes at an offset meant fetching the
  file's `Seafile` fs object for its block list and reassembling. It works, but
  it puts you partway to replicating the object store, and blocks default to
  8MB, so a 4KB read pulls a whole block. Use it only if you find a reason
  `entries/` cannot serve.
- **You do not need a whole-file local cache to be correct** — though you may
  still want one for latency. `entries/` plus ETags gives cheap revalidation,
  which is most of what a cache needs anyway.

What is worth doing:

1. **Cache attributes**, keyed by path, revalidated with `If-None-Match`. A 304
   reads no blocks, so `getattr` storms are nearly free.
2. **Invalidate from `/changes`** rather than by polling paths. One request tells
   you everything that moved since your last anchor.
3. **Read through to `entries/` with `Range`**, and cache block-aligned chunks
   locally if the workload rereads. The server does not care how you align; it
   will serve any range.

Encrypted libraries are the exception — they serve whole files but not ranges,
because the stored blocks are ciphertext. Out of scope for a v1 client.

`../seafile/seadrive-fuse` remains a useful reference for FUSE mechanics —
inode allocation, handle lifetime, writeback — even though its network layer is
not the one to copy.

## Asking for server changes

The server is ours and additive endpoints are cheap. Every ask from the first draft of this
document — ranged reads without a capability URL, `PUT` accepting file content,
and conditional writes — has been built. What is left:

1. Resumable upload, if whole-file PUT turns out to be painful over flaky links
2. A batch block fetch (`pack-blocks`), if per-object round trips dominate

If Porter wants something else, ask rather than working around it in Swift.
Reimplementing server logic client-side is what makes sync clients enormous —
SeaDrive carries tens of thousands of lines to answer questions we can answer
with a GET.
