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

There is one surface and one credential now. The `Authorization: Token …` that
`/api2/…` and `/api/v2.1/…` took, and the `Seafile-Repo-Token` on `/repo/…`,
were deleted along with the lanes that read them.

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
{"version":"0.5.0","features":["libraries","entries","entries-copy","conditional-writes","ranged-reads","changes","library-rename","blocks","pagination","batch","usage","notifications"]}
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
batching. **0.4.6** changed nothing here — a server-bootstrap and client
release — and was never tagged.

**0.5.0 breaks the wire, and is the first release that does.** Every route,
three JSON names and one notification frame type changed spelling. There is no
shim and no alias: the old paths answer 404. What moved, and what to grep your
own code for, is [`upgrading-to-0.5.0.md`](upgrading-to-0.5.0.md) — kept
separate because it is the one document here that has to write out the names
this release retired.

Check `features` for `libraries` before the first authenticated request. A
server without it predates the change, and the 404 that follows on the library
listing otherwise reaches a person as an account with nothing in it rather than
as a version mismatch.

A client built against this document talking to an older server
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
GET    /api/silo/v1/libraries              list (see the delta endpoint for head_commit_id)
POST   /api/silo/v1/libraries              {"name":"..."} → the new library
DELETE /api/silo/v1/libraries/{libraryid}     delete a library
```

The container app needs create and delete; the extension itself only enumerates.
Deleting a library is not a `DELETE entries/` on its root — the root is not
deletable (**400**), because removing a library is a different operation from
emptying one.

### Size and quota

Feature-detect with `"usage"` in `server-info`'s `features`.

Each row of `GET libraries` carries `size` and `file_count` for that library, and
the account's total lives on its own endpoint:

```
GET /api/silo/v1/account/usage
{"usage": 5000000, "quota": 6000000, "kind": "logical-at-head"}
```

Four things about these numbers, all of which a GUI will otherwise get wrong.

**`quota` is absent when there is no ceiling.** Not `-1`, not `0`, not `-2` —
absent. If the key is missing the account has no limit; render it that way
rather than as a number. Same rule on the listing: `size` and `file_count` are
absent, not zero, when the server could not work out a library's size. Zero
means an empty library, and showing "0 bytes" for "unknown" states a fact you
were never told.

**Per-library sizes are on the listing and not under `account/usage`, and this
is not an oversight.** A library shared with you appears in your listing but is
charged to its *owner's* quota. If per-library figures were reported under an
account total they would either leak libraries you do not own into a sum they
must not belong to, or omit them and leave you with rows you cannot size. On
the listing each row carries its own number and nothing has to add up — do not
sum the listing to reproduce `usage`.

**`kind` says which number this is, and it matters.** `logical-at-head` is the
sum of the sizes of the files the library currently holds — what you get back
by deleting them. It is not bytes on disk. Deduplication and deferred
compaction make those diverge by multiples in *both* directions: two identical
5 MB files read as 10 MB of usage and occupy 5 MB on disk, and a freshly
deleted file frees its usage immediately while its bytes sit on disk until the
collector runs. Both are correct. If you show a figure next to anything a user
might compare with `du`, label it.

**Over quota is `507`.** Not 403 — 403 would tell you the request was not
allowed and to stop, where the truth is to free some space and retry. It can
come back from `PUT entries/{path}` (before the body is read, if you sent a
`Content-Length`, and again after), from `POST batch` (before anything is
applied — the batch is still all-or-nothing), and from `PUT blocks/{id}`.
Treat it as a user-facing condition with an actionable message, not a
transport error to retry blindly: retrying without freeing space returns 507
again.

The account may end up marginally over its ceiling — a single write is admitted
against the figure before it, so the last one through can cross the line. The
next one is refused. Do not build a client that depends on `usage <= quota`
always holding.

### `kind` is going to change, which is why it is there

`logical-at-head` is not the last word on what quota counts. The plan on the
table — [`quota.md`](quota.md) — is to charge the **blocks an account actually
occupies** instead: dedup and compression mean two identical 5 MB files are
billed as 10 MB today while costing 5 MB of disk, and the number the user is
charged for and the number the operator buys disk for should be the same
number.

Nothing about the request or the response shape changes. What changes is the
value of `kind`, which is on the wire for exactly this reason.

**So: branch on `kind`, or display it. Never hard-code the string, and never
assume the number means what it meant last release.** A client that ignores it
shows an account halving overnight and calls it a bug in the server. Concretely,
when `blocks-occupied` arrives:

- **The figure becomes what the account is billed for**, not what its files add
  up to. Those coincide only when nothing is deduplicated or compressed.
- **You cannot estimate it locally by summing listing sizes.** Any optimistic
  "will this fit" check computed client-side has to become a request instead.
- **Deleting a file stops freeing space immediately.** The blocks stay reachable
  from older commits until history retention expires them — ZFS and NetApp
  semantics. A UI that re-reads usage after a delete to show the space coming
  back needs to stop promising that.

Both kinds may be reported together, and a client that shows both can explain
itself: `logical-at-head` is what a file manager would total, `blocks-occupied`
is the bill, and the gap between them is dedup and compression doing their job.

### If you are filling in `df`

`statfs(2)` reports block *counts*, so a `df` row will not match
`account/usage` to the byte and is not meant to. Dividing the account figures by
the block size truncates twice — once for the total, once for what is free — so
the used column lands within one block of the real number, on whichever side
depends on where the quota falls relative to a block boundary.

Two ways to state that wrongly, both tempting:

- It is **not** "the partial last block counted as occupied". That is what a
  local filesystem does, and it does it *per file*. An account holding five
  files of 0, 33, 23,740,889, 5,000,000 and 5,000,011 bytes totals 8238 blocks
  as an aggregate and 8240 rounded file by file.
- It is **not** reliably a round *up*. With `quota = q*B + r` and
  `usage = u*B + s`, the used column is `u` when `r >= s` and `u + 1` when
  `r < s` — it equals `ceil(usage/B)` exactly when the quota's remainder is
  smaller than the usage's, and `floor` otherwise. Raising somebody's cap by a
  few hundred bytes can move the used column by a block without a byte being
  written.

**The block size is yours, not `statfs`'s.** The kernel rounds nothing — it is
handed `Bsize`, `Blocks` and `Bfree` and reports them. You pick the divisor and
you do both divisions, so the size of the discrepancy is set by the constant you
chose. Porter picks 4096 because it is what every local filesystem on the
machine reports and its `df` row then lines up with the others; that is a
tradeoff worth making, not a property of the interface.

Only the magnitude scales with it, though. How *often* the used column differs
from `ceil` is `(B - usage mod B)/B` — a fact about the usage, not about the
block size, averaging about half whichever divisor you pick. A smaller block is
not a rarer discrepancy, only a smaller one.

If you report a measurement, report it as "the aggregate divided by the block
size", not as per-file occupancy.

## The entries endpoint

```
GET    /api/silo/v1/libraries/{library}/entries/{path}   dir → listing, file → bytes
HEAD   /api/silo/v1/libraries/{library}/entries/{path}   headers only
PUT    /api/silo/v1/libraries/{library}/entries/{path}   body → file, or ?type=dir
DELETE /api/silo/v1/libraries/{library}/entries/{path}
POST   /api/silo/v1/libraries/{library}/entries/{path}   {"op":"move","to":"/x/y"}
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
older server, treat a 404 on a library that `GET /libraries` still lists as a
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
GET /api/silo/v1/libraries/{library}/entries/
```
```json
[
  {"name":"a.txt", "type":"file",
   "id":"f7eef0341b337c218dce12bb06c55e52bf98f863a0a749097682016e3ae84cb1",
   "size":2,"mtime":1787578220},
  {"name":"empty", "type":"dir",
   "id":"fb50dc0717ff266cf9baf82b1ce7a1c2ef6d9247859680b11a19fb7077f5f222",
   "mtime":1787578220},
  {"name":"sub",   "type":"dir",
   "id":"fb50dc0717ff266cf9baf82b1ce7a1c2ef6d9247859680b11a19fb7077f5f222",
   "mtime":1787578220}
]
```

**Those are the only keys.** A file row carries `id`, `mtime`, `name`, `size`
and `type`; a directory row the same without `size`. An earlier draft of this
example showed a `modifier` on file rows. No code path has ever set one, so it
never reached a client — only this page — and it is now gone from the struct
too. Pinned by a test, so the example and the wire cannot drift apart again.

**Two of those rows share an id, and that is the format working.** `empty` and
`sub` are both empty directories, so they are the same object and have the same
name. An id names content, never an item.

That has a consequence worth sitting with if you are building a File Provider
extension: **an id is not an identity.** Two paths holding identical bytes share
one, and a file that is edited back to its previous content returns to the id it
had. If you need a handle that survives a rename and stays distinct between two
identical files — and macOS requires exactly that — the id cannot be it. Keep
your own mapping. As a *cache* key it is not merely safe but ideal, since two
identical objects sharing one entry is the point.

**There is no all-zeros sentinel, and there never will be again.** This page
used to say an empty directory and a zero-byte file both carried an all-zeros
id, and warned against keying a cache on it. Under store-v2 objects are typed,
so those two hash differently — an empty directory is
`fb50dc07…`, a zero-byte file is `d7b3d401…` — and the collision that warning
described cannot occur. If your client has a constant for the empty-directory
id, delete it rather than widening it to 64 characters: it was a workaround for
an ambiguity that no longer exists.

For symmetry, the id of an empty directory is also what an empty library's root
listing reports as its `ETag` — the root *is* an empty directory. That is
consistent rather than special, and nothing should read meaning into the two
matching.

### Reading a file

`GET entries/{path}` on a file streams the bytes back on that response. One
request, authenticated by the bearer header on it. No redirect.

```
HTTP/1.1 200 OK
Accept-Ranges: bytes
Content-Length: 14
Content-Type: text/plain
Etag: "v1-f7eef0341b337c218dce12bb06c55e52bf98f863a0a749097682016e3ae84cb1"
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
| rename a library | `PATCH libraries/{libraryid}` with `{"name":"New name"}` |

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

