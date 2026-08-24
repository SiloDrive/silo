# Protocol Compatibility

This document is the server's **contract with the clients**. It lists the
HTTP endpoints the Go fileserver implements.

**The Seafile/SeaDrive sync lane — `/repo/*`, `/api2/*`, `/api/v2.1/*`,
`/files/{token}/*`, `/seafhttp/*` — was deleted in `5d4baa0` (0.5.0).** Every
one of those paths answers 404 now; nothing here still stubs or shims them.
See [`docs/target.md`](target.md) for why, and
[`porter-brief.md`](porter-brief.md#the-seafile-lanes-are-gone) for the
one-paragraph version. This document now covers the one lane that remains.

It says which endpoints exist. For what the *status codes* mean — and which are
already spoken for — see [`responses.md`](responses.md), which is the file to
check before a new handler picks one. For what a client would want that is *not*
here, and why, see [`protocol-gaps.md`](protocol-gaps.md).

Treat this as the source of truth when a new client release starts hitting
an endpoint we don't support — find it in the "not implemented" list and
decide whether to shim it.

## Who reads what

| if you are | read |
|---|---|
| writing a new client | the Silo lane below, then [`porter-brief.md`](porter-brief.md) for the wire contract with captured responses |
| choosing a status code for a new handler | [`responses.md`](responses.md). Always, and before you write the handler |
| wondering why something is missing | [`protocol-gaps.md`](protocol-gaps.md) |

Audience-shaped documents are the *briefs* — `porter-brief.md` is one, written
for someone building a File Provider extension. A brief names a subset and the
traps in it, and links here rather than restating.

## Tested clients

- **silo** — our own Go TUI (`cmd/silo`)
- **Porter** — our macOS File Provider client. Speaks `/api/silo/v1` only:
  `entries`, `changes`, `notify-token` and the notification socket.
- **porter-fuse** — our FUSE client, same lane.

## Authentication

One scheme: JWT Bearer.

| Scheme | Header | Used by | Validated against |
|---|---|---|---|
| JWT Bearer | `Authorization: Bearer <jwt>` | silo (TUI), Porter, porter-fuse, `/api/silo/v1/*` | `authmgr.ValidateSessionToken` (24h expiry) |

The middleware is `RequireAuth`, in `fileserver/middleware/`.

Two more credential types are minted and stored but currently validate
nothing live: `RequireAPIToken` (`Authorization: Token`) is orphaned — no
route mounts it — since the routes that used it were deleted with the sync
lanes; `libmgr.GetAccountByToken`/`GetAccountForToken` (the
`LibraryUserToken`/"sync token" table) have no caller outside `libmgr` itself.
Both credential types are still created and revoked through `silo token`;
neither currently gates access to anything. See
`fileserver/middleware/apitoken.go` and [`docs/auth.md`](auth.md).

## Endpoints

### Native management API — `/api/silo/v1/*`

JSON request/response bodies. Used by the silo TUI, the CLI in `client/`, and
the sync clients. Protected by `RequireAuth` (JWT Bearer), except the two marked
**No auth** below — they are registered above the authenticated subrouter
(`server.go:701`) because they are what a client needs *before* it has a
credential: one to learn what it is talking to, one to get a token.

| Method | Path | Purpose |
|---|---|---|
| GET | `/api/silo/v1/server-info` | **No auth.** `{"version":"0.5.0","features":[…]}` — semver with no leading `v`, and the capability list a client should branch on instead of the version. No chunker parameters: they belong to the library, and the libraries listing carries them. The `libraries` name says this server serves `/libraries/…`; a client that does not find it is talking to a build that predates the word and should say so rather than read the 404 that follows as an empty account |
| POST | `/api/silo/v1/auth/login` | **No auth.** Email + password → JWT |
| POST | `/api/silo/v1/access-tokens` | Create a time-limited access token for a specific object |
| GET | `/api/silo/v1/libraries` | List the caller's libraries — owned, plus any shared directly to them through `SharedLibrary` — each with `head_commit_id`, the anchor `changes` starts from. `[]`, never `null`, for an empty account. Group shares are honoured by `CheckPerm` but do not appear in this list |
| POST | `/api/silo/v1/libraries` | Create a new library |
| DELETE | `/api/silo/v1/libraries/{libraryid}` | Delete a library |
| PATCH | `/api/silo/v1/libraries/{libraryid}` | `{"name":"New name"}` — rename a library. `PATCH` because the body names only what changes |
| POST | `/api/silo/v1/libraries/{libraryid}/batch` | `{"ops":[…]}` — many operations, one commit. See the batch surface below |
| POST | `/api/silo/v1/libraries/{libraryid}/notify-token` | Mint a notification JWT for `WS /notification` (72h; `404` if notifications are disabled) |

#### The entries surface

One addressable noun with the HTTP methods as its verbs. This is what new
clients speak, and what `client/` speaks; the wire contract with captured
responses is in [`porter-brief.md`](porter-brief.md), and the status codes are
in [`responses.md`](responses.md).

| Method | Path | Purpose |
|---|---|---|
| GET | `/api/silo/v1/libraries/{libraryid}/entries/{path}` | Read a file's bytes, or list a directory. Ranged; `ETag`/`304` |
| HEAD | `/api/silo/v1/libraries/{libraryid}/entries/{path}` | The same headers as `GET`, no body. On a directory `Content-Length` is the size of the listing, not of its contents |
| PUT | `/api/silo/v1/libraries/{libraryid}/entries/{path}` | Store a file — body is the content |
| PUT | `/api/silo/v1/libraries/{libraryid}/entries/{path}?type=dir` | Create a directory (a trailing slash also works; prefer the parameter) |
| PUT | `/api/silo/v1/libraries/{libraryid}/entries/{path}?type=blocks` | Store a file from blocks already uploaded — body is `{"blocks":[sha1,…]}`, no content. See the block surface below |
| POST | `/api/silo/v1/libraries/{libraryid}/entries/{path}` | `{"op":"move","to":"/dst"}` — moving covers renaming |
| POST | `/api/silo/v1/libraries/{libraryid}/entries/{path}` | `{"op":"copy","to":"/dst"}` — server-side copy; `201` and the source's `ETag`, no content transferred |
| DELETE | `/api/silo/v1/libraries/{libraryid}/entries/{path}` | Delete a file or directory |
| GET | `/api/silo/v1/libraries/{libraryid}/changes?since=` | Changes since an anchor (`410` when the anchor is too old) |
| GET | *any of the above* `?limit=N` | Page the answer. `Link: …; rel="next"` until the last page. See pagination below |

`{path}` is relative to the library root and never repeats the library name;
`entries/` with nothing after it is the root. Every mutating method honours
`If-Match` and `If-None-Match`; on a `move` or a `copy` the precondition is
about the *source*, which is the thing the caller looked at before deciding.

Copy is cheap in a way worth stating plainly: the destination dirent points at
the object the source already names, so a copy costs one dirent and one commit
whether it is an empty file or a hundred-gigabyte subtree, and reads no content
at all. A client emulating it with `GET` then `PUT` pays for the content twice.

Directory creation is `PUT …?type=dir` rather than a `MKCOL`-style verb or an
RPC: `PUT` already means "make the resource at this URI have this state", and
the marker resolves the one ambiguity — a directory has no body, so without it a
bodiless `PUT` is indistinguishable from writing an empty file. Parents are not
created implicitly; a `PUT` into a missing directory is a `404`, for the same
reason WebDAV's `MKCOL` answers `409` rather than building the tree.

#### The batch surface

Feature name `batch`.

```
POST /api/silo/v1/libraries/{library}/batch
{"ops":[
  {"op":"mkdir",  "path":"/reports"},
  {"op":"create", "path":"/reports/q3.txt", "blocks":["<sha1>", …]},
  {"op":"move",   "path":"/old.txt", "to":"/reports/old.txt"},
  {"op":"copy",   "path":"/tpl.txt", "to":"/reports/tpl.txt"},
  {"op":"delete", "path":"/stale.txt"}
]}
→ 200 {"commit_id":"…","ops":5,"changed":true}   + ETag of the new root
```

**All or nothing.** The operations apply to a working tree that exists only for
the duration of the request. If any fails, nothing is written and the library is
untouched — the reply names the index, the op and the path that stopped it, with
the status code that operation would have answered on its own. Half-applied is
the one outcome a client cannot recover from, because it has no way to find out
which half.

**Ordered.** Each operation sees the ones before it, which is what makes a
`mkdir` followed by writes into it a single request rather than two.

**`create` takes blocks, not bytes.** Upload them to the block surface first;
this is the call that makes them a file. That pairing is the point: five hundred
files become five hundred block uploads — only for content the server does not
already hold — and one commit, instead of five hundred commits and five hundred
rounds of branch-head contention.

**`mkdir` of a directory that already exists is not a failure.** A batch
describes where the library should end up, and "make sure this folder exists"
has to be expressible in one request or a client is back to asking first and
racing the answer. A *file* at that path is still a `409`.

**`If-Match` applies to the library root**, whose id is the ETag `GET entries/`
already returns — so "apply only if the library is still what I read" is spelled
here the way it is everywhere else. On contention the answer is `503` with
`Retry-After` rather than a merge: merging is right for uploads, which only add,
but a batch can delete and move and a three-way merge of those against an unseen
commit is a guess.

`changed: false` means the operations left the tree exactly as it was, so no
commit was minted. Limits: 1000 operations, 4 MiB of body.

#### Pagination

Feature name `pagination`. Two things page: `GET changes` and a directory
listing from `GET entries/{path}`.

**It is opt-in and it is a header.** No `limit`, no paging — the response is
whole, exactly as before. With `limit`, the body keeps the shape it always had
and the next page arrives as an RFC 8288 header:

```
Link: </api/silo/v1/libraries/{library}/changes?cursor=eyJ2Ijox…&limit=1000>; rel="next"
```

Follow it verbatim. The cursor is opaque; it carries an offset today and may
carry a key tomorrow, and no client should need changing for that. The last
page carries no `Link`.

There is no default page size, deliberately. A truncated answer that looks
complete is the worst failure available here — a sync client would apply half a
diff and record the anchor for all of it — so a client that never asks to page
is never paged.

**A cursor pins the version it started on.** Objects are immutable, so the
cursor names the exact commit or directory object the first page was computed
from, and every later page is served from that one. Items do not skip or repeat
across a page boundary while the library is written to; the writes show up on
the client's next pass, which is the bargain the delta feed already makes.

**On `changes`, `anchor` is absent until the last page.** That is the contract
rather than an omission: a client that records the anchor whenever it is present
is correct by construction, where one told "record it only at the end" has to
remember to. Recording it early marks the client up to date for changes it has
not seen.

**A paged listing carries no `ETag` or `Last-Modified`.** A window is not the
representation the id names, and a validator there would answer `304` to a
request for a different page.

`limit` is capped at 10,000, and a bad `limit` is a `400` rather than a clamp.

What paging bounds is the response and the client's apply loop, not the
server's work: a diff is recomputed per page, because a Merkle diff is
proportional to what changed and cannot be resumed part-way. A client that wants
the server to do less should ask more often, not for smaller pages.

`GET /libraries` does not page. A library count is bounded by how many an account
has, which is tens, not by anything a client can grow without noticing.

#### The block surface

Feature name `blocks`. Three calls, and the shape of every resumable upload:

| Method | Path | Purpose |
|---|---|---|
| POST | `/api/silo/v1/libraries/{libraryid}/blocks/missing` | `{"blocks":[id,…]}` → `{"missing":[id,…]}` — which of these do you not already have? |
| PUT | `/api/silo/v1/libraries/{libraryid}/blocks/{id}` | Upload one chunk. `201` when stored, `204` when it was already there |
| PUT | `/api/silo/v1/libraries/{libraryid}/entries/{path}?type=blocks` | `{"blocks":[id,…]}` — create the file from them. `201` and an `ETag`, as any other write |

An id is the SHA-256 of the chunk's bytes, so a client computes the names the
server would without asking. Where the cuts fall is the other half, and that
one is per library: the chunker is content-defined, and its parameters come
from the `chunker` object on the libraries listing. Chunking under anything else
still uploads correctly and still reads back — the server verifies bytes
against the id it was given — but the ids match nothing already in the store,
so nothing dedups. **A client that cannot read a library's parameters must
upload whole files rather than guess them**: every id would be well-formed,
every request would succeed, and the failure would be invisible.

Content-defined boundaries are what make the question worth asking at all.
Under the fixed offsets this surface started with, inserting one byte near the
front of a file shifted every boundary after it, so a re-upload matched nothing
and sent the whole file again.

The id is not taken on trust in either direction. The server hashes what
arrives and refuses a block that does not match the id it was offered under
(`400`), which makes a successful `PUT` an end-to-end integrity check of the
transfer as well as a store.

**Nothing exists until the last call.** Blocks are immutable and addressed by
content, so uploading them commits to nothing: no path changes, no commit is
minted, and the destination is untouched. Upload in any order, in parallel,
across restarts, over days. An interrupted upload leaves the library exactly as
it was, and the retry is the same three calls — `blocks/missing` simply returns
a shorter list the second time. That is what makes it resumable; there is no
session, no offset and no upload id to keep.

A commit naming a block the server does not hold is `424 Failed Dependency`,
with the missing ids in the body. It is not `400`: the request is not wrong and
the identical one succeeds once the blocks are up.

Encrypted libraries are excluded (`400`). Their blocks are ciphertext, so a
client cannot name one without performing the encryption itself; `PUT` of the
file content still works there, and the server encrypts from the cached key.

The limit worth knowing: fixed-offset chunking means inserting a byte near the
front of a file shifts every boundary after it and nothing dedups. Appends and
unchanged regions are free; edits in the middle are not.
[`protocol-gaps.md`](protocol-gaps.md) has what that would cost to fix.

#### Reading `changes`

`{"anchor": "<commit>", "changes": [...]}`, where each change is
`{op, path, old_path?, id?, size, is_dir}` and `op` is `create`, `delete`,
`modify` or `move`. The anchor comes back even when nothing changed, so a
polling caller can always advance — with the one exception that if you asked to
[page](#pagination), it is absent on every page but the last. `old_path` is set
for moves, and a rename is a move — compare the parent directories if you need
to tell them apart.

It is a **net diff between two trees**, not a replay of what happened. A file
created and then renamed twice arrives as one create at its final path; a file
created and deleted between the two anchors does not arrive at all. That is the
right shape for reconciliation and the wrong shape for an audit log.

The consequence worth designing around: **one batch can carry two operations for
the same path**, distinguishable only by `is_dir`. Deleting a directory `/Z` and
creating a file called `Z` between two anchors produces

```json
{"op": "create", "path": "/Z", "is_dir": false}
{"op": "delete", "path": "/Z", "is_dir": true}
```

A client keyed on path alone will apply those in some order and can delete the
file it just created. Key on `(path, is_dir)`, or carry stable identifiers of
your own.

#### Removed in 0.4.4

`dir/`, `mkdir`, `file`, `download`, `rename` and `move` were the pre-entries
shape: verbs in paths, one resource under two names, the path passed as a query
parameter. Every one of them had an equivalent on the entries surface, so they
were duplicates rather than capabilities, and they are gone.

| removed | call instead |
|---|---|
| `GET /libraries/{libraryid}/dir/?path=` | `GET libraries/{libraryid}/entries/{path}` on a directory |
| `POST /libraries/{libraryid}/mkdir` | `PUT libraries/{libraryid}/entries/{path}?type=dir` |
| `DELETE /libraries/{libraryid}/file?path=` | `DELETE libraries/{libraryid}/entries/{path}` |
| `GET /libraries/{libraryid}/download?path=` | `GET libraries/{libraryid}/entries/{path}` — streams on the same response |
| `POST /libraries/{libraryid}/rename` | `POST libraries/{libraryid}/entries/{path}` with `{"op":"move"}` |
| `POST /libraries/{libraryid}/move` | `POST libraries/{libraryid}/entries/{path}` with `{"op":"move"}` |

`download`'s `302` to `/files/{token}/{name}` is the one behaviour that does not
survive verbatim, and its removal was already the plan: this lane has no
browser-shaped consumer, and for a client that sets a bearer header anyway a
capability URL is overhead — for a FUSE client, two round trips per read. See
[`capability-urls.md`](capability-urls.md), which is the decision; this removes
the last route that still contradicted it.

`POST /api/silo/v1/access-tokens` with `{"library_id":…, "obj_id":<file
id>, "op":"download"}` still mints a token of the same shape the old redirect
used, if a browser-usable URL is ever wanted here — but nothing currently
serves it at a `/files/{token}/...`-style path; that route went with the sync
lanes. See [`capability-urls.md`](capability-urls.md).

### Change notifications — `WS /notification`

A WebSocket, served in-process when `EnableNotification` is set (it is, by
default). Clients subscribe per library and receive `library-update` when a commit
lands, which is what lets a sync client react in about a second instead of
polling.

Since 0.4.4 getting a subscribe token is one call on this lane:

```
POST /api/silo/v1/libraries/{id}/notify-token   Authorization: Bearer <jwt>
  → {"jwt_token": "<jwt>", "expires_at": 1787312025}
```

72h, authorized with `share.CheckPerm` against the session user, and `404` —
not `403` — when notifications are disabled, so a client can tell "no such
feature" from "not your library". Note `expires_at` is a **number** in a family
of responses that are otherwise strings.

Then connect to `/notification` and send one frame per batch of libraries:

```json
{"type": "subscribe", "content": {"libraries": [{"id": "<library>", "jwt_token": "<jwt>"}]}}
```

Inbound frames are `{"type": "library-update", "content": {"library_id": …, "commit_id": …}}`
and `{"type": "jwt-expired", "content": …}`; unknown types are ignored rather
than closing the socket. `unsubscribe` takes the same frame shape as
`subscribe`. The server pings every 30s and drops a client that has not ponged
within 90s; most WebSocket libraries answer pings for you.

### Debug middleware

The `-debug` flag wraps the whole server in `middleware.DebugLogger`,
which logs `HTTP <METHOD> <PATH> -> <STATUS> (<DURATION>) from <REMOTE>`
for every request and flags 404s as `WARN`. Off by default.

## Not implemented (intentionally)

Anything outside `/api/silo/v1/*` and `/notification` is a 404, including
every path listed in older revisions of this document under the
Seafile/SeaDrive sync lane — see the note at the top. That includes
`/protocol-version` itself: `handleProtocolVersion` (`server.go:570`) still
exists but nothing mounts it, so it's dead code rather than a live route.

## Divergence policy

We track our own **protocol**, not any upstream codebase. When
`/api/silo/v1` gains an operation, it lands here in the same commit — see
[`docs/plan.md`](plan.md) for the migration record and
[`docs/target.md`](target.md) for what this server is aiming at now.
