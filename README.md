# Silo

A single-binary Go file sync server, protocol-compatible with Seafile clients.

This code is experimental.  Assume dataloss is likely.

## What is Silo?

Silo is a Go rewrite of the Seafile server architecture. Where upstream Seafile ships a C daemon (`seaf-server`), a Python/Django web layer (Seahub), and a process manager to tie them together, Silo collapses all of that into a single Go binary that speaks HTTP directly and talks directly to its database.

It keeps full wire compatibility with existing Seafile clients. The sync protocol, block storage layout, and database schema are unchanged, so Seafile desktop, mobile, and SeaDrive clients work against Silo without modification.

Silo also ships with `silo`, a terminal UI built on [Bubble Tea](https://github.com/charmbracelet/bubbletea) for interactive file management without a browser.

## Architecture

```
  Client (TUI / SeaDrive / Seafile Desktop)
              │
              │ HTTP :8082
              ▼
         ┌──────────┐
         │   Silo   │   single Go binary
         └────┬─────┘
              │
        ┌─────┴─────┐
        ▼           ▼
     SQLite     Filesystem
                (content-addressable
                blocks / commits / fs)
```

- One process. No RPC, no Python, no controller.
- Two logical databases — `ccnet` (users, groups) and `seafile` (repos, shares, tokens). Both live in embedded SQLite files in the data directory.
- Content-addressable object store under `{data-dir}/storage/` with separate trees for blocks, commits, and filesystem objects.

## Features

- Single-admin bootstrap via environment variables
- JWT session tokens for the management API
- Persistent API tokens for SeaDrive compatibility
- Repo create / list / delete
- File operations: upload, download, mkdir, rename, move, delete
- Directory listing via `/api/silo/v1/repos/{id}/dir/`
- Full Seafile sync protocol for desktop and SeaDrive clients
- In-process notification server (WebSocket `/notification`) so SeaDrive / Seafile Desktop get push events on repo updates instead of polling
- Embedded SQLite backend (WAL mode, read/write connection split)
- Auto-generated ephemeral JWT signing key if `SILO_JWT_SECRET` is unset
- Seafile-compatible endpoints: `/api2/auth-token/`, `/api2/repos/`, `/api2/repos/{id}/repo-tokens/`, `/api2/repos/{id}/download-info/`, plus the full sync path

## Quick start

### Install

On macOS or Linux via Homebrew:

```bash
brew install dkam/silo/silo
```

Homebrew auto-taps `dkam/homebrew-silo` on first install, so no separate `brew tap` step is needed.

### Download a release

Prebuilt binaries for macOS and Linux are published on the [releases page](https://github.com/dkam/silo/releases).

### Build from source

Alternatively, build the binary yourself. From the repo root:

```bash
go build ./cmd/silo
```

This produces a `silo` executable (~20 MB) that contains the file server daemon, the interactive TUI, and the scripting CLI.

### Docker

Build and run directly from GitHub — no clone needed:

```bash
docker build -t silo https://github.com/dkam/silo.git
docker run -d -p 8082:8082 -v /path/to/silo-data:/data \
  -e SILO_ADMIN_EMAIL=admin@example.com \
  -e SILO_ADMIN_PASSWORD=changeme \
  silo
```

Multi-arch images (linux/amd64, linux/arm64) are also published to GitHub Container Registry on each release:

```bash
docker run -d -p 8082:8082 -v /path/to/silo-data:/data \
  -e SILO_ADMIN_EMAIL=admin@example.com \
  -e SILO_ADMIN_PASSWORD=changeme \
  ghcr.io/dkam/silo:latest
```

Or with Docker Compose — save as `docker-compose.yml`:

```yaml
services:
  silo:
    image: ghcr.io/dkam/silo:latest
    ports:
      - "8082:8082"
    volumes:
      - /path/to/silo-data:/data
    environment:
      SILO_ADMIN_EMAIL: admin@example.com
      SILO_ADMIN_PASSWORD: changeme
```

Then `docker compose up -d`.

### Run the server

```bash
SILO_ADMIN_EMAIL=admin@example.com \
SILO_ADMIN_PASSWORD=changeme \
./silo serve -d /path/to/silo-data
```

The server listens on `:8082`. On first run it creates the ccnet and seafile SQLite databases, the storage directory, and the admin user. No config file is required — Silo runs on compiled defaults plus environment variables. If you want to tweak low-level settings (quota defaults, cache limits, cluster options) you can pass a `seafile.conf` with `-C /path/to/seafile.conf`.

### Run the TUI client

In another terminal:

```bash
./silo tui http://localhost:8082
```

The server URL can also come from `SILO_URL`. If unset, it defaults to `http://localhost:8082`.

Auto-login kicks in if both `SILO_EMAIL` and `SILO_PASSWORD` are set.

A typical `.envrc` for local development with [direnv](https://direnv.net/):

```bash
export SILO_HOST=127.0.0.1
export SILO_PORT=8082
export SILO_ADMIN_EMAIL=admin@example.com
export SILO_ADMIN_PASSWORD=changeme
export SILO_EMAIL=admin@example.com
export SILO_PASSWORD=changeme
```

With that loaded, `./silo serve -d /tmp/silo-data` and `./silo tui` both pick up the same host, port, and credentials — no flags needed.

From the TUI: `n` to create a library, `enter` to open it, `u` to upload a local file, `v` to move, `r` to rename, `x` to delete, `q` to quit.

### Use the CLI

The same binary also exposes non-interactive subcommands for scripting:

```bash
silo repos                          # list libraries (use --json for scripts)
silo repo create "My library"       # prints the new repo ID
silo ls <repo-id> [/path]           # list a directory
silo put <repo-id> ./file.txt /     # upload
silo get <repo-id> /file.txt ~/out  # download
silo mkdir <repo-id> /sub
silo mv <repo-id> /a.txt /sub/a.txt
silo rename <repo-id> /sub/a.txt b.txt
silo rm <repo-id> /sub/b.txt
silo repo rm <repo-id>
```

`silo help` prints the full subcommand list.

## Configuration

### Environment variables

| Variable | Purpose | Default |
|---|---|---|
| `SILO_DATA_DIR` | Data directory | `~/.local/share/silo` |
| `SILO_HOST` | Bind address | `0.0.0.0` |
| `SILO_PORT` | Listen port | `8082` |
| `SILO_ADMIN_EMAIL` | Create admin user on startup | — |
| `SILO_ADMIN_PASSWORD` | Admin password | — |
| `SILO_JWT_SECRET` | JWT signing key | auto-generated (ephemeral) |
| `SILO_LOG_LEVEL` | Log level: debug, info, warn, error | — |
| `SILO_SYNC_OBJECT_WRITES` | fsync objects before publishing them | `true` |
| `SILO_AUTH_CACHE_TTL` | How long token/permission lookups are cached (`0` disables) | `5m` |
| `SILO_URL` | Server base URL (client/TUI) | `http://localhost:8082` |
| `SILO_EMAIL` | Account email (client/TUI) | — |
| `SILO_PASSWORD` | Account password (client/TUI) | — |

Env vars take precedence over `seafile.conf`, so the same binary can be pointed at different deployments without editing files. `seafile.conf` itself is optional — if you don't pass `-C`, Silo uses compiled defaults.

### CLI flags

| Flag | Purpose |
|---|---|
| `-d <dir>` | Data directory (default: `$SILO_DATA_DIR` or `~/.local/share/silo`) |
| `-C <file>` | Path to `seafile.conf` (optional; only needed to override compiled defaults) |
| `-l <file>` | Log file path |
| `-P <file>` | PID file path |
| `-debug` | Log every HTTP request |

## Backups

`cp seafile.db` is not a backup — the databases run in WAL mode, so a plain
copy silently loses every write since the last checkpoint. Use:

```sh
silo backup-db /backup/silo/$(date +%F)                       # databases, first
rsync -a "$SILO_DATA_DIR"/storage/ /backup/silo/$(date +%F)/storage/   # objects, second
```

Both steps are safe with the server running, and the order matters. See
[`docs/backup.md`](docs/backup.md) for why, plus cold backup, restore and
verification.

## Revoking access

Sync tokens have no expiry — Seafile clients persist them and treat them as
durable — so revoking one is the only way to cut a device off. A password
change does not.

```sh
silo token list bob@example.com      # sync tokens (per device) and API tokens
silo token revoke bob@example.com    # every token: all devices, all libraries
silo token revoke bob@example.com <token>   # just one device
```

Revoking through the CLI takes effect within `SILO_AUTH_CACHE_TTL` (5 minutes
by default), since a separate process cannot purge the running server's auth
cache. Set it to `0` to check the database on every request, or restart the
server to apply a revocation at once.

## Client compatibility

Silo has been tested with:

- **Silo TUI** (`cmd/silo`) — full CRUD and browse
- **SeaDrive** 3.0.21 — sync and file operations via `/api2/` endpoints
- **Seafile Desktop** — sync via the standard repo token protocol

The JWT management API (`/api/silo/v1/`) is new and Silo-specific; existing Seafile clients don't know about it.

## What's not implemented

Silo is a lean rewrite focused on the sync path and a minimal management API. The following upstream Seafile features are **not** available:

- No user management API — users are created via `SILO_ADMIN_EMAIL`/`SILO_ADMIN_PASSWORD` or direct database insert
- No repo sharing API — users can only access repos they own (share tables exist in the schema but have no HTTP endpoints)
- No group management API
- No `is_staff` / admin privilege check in the API layer — all authenticated users have equal permissions
- No web UI — use the TUI or a Seafile client
- No trash / restore or history / revision endpoints
- No encrypted-repo support in the TUI (sync clients can still use encrypted repos)

See [`docs/future-features.md`](docs/future-features.md) for the rough roadmap.

## Directory layout

```
fileserver/        Active Go server
  ├── api/         Management API handlers (/api/silo/v1/*)
  ├── authmgr/     Password validation + JWT
  ├── dbutil/      SQLite connection management and query helpers
  ├── share/       Permission checking
  ├── tokenstore/  In-memory access token cache
  ├── keycache/    In-memory decrypt key cache
  └── ...
cmd/silo/          Bubble Tea TUI client
docs/              Architecture notes, migration plan, future features
server/            Legacy C seaf-server code — not built, kept for reference
python/            Legacy Seahub code — not used
```

## Origin and license

Silo started as a fork of [haiwen/seafile-server](https://github.com/haiwen/seafile-server). It reuses the on-disk format, database schema, and wire protocol so that upstream clients keep working.

Licensed under **AGPLv3**, inherited from the upstream project. See [`NOTICE`](NOTICE) for attribution and [`LICENSE.txt`](LICENSE.txt) for the full license text.
