# Silo

A single-binary Go file sync server, protocol-compatible with Seafile clients.

Status: pre-1.0 and young, but no longer reckless with your data. Object writes are fsynced before they are published, and every uploaded object is verified against its content hash on the way in, so a crash or a bad client can no longer silently corrupt a library. That said, it has not yet seen wide real-world use — run it, but keep an independent backup of anything you care about.

## What is Silo?

Silo is a Go rewrite of the Seafile server architecture. Where upstream Seafile ships a C daemon (`seaf-server`), a Python/Django web layer (Seahub), and a process manager to tie them together, Silo collapses all of that into a single Go binary that speaks HTTP directly and talks directly to its database.

It keeps full wire compatibility with existing Seafile clients: Seafile desktop, mobile, and SeaDrive all work against Silo without modification. The promise is the wire protocol — the database schema and the on-disk layout are Silo's own, and are free to change.

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

The image runs as uid 65532, not root, so a bind-mounted data directory has to
be writable by that uid:

```bash
mkdir -p /path/to/silo-data && chown -R 65532:65532 /path/to/silo-data
```

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

The server listens on `127.0.0.1:8082`. On first run it creates the ccnet and seafile SQLite databases, the storage directory, and the admin user.

The credentials are optional. Started with an empty user table and no `SILO_ADMIN_PASSWORD`, the server creates `admin@silo.local` with a random password and prints it once, at warning level:

```
[WARNING] No users existed and no SILO_ADMIN_PASSWORD was set, so an admin account was created:
[WARNING]     email:    admin@silo.local
[WARNING]     password: meKFutKgmKnYF9rFesaJ
[WARNING] This password is stored hashed and will not be shown again. Save it now.
```

Save it — the password is stored hashed, so later runs cannot print it again. Setting `SILO_ADMIN_EMAIL` alone names the account and still generates the password; setting `SILO_ADMIN_PASSWORD` skips the whole thing. Once any user exists, this never fires again. No config file is required — Silo runs on compiled defaults plus environment variables. If you want to tweak low-level settings (quota defaults, cache limits, cluster options) you can pass a `seafile.conf` with `-C /path/to/seafile.conf`.