**Check the id you get back.** It is the file's **manifest** id, not a hash of
the bytes you sent. A client that hashes as it uploads computes the wrong thing
and mismatches on every file, including the ones that transferred perfectly —
so do not read a mismatch as corruption until the check itself is right.

Computing it means building the manifest, which a client holding `store/` can
do. Chunk the content under the library's **own** `chunker` parameters, taken
from its listing row: they are per-library data and a client that compiles them
in as constants gets different ids the moment a library is created with
anything else. Each chunk's id is the SHA-256 of its bytes. Encode the manifest
and hash that, and you have the id the `PUT` will return.

A file under 64 KiB never reaches the chunker at all: its bytes are inlined
into the manifest, so the id is still the hash of a manifest — one that wraps
the bytes behind a header — and never of the bytes alone.

Done that way it is a complete end-to-end integrity check for the transfer, and
it costs a comparison. `fileserver/manifest_id_test.go` runs exactly this
against a live server for an inline and a chunked file, if you want a worked
example to check an implementation against.

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
marker creates an empty file where a directory was meant — silently, and with a
`201`. It is at least detectable now: an empty file and an empty directory are
typed objects with different ids, where they once shared an all-zeros sentinel
and could not be told apart at all. Detectable is not the same as noticed,
though, and nothing will raise it for you.

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
POST /api/silo/v1/libraries/{library}/batch
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
GET /api/silo/v1/libraries/{library}/changes?since={commit}&limit=1000
→ 200  {"changes":[…]}
   Link: </api/silo/v1/libraries/{library}/changes?cursor=…&limit=1000>; rel="next"
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

