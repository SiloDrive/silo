# Seafile Server - Architecture Notes

## Overview

Seafile server is a multi-process, multi-language file sync and storage system.
It's a content-addressable object store (like Git) with repos, commits, directory
trees, and deduplicated blocks.

## Components

### Seahub (Python/Django) - The Web UI (port 8000)
- Serves the browser-facing web interface and REST API
- Has zero direct access to file storage
- All backend operations go through searpc to the C server
- Generates short-lived access tokens for file downloads/uploads
- Redirects browsers/clients to the Go fileserver with those tokens
- Desktop clients authenticate here first: `POST /api2/auth-token/`

### seaf-server (C, ~71K LOC) - The Brain
- All business logic: repo CRUD, sharing, permissions, quotas, encryption, trash
- Exposes 174+ RPC functions over a Unix socket using libsearpc
- Seahub is completely dependent on it
- Built on GLib/GObject type system throughout
- Manages in-memory caches: access tokens, decrypt keys, event queues
- Reads/writes directly to DB and filesystem
- Generates repo sync tokens and writes them to `RepoUserToken` table

### Go fileserver (~14.5K LOC) - The Muscle (port 8082)
- HTTP server for file sync traffic (block upload/download, commits, fs objects)
- Desktop/mobile clients talk directly to it for sync
- Reads/writes DB and filesystem independently
- **Dependencies on C server** (now removed — replaced with local Go implementations):
  - Access token validation
  - Decrypt key lookup
  - Event publishing
- **Dependencies on Seahub** (still present in web access path):
  - `GET /api/v2.1/internal/repos/{id}/check-access/` — web file access auth
  - `GET /api/v2.1/internal/check-share-link-access/` — public share link auth
  - `GET /api/v2.1/internal/user-list/` — display names for merge conflicts

### notification-server (Go, ~1K LOC) — now integrated into Silo
- WebSocket server for real-time repo change notifications
- JWT-authenticated
- Ported to `fileserver/notif/`; standalone binary removed

### seafile-controller
- Process manager that launches and monitors the other services

## Authentication — Three Different Paths

### Path 1: Web browser access (Seahub-mediated)
```
Browser → POST /api2/auth-token/ on Seahub (port 8000)
       ← API token (long-lived)

Browser → Seahub "download file" (using API token)
Seahub  → searpc → C server: create short-lived access token
Seahub  → Browser: 302 redirect to http://host:8082/files/{token}/file.pdf

Browser → GET /files/{token}/file.pdf on Go fileserver (port 8082)
Go      → validates token (was RPC to C, now local in-memory)
Go      → streams file
```
ALSO: Go fileserver calls back to Seahub to verify web access:
```
Go → http://127.0.0.1:8000/api/v2.1/internal/repos/{id}/check-access/
```
This callback is JWT-signed and only used for browser-originated requests (cookies,
share links). Not used by desktop sync.

### Path 2: Desktop client / SeaDrive sync (DB-based tokens)
```
Client  → POST /api2/auth-token/ on Seahub (port 8000)
        ← API token

Client  → Seahub API: "give me a sync token for repo X"
Seahub  → searpc → C server: seafile_generate_repo_token()
C server writes to RepoUserToken table in MySQL
        ← 41-char repo token

Client  → GET /repo/{id}/commit/HEAD on Go fileserver (port 8082)
          Header: Seafile-Repo-Token: {token}
Go      → SELECT email FROM RepoUserToken WHERE repo_id=? AND token=?
Go      → proceeds with sync
```
Key insight: the sync path NEVER uses searpc. Token validation is a direct
DB lookup via repomgr.GetEmailByToken(). The only Seahub/C involvement is
in the initial token creation.

### Path 3: New management API (our addition, Go-only)
```
Client  → POST /api/silo/v1/auth/login on Go fileserver (port 8082)
          body: {"email": "...", "password": "..."}
Go      → validates against EmailUser table directly
        ← JWT session token (24hr)

Client  → POST /api/silo/v1/access-tokens (with Bearer JWT)
        ← short-lived file access token

Client  → GET /files/{token}/file.pdf
Go      → validates token locally (in-memory)
Go      → streams file
```
This is a NEW path that doesn't exist in upstream Seafile. It replaces Seahub
for new clients (TUI, scripts, etc).

### Missing piece for desktop client compatibility
Desktop clients currently get repo sync tokens via Seahub → C server →
RepoUserToken table. With Seahub gone, we need:
```
POST /api/silo/v1/repos/{id}/token  (with Bearer JWT)
→ generates 41-char token, writes to RepoUserToken table
← {"token": "..."}
```
Then desktop clients can use their existing Seafile-Repo-Token sync protocol
unchanged.

## The Three Former RPC Calls (Go → C, now eliminated)

### 1. `seafile_web_query_access_token` → replaced by tokenstore package
- Was: in-memory hash table in C server, queried over Unix socket
- Now: Go `sync.Map` with TTL in `fileserver/tokenstore/`