**Loopback is the default on purpose.** Silo speaks plaintext unless given a certificate, and every credential it uses — sync tokens, API tokens, JWTs — is a bearer token in a header. To reach it from other machines, see [Exposing the server](#exposing-the-server).

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
| `SILO_HOST` | Bind address | `127.0.0.1` (`0.0.0.0` in the Docker image) |
| `SILO_TLS_CERT` / `SILO_TLS_KEY` | Serve HTTPS directly (set both) | — |
| `SILO_PORT` | Listen port | `8082` |
| `SILO_ADMIN_EMAIL` | Create admin user on startup | `admin@silo.local` when the user table is empty |
| `SILO_ADMIN_PASSWORD` | Admin password | generated and logged on first run |
| `SILO_JWT_SECRET` | JWT signing key | auto-generated (ephemeral) |
| `SILO_LOG_LEVEL` | Log level: debug, info, warn, error | — |
| `SILO_SYNC_OBJECT_WRITES` | fsync objects before publishing them | `true` |
| `SILO_VERIFY_FS_OBJECT_HASHES` | Check uploaded fs objects hash to their id (costs a decompress each; blocks and commits are always checked) | `true` |
| `SILO_AUTH_CACHE_TTL` | How long token/permission lookups are cached (`0` disables) | `5m` |
| `SILO_LOGIN_RATE_LIMIT` | Throttle failed logins per address and per account | `true` |
| `SILO_TRUST_PROXY_HEADERS` | Believe `X-Forwarded-For` / `X-Real-Ip` — **set this behind a reverse proxy** | `false` |
| `SILO_SENTRY_DSN` | Send errors, panics and request timings to Sentry, [Splat](https://github.com/dkam/splat) or GlitchTip (`SENTRY_DSN` also works) | — (send nothing) |
| `SILO_SENTRY_ENVIRONMENT` | Environment name on reported events | `production` |
| `SILO_SENTRY_RELEASE` | Release name on reported events | `silo@<version>` |
| `SILO_SENTRY_TRACES_SAMPLE_RATE` | Share of requests timed as performance transactions, `0` to `1` | `0.1` |
| `SILO_SENTRY_SERVER_NAME` | Name distinguishing this instance from others reporting to the same project | hostname |
| `SILO_URL` | Server base URL (client/TUI) | `http://localhost:8082` |
| `SILO_EMAIL` | Account email (client/TUI) | — |
| `SILO_PASSWORD` | Account password (client/TUI) | — |

Env vars take precedence over `seafile.conf`, so the same binary can be pointed at different deployments without editing files. `seafile.conf` itself is optional — if you don't pass `-C`, Silo uses compiled defaults.

### CLI flags

| Flag | Purpose |
|---|---|
| `-d <dir>` | Data directory (default: `$SILO_DATA_DIR` or `~/.local/share/silo`) |
| `-b <addr>` | Bind address, `serve` only (default: `$SILO_HOST` or `127.0.0.1`) |
| `-C <file>` | Path to `seafile.conf` (optional; only needed to override compiled defaults) |
| `-l <file>` | Log file path |
| `-P <file>` | PID file path |
| `-debug` | Log every HTTP request |

Flags beat environment variables, which beat `seafile.conf`, which beats the
compiled defaults. So `-b` overrides `SILO_HOST` for one invocation without
disturbing whatever the service normally runs with.

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

## Error reporting

The server can report its own errors, panics and request timings to any
Sentry-compatible receiver — [Splat](https://github.com/dkam/splat), GlitchTip,
or sentry.io:

```sh
SILO_SENTRY_DSN=https://<public-key>@splat.example.com/1 silo serve
```

Nothing is sent without a DSN, and nothing is added to the request path either:
with the variable unset there is no client, no logging hook and no middleware.
The TUI and CLI never report — they fail in front of the person who ran them.

`silo sentry-test` sends a test error and transaction and reports what the
receiver said about them, which is how you tell "nothing has gone wrong yet"
apart from a DSN pointing somewhere unreachable:

```
$ silo sentry-test
Reporting to http://splat.example.com:3304/silo
  envelope     accepted (HTTP 200)
```

See [`docs/error-reporting.md`](docs/error-reporting.md) for what gets sent, how
issues are grouped, and how to change the tracing sample rate.

## Exposing the server

Silo binds `127.0.0.1` by default and speaks plaintext. Every credential it
uses is a bearer token in a header, so publishing that port without TLS hands
those tokens to anyone on the path. Two supported ways to expose it:

**Behind a TLS reverse proxy** (recommended). Leave Silo on loopback and
terminate TLS in front of it:

```nginx
server {
    listen 443 ssl;
    server_name silo.example.com;
    ssl_certificate     /etc/letsencrypt/live/silo.example.com/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/silo.example.com/privkey.pem;

    # Uploads are whole files; do not let the proxy cap them.
    client_max_body_size 0;
    proxy_request_buffering off;

    location / {
        proxy_pass http://127.0.0.1:8082;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    }
}
```

Then set `SILO_TRUST_PROXY_HEADERS=true` so per-address rate limiting sees the
real client rather than the proxy.

**Serving TLS directly**, for a deployment with no proxy:

```bash
SILO_HOST=0.0.0.0 \
SILO_TLS_CERT=/etc/silo/fullchain.pem \
SILO_TLS_KEY=/etc/silo/privkey.pem \
./silo serve -d /path/to/silo-data
```

Setting `SILO_HOST` to a non-loopback address without either option logs a
warning on every start.

## Revoking access

Sync tokens have no expiry — Seafile clients persist them and treat them as
durable — so revoking one is the only way to cut a device off. A password
change does not.

```sh
silo token list bob@example.com      # sync tokens (per device) and API tokens
silo token revoke bob@example.com    # every token: all devices, all libraries
silo token revoke bob@example.com <token>   # just one device
```

Failed logins are throttled per client address and per account, so online
password guessing is bounded. **Behind a reverse proxy, set
`SILO_TRUST_PROXY_HEADERS=true`** — otherwise every client arrives as the
proxy's address and shares one bucket, and one attacker throttles everyone.

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

- No user management API — the first user is created at startup (from `SILO_ADMIN_EMAIL`/`SILO_ADMIN_PASSWORD`, or generated and logged), and any further users need a direct database insert
- No repo sharing API — users can only access repos they own (share tables exist in the schema but have no HTTP endpoints)
- No group management API
- No `is_staff` / admin privilege check in the API layer — all authenticated users have equal permissions
- No web UI — use the TUI or a Seafile client
- No trash / restore or history / revision endpoints
- No encrypted libraries — Silo cannot create them, and Seafile's format will not
  be supported. See [`docs/encryption.md`](docs/encryption.md) for why, and for
  the sketch of what replaces it

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
client/            HTTP client for the management API
internal/          TUI, CLI plumbing, observability, XDG paths
docs/              Architecture notes, error reporting, backups, future features
test/              Ruby integration harness against a running server (test/README.md)
```

## Origin and license

Silo started as a fork of [haiwen/seafile-server](https://github.com/haiwen/seafile-server). It reuses the on-disk format, database schema, and wire protocol so that upstream clients keep working.

Licensed under **AGPLv3**, inherited from the upstream project. See [`NOTICE`](NOTICE) for attribution and [`LICENSE.txt`](LICENSE.txt) for the full license text.
