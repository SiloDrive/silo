# Capability URLs: what to drop from the Silo lane, and what to build if we ever want signed URLs

A note, not a plan. Records why `/files/{token}/{name}` exists, why the new API
should not use it, and what "signed URLs" should mean if we ever want them —
so the answer isn't re-derived, and so nobody resurrects the current mechanism
by mistake.

## What it is today

Downloading a file through `/api/silo/v1` currently takes two requests:

1. `GET /api/silo/v1/repos/{id}/entries/{path}` — bearer auth → **302**
2. `GET /files/{token}/{name}` — **no auth header**; the token *is* the credential

The token is a UUID in a `sync.Map` (`fileserver/tokenstore`), created with
`oneTime=true` at `api_handlers.go`, and redeemed by `QueryToken` with a
`LoadAndDelete`. Uploads mirror it: `POST /api/silo/v1/access-tokens` then
`POST /upload-api/{token}`.

## Why it exists — and it is a good design, for something else

It was inherited from upstream Seafile, whose web layer was **Seahub**, a Django
app. Silo replaced Seahub outright and does not run it; the source at
`../seafile/seahub` is reference material, not a component. The call sites there
are still the clearest statement of what the pattern is for.
`seahub/utils/__init__.py`:

```python
# Format: http://<domain:port>/files/<token>/<filename>
return '%s/files/%s/%s' % (get_fileserver_root(), token, quote(filename))
```

Its consumers are things that **can only be handed a URL**:

- a browser navigating a download link, or an `<img>` / `<video>` src
- `seahub/wiki/views.py`, building an `href` for a page
- `seahub/onlyoffice/views.py`, handing an upload URL to OnlyOffice — a
  *separate document server* that will POST the edited file back
- public share links, given to someone with no account at all

None of those can set an `Authorization` header. A URL is the only thing you can
pass them, so the URL has to be the credential. And once that is true, one-time
redemption plus a short expiry is the correct mitigation, because URLs leak into
places headers never reach: browser history, `Referer`, proxy and access logs,
chat pastes, screenshots. A header dies with the request; a URL gets written
down.

So this is not junk in its original context. It is junk **in ours** — and note
that every one of those consumers lived in Seahub. The wiki, the OnlyOffice
integration and the share-link pages are all gone along with it. The mechanism
outlived every caller that justified it.

## Why the Silo lane doesn't need it

`/api/silo/v1` has no browser-shaped consumer. There is no web UI in this
binary — no templates, no `http.FileServer`, no embedded assets — and the share
link and web file-access routes (`/f/`, `/u/`, `/d/`, `/repos/{id}/files/{path}`)
were already removed for exactly this reason: each authorized by POSTing to
Seahub, so with Seahub gone they could only ever fail.

Every caller of this lane is a programmatic HTTP client that sets a bearer
header on every request: the TUI, the CLI, and the sync clients being written
now. For them the capability URL is pure overhead — and for a FUSE client it is
worse than overhead, because the one-time token means every `read()` at an
offset costs a redirect plus a ranged GET. Two round trips per read.

## What to do instead

Both changes are additive and neither touches the Seafile lane.

**Reads — serve bytes directly from `entries/`.** `GET entries/{path}` on a file
should stream the content itself, honouring `Range`, instead of redirecting. It
is already bearer-authenticated, so no capability is minted and nothing is
one-time. The machinery exists and takes no token:

```go
doFileRange(rsp, r, repo, fileID, fileName, op, byteRanges, user)  // fileop.go
doFile(rsp, r, repo, fileID, fileName, op, cryptKey, user)
```

`accessCB` shows the dispatch to copy: range when `!repo.IsEncrypted &&
len(byteRanges) != 0`, otherwise whole-file with the crypt key.

**Writes — accept a body on `PUT entries/{path}`.** Currently 501. Note that
`indexFileWorker` and `chunkFile` *already* handle a `filePath` with a nil
multipart handler, so the cheap route is to spool the request body to a temp
file and pass that — no rewrite of the chunking path. `chunkFile` seeks per
block, so the source has to be seekable either way; spooling is not a
workaround, it is the requirement.

Then `/api/silo/v1/access-tokens` has no callers in the new lane, and the
redirect, the token map and the two-step upload are all gone from it.

### One wart to know about

`setCommonHeaders` labels any `text/*` file `charset=gbk`, which is a Seafile
inheritance and simply wrong for UTF-8 content. It sits in the shared streaming
path, so fixing it for the new lane means either parameterising
`setCommonHeaders` or duplicating `doFile` — and duplicating the streaming path
is exactly the kind of drift worth avoiding. Left alone for now; a client that
honours the declared charset will mangle text.

## If we ever want signed URLs

We might. `future-features.md` keeps a web UI on the table — "a small SPA
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
| Scope | whatever was stored | encoded — repo, path, op, expiry |
| Revocation | delete the entry | rotate the key, or a short TTL |

Concretely: HMAC or JWT over `(repo_id, file_id, op, expiry)` with a server key,
in the query string. Nothing to store, nothing to redeem, no reason to be
one-time — a short expiry does that job — and it works unchanged behind a load
balancer, which the current one does not.

It should also be **an explicit, additive endpoint** — something like
`POST /api/silo/v1/repos/{id}/entries/{path}/signed-url` returning a URL — and
never a redirect the normal read path forces every client through. That is the
actual mistake to avoid repeating: not the capability URL itself, which is a
sound tool, but making every programmatic client walk through one to reach its
own bytes.

## Do not touch

The Seafile lane. `/files/`, `/upload-api/`, `/update-api/`, `/blks/`, `/zip/`
and the token store stay exactly as they are: SeaDrive and Seafile Desktop use
them, they are frozen for compatibility, and Silo's reason for existing is that
unmodified clients keep working. This note is about which lane the *new* API
should live in, not about deleting the old one.
