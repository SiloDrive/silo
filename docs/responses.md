# Response codes

What each status code means on Silo's API, and — more useful — which ones are
already spoken for.

**Check this file before choosing a code for a new handler.** The reason it
exists is a collision that already happened: `409` was given to write conflicts
in one release and to destination collisions in the next, by two people reading
two different documents, and the two want opposite handling from the client.
Nothing catches that but a list.

The *why* behind a code lives in the bug report that decided it. This file is
the index; `docs/bugs/fixed/` is the reasoning.

## Three lanes, and codes do not mean the same thing on all of them

| lane | prefix | who speaks it | codes |
|---|---|---|---|
| **Silo** | `/api/silo/v1/…` | porter-fuse, the File Provider extension, `client/`, anything new | standard HTTP, documented below |
| **Seafile sync** | `/repo/…`, `/protocol-version` | the upstream Seafile desktop client | standard, plus the `44x` block |
| **Seahub compat** | `/api2/…` | SeaDrive | DRF-shaped, follows the Silo lane's meanings |

New work belongs on the Silo lane. The other two are compatibility surfaces
whose codes are fixed by what an existing client already believes.

## Silo lane

Paths below are written relative to the prefix **`/api/silo/v1`**, and the
entries surface is:

```
/api/silo/v1/repos/{repoid}/entries/{path}
                   ^^^^^^^^         ^^^^^^
                   library UUID     path within that library
```

`{repoid}` is the library's UUID from `GET /repos`, never its name — names are
neither unique nor stable. `{path}` is relative to the library root and does
not repeat the library; `entries/` with nothing after it is the root. The route
is greedy (`{path:.*}`), so the whole remainder including slashes is one
variable.

### Success

| code | means | notes |
|---|---|---|
| `200 OK` | done | |
| `201 Created` | the entry, library or block now exists | `PUT repos/{repoid}/entries/{path}` (content or `?type=blocks`), `PUT repos/{repoid}/blocks/{sha1}`, `POST /repos`, and `POST entries/{path}` with `{"op":"copy"}`. Carries the new `ETag` on a write, so a client can record the version without a follow-up `GET`. A copy carries the *source's* `ETag`, because a copy shares its id — so a client that already holds the content knows it does |
| `204 No Content` | the block is already here | `PUT repos/{repoid}/blocks/{sha1}` only. Answered before the body is read, so a client sending `Expect: 100-continue` never transfers it |
| `206 Partial Content` | range request satisfied | `GET repos/{repoid}/entries/{path}` advertises `Accept-Ranges: bytes` and honours `Range`. An encrypted library cannot be ranged and says so up front with `Accept-Ranges: none` — see the note below |
| `302 Found` | **not emitted on this lane.** `GET repos/{repoid}/entries/{path}` streams on the same response — one request, no redirect. The `/files/{token}/…` capability URL still exists; mint its token with `POST /access-tokens` and build the URL yourself |
| `304 Not Modified` | your `If-None-Match` matched | the entry is unchanged; use your copy |

### The client asked for something wrong

| code | means | what a client should do |
|---|---|---|
| `400 Bad Request` | malformed or contradictory request — missing `path`, moving the library root, a `src` equal to its `dst`, a block whose bytes do not hash to the id it was sent under, `?type=blocks` on an encrypted library, a `limit` that is not a positive integer within range, a `cursor` this server did not issue | fix the request; never retry unchanged |
| `401 Unauthorized` | no `Authorization` header, a malformed one, or an expired session token | re-authenticate, then retry once. Do not loop |
| `403 Forbidden` | authenticated, but not permitted — including libraries you cannot see | surface it; do not retry. A library you cannot see and a library that does not exist both answer `403` from the token endpoints on purpose, so they cannot be used to probe for valid ids |
| `404 Not Found` | the named thing does not exist — see the overload note below | depends on *what* was not found |
| `405 Method Not Allowed` | wrong verb on a real path; carries `Allow` | the path was fine, the verb was not |
| `409 Conflict` | a destination collision, or an attempt to create `/` — see the overload note below | rename and retry, or fix the client |
| `410 Gone` | your `since` anchor, or the commit your page cursor was issued against, is no longer reachable | stop incremental sync and enumerate from scratch. `GET repos/{repoid}/changes` only |
| `412 Precondition Failed` | your `If-Match` did not match; someone else wrote first | re-read, reapply your change, write again. Not an error — it is the mechanism working |
| `413 Payload Too Large` | body over the limit | do not retry |
| `416 Range Not Satisfiable` | the range is outside the entry | |
| `424 Failed Dependency` | the file names blocks the server does not hold | `PUT entries/{path}?type=blocks` only. The body is `{"error":…,"missing":[sha1,…]}` — upload those, then send the *identical* request again. Not `400`, because nothing about the request is wrong |
| `429 Too Many Requests` | login rate limiting; carries `Retry-After` | wait the stated time. Only the login endpoints produce this |

