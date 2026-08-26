# Silo

A single-binary Go file sync server.

Status: pre-1.0 and young, but no longer reckless with your data. Object writes are fsynced before they are published, and every uploaded object is verified against its content hash on the way in, so a crash or a bad client can no longer silently corrupt a library. That said, it has not yet seen wide real-world use — run it, but keep an independent backup of anything you care about.

## What is Silo?

Silo started as a Go rewrite of the Seafile server architecture. Where upstream Seafile ships a C daemon (`seaf-server`), a Python/Django web layer (Seahub), and a process manager to tie them together, Silo collapses all of that into a single Go binary that speaks HTTP directly and talks directly to its database.

Early releases kept full wire compatibility with existing Seafile clients — Seafile desktop, mobile, and SeaDrive worked against Silo without modification. That compatibility was dropped on purpose in favour of Silo's own protocol and object format (`/api/silo/v1/`, content-defined chunking, per-library E2EE); a Seafile-family client can no longer talk to a current Silo server at all. See [`docs/target.md`](docs/target.md) for why, and [`docs/porter-brief.md`](docs/porter-brief.md) for the wire contract that replaced it.

Silo also ships with `silo`, a terminal UI built on [Bubble Tea](https://github.com/charmbracelet/bubbletea) for interactive file management without a browser.

## Architecture

```
  Client (TUI / porter-fuse / File Provider / …)
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
- One embedded SQLite database, `silo.db`, in the data directory: users, groups, libraries, shares and tokens together.
- Content-addressable object store under `{data-dir}/storage/` with separate trees for blocks, commits, and filesystem objects.

## Features

- Single-admin bootstrap via environment variables
- JWT session tokens for the management API
- Library create / list / delete
- File operations: upload, download, mkdir, rename, move, delete
- Directory listing via `/api/silo/v1/libraries/{id}/dir/`
- Content-defined chunking, SHA-256 content addressing, per-library end-to-end encryption — see [`docs/plans/store-v2.md`](docs/plans/store-v2.md)
- In-process notification server (WebSocket `/notification`) so a client gets push events on library updates instead of polling
- Embedded SQLite backend (WAL mode, read/write connection split)
- Auto-generated ephemeral JWT signing key if `SILO_JWT_SECRET` is unset

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

Alternatively, build the binary yourself. From the project root:

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

The server listens on `127.0.0.1:8082`. On first run it creates the SQLite database, the storage directory, and the admin user.

The credentials are optional. Started with an empty user table and no `SILO_ADMIN_PASSWORD`, the server creates `admin@silo.local` with a random password and prints it once, at warning level:

```
[WARNING] No users existed and no SILO_ADMIN_PASSWORD was set, so an admin account was created:
[WARNING]     email:    admin@silo.local
[WARNING]     password: meKFutKgmKnYF9rFesaJ
[WARNING] This password is stored hashed and will not be shown again. Save it now.
```

Save it — the password is stored hashed, so later runs cannot print it again. Setting `SILO_ADMIN_EMAIL` alone names the account and still generates the password; setting `SILO_ADMIN_PASSWORD` skips the whole thing. Once any user exists, this never fires again. No config file is required — Silo runs on compiled defaults plus environment variables. If you want to tweak low-level settings (quota defaults, cache limits, cluster options) you can pass a `silo.conf` with `-C /path/to/silo.conf`.

**Loopback is the default on purpose.** Silo speaks plaintext — TLS is the reverse proxy's job — and every credential it uses is a bearer token in a header. To reach it from other machines, see [Exposing the server](#exposing-the-server).

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

From the TUI: `n` to create a library, `enter` to open it, `u` to upload a local file or directory, `v` to move, `r` to rename, `x` to delete, `q` to quit. Lists scroll: `j`/`k` or the arrow keys move the cursor, `g`/`G` jump to the top and bottom, and page up/down move a screen at a time.

### Use the CLI

The same binary also exposes non-interactive subcommands for scripting:

```bash
silo libraries                          # list libraries (use --json for scripts)
silo library create "My library"       # prints the new library ID
silo ls <library-id> [/path]           # list a directory
silo put <library-id> ./file.txt /     # upload a file
silo put -r <library-id> ./photos /    # upload a directory, one commit
silo get <library-id> /file.txt ~/out  # download
silo mkdir <library-id> /sub
silo mv <library-id> /a.txt /sub/a.txt
silo rename <library-id> /sub/a.txt b.txt
silo rm <library-id> /sub/b.txt
silo library rm <library-id>
silo changes <library-id> <since-commit>   # what changed since a commit
```

And two that run against the data directory rather than the API:

```bash
silo gc                             # report what deleted libraries left on disk
silo gc -delete                     # reclaim it — stop the server first
silo backup-db <dir>                # snapshot the database; see Backups below
```

`silo gc` only ever touches libraries that have already been deleted; it does
not reclaim unreferenced history inside a library that still exists.

`silo help` prints the full subcommand list.

## Configuration

### Environment variables

| Variable | Purpose | Default |
|---|---|---|
| `SILO_DATA_DIR` | Data directory | `~/.local/share/silo` |
| `SILO_HOST` | Bind address | `127.0.0.1` (`0.0.0.0` in the Docker image) |
| `SILO_PORT` | Listen port | `8082` |
| `SILO_ADMIN_EMAIL` | Create admin user on startup | `admin@silo.local` when the user table is empty |
| `SILO_ADMIN_PASSWORD` | Admin password | generated and logged on first run |
| `SILO_JWT_SECRET` | JWT signing key | auto-generated (ephemeral) |
| `SILO_LOG_LEVEL` | Log level: debug, info, warn, error | — |
| `SILO_SYNC_OBJECT_WRITES` | fsync objects before publishing them | `true` |
| `SILO_VERIFY_FS_OBJECT_HASHES` | Check uploaded fs objects hash to their id (costs a decompress each; blocks and commits are always checked) | `true` |
| `SILO_ENABLE_NOTIFICATIONS` | Serve the WebSocket notification endpoint. `false` turns it off, and `notify-token` then answers `404` | `true` |
| `SILO_GROUP_TABLE_NAME` | Name of the groups table, for a database inherited from a deployment that renamed it | `Group` |
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

Env vars take precedence over `silo.conf`, so the same binary can be pointed at different deployments without editing files. `silo.conf` itself is optional — if you don't pass `-C`, Silo uses compiled defaults.

### CLI flags

| Flag | Purpose |
|---|---|
| `-d <dir>` | Data directory (default: `$SILO_DATA_DIR` or `~/.local/share/silo`) |
| `-b <addr>` | Bind address, `serve` only (default: `$SILO_HOST` or `127.0.0.1`) |
| `-C <file>` | Path to `silo.conf` (optional; only needed to override compiled defaults) |
| `-l <file>` | Log file path |
| `-P <file>` | PID file path |
| `-debug` | Log every HTTP request |

Flags beat environment variables, which beat `silo.conf`, which beats the
compiled defaults. So `-b` overrides `SILO_HOST` for one invocation without
disturbing whatever the service normally runs with.

## Backups

`cp silo.db` is not a backup — the database runs in WAL mode, so a plain
copy silently loses every write since the last checkpoint. Use:

```sh
silo backup-db /backup/silo/$(date +%F)                       # database, first
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

Silo binds `127.0.0.1` by default and speaks plaintext, and it has no HTTPS of
its own — terminating TLS is the reverse proxy's job, and a proxy does it
better: it reloads a renewed certificate without restarting the file server.
Every credential Silo uses is a bearer token in a header, so publishing that
port without a proxy in front hands those tokens to anyone on the path.

Leave Silo on loopback and terminate TLS in front of it:

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

Setting `SILO_HOST` to a non-loopback address logs a warning on every start,
since from there Silo cannot tell whether anything is terminating TLS for it.

## Revoking access

Every credential a client presents is a row in one table, so revoking is a
delete and it reaches every lane at once. A password change does not revoke
anything.

```sh
silo token list bob@example.com          # id, kind, label, expiry, last used
silo token revoke bob@example.com        # every credential, every device
silo token revoke bob@example.com <id>   # just one, by the id list prints
```

The label is what the client called itself when it logged in, and `last used`
is stamped at five-minute granularity — together they are what makes choosing
which one to revoke a decision rather than a guess.

Failed logins are throttled per client address and per account, so online
password guessing is bounded. **Behind a reverse proxy, set
`SILO_TRUST_PROXY_HEADERS=true`** — otherwise every client arrives as the
proxy's address and shares one bucket, and one attacker throttles everyone.

Revoking takes effect on the next request. There is no cache in front of the
credential table and no restart to perform — the row is read on every
authenticated call. The same is true of `silo user disable`, which stops every
credential an account holds without deleting any of them, so re-enabling
restores the user's devices rather than making everyone log in again.

## Client compatibility

Silo speaks its own protocol (`/api/silo/v1/`) rather than Seafile's. Seafile
and SeaDrive clients could talk to early Silo releases; that compatibility was
dropped on purpose and a Seafile-family client gets a 404 on every route
today — see [`docs/target.md`](docs/target.md).

Tested clients:

- **Silo TUI** (`cmd/silo`) — full CRUD and browse
- **porter-fuse** and the macOS File Provider client (Porter) — `entries`,
  `changes`, `notify-token` and the notification socket; see
  [`docs/porter-brief.md`](docs/porter-brief.md)

## What's not implemented

Silo is a lean rewrite focused on the sync path and a minimal management API. The following are **not** available:

- No user management API — the first user is created at startup (from `SILO_ADMIN_EMAIL`/`SILO_ADMIN_PASSWORD`, or generated and logged), and any further users need a direct database insert
- No library sharing API — nothing can *create* a share. The share tables are read
  and honoured: a row in `SharedLibrary` or `LibraryGroup` grants the access it
  describes, and `GET /api/silo/v1/libraries` lists directly shared libraries beside
  owned ones. Putting the row there means a direct database insert
- No group management API
- No `is_staff` / admin privilege check in the API layer — all authenticated users have equal permissions
- No web UI — use the TUI
- No trash / restore or history / revision endpoints
- No encrypted libraries — Silo cannot create them, and Seafile's format will not
  be supported. See [`docs/encryption.md`](docs/encryption.md) for why, and for
  the sketch of what replaces it

See [`docs/future-features.md`](docs/future-features.md) for the rough roadmap.

## Directory layout

```
fileserver/        Active Go server
  ├── api/         Management API handlers (/api/silo/v1/*)
  ├── account/     Accounts, addresses and external identities
  ├── credential/  The one credential store: mint, resolve, scope, revoke
  ├── authmgr/     Password validation and hashing
  ├── middleware/  Credential resolution and the permission ceiling
  ├── dbutil/      SQLite connection management and query helpers
  ├── share/       Permission checking
  └── ...
cmd/silo/          Bubble Tea TUI client
client/            HTTP client for the management API
internal/          TUI, CLI plumbing, observability, XDG paths
docs/              Architecture, protocol, backups, and the plans (docs/README.md)
test/              Ruby integration harness against a running server (test/README.md)
```

[`docs/README.md`](docs/README.md) is the index, and it marks each document as
describing the server *as it is*, as a *plan* for where it is going, or as a
*record* of something already decided. Several of the documents are plans, and
reading one as a description is the mistake it exists to prevent.

## Origin and license

Silo started as a fork of [haiwen/seafile-server](https://github.com/haiwen/seafile-server). Early releases reused the on-disk object format and the wire protocol to keep upstream clients working; both have since been replaced by Silo's own (see [`docs/target.md`](docs/target.md)), and nothing about compatibility with upstream depends on the database schema, which diverged early and was never part of the promise.

Licensed under **AGPLv3**, inherited from the upstream project. See [`NOTICE`](NOTICE) for attribution and [`LICENSE.txt`](LICENSE.txt) for the full license text.
