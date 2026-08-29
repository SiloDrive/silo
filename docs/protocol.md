# Protocol Compatibility

This document is the server's **contract with the clients**. It lists the
HTTP endpoints the Go fileserver implements.

**The legacy sync lane — `/repo/*`, `/api2/*`, `/api/v2.1/*`,
`/files/{token}/*` and their prefixed variants — was deleted in `5d4baa0` (0.5.0).** Every
one of those paths answers 404 now; nothing here still stubs or shims them.
See [`docs/target.md`](target.md) for why, and
[`porter-brief.md`](porter-brief.md#the-legacy-lanes-are-gone) for the
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

One scheme, one table, one verification path.

| Scheme | Header | Used by | Validated against |
|---|---|---|---|
| Bearer credential | `Authorization: Bearer silo_<kind>_<id>_<secret><check>` | silo (TUI), Porter, porter-fuse, `/api/silo/v1/*` | `credential.Resolve` against the `Credential` table |

The middleware is `RequireCredential`, in `fileserver/middleware/credential.go`.

`POST /api/silo/v1/auth/login` does double duty, and **the request decides
which response comes back**:

```
{"email":…, "password":…}
  → 200 {"token": "silo_session_…"}          a 24h session; unchanged, byte for byte

{"email":…, "password":…, "kind":"device",   enrolment: any of kind, client_name,
 "client_name":"Porter 1.2 (macOS)",         public_key, perm or scope makes it one
 "perm":"r", "scope":"<library-id>"}
  → 201 {"credential": "silo_device_…", "expires_at": …, "email": …}
```

A device credential lives 90 days, absolute. `client_name` becomes the label an
operator revokes by and is required for enrolment. `perm` and `scope` can only
narrow, so asking for more than the account has yields what the account has
rather than a 403. `public_key` answers `501` — proof of possession is designed
and not built.

The password is an *enrolment* credential rather than a request credential:
presented once, exchanged, and not held afterwards.

Only `SHA-256(secret)` is stored, and a row is found by the public `id` in the
middle of the token, so the secret never reaches a query or a query log. The
last six characters are a checksum over everything before them, so a truncated
paste is refused as *malformed* before the database is touched. Revocation is
a row delete and takes effect on the next request; `silo token list|revoke` is
the operator's side of it.

`Authorization: Silo` is reserved for
[proof of possession](auth.md#proof-of-possession) and answers `501` until the
RFC 9421 verifier exists. `Authorization: Token` (the forty-hex legacy form)
parses but is mounted on no route.

The notification socket takes the same credential and accepts requests without
one — see `/notification` below.

A credential can carry a **ceiling**: a permission (`r` or `rw`) and a scope
(every library, one library, or one folder and everything beneath it). What a
caller may do is the account's permission intersected with that ceiling, never
the account's alone, so a narrowed credential answers `403` where the account
behind it would have been allowed. Library-wide operations — `changes`,
`commits`, `notify-token` — and the id-addressed chunk and object surfaces are
refused to a folder-scoped credential outright, since neither can be answered
partially. A plain login mints one unscoped `rw` session; an enrolment request
mints whatever narrowing it asks for, since `perm` and `scope` can only take
access away.

A scoped credential is refused every route that names no library, because such
a route answers about the account and that is wider than the scope. `POST
/auth/logout` is the one exception, and it is not a special case: its subject
is the row presenting it rather than the account, so it is mounted on
`RequireOwnCredential`, which is `RequireCredential` without the scope check.
Nothing else uses that lane.

See [`docs/auth.md`](auth.md) for the model.

### Discarding a credential, and changing a password

```
POST /api/silo/v1/auth/logout             → 200 {"revoked": 1}   this credential
POST /api/silo/v1/auth/logout/everywhere  → 200 {"revoked": n}   all of them
POST /api/silo/v1/auth/password           → 200 {"revoked": n}   n sessions signed out
{"current_password": "…", "new_password": "…"}
```

Logging out needs no write permission — revocation only ever takes access away.
Changing a password needs `rw`, needs the **current** password even though the
request is authenticated, and signs out every `session` credential on the
account including the one that asked, while leaving `device` credentials
mounted. A wrong current password answers `401` and charges the login rate
limiter.

### The account's key material

Four things per account that the server stores and cannot read: the published
X25519 public key, the identity private key wrapped under a password-derived
key, that same key wrapped once per recovery code, and the argon2id parameters
the password is stretched under. See
[`auth.md`](auth.md#the-accounts-key-material).

```
GET    /api/silo/v1/account/keys                → 200 {public_key, wrapped_key, kdf_params, recovery:[…]}
                                                  404 if nothing has been published
PUT    /api/silo/v1/account/keys                → 200 {"updated_at": …, "recovery": 10}
{"public_key": "<b64>", "wrapped_key": "<b64>", "kdf_params": "$argon2id$…",
 "recovery": [{"ordinal": 0, "wrapped_key": "<b64>"}, …]}

DELETE /api/silo/v1/account/keys/recovery/{n}   → 200 {"remaining": 9}

POST   /api/silo/v1/auth/kdf                    → 200 {"kdf_params": "$argon2id$…"}
{"email": "…"}
```

Every blob is base64. `PUT` replaces the whole set rather than merging into it,
in one transaction, because a password change re-wraps all of them at once and
a stale blob is one that still opens with the old secret. The server refuses a
publish whose `kdf_params` disagree with the parameters sealed inside
`wrapped_key` — they are one fact stored twice, and a pair that can drift makes
a blob nobody can open.

`POST auth/kdf` is **unauthenticated**, because a client needs the parameters
before it can turn a password into anything. It never answers `404`: an address
nobody holds gets plausible parameters derived from a stored server secret, the
same ones every time, because the difference between two answers would be an
account-enumeration oracle. `POST` rather than `GET` so the address does not
travel in a URL and into every log along the way. Rate limited per address at
sixty a minute.

Nothing sends the derived `authKey` yet — `POST auth/login` still takes the
password. The endpoint ships with the account model rather than after it; see
[`storage.md`](storage.md)'s split-derivation login.

### Creating an encrypted library

A different request from creating a plain one, because the server cannot build
any of it. The root directory and the initial commit are sealed under a content
key the server never holds, so both arrive with the request; and the library id
arrives with them, because `store.WrapCK` binds the library id into the wrap as
associated data and so the id must exist before the key can be wrapped.

```
POST /api/silo/v1/libraries                     → 201 {"id": …, "name": …}
{"name": "…", "e2ee": true,
 "library_id":  "<canonical lower-case hyphenated UUID>",
 "root":        "<b64 of the sealed empty root directory object>",
 "commit":      "<b64 of the sealed initial commit object>",
 "wrapped_key": "<b64 of the content key wrapped to your identity key>"}

GET /api/silo/v1/libraries/{libraryid}/key      → 200 {"wrapped_key": "<b64>"}
                                                  404 on a plain library
```

The alternative — the server mints the id, the client publishes the key
afterwards — leaves a window holding a library whose content key nobody stored.
One request, or none of it.

**The account must have published an identity key first** (`PUT account/keys`),
or the request is `409`: a content key wrapped to nothing lives on the device
that made it and dies with it.

What the server checks, and it is everything it can check without a key: each
object's id is the SHA-256 of its bytes, each decodes, both are sealed rather
than plain, the root is empty, the commit has no parents and names that root,
and the id is free. `400` names the specific failure; `409` means the id is
taken. Sending `root`, `commit`, `library_id` or `wrapped_key` **without**
`"e2ee": true` is `400` rather than ignored — a client that sent a wrapped key
and got a server-readable library would not find out until somebody read the
data.

`GET libraries/{libraryid}/key` needs read permission and nothing more: whoever
can read the ciphertext and holds this can read the library, and whoever cannot
gains nothing from a blob they cannot open. The library id is in the route, so
a scoped credential reaches its own library's key and no other.

## Endpoints

### Native management API — `/api/silo/v1/*`

JSON request/response bodies. Used by the silo TUI, the CLI in `client/`, and
the sync clients. Protected by `RequireCredential`, except the four marked
**No auth** below — they are registered above the authenticated subrouter
(`fileserver/server.go:555`) because they are what a client needs *before* it
has a credential: one to learn what it is talking to, one to get the parameters
that turn a password into what it sends, one to get a token, and one to create
the first account on a server that has none. `auth/logout`
is registered there too, but is authenticated: see the lane note above.

| Method | Path | Purpose |
|---|---|---|
| GET | `/api/silo/v1/server-info` | **No auth.** `{"version":"0.5.0","features":[…]}` — semver with no leading `v`, and the capability list a client should branch on instead of the version. Carries `"setup_required": true` on a server that has no accounts yet, and omits the key entirely otherwise, so a claimed server's body is unchanged from before the field existed. No chunker parameters: they belong to the library, and the libraries listing carries them. The `libraries` name says this server serves `/libraries/…`; a client that does not find it is talking to a build that predates the word and should say so rather than read the 404 that follows as an empty account |
| POST | `/api/silo/v1/auth/login` | **No auth.** Email + password → a `session` credential, or an enrolled one. Not a JWT: it names a row in `Credential` that can be revoked, labelled and narrowed |
| POST | `/api/silo/v1/auth/logout` | Discard the credential that made the request. No write permission needed, and a scoped credential may reach it |
| POST | `/api/silo/v1/auth/logout/everywhere` | Discard every credential the account holds, including this one |
| POST | `/api/silo/v1/auth/password` | `{"current_password":…,"new_password":…}` — change the password. Needs `rw` and the current password; revokes `session` credentials and leaves `device` ones mounted |
| POST | `/api/silo/v1/auth/setup` | **No auth**, because it is the request that creates the first account — there is nothing to authenticate it against yet. `{"email":…,"password":…,"setup_token":…}` → `201 {"token":…}`, login's shape exactly. The address and password are the operator's choice; the setup token, printed at boot and by `silo setup-token`, is what proves they own the host. `401` for a wrong *or* malformed token, indistinguishably; `409` once any account exists. Guarded by `setup_required` above rather than by trying it |
| POST | `/api/silo/v1/auth/kdf` | **No auth.** `{"email":…}` → the argon2id parameters that address's password is stretched under, client-side. Never `404`: an address with no account gets plausible, stable, per-address parameters, so this cannot be used to ask which addresses exist |
| GET | `/api/silo/v1/account/keys` | The account's published X25519 public key, its wrapped identity private key, its recovery wraps and its `kdf_params`. `404` before anything is published. Readable with a `perm: "r"` credential |
| PUT | `/api/silo/v1/account/keys` | Publish all of it, replacing what was there. Needs `rw`. `400` names the specific refusal — every one is a client bug whose symptom otherwise appears on a device months later |
| DELETE | `/api/silo/v1/account/keys/recovery/{n}` | Redeem one recovery wrap; the rest of the set stands. Needs `rw`. `404` if that ordinal is already spent |
| GET | `/api/silo/v1/libraries` | List the caller's libraries — owned, plus any shared directly to them through `SharedLibrary` — each with `head_commit_id`, the anchor `changes` starts from. `[]`, never `null`, for an empty account. Group shares are honoured by `CheckPerm` but do not appear in this list |
| POST | `/api/silo/v1/libraries` | Create a new library. `{"name":…}` for a plain one; add `"e2ee": true` and the four fields above for an encrypted one |
| GET | `/api/silo/v1/libraries/{libraryid}/key` | The library's content key, wrapped to the calling account. `404` on a plain library, and on an encrypted one nobody has shared with you |
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
| PUT | `/api/silo/v1/libraries/{libraryid}/entries/{path}?type=blocks` | Store a file from blocks already uploaded — body is `{"blocks":[sha256,…]}`, no content. See the block surface below |
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
  {"op":"create", "path":"/reports/q3.txt", "blocks":["<sha256>", …]},
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
| PUT | `/api/silo/v1/libraries/{libraryid}/blocks/{id}` | Upload one chunk. `201` when stored, `200` when it was already there — re-sending is what a resumed upload does, so it succeeds rather than conflicts. `400` if the bytes do not hash to the id |
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

Edits in the middle of a file are cheap, and this is the paragraph that used to
say the opposite. Chunking is `fastcdc-gear64/v1` (`store/params.go`) —
content-defined, 256 KiB minimum, 1 MiB target, 4 MiB maximum. An edit shifts
the boundaries around it and the rolling hash re-syncs within a chunk or two, so
a change in the middle of a 1 GB file re-transfers single-digit megabytes rather
than the file. Appends and unchanged regions cost nothing, as before.

The claim this replaces — that a byte inserted near the front reshapes every
boundary after it and nothing dedups — was true of the fixed offsets this
surface started with, and stopped being true when store-v2 landed
content-defined chunking. It is worth naming as a correction rather than
silently deleting: a client author who read it would reasonably have decided the
block surface was not worth implementing for edit-heavy content, which is the
workload it helps most.

What remains true is narrower. Dedup depends on both sides cutting at the same
places, so a client that uploads without reading the library's `chunker`
parameters produces ids that match nothing already stored — see above. And no
amount of block negotiation helps a file whose bytes are rewritten wholesale on
every save, which is what [`protocol-gaps.md`](protocol-gaps.md) means when it
separates a photo library from a library of VM images.

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

`POST /api/silo/v1/access-tokens`, which minted the token that redirect used,
is gone too. It outlived the route that redeemed it and went on handing clients
a string nothing would accept. If a browser-usable URL is ever wanted here, the
thing to build is a signed URL rather than a stateful token —
[`capability-urls.md`](capability-urls.md) is the decision and the design.

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

Then connect to `/notification`. The upgrade takes an **optional**
`Authorization: Bearer <jwt>` — the same session token every other call uses:

```
GET /notification    Authorization: Bearer <jwt>      (optional)
```

Optional because the endpoint predates the header and existing clients dial it
with nothing. What sending it buys is the right to hold a socket with nothing
subscribed: a connection that authenticated may sit idle indefinitely, which is
what a client that opens the socket at login and subscribes later needs. A
connection that did **not** authenticate has 30 seconds to subscribe to
something before the server closes it — a socket with no subscriptions receives
nothing anyway, so an anonymous one that never subscribes is pure cost to the
server and of no use to the client.

A token that is sent and is bad is refused with `401` rather than treated as
absent, in this lane as in every other.

Send one frame per batch of libraries:

```json
{"type": "subscribe", "content": {"libraries": [{"id": "<library>", "jwt_token": "<jwt>"}]}}
```

Inbound frames are `{"type": "library-update", "content": {"library_id": …, "commit_id": …}}`
and `{"type": "jwt-expired", "content": …}`; unknown types are ignored rather
than closing the socket. `unsubscribe` takes the same frame shape as
`subscribe`. The server pings every 30s and drops a client that has not ponged
within 90s; most WebSocket libraries answer pings for you.

An update that cannot be delivered because a client is behind is **deferred,
not dropped**: the server remembers the latest commit id per library and sends
it as an ordinary `library-update` as soon as the client's queue moves.
Repeated misses for one library collapse into the newest. Nothing extra is
needed on the client side to receive this — it is the same frame.

### Debug middleware

The `-debug` flag wraps the whole server in `middleware.DebugLogger`,
which logs `HTTP <METHOD> <PATH> -> <STATUS> (<DURATION>) from <REMOTE>`
for every request and flags 404s as `WARN`. Off by default.

## Not implemented (intentionally)

Anything outside `/api/silo/v1/*` and `/notification` is a 404, including
every path listed in older revisions of this document under the
legacy sync lane — see the note at the top. That includes
`/protocol-version` itself: `handleProtocolVersion` (`server.go:577`) still
exists but nothing mounts it, so it's dead code rather than a live route.

## Divergence policy

We track our own **protocol**, not any upstream codebase. When
`/api/silo/v1` gains an operation, it lands here in the same commit — see
[`docs/plan.md`](plan.md) for the migration record and
[`docs/target.md`](target.md) for what this server is aiming at now.
