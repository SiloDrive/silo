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

The older endpoints are **gone as of 0.4.4** — `dir/`, `mkdir`, `file`,
`download`, `rename` and `move` no longer route. Nothing outside this repository
had ever used them, and every one had an equivalent here, so they were removed
while that was still true rather than carried indefinitely. `protocol.md` lists
what to call instead.

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

> **This lane is transitional. Do not architect around a password at rest.**
>
> The advice above is correct for the server as it is today, and re-presenting
> the password on a 401 is the only thing that works right now. It is also the
> one piece of this brief that two landed designs go on to prohibit, so it is
> worth knowing before the Keychain entry exists on real machines:
>
> - [`auth.md`](auth.md)'s device lane makes the password an **enrolment
>   credential** — presented once, exchanged for a registered keypair,
>   forgotten. Requests are signed thereafter; there is nothing to re-present.
> - [`plans/store-v2.md`](plans/store-v2.md)'s split derivation goes further:
>   nothing persists the password at all. The client derives `wrapKey` at
>   enrolment, unwraps its identity key with it, stores **the identity key** in
>   the platform key store, and discards both the password and `wrapKey`.
>
> What that costs you if you build on today's shape unexamined: a re-login path
> that assumes a re-derivable secret, and any feature that quietly depends on
> "we can always log in again". Keep the credential behind a narrow interface —
> something that answers *authenticate this request*, not *give me the
> password* — and the swap is a new implementation rather than an unwind.

If several requests are in flight when it expires they will all 401 at once.
Collapse that into one re-login rather than a stampede; `client/client.go` does
it by recording which token a caller observed and only re-logging in if it has
not already been replaced.

## Server info and version

```
GET /api/silo/v1/server-info        (no auth)
```
```json
{"version":"0.4.6","features":["entries","entries-copy","conditional-writes","ranged-reads","changes","repo-rename","blocks","pagination","batch","notifications"],"block_size":8388608}
```

**`version` is semver with no leading `v`**, and that is a contract, not an
artifact of the example. It used to be one: the source default was `0.4.1` while
CI stamped `git describe --tags`, which is `v0.4.1` for the same commit — so the
spelling depended on how the binary was built, and a client that met the release
build after reading a doc captured from a development build got a string its
parser rejected. The server now normalises before reporting.

A build from an untagged or dirty tree keeps its suffix — `0.4.3-3-gabc1234`,
`0.4.3-dirty` — because that says what is actually running. Parse the leading
`major.minor.patch` and ignore the rest.

Strip an optional leading `v` anyway when you parse. It costs one line and it is
the difference between a clear failure and a silent one if this ever regresses.

Check it at domain setup. **This surface changed materially in 0.4.0** — reads
stopped redirecting, `PUT` started accepting file content — **0.4.1** added
conditional writes, **0.4.3** stopped reporting a damaged library as a
deleted one, **0.4.4** added copy, library rename by `PATCH`, block-by-block
upload, and this feature list itself, and **0.4.5** added pagination and
batching. **0.4.6** changes nothing here — it is a server-bootstrap and client
release — so a client written against 0.4.5 needs no attention. A client built against this document talking to an older server
will fail in confusing ways: against 0.3.x the reads and writes break outright;
against 0.4.0 the `If-Match` headers are silently ignored, which is worse,
because losing an edit looks like success; and before 0.4.3 a server that has
lost an object answers 404, which is worse again, because acting on it deletes
the client's copy. Require **0.4.3 or newer** and say why, rather than
discovering it one broken callback at a time.

**Prefer `features` to the version.** The version says which build answered; the
feature list says what that build will accept, and only the second is the
question a client actually has. Test for a name, never for a version range, and
never by probing behaviour and inferring — when probing works it looks like
success, while the check you wrote is no longer the thing making the decision.

A name is added in the release its capability ships and is then never removed
and never reused, so `has("entries-copy")` stays a safe question forever. An
older server sends no list at all, which reads as "no features" and is the
correct answer: absent means do not call it.

`notifications` is the one entry that depends on how the server was started
rather than on which build it is. Seeing it is how a client knows to mint a
`notify-token`, instead of learning from a 404 that this deployment has the
notification endpoint switched off.

The version is still worth checking once at domain setup, because the entries
surface itself moved under clients before the feature list existed.

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

### What a 404 on the library means, and what it does not