### The server could not do it

| code | means | what a client should do |
|---|---|---|
| `500 Internal Server Error` | an unexpected condition, and the request **may have been partly applied** | stop and surface it. This is the one class you must not retry blind. It also covers a library whose storage is damaged — see the overload note |
| `503 Service Unavailable` | transient, nothing was applied; carries `Retry-After` | retry the identical request. Three sources: write contention (`write contention; retry`), a write that raced the garbage collector (`GC conflict; retry`), and an unreachable database (`Library metadata is temporarily unavailable; retry`). The bodies differ so the logs can tell them apart; the handling does not |

`Retry-After` is the tell. A response carrying it is one the server expects to
succeed later; `429` and `503` are the only two that do.

### Ranged reads on an encrypted library

An encrypted library cannot be ranged — the stored blocks are ciphertext, so a
byte range of the plaintext is not a byte range of what is stored — and `Range`
against one is ignored: the whole file arrives with `200`.

Ignoring a `Range` is allowed. Advertising support for one and then ignoring it
is not, and `serveFile` used to set `Accept-Ranges: bytes` unconditionally — so
a client that trusted the header wrote the whole file at the requested offset.
From 0.4.4 an encrypted library answers `Accept-Ranges: none`.

The general rule still holds and is cheaper than any header: check for `206`
before trusting the offset, rather than assuming the request implies the
response.

## The overloads, and how to tell them apart

These are the places where one code carries two meanings. Each is a hazard for
a client written from the code alone.

### `409` — one meaning again

| body | means | correct handling |
|---|---|---|
| `Destination exists and is a directory` / `…and is a file` | a move or copy would destroy the destination | rename and retry (`NSFileProviderError.filenameCollision`) |
| `The library root already exists` | you tried to create `/` | a bug in the client; do not retry |

Both are collisions, and both want the same shape of response, so `409` is no
longer overloaded on this lane.

It used to be. `GC conflict; retry` — a write that raced the garbage collector —
also answered `409`, which meant a client reading `409` as "collision" would
rename a file in response to something whose only correct handling is to send
the identical request again. From 0.4.4 it answers `503` with `Retry-After`,
alongside write contention, which is what it always meant.

The Seafile lane still answers `409` on its own upload paths. That number is
upstream's contract, not ours, and is not to be changed. See
`docs/bugs/fixed/write-contention-returns-500.md`.

### `404` — three different subjects

| what is missing | example body | correct handling |
|---|---|---|
| the **library** | `Repo not found` | it is genuinely gone; remove your local copy. This is how a deletion propagates |
| the **parent directory** of a write | `Parent directory does not exist` | create the parents yourself. `PUT` is not `mkdir -p`, deliberately — a typo should not build a tree |
| the **entry** | `Not found`, `File not found` | ordinary absence |

Only the first is an instruction to delete anything. Since 0.4.3 the server
says `Repo not found` **only** when the library genuinely has no row — a
library whose storage is damaged answers `500` and an unreachable database
answers `503`, both meaning *nothing was deleted, do not act on it*. Before
0.4.3 all three arrived as `404`, so a server that lost an object told every
client its libraries had been deleted. See
`docs/bugs/fixed/missing-object-reports-repo-not-found.md`.

