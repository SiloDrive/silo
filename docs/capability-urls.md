# Capability URLs: what to drop from the Silo lane, and what to build if we ever want signed URLs

Records why `/files/{token}/{name}` exists, why the Silo lane no longer uses it,
and what "signed URLs" should mean if we ever want them — so the answer isn't
re-derived, and so nobody resurrects the current mechanism by mistake.

**Status: done.** The Silo lane serves file bytes on the authenticated request
and accepts them on a `PUT`. What follows is the reasoning, kept because the
same question will come up again the first time someone wants a browser to
fetch a file.

## What it was

Downloading a file through `/api/silo/v1` used to take two requests:

1. `GET /api/silo/v1/libraries/{id}/entries/{path}` — bearer auth → **302**
2. `GET /files/{token}/{name}` — **no auth header**; the token *is* the credential

The token was a UUID in a `sync.Map` (`fileserver/tokenstore`, since deleted), created with
`oneTime=true` at `api_handlers.go`, and redeemed by `QueryToken` with a
`LoadAndDelete`. Uploads mirrored it: `POST /api/silo/v1/access-tokens` then
`POST /upload-api/{token}`. At the time this was written both stayed exactly
as they were for the legacy sync lane, which was the lane that needed them;
that lane — `/upload-api/`, `/files/{token}/...` and the rest — was deleted
outright in `5d4baa0`, after the "Do not touch" note below was written and
before it was corrected.

## Why it exists — and it is a good design, for something else

It was inherited from upstream, whose web layer was a Django app. Silo replaced
that layer outright and does not run it. Its call sites are still the clearest
statement of what the pattern is for — the URL builder read:

```python
# Format: http://<domain:port>/files/<token>/<filename>
return '%s/files/%s/%s' % (get_fileserver_root(), token, quote(filename))
```

Its consumers are things that **can only be handed a URL**:

- a browser navigating a download link, or an `<img>` / `<video>` src
- a wiki view, building an `href` for a page
- an office-integration view, handing an upload URL to a *separate document
  server* that will POST the edited file back
- public share links, given to someone with no account at all

None of those can set an `Authorization` header. A URL is the only thing you can
pass them, so the URL has to be the credential. And once that is true, one-time
redemption plus a short expiry is the correct mitigation, because URLs leak into
places headers never reach: browser history, `Referer`, proxy and access logs,
chat pastes, screenshots. A header dies with the request; a URL gets written
down.

So this is not junk in its original context. It is junk **in ours** — and note
that every one of those consumers lived in that web layer. The wiki, the office
integration and the share-link pages are all gone along with it. The mechanism
outlived every caller that justified it.

## Why the Silo lane doesn't need it

`/api/silo/v1` has no browser-shaped consumer. There is no web UI in this
binary — no templates, no `http.FileServer`, no embedded assets — and the share
link and web file-access routes (`/f/`, `/u/`, `/d/`, `/libraries/{id}/files/{path}`)
were already removed for exactly this reason: each authorized by POSTing to the
web layer, so with that layer gone they could only ever fail.

Every caller of this lane is a programmatic HTTP client that sets a bearer
header on every request: the TUI, the CLI, and the sync clients being written
now. For them the capability URL is pure overhead — and for a FUSE client it is
worse than overhead, because the one-time token means every `read()` at an
offset costs a redirect plus a ranged GET. Two round trips per read.

## What was done instead

Both changes were additive and neither touched the legacy sync lane.

**Reads — bytes come from `entries/`.** `serveFile` in `fileserver/entries.go`
streams the content on the authenticated request, honouring `Range`, instead of
redirecting. No capability is minted and nothing is one-time, so a client can
issue as many ranged reads against one URL as it likes. It reuses the existing
machinery, which never needed a token in the first place:

```go
doFileRange(rsp, r, library, fileID, fileName, op, byteRanges, user, textCharset)
doFile(rsp, r, library, fileID, fileName, op, cryptKey, user, textCharset)
```

`accessCB` was the dispatch to copy: range when `!library.IsEncrypted &&
len(byteRanges) != 0`, otherwise whole-file with the crypt key.

**Writes — `PUT entries/{path}` takes the body.** It replaces, which is what
PUT means and is deliberately unlike the legacy upload's "rename the
collision" behaviour. The body is spooled to a temp file before indexing:
`chunkFile` seeks to each chunk boundary, so the source has to be seekable, and
`indexFileWorker` already accepted a `filePath` with a nil multipart handler for
exactly that reason. Spooling is the requirement, not a shortcut around it.

`/api/silo/v1/access-tokens` and the `fileserver/tokenstore` package behind it
are gone as well. They survived the route that redeemed them by a release,
minting tokens no path would accept while `protocol.md` went on advertising the
endpoint — which is the failure mode a mechanism outliving its only caller
always has.

### The charset wart, also fixed

`setCommonHeaders` labelled every `text/*` file `charset=gbk` — inherited, and
wrong for anything not actually GBK. It sat in the shared streaming path, so
rather than duplicate `doFile` (the drift worth avoiding) it took the charset as
a parameter: the legacy lane passed `"gbk"` and stayed byte-identical, the Silo
lane passed `""` and declared no charset at all, because the server does not
know how a file it is handing back is encoded. Guessing wrong is worse than not
saying. With the legacy lane deleted, only the second half survives.

## If we ever want signed URLs

We might. `roadmap.md` keeps a web UI on the table — "a small SPA
served from the same Go binary" — and the moment a browser renders a thumbnail
or plays a video, we are back to a consumer that cannot set a header. Third-party
integrations and public share links would do the same.

**Build them properly then; do not resurrect this.** What exists today is a
*stateful capability token*. A signed URL is a different mechanism:

| | today's token | a signed URL |
|---|---|---|
| Server state | a `sync.Map` entry per token | none |
| Survives restart | no | yes |
| Works across replicas | no — the map is per-process | yes |
| Expiry | a field checked on redemption | encoded in the payload, covered by the signature |
| Scope | whatever was stored | encoded — library, path, op, expiry |
| Revocation | delete the entry | rotate the key, or a short TTL |

Concretely: HMAC or JWT over `(library_id, file_id, op, expiry)` with a server key,
in the query string. Nothing to store, nothing to redeem, no reason to be
one-time — a short expiry does that job — and it works unchanged behind a load
balancer, which the current one does not.

It should also be **an explicit, additive endpoint** — something like
`POST /api/silo/v1/libraries/{id}/entries/{path}/signed-url` returning a URL — and
never a redirect the normal read path forces every client through. That is the
actual mistake to avoid repeating: not the capability URL itself, which is a
sound tool, but making every programmatic client walk through one to reach its
own bytes.

## Do not touch — superseded

**This section no longer applies.** It said not to touch the legacy sync lane —
`/files/`, `/upload-api/`, `/update-api/`, `/blks/`, `/zip/` and the token
store — because upstream clients used it and it was frozen for compatibility.
That compatibility promise was dropped on purpose and the whole lane was deleted
in `5d4baa0`; every one of those paths now answers 404. See
[`docs/target.md`](target.md) for the decision and
[`docs/protocol.md`](protocol.md) for what the server serves today.

Original text, kept for the record: *"The legacy lane. `/files/`,
`/upload-api/`, `/update-api/`, `/blks/`, `/zip/` and the token store stay
exactly as they are: the upstream clients use them, they are frozen for
compatibility, and Silo's reason for existing is that unmodified clients keep
working. This note is about which lane the new API should live in, not about
deleting the old one."*
