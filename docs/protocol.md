# Protocol Compatibility

This document is the server's **contract with the clients**. It lists the
HTTP endpoints the Go fileserver implements, groups them by which client uses
them, and notes which Seafile-protocol endpoints are deliberately stubbed or
left unimplemented.

It says which endpoints exist. For what the *status codes* mean — and which are
already spoken for — see [`responses.md`](responses.md), which is the file to
check before a new handler picks one.

Treat this as the source of truth when a new client release starts hitting
an endpoint we don't support — find it in the "not implemented" list and
decide whether to shim it.

## Tested clients

- **SeaDrive for macOS** — tested against 3.0.21 (via Homebrew cask)
- **silo** — our own Go TUI (`cmd/silo`)

Other Seafile clients (desktop, CLI, mobile) should work in principle since
they speak the same underlying sync protocol, but have not been verified.

## Authentication schemes

Three coexisting auth mechanisms, each for a different client surface:

| Scheme | Header | Used by | Validated against |
|---|---|---|---|
| JWT Bearer | `Authorization: Bearer <jwt>` | silo (TUI), `/api/silo/v1/*` | `authmgr.ValidateSessionToken` (24h expiry) |
| API Token | `Authorization: Token <40-hex>` | SeaDrive, `/api2/*` | `apitokenstore.Lookup` (persistent in `ApiToken` SQL table) |
| Repo Token | `Seafile-Repo-Token: <40-hex>` | All sync clients, `/repo/*`, `/accessible-repos` | `repomgr.GetEmailByToken` (persistent in `RepoUserToken` SQL table) |

The middleware for each lives in `fileserver/middleware/`:
- `RequireAuth` (Bearer JWT)
- `RequireAPIToken` (Token)
- (repo-token validation is inline in `sync_api.go:validateToken`)

## Endpoints

### Native management API — `/api/silo/v1/*`

JSON request/response bodies. Used by the silo TUI, the CLI in `client/`, and
the sync clients. Protected by `RequireAuth` (JWT Bearer), except the two marked
**No auth** below — they are registered above the authenticated subrouter
(`server.go:698`) because they are what a client needs *before* it has a
credential: one to learn what it is talking to, one to get a token.

| Method | Path | Purpose |
|---|---|---|
| GET | `/api/silo/v1/server-info` | **No auth.** `{"version":"0.4.4"}` — semver, no leading `v` |
| POST | `/api/silo/v1/auth/login` | **No auth.** Email + password → JWT |
| POST | `/api/silo/v1/access-tokens` | Create a time-limited access token for a specific object |
| GET | `/api/silo/v1/repos` | List repos owned by authenticated user |
| POST | `/api/silo/v1/repos` | Create a new repo |
| DELETE | `/api/silo/v1/repos/{repoid}` | Delete a repo |
| POST | `/api/silo/v1/repos/{repoid}/sync-token` | Generate a repo sync token (for subsequent sync-protocol calls) |
| POST | `/api/silo/v1/repos/{repoid}/notify-token` | Mint a notification JWT for `WS /notification` (72h; `404` if notifications are disabled) |

#### The entries surface

One addressable noun with the HTTP methods as its verbs. This is what new
clients speak, and what `client/` speaks; the wire contract with captured
responses is in [`porter-brief.md`](porter-brief.md), and the status codes are
in [`responses.md`](responses.md).

| Method | Path | Purpose |
|---|---|---|
| GET | `/api/silo/v1/repos/{repoid}/entries/{path}` | Read a file's bytes, or list a directory. Ranged; `ETag`/`304` |
| PUT | `/api/silo/v1/repos/{repoid}/entries/{path}` | Store a file — body is the content |
| PUT | `/api/silo/v1/repos/{repoid}/entries/{path}?type=dir` | Create a directory (a trailing slash also works; prefer the parameter) |
| POST | `/api/silo/v1/repos/{repoid}/entries/{path}` | `{"op":"move","to":"/dst"}` — moving covers renaming |
| DELETE | `/api/silo/v1/repos/{repoid}/entries/{path}` | Delete a file or directory |
| GET | `/api/silo/v1/repos/{repoid}/changes?since=` | Changes since an anchor (`410` when the anchor is too old) |

