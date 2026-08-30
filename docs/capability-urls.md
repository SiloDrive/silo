# Capability URLs: why the Silo lane has none, and what signed URLs should be

Records why the inherited `/files/{token}/{name}` mechanism existed, why the
Silo lane does not use it, and what "signed URLs" should mean if they are ever
wanted — so the answer is not re-derived, and so nobody resurrects the old
mechanism by mistake.

**Status: settled.** The Silo lane serves file bytes on the authenticated
`GET entries/{path}` request, honouring `Range`, and accepts them on `PUT`.
The capability-URL lane and the `tokenstore` package behind it are gone.
[`plans/sharing.md`](plans/sharing.md) designs share links as credential rows
at `/s/{code}`, and defers signed URLs until the share page renders media
inline.

## Why it exists — and it is a good design, for something else

Upstream's web layer minted a one-time token, stored it in a per-process map,
and handed the browser `/files/<token>/<filename>` to redeem with no auth
header. Its consumers were things that **can only be handed a URL**:

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

Silo has no such consumer. Every caller of `/api/silo/v1` is a programmatic
client that sets a bearer header on every request, and for a mounted
filesystem a one-time token is worse than overhead: every `read()` at an
offset would cost a redirect plus a ranged GET. The mechanism outlived every
caller that justified it.

## If we ever want signed URLs

We might. [`roadmap.md`](roadmap.md) keeps a web UI on the table, and the
moment a browser renders a thumbnail or plays a video, we are back to a
consumer that cannot set a header. Third-party integrations and inline media
on the share page would do the same.

**Build them properly then; do not resurrect the token store.** That was a
*stateful capability token*. A signed URL is a different mechanism:

| | a stored token | a signed URL |
|---|---|---|
| Server state | a map entry per token | none |
| Survives restart | no | yes |
| Works across replicas | no — the map is per-process | yes |
| Expiry | a field checked on redemption | encoded in the payload, covered by the signature |
| Scope | whatever was stored | encoded — library, path, op, expiry |
| Revocation | delete the entry | rotate the key, or a short TTL |

Concretely: HMAC or JWT over `(library_id, file_id, op, expiry)` with a server
key, in the query string. Nothing to store, nothing to redeem, no reason to be
one-time — a short expiry does that job — and it works unchanged behind a load
balancer.

It should also be **an explicit, additive endpoint** — something like
`POST /api/silo/v1/libraries/{id}/entries/{path}/signed-url` returning a URL —
and never a redirect the normal read path forces every client through. That is
the mistake to avoid repeating: not the capability URL itself, which is a sound
tool, but making every programmatic client walk through one to reach its own
bytes. When one lands, [`error-reporting.md`](error-reporting.md)'s path
redaction becomes load-bearing.