### 2. `seafile_get_decrypt_key` → replaced by keycache package
- Was: in-memory key cache in C server
- Now: Go `sync.Map` with TTL in `fileserver/keycache/`

### 3. `publish_event` → replaced by logging (for now)
- Was: in-memory async queue, consumed by Seahub polling
- Now: logged via logrus, can add WebSocket/SSE later

## Go → Seahub HTTP callbacks (still present, TODO)

These are used only for web browser access, not desktop sync:

1. `check-access` (fileop.go:331) — verifies browser cookie/token access to files
2. `check-share-link-access` (fileop.go:3843) — validates public share links
3. `user-list` (merge.go:414) — gets display names for merge conflict UI

These will need to be replaced with direct DB queries when we fully remove Seahub.
Low priority since the TUI won't use these paths.

## Database

Two logical databases (`ccnet` and `seafile`). SQLite is the default and is
production-ready — embedded, WAL mode, a single serialized write connection and
a read-only read pool. MySQL is also supported for existing deployments:

### ccnet DB
- `EmailUser` - users (id, email, passwd, is_staff, is_active, ctime, reference_id)
- `GroupUser` - group membership
- Groups table (configurable name)

### seafile DB
- `Repo` - repositories
- `Branch` - branch heads (repo_id, name, commit_id)
- `RepoOwner` - repo ownership
- `SharedRepo` - user-to-user shares
- `RepoGroup` - group shares
- `VirtualRepo` - virtual repo mappings (subdirs shared as repos)
- `RepoInfo` - repo metadata/settings
- `RepoUserToken` - per-user per-repo sync tokens (41 chars, stored in DB)
- `FileLocks` - file locking
- `InnerPubRepo` - publicly shared repos
- Various permission tables

## Storage Layout

Content-addressable filesystem under `seafile-data/storage/`:
```
storage/
  blocks/{repo-id}/{first-2-chars}/{remaining-38-chars}
  commits/{repo-id}/{first-2-chars}/{remaining-38-chars}
  fs/{repo-id}/{first-2-chars}/{remaining-38-chars}
```

Virtual repos share the storage of their origin repo (via StoreID mapping).

## Data Model

```
Repo (UUID)
  -> Branch (name, commit_id)
    -> Commit (SHA1, root_id, parent_id, creator, description)
      -> Dir / Seafile objects (content-addressable tree)
        -> Blocks (variable-size, Rabin CDC chunked, 4KB-8MB)
```

## Go Fileserver Internals

### Handler Pattern
```go
type appError struct {
    Error   error   // logged for 500s
    Message string  // HTTP response body
    Code    int     // HTTP status code
}
type appHandler func(http.ResponseWriter, *http.Request) *appError
```

### Packages (after our changes)
- `repomgr` - repo queries (mostly read-only, some write ops)
- `fsmgr` - filesystem object read/write with caching
- `blockmgr` - block storage
- `commitmgr` - commit objects
- `share` - permission checking (owner, direct share, group share, virtual repo)
- `objstore` - storage backend abstraction
- `tokenstore` - NEW: in-memory access token store (replaced searpc)
- `keycache` - NEW: in-memory decrypt key cache (replaced searpc)
- `authmgr` - NEW: password validation + JWT session tokens
- `middleware` - NEW: Bearer token auth middleware
- `api` - NEW: management API handlers (/api/silo/v1/)
- `option` - config loading (seafile.conf, env vars)
- `utils` - JWT generation (already using golang-jwt/jwt/v5)
- `metrics` - prometheus metrics

### Existing Dependencies (go.mod)
- gorilla/mux - HTTP routing
- go-sql-driver/mysql - database
- golang-jwt/jwt/v5 - JWT tokens
- sirupsen/logrus - logging
- go-redis/redis - Redis client
- dgraph-io/ristretto - in-memory cache
- golang.org/x/crypto - NEW: pbkdf2 for password validation

### Password Hashing Formats (must support all for login)
1. `PBKDF2SHA256$iter$salt_hex$hash_hex` - current standard
2. Argon2id - modern alternative (TODO: not yet implemented in authmgr)
3. SHA256 with fixed salt (64-char hex) - legacy, flag for upgrade
4. Plain SHA1 (40-char hex) - very old, flag for upgrade

## Build System

GNU Autotools for C code. Standard `go build ./cmd/silo` from the repo root for the Go unified binary (server + TUI + CLI, single go.mod at the root).

## Client Repositories

- **seadrive-gui** (https://github.com/haiwen/seadrive-gui) - Qt GUI for SeaDrive
- **seadrive-fuse** (https://github.com/haiwen/seadrive-fuse) - FUSE filesystem,
  the actual sync engine. seadrive-gui is just a frontend for it.
- **seafile-client** - Desktop sync client (Qt)
- **seahub** (https://github.com/haiwen/seahub) - Python/Django web frontend