`{path}` is relative to the library root and never repeats the library name;
`entries/` with nothing after it is the root. Every mutating method honours
`If-Match` and `If-None-Match`.

Directory creation is `PUT …?type=dir` rather than a `MKCOL`-style verb or an
RPC: `PUT` already means "make the resource at this URI have this state", and
the marker resolves the one ambiguity — a directory has no body, so without it a
bodiless `PUT` is indistinguishable from writing an empty file. Parents are not
created implicitly; a `PUT` into a missing directory is a `404`, for the same
reason WebDAV's `MKCOL` answers `409` rather than building the tree.

#### Removed in 0.4.4

`dir/`, `mkdir`, `file`, `download`, `rename` and `move` were the pre-entries
shape: verbs in paths, one resource under two names, the path passed as a query
parameter. Every one of them had an equivalent on the entries surface, so they
were duplicates rather than capabilities, and they are gone.

| removed | call instead |
|---|---|
| `GET /repos/{repoid}/dir/?path=` | `GET repos/{repoid}/entries/{path}` on a directory |
| `POST /repos/{repoid}/mkdir` | `PUT repos/{repoid}/entries/{path}?type=dir` |
| `DELETE /repos/{repoid}/file?path=` | `DELETE repos/{repoid}/entries/{path}` |
| `GET /repos/{repoid}/download?path=` | `GET repos/{repoid}/entries/{path}` — streams on the same response |
| `POST /repos/{repoid}/rename` | `POST repos/{repoid}/entries/{path}` with `{"op":"move"}` |
| `POST /repos/{repoid}/move` | `POST repos/{repoid}/entries/{path}` with `{"op":"move"}` |

`download`'s `302` to `/files/{token}/{name}` is the one behaviour that does not
survive verbatim, and its removal was already the plan: this lane has no
browser-shaped consumer, and for a client that sets a bearer header anyway a
capability URL is overhead — for a FUSE client, two round trips per read. See
[`capability-urls.md`](capability-urls.md), which is the decision; this removes
the last route that still contradicted it.

The mechanism is untouched. `/files/{token}/{name}` still serves the Seafile
lane, and `POST /api/silo/v1/access-tokens` with `{"repo_id":…, "obj_id":<file
id>, "op":"download"}` still mints the same token the redirect used, if a
browser-usable URL is ever wanted here.

### Seahub compatibility API — `/api2/*`

Seahub/DRF-shaped endpoints for SeaDrive. Request/response shapes match
what the original Seafile Seahub returns. Form-encoded bodies where the
original used form-encoded; JSON where the original used JSON.

Protected by `RequireAPIToken` (`Authorization: Token <40-hex>`), except
the login endpoint itself.

| Method | Path | Auth? | Notes |
|---|---|---|---|
| POST | `/api2/auth-token/` | No | Form-encoded `username` + `password` → `{"token": "<40-hex>"}` |
| GET | `/api2/auth/ping/` | Yes | Returns `"pong"`. SeaDrive uses as a token-validity probe |
| GET | `/api2/account/info/` | Yes | Returns `{email, name, usage, total, institution}` |
| GET | `/api2/server-info/` | Yes | Returns `{version, features}` |
| GET | `/api2/repos/` | Yes | List accessible repos (owned + shared + group) in Seahub format |
| POST | `/api2/repos/` | Yes | Create a new repo. SeaDrive calls this when you `mkdir` in "My Libraries" |
| GET | `/api2/repos/{repoid}/download-info/` | Yes | Returns token + metadata + file server URL so SeaDrive can begin sync |
| POST | `/api2/repos/{repoid}/repo-tokens/` | Yes | Alternate path to generate a repo sync token (unused by current SeaDrive; kept for other clients) |

Handler implementations: `fileserver/api/seadrive.go`.

### Sync protocol — `/repo/*`, `/files/*`, `/seafhttp/*`

The file-level protocol spoken by all Seafile-family clients (SeaDrive,
desktop client, CLI). Authentication is via `Seafile-Repo-Token` header,
validated against the `RepoUserToken` SQL table per request (with a
2-hour in-memory cache).