A **404** naming the library is a positive assertion: it is gone, and removing
your copy is the correct handling — it is how a library deleted from the web UI
reaches you. Because of that, from 0.4.3 the server will only say it when the
library really has no row.

A library the server holds but cannot read — its head commit object is missing
from the store — answers **500**, and a database it cannot reach answers **503**.
Both mean *something on the server is broken, nothing has been deleted, do not
act on it*: `EIO` for a FUSE client, a transient error for a File Provider
extension, never an `NSFileProviderItem` removal. Neither is worth a full
re-enumeration; retry, and surface it if it persists.

Before 0.4.3 all three arrived as 404, so a server that lost an object told
every client the library had been deleted — and the copy a client would then
delete is the one that could have restored it. If you must run against an
older server, treat a 404 on a library that `GET /repos` still lists as a
server fault rather than a deletion; the two surfaces disagreeing is the tell.

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

The all-zeros id is the id of an empty object, not an error. Both an empty
directory and a zero-byte file carry it — ids are content hashes and empty
content hashes to one value — so it says nothing about which one you have. Read
`type` for that, and do not use the id to tell them apart or as a cache key: a
content-addressed cache keyed on it collides every empty object in the account
onto one entry.

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
| copy | `POST entries/{path}` with `{"op":"copy","to":"/new/path"}` → **201** |
| delete | `DELETE entries/{path}` |
| rename a library | `PATCH repos/{repoid}` with `{"name":"New name"}` |

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

**Check the id you get back.** You can compute it yourself: files are chunked at
fixed 8 MiB offsets and a block's name is the SHA-1 of its bytes, so a client
that hashes as it uploads knows what id the server should arrive at. If the two
differ, the bytes that landed are not the bytes you sent. That is a complete
end-to-end integrity check for the transfer, and it costs a comparison.

**The parent directory must exist** — a PUT into a missing directory is a
**404**, not an implicit `mkdir -p`. A typo in a path should not silently build
a directory tree. The same holds for the destination of a `move` or a `copy`.

**Copy transfers nothing.** The destination dirent points at the object the
source already names, so a server-side copy costs one dirent and one commit
whether it is an empty file or a hundred-gigabyte subtree — and it returns
**201** with the *source's* `ETag`, because the copy shares its id. If you
already hold that content, you already hold the copy's content. Emulating this
with `GET` then `PUT` pays for every byte twice and produces a different id only
because the mtime differs.

Both `move` and `copy` take the precondition on the **source**: `If-Match` there
means "act on this version, not on whatever it has become". Neither will
overwrite a directory, or replace a file with a directory — that is a **409**,
and the answer is to rename and retry.

A trailing slash also means "directory", but prefer the explicit `?type=dir`.
The distinction is load-bearing: a bare `PUT` stores the body, so dropping the
marker creates an empty file where a directory was meant — silently, with a
`201`, and indistinguishable by id from an empty directory (both carry the
all-zeros sentinel).

Prefer the parameter because the slash does not survive ordinary path handling.
Go's `path.Join` and `path.Clean` both drop it; `url.PathEscape` turns it into
`%2F`; proxies and routers normalise it. Silo's own CLI cannot express the
trailing-slash form at all — `escapePathSegments` (`client/client.go:270`)
opens with `strings.Trim(p, "/")`, which is exactly the line an obvious client
implementation writes. A query parameter is untouched by all of that, and it
says the same word `type` that the listing says back to you.

The library root cannot be moved (**400**) or deleted (**400**), and creating it
is a **409**.

### What a 503 on a write means

The full status-code reference, including the overloaded ones, is
[`responses.md`](responses.md); the paragraphs here cover only what bites a sync
client in practice.


Concurrent writers into one library race for the branch head, and the loser is
told **503** with `Retry-After: 1` and a body of `write contention; retry`.
Nothing was applied. The identical request will usually succeed on the retry,
unchanged — retry it rather than surfacing a failure.

This is the case where a wrong status code costs data. A **500** means the
server hit an unexpected condition and *may have applied part of the request*,
so the only safe handling is to stop and surface it — `EIO` from a FUSE client,
which to the application that already wrote the bytes is data loss. Before
0.4.4 a contended write arrived as exactly that, after a median wait of 14
seconds, on a write the server would have accepted a second later. If you run
against an older server, a 500 from a write during concurrent activity is worth
one retry before you believe it.

