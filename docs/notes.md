# Silo — Architecture Notes

## Overview

Silo is a single Go binary that serves file sync over HTTP and talks directly to
its database and object store. The store is content-addressable, like Git: libraries
point at branches, branches at commits, commits at a tree of directory and file
objects, and file objects at deduplicated chunks.

## Where it came from

Silo began as a fork of an AGPL server that ran four processes — a C daemon
holding all business logic behind 174+ RPC calls, a Python/Django web layer, a
Go fileserver for sync traffic, and a controller to supervise them. Silo
collapsed that into one process: the C daemon and the web layer are gone, the
notification server was ported into `fileserver/notif/`, and the RPC socket no
longer exists.

Nothing client-visible survives that inheritance. The sync wire protocol was
deleted in `5d4baa0`, the on-disk object layout was replaced by the store
format in [`spec/store-format.md`](spec/store-format.md), and the database
schema diverged early. The fork is why the package layout looks the way it
does, and it is no longer why anything else does.

What replaced what: the web layer's login became `POST auth/login` and a
`Credential` row (`fileserver/credential/`); the 174 RPC handlers became the
`/api/silo/v1/` handlers in `fileserver/api/`; the separate notification
server runs in-process on `/notification`; the RPC client, its socket and the
controller were removed outright. The one compatibility constraint that
survived the migration is password hashes — see § Password hash formats below.

One consequence worth knowing: anything the web layer authorized is simply
absent. The share-link routes (`/f/`, `/u/`, `/d/`) and the web
file-access route were removed rather than ported, because every one of them
authorized by calling out to a service Silo does not run. See
`docs/capability-urls.md`.

## Authentication — one path

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

## Database

One database, `<data-dir>/silo.db`. SQLite is the only engine — embedded, WAL
mode, one serialized write connection and a read-only read pool. Accounts are
in the same file as everything else: Silo runs one process, so it has one
database. There is no upgrade path from upstream's two-file pair —
`docs/backup.md` says why, and what a current server checks instead.

### Accounts
- `Account` / `AccountEmail` / `AccountIdentity` / `AccountPassword` — the
  identity split; `account_id` is the only user key on the live tables
- `Credential` — every secret a client presents; see Authentication above
- `Invite` — the address an invite was minted for, beside its credential row

### Libraries
- `Library` — repositories
- `Branch` — branch heads (library_id, name, commit_id)
- `LibraryOwner` — library ownership
- `LibraryGrant` — who may do what in a library: the one table `CheckPerm`
  reads, keyed by principal (`user:`, `link:`, `anon`)
- `VirtualLibrary` — virtual library mappings (subdirs shared as libraries)
- `LibraryInfo` — library metadata/settings

Schema lives in `fileserver/dbutil/schema.go`, and the migrations that take
an older database to it in `migrate.go` beside it. A fresh database loads the
schema; an existing one runs the migrations it has not recorded in
`SchemaMigration`, on `silo serve` or `silo migrate` — see
[`backup.md`](backup.md#schema-migrations).

## Storage layout

Content-addressable filesystem under `{data-dir}/storage/`:

```
storage/
  chunks/{store-id}/{first-2-chars}/{remaining-62-chars}
  objects/{store-id}/{first-2-chars}/{remaining-62-chars}
```

Two types, not three (`objstore.Types`): a chunk is content and an object is a
manifest, directory or commit. Ids are 64 hex characters — SHA-256 — so the
shard is two and the leaf sixty-two. This block named `blocks/`, `commits/` and
`fs/` at forty hex, which was the layout before store-v2 and has not been on
disk since.

Virtual libraries share the storage of their origin library, via the StoreID mapping —
which is why the path component is a store ID, not a library ID.

## Data model

```
Library (UUID)
  -> Branch (name, commit_id)
    -> Commit (SHA-256, root_id, parent_id, creator, description)
      -> Directory / manifest objects (content-addressable tree)
        -> Chunks (content-defined, `fastcdc-gear64/v1`)
```

A manifest is the file object: the list of chunk ids that reconstitutes one
file. [`spec/store-format.md`](spec/store-format.md) is normative for all of
it.

Chunking is `fastcdc-gear64/v1` with per-library parameters;
[`chunking.md`](chunking.md) is the decision and the measurements.

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
- `objmgr` — object read/write over the store format
- `objstore` — storage backend abstraction
- `share` — permission checking (owner, direct share, group share, virtual library)
- `credential` — the one credential store: minting, resolving, scoping, revoking
- `account` — accounts, addresses and external identities
- `authmgr` — password validation and hashing
- `middleware` — credential resolution and the permission ceiling, transaction naming
- `api` — management API handlers (`/api/silo/v1/`)
- `notif` — WebSocket notification server
- `option` — config loading (`silo.conf`, env vars)
- `dbutil` — connection management, schema, query helpers
- `setup` — claiming a fresh server with the single-use setup token
- `serversecret` — secrets that are the server's own and outlive the process
- `share` — library sharing and `CheckPerm`
- `ratelimit`, `utils`

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

## Clients

- **Silo TUI** (`cmd/silo`) — ships in this repository, same binary as the server
- **silo-drive** — a FUSE filesystem over `/api/silo/v1/`, and the macOS File
  Provider client; see
  [`protocol.md`](protocol.md#writing-a-client) § Writing a client for the wire contract both of them use