`GET /libraries` does not page — the count is bounded by how many libraries the
account has.

## Uploading in blocks

Check `features` for `blocks` first. Three calls:

```
POST /api/silo/v1/libraries/{library}/blocks/missing      {"blocks":[id,…]} → {"missing":[id,…]}
PUT  /api/silo/v1/libraries/{library}/blocks/{id}         the chunk's bytes
PUT  /api/silo/v1/libraries/{library}/entries/{path}?type=blocks   {"blocks":[id,…]}
```

**You can compute the ids yourself, and that is the point.** An id is the
**SHA-256** of the chunk's bytes, sixty-four hex characters. Those are the
names the server uses, so you can ask what it already holds before sending
anything.

**The boundaries are content-defined, and the parameters belong to the
library.** There is no `block_size` in `server-info` and there has not been
one since chunking stopped happening at fixed offsets — one number shared by
every library is meaningless when boundaries fall where the content puts them.
Read the `chunker` object off the library's row in `GET /libraries`
(`algorithm`, `min_size`, `target_size`, `max_size`, `normalization`), frozen
at creation.

**If the library's row carries no `chunker`, upload whole files instead.** Do
not substitute defaults. Chunking under the wrong parameters uploads
*correctly* — the server verifies every chunk against its id — and dedups
against nothing, so there is no error to see and no point at which it starts
working. It is the one failure on this surface that nothing detects, which is
why the only safe response to "I cannot read the parameters" is not to use the
surface. Our own client had this bug: it cut at fixed 8 MiB offsets and hashed
SHA-1, and files under the threshold hid it because they never took the
chunked path at all.

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