Not to be confused with the other write refusals: **412** is a failed `If-Match`
(someone else's edit — re-read and merge), and **409** is a destination
collision (`filenameCollision` — rename and retry).

**409 means exactly one thing now.** It used to be overloaded: a write that
collided with a running GC also answered 409, with the body `GC conflict;
retry`, and that one wants a retry rather than a rename — so a client reading
409 as "collision" renamed the user's file in response to a transient server
condition. From 0.4.4 GC conflicts answer 503 with `Retry-After`, where they
belong. Against an older server, keep the old rule: retry a 409 whose body says
`retry`, rename only on `Destination exists`.

## Conditional writes

Every mutating request honours `If-Match` and `If-None-Match`, so a client can
write without losing someone else's edit. This is the same pair S3 added in
2024.

An earlier draft said the 412 "maps onto `NSFileProviderError.versionOutOfDate`".
There is no such error — the enum runs `-1000…-1007` and `-2001…-2015`, and none
of them is that. The real answer is better than an error, and is spelled out
below.

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

### What a 412 means to a File Provider extension

Not an error. `modifyItem` reports a conflict on the **success** path.

The system hands `modifyItem` a `baseVersion` — the version it believes is on
disk. Send its `contentVersion` as `If-Match`. On a 412 someone else moved the
entry underneath you, so re-read it and call the completion handler with the
**server's** item, carrying the server's new `contentVersion`, and
`shouldFetchContent: true`. The system sees the content version move, calls
`fetchContents`, and replaces the local copy. Returning an error instead gets
the whole modification retried from the top, against the same stale version,
forever.

That resolution discards the local edit, which is the right default for a
first cut and the wrong one in general — but *which* version wins is a policy
decision the extension makes item by item, and the API is built to let it make
that decision rather than to make it for you.

Two traps in the same completion handler:

- The second argument is `stillPendingFields`, the subset of `changedFields`
  you did **not** apply. Since macOS 12, returning a set *identical* to the
  fields you were passed does not mean "try me again later" — the system reads
  it as "this provider does not support these fields" and stops sending them
  until the item changes again. Never return the whole set to signal a
  temporary failure.
- `NSFileProviderError.localVersionConflictingWithServer` does exist and does
  mean exactly this conflict, but only under the `failUploadOnConflict` policy,
  which needs `NSExtensionFileProviderSupportsFailingUploadOnConflict` in the
  extension's `Info.plist` — and it is macOS 26.0 and later. Under it the
  provider *does* fail the call and the system merges and re-calls with a fresh
  `baseVersion`. Below 26.0 the paragraph above is the only route.

Outside `modifyItem` — a conditional write the extension issues on its own
behalf — a 412 is just a 412: re-read, reapply, write again.

**Both are opt-in.** A request with neither header behaves exactly as before, so
last-writer-wins is still available — it just has to be chosen now rather than
arrived at by accident. For a File Provider extension, send `If-Match` on every
`modifyItem`: the version you were handed is precisely the tag to send.

Note the header changes meaning with the method. On `GET`, `If-None-Match` asks
"skip the body if unchanged" and yields **304**. On a write it asks "fail if it
exists" and yields **412**. Same header, different question, as RFC 9110
specifies.

## Doing many things in one commit

Check `features` for `batch`.

```
POST /api/silo/v1/repos/{repo}/batch
{"ops":[{"op":"mkdir","path":"/a"},{"op":"create","path":"/a/x.txt","blocks":[…]}]}
→ 200 {"commit_id":"…","ops":2,"changed":true}
```

`mkdir`, `delete`, `move`, `copy`, `create`. Ordered, and each sees the ones
before it.

**All or nothing.** A failure writes nothing and tells you which operation
stopped it: `{"error":…,"index":3,"op":"move","path":"/x"}`, with the status
code that operation would have answered alone. Retry the whole batch after
fixing it — there is no partial state to reconcile, which is the reason to use
this rather than a loop.

**`create` names blocks you already uploaded.** This is the other half of the
block surface, and together they are the answer to a large drop: upload the
blocks, skipping everything the server holds, then create every file in one
commit. Doing it a file at a time costs one commit each, and the user's history
becomes five hundred entries deep for one drag.

**`If-Match` is about the library root** — the ETag from `GET entries/`. On
contention you get **503** and `Retry-After`, not a merge; re-read and rebuild.

**`mkdir` over an existing directory succeeds.** Over a file it is a **409**.

`changed:false` means nothing needed doing and no commit was minted. Limits:
1000 operations, 4 MiB.

## Paging a long answer

Check `features` for `pagination`. Two endpoints take `?limit=N`: `GET changes`
and a directory listing.

```
GET /api/silo/v1/repos/{repo}/changes?since={commit}&limit=1000
→ 200  {"changes":[…]}
   Link: </api/silo/v1/repos/{repo}/changes?cursor=…&limit=1000>; rel="next"
…
→ 200  {"anchor":"<commit>","changes":[…]}       (no Link — you are done)
```

**Follow the `Link` verbatim.** It is a relative URL and the cursor inside it is
opaque. Do not build the next request from parts, and do not parse the cursor:
it carries an offset today and is free to carry something else tomorrow.

**Do not record the anchor until you have it.** It is absent on every page but
the last. If you save it after page one you have told yourself you are up to
date for changes you have not applied, and nothing will ever tell you otherwise.

**You are reading a snapshot.** The cursor pins the commit — or the directory
object — the first page came from, so writes landing mid-sequence do not make
items skip or repeat. You will see those writes on your next pass.

**A paged listing carries no `ETag`.** A window is not the thing the id names.
Revalidate the whole listing, not a page of it.

**No `limit` means no paging**, and that is a real choice rather than a legacy
default: a truncated answer that looked complete would be worse than a large
one. `limit` is capped at 10,000 and a bad value is a **400**, not a clamp.

`GET /repos` does not page — the count is bounded by how many libraries the
account has.

## Uploading in blocks

Check `features` for `blocks` first. Three calls:

```
POST /api/silo/v1/repos/{repo}/blocks/missing      {"blocks":[sha1,…]} → {"missing":[sha1,…]}
PUT  /api/silo/v1/repos/{repo}/blocks/{sha1}       the block's bytes
PUT  /api/silo/v1/repos/{repo}/entries/{path}?type=blocks   {"blocks":[sha1,…]}
```

**You can compute the ids yourself, and that is the point.** Cut the file at
`block_size` offsets — the number is in `server-info` — and SHA-1 each piece.
Those are the names the server uses, so you can ask what it already holds
before sending anything. Do not guess the block size: chunking at a different
offset uploads correctly and dedups against nothing, which fails silently and
forever.

**Nothing exists until the third call.** Blocks are immutable and named by
their content, so uploading them changes no path, mints no commit and touches
nothing at the destination. Upload in parallel, in any order, across app
launches. If the transfer dies, run the same three calls again — the second
`blocks/missing` returns a shorter list. There is no session, no upload id and
no offset to persist; the server's answer *is* your resume state.

**The last call is the write**, so it behaves like any other: `If-Match` and
`If-None-Match` apply, it answers **201** with the new `ETag`, and it is the
only point at which another client can see anything.

**424 means upload first, then send the same request again.** The body carries
`{"missing":[…]}`. It is not a 400 — nothing about the request is wrong.

**A rejected block is a corrupt transfer.** The server hashes what arrives and
answers **400** if it does not match the id you sent it under, so a successful
PUT is an end-to-end integrity check and not merely an acknowledgement.

**Already-present blocks answer 204 before reading the body.** Send `Expect:
100-continue` and you skip the transfer entirely, which matters when a
`blocks/missing` answer has gone stale under you.

**Encrypted libraries are excluded** (**400**). Their blocks are ciphertext, so
you cannot name one without doing the encryption yourself. Under store-v2 that
stops being an exclusion and becomes the normal case — the client *does* do the
encryption, and names the ciphertext. See *What store-v2 changes* below.

## Gaps — read this before planning M4

Short list, and shorter than it was.

**A plain PUT that dies partway has to start over.** For anything large, use
the block surface above instead — that is what it is for. The remaining limit
is that chunking is at fixed offsets, so inserting a byte near the front of a
file shifts every boundary after it and nothing dedups;
[`protocol-gaps.md`](protocol-gaps.md) has what fixing that would cost.

**Encrypted libraries are not readable over this API, at all.** An earlier draft
said they serve whole files but not ranges. That was wrong: every read reaches a
server-side key cache that nothing ever populates, so the honest answer for any
file in an encrypted library is

```
400  Repo is encrypted. Please provide password to view it.
```

and no endpoint accepts one. Silo cannot create such a library either — the only
way to have one is to import it from an upstream Seafile install, and Seafile's
format will not be supported going forward (`docs/encryption.md` has the audit).
A future Silo-native scheme is sketched there and would serve ranges normally,
since the server would not be decrypting anything.

For Porter: `encrypted: true` in the library listing means unusable. Grey it out
at enumeration rather than discovering it one 400 per file.

That is true of today's server and of Seafile-format libraries permanently. The
Silo-native scheme mentioned above is no longer a sketch — it is specified,
implemented as a Go package, and being wired in now. *What store-v2 changes*,
below, is what to build against.

## What store-v2 changes — read this before writing anything you would hate to unwind

Everything above describes the server as it runs today. A branch is replacing
the storage format underneath it, and enough of that is now pinned that
building against it is cheaper than building around it.

**Status, honestly.** The format itself is done and committed: spec in
[`spec/store-format.md`](spec/store-format.md), Go in
[`store/`](../store), with test vectors for every piece. The server handlers
are being cut over now, and nothing in this section answers on a running server
yet. The plan and its build order are in
[`plans/store-v2.md`](plans/store-v2.md).

**porter-fuse should import `store/`, not reimplement it.** It is a Go package
in this module with no server dependencies — it holds the chunker, the ids, the
manifest/directory/commit codecs, the content crypto, name encryption and key
wrapping, and it is deliberately free of logging and of anything that assumes a
server around it, because it is meant to ship inside a client. porter-mac ports
it to Swift against the shared vectors; a second Go implementation would be a
second thing to keep in step with the vectors for no gain.

### The four wire changes

**Ids become 64 hex characters.** SHA-256 of the stored bytes, everywhere a
40-character SHA-1 appears today: blocks, objects, commits, `head_commit_id`,
`since` anchors, ETags. Anything holding a width of 40 — a route regex, a
column, a validator, a fixed-size buffer — breaks.

**`server-info`'s `block_size` dies, and chunking becomes per-library.** The
library listing serves that library's chunker parameters — algorithm, minimum,
target, maximum, normalisation — frozen at creation. The warning attached to
`block_size` today gets sharper rather than softer: chunk with anything but the
library's own parameters and the upload succeeds, dedups against nothing, and
never tells you. The number is now per-library, so a client that caches one
globally is wrong the moment a second library exists.

**Boundaries are content-defined, and the chunker is keyed.** FastCDC, so
inserting a byte near the front of a file no longer shifts every boundary after
it — the dedup gap named under Gaps above is what this closes. The seed is
derived from the library's content key, so the same file cut in two different
encrypted libraries lands on different boundaries. That is deliberate; it stops
a server confirming what a file is from its boundary fingerprint. For you it
means **the boundary cache is per-library**, never shared across them.

**Small files inline.** Below a threshold the bytes live inside the manifest
and there are no chunks at all, so a directory of ten thousand small files
costs one fetch each rather than three. Whether a file inlines is decided by
its size and never by the writer: two clients disagreeing about a 30 KB file
would mint two ids for identical content, and every reader downstream would see
a file that changed when it did not. `store.Inlined(size)` is the one answer;
do not carry a second.

### The part that changes the shape of the client: E2EE

End-to-end encryption is **on by default** for new libraries, and it splits the
API in a way worth designing for now.

- **A plain library** works exactly as documented above. `entries/{path}`,
  ranged GETs, conditional writes, the block surface, the delta endpoint.
- **An E2EE library keeps `entries/{path}` for structure and loses it for
  content.** The line is not where you might guess, so it is worth stating
  precisely.

**The server can route on an encrypted path, and you supply it.** Names are
AES-SIV ciphertext, and SIV is used *because* it is deterministic: you compute
the same bytes the directory object holds, base64url-encode each segment
(unpadded, RFC 4648 §5) and send that as the path. The server matches
ciphertext against ciphertext without knowing what either says. So resolve,
`HEAD`, listing, delete and move all work on an E2EE library — a listing comes
back with ciphertext names and you decrypt them.

Two things do not, and both are about bytes rather than names:

- **Content reads.** The stored bytes are per-chunk sealed, so a byte range of
  the plaintext is not a byte range of anything the server holds. Read the
  manifest's chunk list and fetch chunks.
- **All writes.** The server cannot chunk an encrypted library — the chunker
  seed is derived from the content key — and cannot build a manifest or a
  directory object. Writes are addressed by id, below.

So: **structure by path, content and writes by id.** Build one interface with
two implementations — the thing that answers *list this directory*, *read this
file*, *write these bytes* — rather than branching on `e2ee` at each call site.

**Reading an E2EE library:**

```
GET  repos/{repo}/entries/{ct-path}              → listing, names as ciphertext
GET  repos/{repo}/entries/{ct-path}?type=blocks  → the ordered chunk list
GET  repos/{repo}/blocks/{id}                    → chunk ciphertext → decrypt
```

or straight down the object graph, which is what a cold start does:

```
GET  repos/{repo}                      → head_commit_id
GET  repos/{repo}/objects/{commit}     → decode → root directory id
GET  repos/{repo}/objects/{dir}        → decrypt names → child ids and types
GET  repos/{repo}/objects/{manifest}   → chunk list, or the inline bytes
```

**Encrypting a depth-N path costs N sequential fetches the first time**, and no
amount of cleverness removes it: each segment's key is derived from its parent
directory's salt, so the parent has to be read before the child can be named.
That is a cost you pay to *build* the ciphertext path, not one the server pays
to route it. Cache the salt map beside your local index — a cold resolve is a
tree walk, and that is why the local index earns its keep here in a way it does
not on a plain library.

**Writing, and this shape is the same for both library types:**

```
POST repos/{repo}/blocks/missing       {"blocks":[id,…]} → {"missing":[id,…]}
PUT  repos/{repo}/blocks/{id}          the chunk's bytes  (or batched pack-blocks)
PUT  repos/{repo}/objects/{id}         manifest, then each directory up the spine,
                                       then the commit
PUT  repos/{repo}/head                 If-Match: <current head commit id>
```

The server verifies that each object's id is the SHA-256 of the bytes and that
it decodes, and verifies nothing else, because on an encrypted library there is
nothing else it can check.

**A write rewrites the spine, and under E2EE that is now your job.** Changing
one file means a new manifest, a new parent directory, a new grandparent, up to
a new root and a new commit. Three rules the server used to enforce and now
cannot:

- **Carry the salt forward.** A rewritten directory keeps the salt of the
  object it replaces. Mint a fresh one and its id changes, so every ancestor's
  does, so the root does — and the delta endpoint reports the entire library
  modified on every commit.
- **Only the directory whose entry list changed gets a new mtime**, and that
  mtime lives one level up, in its parent's entry for it. Stamping the whole
  spine makes every commit look like it touched everything between the change
  and the root.
- **The mutation's timestamp is not the file's mtime.** You preserve a file's
  mtime, which may be years old; the directory it lands in changed just now.

**Server-side merge is gone.** Today a write that loses the race for the branch
head is merged for you. It cannot be: merging trees means reading names.
`PUT head` is a compare-and-swap, and a writer that loses gets a refusal, not a
merge — re-read the head, rebuild your change on the new root, retry. Write
that loop deliberately; it is not the 503-and-retry described under *What a 503
on a write means*, because the root you built on is stale rather than the
server being busy.

**Libraries are created with a client-supplied UUID.** Key wrapping binds to
the library id, so the client mints it and the server accepts it rather than
assigning one.

### Where the encryption boundary is

**What is in a library is private; that the library exists and what it is
called is not.** E2EE covers a library's content and the names of the files
inside it. A library's own display name and description are server-plaintext,
permanently and by decision: the server has to sort them, search them and put
them in `GET /repos` for a client that has not unlocked anything — including a
client that holds no key for that library at all. Sealing them would produce a
listing of untitled libraries, or a second name kept in the clear beside the
sealed one, which is the same disclosure with an extra step.

Two more things the server knows and does not pretend otherwise about: **who
wrote last, and when.** The account is the one that authenticated when the head
moved and the timestamp is the server's clock at that moment, and they are
recorded outside the commit. They may differ from the author and `created_at`
you sealed inside it — you may seal whatever attribution you like — and that is
the intended relationship rather than a bug. They answer different questions:
what the library's members say happened, and what the server witnessed.

Surface the sealed pair where you show history and the server pair where you
show sync state; do not try to reconcile them.

### Keys, in one paragraph

The password splits client-side: an auth key that goes to the server and a
wrap key that never leaves the device. The wrap key unwraps your X25519
identity key; the identity key unwraps each library's content key. **Store the
identity key in the platform key store and discard both the password and the
wrap key** — which is the same instruction the blockquote under
*Authentication* gives, arriving from the other direction. `store/` has the
whole path: `DeriveCredentials`, `OpenIdentityWithPassword`, `UnwrapCK`.

### Things that will never be added, so do not wait for them

- **No password-to-the-server endpoint for an encrypted library.** The
  server-side key cache that today's 400 mentions is being deleted, not
  completed.
- **No path-addressed API on an E2EE library**, per above.
- **No pack ids or offsets on the wire.** Chunks live in packs on the server;
  compaction moves them, so a pack id in a response would be a lie by the time
  you used it. Address chunks by id and nothing else.
- **No chunk-parameter renegotiation.** A library's parameters are frozen at
  creation; changing them is a full rewrite, done deliberately or not at all.

### The Seafile lanes are being deleted

`/repo/…`, `/seafhttp/…`, `/api2/…` and `/api/v2.1/…` go away in this same
work. Nothing in this brief depends on them, but if anything in porter still
reaches for one — `check-blocks` and `commit/HEAD` are the two that used to be
tempting — move it to `/api/silo/v1` now. `seafile-compat-end` is the tag to
revert to if that turns out to be wrong, and [`target.md`](target.md) records
why it will not be.

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
| duplicate an item | `POST /api/silo/v1/repos/{id}/entries/{path}` `{"op":"copy",…}` — no content transferred |
| upload a large file | `POST blocks/missing`, `PUT blocks/{sha1}` for each, then `PUT entries/{path}?type=blocks` |
| enumerate a huge directory | `GET entries/{path}?limit=1000`, then follow `Link: …; rel="next"` |
| write many things at once | `POST /api/silo/v1/repos/{id}/batch` — one commit, all or nothing |
| `deleteItem` | `DELETE /api/silo/v1/repos/{id}/entries/{path}` |
| push invalidation | `WS /notification` |

Identifiers never cross the wire: every request is `(repo_id, path)`, resolved
client-side from `IdMap`. Silo's logs and Sentry traces therefore look like
SeaDrive's, which makes SeaDrive a working reference for what correct traffic
looks like.

## Push invalidation

Polling `/changes` is correct but latent. `WS /notification` tells you when a
library moves, so you can call `/changes` immediately instead of on a timer.

Getting subscribed takes one call, on the lane you are already on:

1. `POST /api/silo/v1/repos/{repoid}/notify-token` — **Bearer** →
   `{"jwt_token":…, "expires_at":<unix seconds>}`. Valid **72 hours**, and per
   library. Authorized by your session against the library's permissions, so a
   read-only share can subscribe and nothing else is needed.

   `expires_at` is a **number**, not a string. Every other token endpoint on
   this lane returns a body of strings, so a client that decodes them all
   through one `map[string]string` breaks here — see
   `docs/bugs/fixed/adding-a-number-to-a-token-response-breaks-clients.md`. Decode
   into a typed struct.
2. Connect to `WS /notification` and send one frame:

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

On `jwt-expired`, re-mint at step 1 and re-subscribe. The 72-hour lifetime means
this will happen to any long-running mount, so implement it before you ship
rather than after the first mysterious silence. Better: re-mint on `expires_at`
and never see `jwt-expired` at all — the difference between re-minting on
schedule and re-minting in response to being disconnected mid-session.

The server pings every **30 seconds** and hangs up if it has not seen a pong in
**90**. Most WebSocket libraries answer pings automatically — confirm yours
does, because the failure mode is a connection that looks alive and delivers
nothing.

If notifications are disabled server-side, step 1 returns **404**. Treat that as
"fall back to polling", not as an error.

**Older servers.** `notify-token` landed after 0.4.3. Against a server without
it the request 404s the same way a disabled notification server does, and the
two are worth telling apart only if you want the fallback: mint a repo token
with `POST /api/silo/v1/repos/{repoid}/sync-token` (**Bearer** →
`{"token":…}`), then `GET /repo/{repoid}/jwt-token` with header
`Seafile-Repo-Token: <that token>` → `{"jwt_token":…}`, no `expires_at`. That
pair is the Seafile lane's auth and is kept only for the upstream client; log
when you use it so it stays visible rather than becoming a habit.

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

Encrypted libraries are the exception, and a harder one than "no ranges": they
are unreadable over this API entirely. See the gaps section above.

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