### `500` — broken, or damaged

`500` normally means "unexpected, may be partly applied, stop". It is also the
considered answer for a library whose head commit object is missing, where the
body says so explicitly: *Library exists but its storage is damaged on the
server; do not delete your copy.* Both readings agree on the action — stop, do
not delete — so this one is safe, but read the body before reporting it.

## `424` — retry, but do something first

Every other code splits into "your request is wrong, fix it" and "the server is
having a moment, send it again". A commit naming blocks that are not on the
server is neither. The request is exactly right, and it will succeed unchanged
— once its dependency exists.

Answering `400` would tell a client never to retry, which is wrong. Answering
`503` would tell it to retry immediately and forever, which is worse: nothing
changes on its own. `424` says what is actually true, and the body says which
blocks, so the fix is exact rather than a re-upload of the whole file.

In practice a client sees this only after `blocks/missing` and its uploads
disagree — a skipped upload, or a block that went missing between the two. The
recovery is the same either way: ask again, upload what it names, retry.

## Reserved, and not yet used

Claim one of these by adding a row here in the same commit that starts
returning it.

| code | reserved for | note |
|---|---|---|
| `402`, `451` | — | no plausible use; do not invent one |
| `423 Locked` | **WebDAV `LOCK`** if that frontend lands — see `protocol-frontends.md` | do *not* spend it on "encrypted library, no key supplied" (currently `400`/`403`); WebDAV clients read `423` as a lock they can wait on or steal |
| `428 Precondition Required` | forcing conditional writes | only if Silo ever refuses unconditional writes; it does not today |
| `507 Insufficient Storage` | quota exhausted | `macos-fileprovider-plan.md` already maps it to `.insufficientQuota` alongside `413`. The quota path currently answers `443` on the Seafile lane only |

`424` used to belong here and no longer does — the block surface claimed it.
It is a WebDAV code, but a WebDAV frontend would use it inside a `207
Multi-Status` body rather than as a response of its own, so the two do not
collide.

## Seafile sync lane: the `44x` block

`fileserver/http_code.go`. These are not real HTTP codes — they are Seafile's,
and the upstream desktop client understands them by number. They exist only on
`/repo/…` and the legacy upload and download paths, and **nothing new should
return them**.

| code | constant | means |
|---|---|---|
| `440` | `seafHTTPResBadFileName` | illegal filename |
| `441` | `seafHTTPResExists` / `seafHTTPResNotExists` | already exists / does not exist (yes, the same number for both) |
| `442` | `seafHTTPResTooLarge` | file too large |
| `443` | `seafHTTPResNoQuota` | over quota |
| `444` | `seafHTTPResRepoDeleted` | library deleted |
| `445` | `seafHTTPResRepoCorrupted` | library corrupted |
| `446` | `seafHTTPResBlockMissing` | block missing |

The legacy upload and download paths in `fileop.go` also still answer `400`
"Bad repo id" where the Silo lane would answer `404`/`500`, and `500` for write
contention where the Silo lane answers `503`. Both are known and deliberate:
those paths are entangled with the `44x` codes above, and changing them needs a
Seafile client to test against.

## Rules for adding a code

1. **Read the table first.** If the code is listed, it already means something.
2. **A code is a promise about what the client should do**, not a description
   of what went wrong internally. Pick it by asking "what do I want the client
   to do next" — retry, stop, re-read, delete, rename.
3. **Distinguishing conditions is the server's job.** Collapsing several causes
   into one code is how both of the overloads above happened, and how the
   missing-object bug turned a server fault into client-side data loss.
4. **`Retry-After` whenever you say "later".**
5. **Say it in the body.** Every non-2xx carries a sentence a human can act on;
   several of the distinctions above are only visible there.
6. **Record the decision** in the bug report or feature request that drove it,
   and add the row here in the same commit.
