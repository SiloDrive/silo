# Silo — Architecture Notes

## Overview

Silo is a single Go binary that serves file sync over HTTP and talks directly to
its database and object store. The store is content-addressable, like Git: libraries
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
- Names on disk and on the wire still read "seafile" in places
  (`Seafile-Repo-Token`, `seafile.conf`). The database is now `silo.db`; the
  wire header is fixed by client compatibility and will not change.

## Authentication — one path

> The two-path section that stood here described `/api2/auth-token/`,
> `Seafile-Repo-Token` and `LibraryUserToken`. All three were deleted with the
> sync lanes (`5d4baa0`); it was describing a server that no longer existed.

```
Client  → POST /api/silo/v1/auth/login    {email, password}
        ← {"token": "silo_session_<id>_<secret><check>"}   (24h, absolute)

Client  → GET  /api/silo/v1/libraries/{id}/entries/{path}
          Authorization: Bearer silo_session_...
        ← file bytes or a directory listing
```

One table (`Credential`), one verification path (`credential.Resolve`), one
middleware (`middleware.RequireCredential`). The row stores `SHA-256(secret)`
and is found by the public id embedded in the token, so the secret never
reaches a query. Revoking a row stops it on the next request — there is no
cache in front of it. `silo token list|revoke <email>` is the operator side.

Serving bytes on the endpoint itself, rather than redirecting to a URL that
carries a credential, is deliberate — see `docs/capability-urls.md`.

## Replaced RPC calls

The three calls the Go fileserver used to make into the C server are now local:

| Was | Now |
|---|---|
| `seafile_web_query_access_token` | ~~`fileserver/tokenstore/`~~ — **gone**; the capability URLs it minted for went with the legacy lanes, and a signed URL is what replaces them ([`capability-urls.md`](capability-urls.md)) |
| `seafile_get_decrypt_key` | `sync.Map` with TTL in `fileserver/keycache/` |
| `publish_event` | logrus, plus the WebSocket notification server in `fileserver/notif/` |

## Database

One database, `<data-dir>/silo.db`. SQLite is the only engine — embedded, WAL
mode, one serialized write connection and a read-only read pool. Users and
groups used to live in a second file (`ccnet.db`) because upstream ran two
server processes; Silo runs one, so they are one database. `docs/backup.md`
has the upgrade recipe for a data directory that still has the old pair.

### Users and groups
- `Account` / `AccountEmail` / `AccountIdentity` / `AccountPassword` — the
  identity split; `account_id` is the only user key on the live tables
- `Credential` — every secret a client presents; see Authentication above
- `GroupUser` — group membership
- Groups table (configurable name)

### Repositories
- `Library` — repositories
- `Branch` — branch heads (library_id, name, commit_id)
- `LibraryOwner` — library ownership
- `SharedLibrary` — user-to-user shares
- `LibraryGroup` — group shares
- `VirtualLibrary` — virtual library mappings (subdirs shared as libraries)
- `LibraryInfo` — library metadata/settings
- `FileLocks` — file locking
- `InnerPubLibrary` — publicly shared libraries
- Various permission tables

Schema lives in `fileserver/dbutil/schema.go` and is applied at startup.
`CreateSiloTables` stamps the database with `SchemaVersion` (SQLite's
`PRAGMA user_version`) once the schema has applied cleanly, and refuses to
start against a database already stamped with a different version — see
[`backup.md`](backup.md#schema-version).

## Storage layout

Content-addressable filesystem under `{data-dir}/storage/`:

```
storage/
  blocks/{store-id}/{first-2-chars}/{remaining-38-chars}
  commits/{store-id}/{first-2-chars}/{remaining-38-chars}
  fs/{store-id}/{first-2-chars}/{remaining-38-chars}
```

Virtual libraries share the storage of their origin library, via the StoreID mapping —
which is why the path component is a store ID, not a library ID.

## Data model

```
Library (UUID)
  -> Branch (name, commit_id)
    -> Commit (SHA1, root_id, parent_id, creator, description)
      -> Dir / Seafile objects (content-addressable tree)
        -> Blocks (variable-size, Rabin CDC chunked, 4KB-8MB)
```

("Seafile object" here is the on-disk name for a file object — a list of block
IDs. It is the format's term, not a reference to the upstream server.)

**The block line is unverified and contradicts the rest of the docs.**
`native-client.md` and `protocol.md` both state that chunking is at fixed 8 MiB
offsets, which is certainly true of everything *Silo* writes — `chunkFile`
(`fileop.go:2684`) reads `FixedBlockSize` bytes from a computed offset. "Rabin
CDC chunked, 4KB-8MB" would be a claim about what upstream *clients* produce,
and nobody has checked it. Silo's read path does assume variable sizes
(`doFileRange`, `fileop.go:355`, stats every block rather than dividing), which
is suggestive but not proof. Which document is wrong matters: see the test in
[`chunking.md`](chunking.md).

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

- `libmgr` — library queries and writes
- `fsmgr` / `commitmgr` / `blockmgr` — object read/write with caching
- `objstore` — storage backend abstraction
- `share` — permission checking (owner, direct share, group share, virtual library)
- `keycache` — in-memory decrypt key cache
- `credential` — the one credential store: minting, resolving, scoping, revoking
- `account` — accounts, addresses and external identities
- `authmgr` — password validation and hashing
- `middleware` — credential resolution and the permission ceiling, transaction naming
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

`go build ./cmd/silo` from the project root. One binary, one `go.mod`: server, TUI
and CLI together.

## Client repositories

- **seadrive-fuse** (https://github.com/haiwen/seadrive-fuse) — FUSE filesystem,
  the actual sync engine
- **seadrive-gui** (https://github.com/haiwen/seadrive-gui) — Qt frontend for it
- **seafile-client** — desktop sync client (Qt)