| Method | Path | Purpose |
|---|---|---|
| GET | `/protocol-version` | Returns `{"version": 2}` |
| GET | `/accessible-repos?repo_id={id}` | List accessible repos in sync-protocol format |
| GET | `/repo/{id}/permission-check` | Verify user can read or write the repo |
| GET/PUT | `/repo/{id}/commit/HEAD` | Read or advance the HEAD commit pointer |
| GET/PUT | `/repo/{id}/commit/{commit_id}` | Read or upload a commit object |
| GET/PUT | `/repo/{id}/block/{block_id}` | Read or upload a content block |
| GET | `/repo/{id}/block-map/{file_id}` | Return block size map for a file (SeaDrive on-demand reads) |
| GET | `/repo/{id}/fs-id-list` | Enumerate FS object IDs for a commit range |
| POST | `/repo/{id}/pack-fs` | Bulk download FS objects |
| POST | `/repo/{id}/check-fs` | Check which FS objects exist server-side |
| POST | `/repo/{id}/recv-fs` | Upload FS objects |
| POST | `/repo/{id}/check-blocks` | Check which blocks exist server-side |
| GET | `/repo/{id}/quota-check?delta=N` | Will this write fit in quota? |
| GET | `/repo/{id}/jwt-token` | Get a JWT for notification server |
| POST | `/repo/head-commits-multi` | Get HEAD commits for multiple repos in one round-trip. Silo requires a sync token here and answers only for repos that token's owner can read; upstream leaves it unauthenticated. A client that sends no token gets 400 and should fall back to per-repo `GET /commit/HEAD`. |
| GET | `/files/{token}/{filename}` | Download a file via a short-lived access token |
| GET | `/repos/{repoid}/files/{filepath}` | Download a file by path (uses repo token) |

Uploads and updates also accept tokenized URLs:
`/upload-api/{token}`, `/upload-blks-api/{token}`, `/upload-raw-blks-api/{token}`,
`/update-api/{token}`, `/upload-aj/{token}`, `/update-aj/{token}`.

### Path prefix handling

- **`/seafhttp/` prefix is stripped** by `middleware.StripSeafhttpPrefix`
  before routing. In nginx-reverse-proxied Seafile deployments, the sync
  server sits behind a `/seafhttp/` location block, so SeaDrive and the
  desktop client send requests with that prefix. Our standalone Go server
  strips the prefix so the same routes match without duplication.

### Debug middleware

The `-debug` flag wraps the whole server in `middleware.DebugLogger`,
which logs `HTTP <METHOD> <PATH> -> <STATUS> (<DURATION>) from <REMOTE>`
for every request and flags 404s as `WARN`. Off by default.

## Not implemented (intentionally)

These endpoints are part of the Seafile ecosystem but return 404 here. They
are either handled by separate daemons in a standard Seafile deployment or
not applicable to our single-binary standalone model.

| Path | Normally provided by | Our behavior | Client impact |
|---|---|---|---|
| ~~/notification/ping~~ | Integrated into Silo (`fileserver/notif`) | Handled | — |
| ~~/notification/events~~ | Integrated into Silo (`fileserver/notif`) | Handled | — |
| Anything else under `/api2/` we haven't listed | Seahub | 404 | Logged as WARN so new SeaDrive releases are easy to catch |
| Anything under `/api/v2.1/` | Seahub REST API v2.1 | 404 | No tested client uses this yet |

If a client starts hitting something in this list and breaks, the fix is
usually to add a shim handler that reuses existing `fileserver/` code.
See `fileserver/api/seadrive.go` for the pattern.

## Upgrade / divergence policy

We track the **protocol**, not the upstream Seafile codebase. When a new
SeaDrive release ships:

1. Install it against this server with `-debug` logging.
2. Watch for 404 WARN lines — those are new endpoint probes.
3. Look at what the real Seahub returns for that endpoint (either from
   docs, source, or a packet capture against a real Seafile instance).
4. Add a shim in `fileserver/api/seadrive.go` that reuses existing logic
   (e.g., `share.CheckPerm`, `repomgr.*`).
5. Update the tables in this document.

The goal is that this document stays in sync with what the server actually
serves, so a future maintainer can diff it against any new client release
and know exactly what to build.
