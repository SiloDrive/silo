# Protocol Compatibility

This document is the server's **contract with the clients**. It lists the
HTTP endpoints the Go fileserver implements, groups them by which client uses
them, and notes which Seafile-protocol endpoints are deliberately stubbed or
left unimplemented.

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

JSON request/response bodies. Used by the silo TUI. Protected by
`RequireAuth` (JWT Bearer).

| Method | Path | Purpose |
|---|---|---|
| POST | `/api/silo/v1/auth/login` | Email + password → JWT |
| POST | `/api/silo/v1/access-tokens` | Create a time-limited access token for a specific object |
| GET | `/api/silo/v1/repos` | List repos owned by authenticated user |
| POST | `/api/silo/v1/repos` | Create a new repo |
| DELETE | `/api/silo/v1/repos/{repoid}` | Delete a repo |
| GET | `/api/silo/v1/repos/{repoid}/dir/?path=` | List directory contents |
| POST | `/api/silo/v1/repos/{repoid}/mkdir` | Create a directory |
| DELETE | `/api/silo/v1/repos/{repoid}/file?path=` | Delete a file |
| GET | `/api/silo/v1/repos/{repoid}/download?path=` | Download file (302 → `/files/{token}/...`) |
| POST | `/api/silo/v1/repos/{repoid}/rename` | Rename a file or directory |
| POST | `/api/silo/v1/repos/{repoid}/move` | Move a file or directory |
| POST | `/api/silo/v1/repos/{repoid}/sync-token` | Generate a repo sync token (for subsequent sync-protocol calls) |

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