**Already-present blocks answer 200 before reading the body**, where a stored
one answers 201. Send `Expect: 100-continue` and you skip the transfer
entirely, which matters when a `blocks/missing` answer has gone stale under
you.

**Encrypted libraries are excluded** (**400**). Their blocks are ciphertext, so
you cannot name one without doing the encryption yourself. Under store-v2 that
stops being an exclusion and becomes the normal case — the client *does* do the
encryption, and names the ciphertext. See *What store-v2 changes* below.

## Gaps — read this before planning M4

Short list, and shorter than it was.

**A plain PUT that dies partway has to start over.** For anything large, use
the block surface above instead — that is what it is for.

This paragraph used to end by saying chunking was at fixed offsets, so a byte
inserted near the front of a file shifted every boundary after it and nothing
dedups. That is no longer true and has not been since the chunker landed:
boundaries are content-defined, so an insertion moves one boundary and the rest
of the file still matches. The note is kept rather than deleted because the
correction is the more useful fact — a client written against the old sentence
would cut in the wrong places and dedup against nothing.

**Encrypted libraries are not readable over this API, at all.** An earlier draft
said they serve whole files but not ranges. That was wrong: every read reaches a
server-side key cache that nothing ever populates, so the honest answer for any
file in an encrypted library is

```
400  Library is encrypted. Please provide password to view it.
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

**Status, honestly — and this paragraph has been wrong in both directions.** It
once said a branch was replacing the storage format and that nothing here
answered on a running server yet. That was true when written and is not now, and
it stayed on the page long enough to contradict the sections above it, which is
the failure this brief keeps having: a correction lands in one half of the file
and the other half goes on describing the world it replaced.

What actually answers on a running server today:

- **64-hex ids, SHA-256 of the bytes.** `store.ParseID` demands exactly that
  width, so a 40-character SHA-1 is a 400 at the door rather than a fallback.
- **Content-defined boundaries, per library.** The `chunker` object on the
  library's row in `GET /libraries`, frozen at creation.
- **Manifests, directories and commits by content id**, over `/objects/{id}`
  and `/head`.

What does not yet answer:

- **Per-library E2EE.** The format is specified and implemented in `store/`,
  but the server still refuses the block surface for an encrypted library
  (**400**), so the client-encrypts-and-names-the-ciphertext path is not
  reachable over HTTP yet.

The format's spec is in [`spec/store-format.md`](spec/store-format.md), the Go
in [`store/`](../store) with test vectors for every piece, and the remaining
build order in [`plans/store-v2.md`](plans/store-v2.md).

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
GET  libraries/{library}/entries/{ct-path}              → listing, names as ciphertext
GET  libraries/{library}/entries/{ct-path}?type=blocks  → the ordered chunk list
GET  libraries/{library}/blocks/{id}                    → chunk ciphertext → decrypt
```

or straight down the object graph, which is what a cold start does:

```
GET  libraries/{library}                      → head_commit_id
GET  libraries/{library}/objects/{commit}     → decode → root directory id
GET  libraries/{library}/objects/{dir}        → decrypt names → child ids and types
GET  libraries/{library}/objects/{manifest}   → chunk list, or the inline bytes
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
POST libraries/{library}/blocks/missing       {"blocks":[id,…]} → {"missing":[id,…]}
PUT  libraries/{library}/blocks/{id}          the chunk's bytes  (or batched pack-blocks)
PUT  libraries/{library}/objects/{id}         manifest, then each directory up the spine,
                                       then the commit
PUT  libraries/{library}/head                 If-Match: <current head commit id>
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
them in `GET /libraries` for a client that has not unlocked anything — including a
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

### The Seafile lanes are gone

`/repo/…`, `/api2/…` and `/api/v2.1/…` have been deleted, along with the
credentials that authenticated them. Every one of those paths answers **404**
now. Nothing in this brief depended on them, but if anything in porter still
reaches for one — `check-blocks` and `commit/HEAD` are the two that used to be
tempting — it is broken today and the replacement is on `/api/silo/v1`.
`seafile-compat-end` is the tag to revert to if that turns out to be wrong,
and [`target.md`](target.md) records why it will not be.

## The delta endpoint

```
GET /api/silo/v1/libraries/{library}/changes?since={commit}
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

`GET /api/silo/v1/libraries` now returns `head_commit_id` per library:

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

## Reading history

Two calls, and between them everything a `.history/` view needs.

**`GET /api/silo/v1/libraries/{id}/commits`** lists the library's history,
newest first:

```json
{"commits": [
  {"id": "9f2c…", "created_at": 1756142400, "author": "d@nmilne.com", "message": "…"},
  {"id": "4b71…", "created_at": 1756138800, "author": "d@nmilne.com", "message": "…"}
]}
```

`author` and `message` are **absent under E2EE**, not empty. The server does
not have them — they are sealed under a content key it never holds — while
`id` and `created_at` stay public because the walk needs them. Decode them
yourself from the commit object if you hold the key.

It pages like everything else: `?limit=N` and follow `Link: …; rel="next"`.
The cursor resumes the walk rather than indexing into it, so page ten costs
what page one did.

**The listing ends where history ends.** When a retention limit starts
collecting old commits, the walk meeting a commit that is gone is reported by
the list simply stopping — not by an error. That is the difference from
`changes?since=`, where *you* named a commit and are owed a 410. Here you asked
what history exists, and running out of it is the answer.

**`?at={commit}` on the entries endpoint** resolves a path against that
commit's tree instead of the head's:

```
GET /api/silo/v1/libraries/{id}/entries/notes.txt?at=4b71…
```

It is the ordinary read with a different starting id — files, directory
listings, `Range`, `If-None-Match`, all unchanged. A file deleted three commits
ago is still there in the commit that had it, which is the main thing anybody
wants this for.

| Status | Meaning |
|---|---|
| **400** | `at` is not a commit id — or you sent it on a PUT, POST or DELETE |
| **410** | `at` is a well-formed id this library can no longer resolve |
| **404** | the *path* does not exist in that commit |

**Writes carrying `at` are refused, never ignored.** A client that believes it
is editing the past while it is silently editing the present is the worst
outcome available, and that is exactly what dropping an unrecognised parameter
would produce.

### Building `.history/` on top

The directory is yours to synthesize; the server will not invent one. That is
deliberate — a synthetic entry has no id and appears in no manifest, so it
would have to be excluded from GC's mark, from `changes?since=`, from size
accounting and from every other walk. The whole store rests on an id naming
content.

Two properties fall out of that, both useful:

- **It costs nothing against quota**, today. Usage is logical size at head;
  history is by definition not at head, and nothing was special-cased to make
  that true. Note the "today": under the `blocks-occupied` charge described
  above, *reading* history still costs nothing, but the history being read is
  part of what its owner is billed for — which is what makes retention a lever
  rather than a preference.
- **It documents its own retention.** Once a retention limit exists, what the
  commits list returns *is* the window. The user can see how far back they can
  go rather than being told.

ETags still work across time: an id is a content hash, so an unchanged file has
the same ETag at an old commit as at the head, and a `.history/` copy of a file
you already hold revalidates to 304 without transferring anything.

## Updated request inventory

Supersedes the table in `macos-fileprovider-plan.md`. Every row here has been
exercised against a running server.

