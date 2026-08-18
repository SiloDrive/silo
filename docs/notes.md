# Silo — Architecture Notes

## Overview

Silo is a single Go binary that serves file sync over HTTP and talks directly to
its database and object store. The store is content-addressable, like Git: repos
point at branches, branches at commits, commits at a tree of directory and file
objects, and file objects at deduplicated blocks.

## Where it came from

Silo is a fork of `haiwen/seafile-server`. Upstream ran four processes — a C
daemon holding all business logic behind 174+ libsearpc calls, a Python/Django
web layer, a Go fileserver for sync traffic, and a controller to supervise them.
Silo collapsed that into one process: the C daemon and the web layer are gone,
the notification server was ported into `fileserver/notif/`, and the RPC socket
no longer exists. What survives from upstream is the part clients can see — the
sync wire protocol, the on-disk object layout, and the database schema.

Two consequences worth knowing:

- Anything that used to be authorized by the web layer is simply absent. The
  share-link routes (`/f/`, `/u/`, `/d/`) and the web file-access route were
  removed rather than ported, because every one of them authorized by calling
  out to a service Silo does not run. See `docs/capability-urls.md`.
- Names on disk and on the wire still read "seafile" (`seafile.db`,
  `Seafile-Repo-Token`, `seafile.conf`). Renaming the databases is planned
  separately in `docs/plans/db-rename.md`; the wire header is fixed by client
  compatibility and will not change.

## Authentication — two paths

### Path 1: sync clients (SeaDrive, Seafile Desktop)

```
Client  → POST /api2/auth-token/          {username, password}
        ← 40-char hex API token (no expiry — clients persist it)

Client  → POST /api2/repos/{id}/repo-tokens/   Authorization: Token <api-token>
        ← 41-char repo sync token, written to RepoUserToken

Client  → GET /repo/{id}/commit/HEAD
          Header: Seafile-Repo-Token: <repo-token>
Silo    → SELECT email FROM RepoUserToken WHERE repo_id=? AND token=?
Silo    → proceeds with sync
```

The sync path is a direct DB lookup via `repomgr.GetEmailByToken()`. API tokens
live in `fileserver/apitokenstore/`; the `Authorization: Token` middleware is
`fileserver/middleware/apitoken.go`.

### Path 2: management API (TUI, scripts)

```
Client  → POST /api/silo/v1/auth/login    {email, password}
        ← JWT session token (24h)

Client  → GET  /api/silo/v1/repos/{id}/entries/{path}   Authorization: Bearer <jwt>
        ← file bytes or a directory listing
```

Silo-specific, with no upstream equivalent. It serves bytes on the endpoint
itself rather than redirecting to a URL that carries a credential — see
`docs/capability-urls.md` for why.

## Replaced RPC calls

The three calls the Go fileserver used to make into the C server are now local:

| Was | Now |
|---|---|
| `seafile_web_query_access_token` | `sync.Map` with TTL in `fileserver/tokenstore/` |
| `seafile_get_decrypt_key` | `sync.Map` with TTL in `fileserver/keycache/` |
| `publish_event` | logrus, plus the WebSocket notification server in `fileserver/notif/` |

## Database

Two logical databases: `ccnet` (users, groups) and `seafile` (repos, shares,
tokens). SQLite is the default and the production path — embedded, WAL mode, one
serialized write connection and a read-only read pool. A MySQL path still exists
in `fileserver/option/` but is unused and slated for removal.

### ccnet DB
- `EmailUser` — users (id, email, passwd, is_staff, is_active, ctime, reference_id)
- `GroupUser` — group membership
- Groups table (configurable name)

### seafile DB
- `Repo` — repositories
- `Branch` — branch heads (repo_id, name, commit_id)
- `RepoOwner` — repo ownership
- `SharedRepo` — user-to-user shares
- `RepoGroup` — group shares
- `VirtualRepo` — virtual repo mappings (subdirs shared as repos)
- `RepoInfo` — repo metadata/settings
- `RepoUserToken` — per-user per-repo sync tokens (41 chars)
- `FileLocks` — file locking
- `InnerPubRepo` — publicly shared repos
- Various permission tables

Schema lives in `fileserver/dbutil/schema.go` and is applied at startup.

## Storage layout

Content-addressable filesystem under `{data-dir}/storage/`:

```
storage/
  blocks/{store-id}/{first-2-chars}/{remaining-38-chars}
  commits/{store-id}/{first-2-chars}/{remaining-38-chars}
  fs/{store-id}/{first-2-chars}/{remaining-38-chars}
```

Virtual repos share the storage of their origin repo, via the StoreID mapping —
which is why the path component is a store ID, not a repo ID.

## Data model

```
Repo (UUID)
  -> Branch (name, commit_id)
    -> Commit (SHA1, root_id, parent_id, creator, description)
      -> Dir / Seafile objects (content-addressable tree)
        -> Blocks (variable-size, Rabin CDC chunked, 4KB-8MB)
```

("Seafile object" here is the on-disk name for a file object — a list of block
IDs. It is the format's term, not a reference to the upstream server.)

## Go internals

### Handler pattern

```go
type appError struct {
    Error   error   // logged for 500s
    Message string  // HTTP response body
    Code    int     // HTTP status code
}
type appHandler func(http.ResponseWriter, *http.Request) *appError
```

### Packages

- `repomgr` — repo queries and writes
- `fsmgr` / `commitmgr` / `blockmgr` — object read/write with caching
- `objstore` — storage backend abstraction
- `share` — permission checking (owner, direct share, group share, virtual repo)
- `tokenstore` — in-memory access token store
- `keycache` — in-memory decrypt key cache
- `apitokenstore` — persistent API tokens for sync clients
- `authmgr` — password validation + JWT session tokens
- `middleware` — Bearer JWT and `Authorization: Token` auth, transaction naming
- `api` — management API handlers (`/api/silo/v1/`)
- `notif` — WebSocket notification server
- `option` — config loading (`seafile.conf`, env vars)
- `dbutil` — connection management, schema, query helpers
- `ratelimit`, `workerpool`, `metrics`, `diff`, `utils`

### Password hash formats

`ValidatePassword` dispatches on a known prefix first, then on length:

1. `PBKDF2SHA256$iter$salt_hex$hash_hex` — the format Silo writes. New hashes use
   600,000 iterations (`authmgr.PBKDF2Iterations`); older ones are read at
   whatever iteration count they record, and flagged for rehash on login.
2. SHA256 with a fixed salt (64-char hex) — legacy, flagged for upgrade.
3. Unsalted SHA1 (40-char hex) — very old, flagged for upgrade.

## Build

`go build ./cmd/silo` from the repo root. One binary, one `go.mod`: server, TUI
and CLI together.

## Client repositories

- **seadrive-fuse** (https://github.com/haiwen/seadrive-fuse) — FUSE filesystem,
  the actual sync engine
- **seadrive-gui** (https://github.com/haiwen/seadrive-gui) — Qt frontend for it
- **seafile-client** — desktop sync client (Qt)