| Callback | Method + path |
|---|---|
| domain setup | `POST /api/silo/v1/auth/login` |
| — | `GET /api/silo/v1/server-info` |
| `enumerateItems` (root) | `GET /api/silo/v1/libraries` — rows carry `size`/`file_count` |
| show storage used | `GET /api/silo/v1/account/usage` |
| `enumerateItems` (dir) | `GET /api/silo/v1/libraries/{id}/entries/{path}` |
| `currentSyncAnchor` | `head_commit_id` from `GET /api/silo/v1/libraries` |
| `enumerateChanges` | `GET /api/silo/v1/libraries/{id}/changes?since={commit}` |
| `item(for:)` | *none* — local `IdMap` ⋈ `WorkingSet` |
| `fetchContents` | `GET /api/silo/v1/libraries/{id}/entries/{path}` — bytes on the response |
| revalidate a cached item | the same GET with `If-None-Match` → 304 |
| read at an offset | the same GET with `Range` → 206, repeatable |
| `createItem` (dir) | `PUT /api/silo/v1/libraries/{id}/entries/{path}?type=dir` |
| `createItem` (file) | `PUT /api/silo/v1/libraries/{id}/entries/{path}` — body is the file |
| `modifyItem` (contents) | the same `PUT` — it replaces |
| `modifyItem` (rename) | `POST /api/silo/v1/libraries/{id}/entries/{path}` `{"op":"move",…}` |
| `modifyItem` (reparent) | the same call — a move is a move |
| duplicate an item | `POST /api/silo/v1/libraries/{id}/entries/{path}` `{"op":"copy",…}` — no content transferred |
| upload a large file | `POST blocks/missing`, `PUT blocks/{id}` for each, then `PUT entries/{path}?type=blocks` |
| enumerate a huge directory | `GET entries/{path}?limit=1000`, then follow `Link: …; rel="next"` |
| write many things at once | `POST /api/silo/v1/libraries/{id}/batch` — one commit, all or nothing |
| `deleteItem` | `DELETE /api/silo/v1/libraries/{id}/entries/{path}` |
| list history | `GET /api/silo/v1/libraries/{id}/commits` |
| read at a past commit | `GET /api/silo/v1/libraries/{id}/entries/{path}?at={commit}` |
| push invalidation | `WS /notification` |

Identifiers never cross the wire: every request is `(library_id, path)`, resolved
client-side from `IdMap`. Silo's logs and Sentry traces therefore look like
SeaDrive's, which makes SeaDrive a working reference for what correct traffic
looks like.

## Push invalidation

Polling `/changes` is correct but latent. `WS /notification` tells you when a
library moves, so you can call `/changes` immediately instead of on a timer.

Getting subscribed takes one call, on the lane you are already on:

1. `POST /api/silo/v1/libraries/{libraryid}/notify-token` — **Bearer** →
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
{"type":"subscribe","content":{"libraries":[{"id":"<library-id>","jwt_token":"<jwt>"}]}}
```

`"unsubscribe"` takes the same shape. Events come back in the same envelope:

```json
{"type":"library-update","content":{"library_id":"…","commit_id":"…"}}
{"type":"jwt-expired","content":{…}}
```

`commit_id` is exactly the anchor `/changes` wants, so a `library-update` translates
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
it the request 404s the same way a disabled notification server does. There is
no longer a fallback: the library-token pair that used to serve as one
(`POST libraries/{id}/sync-token` then `GET /repo/{id}/jwt-token`) was the Seafile
lane's own auth and went with that lane. Treat a 404 as "poll".

## Checking your work against the server

The Go CLI speaks exactly this surface, so it is a reference implementation you
can diff against — `client/client.go` is ~100 lines of it.

```bash
export SILO_URL=http://server:8082 SILO_EMAIL=… SILO_PASSWORD=…
silo libraries --json                      # libraries, with head_commit_id
silo ls   <library> /some/dir             # GET entries/
silo put  <library> ./local.txt /dir      # PUT entries/… (body is the file)
silo get  <library> /dir/local.txt out    # GET entries/…
silo mkdir <library> /new                 # PUT entries/…?type=dir
silo mv   <library> /a.txt /sub/a.txt     # POST entries/… {"op":"move"}
silo rm   <library> /sub/a.txt            # DELETE entries/…
silo changes <library> <since> --json     # the delta endpoint
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

- **You do not need the block lane.** `seadrive-fuse` reached bytes at an
  offset by fetching a file's fs object for its block list and reassembling,
  because it had no better option. That put it partway to replicating the
  object store, and blocks defaulted to 8MB, so a 4KB read pulled a whole
  block. That lane no longer exists; `entries/` with a `Range` serves the same
  reads in one request.
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
