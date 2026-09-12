# Silo

A single-binary Go file sync server.

Status: pre-1.0 and young, but no longer reckless with your data. Object writes are fsynced before they are published, and every uploaded object is verified against its content hash on the way in, so a crash or a bad client can no longer silently corrupt a library. That said, it has not yet seen wide real-world use — run it, but keep an independent backup of anything you care about.

## What is Silo?

Silo is one Go binary. It speaks HTTP directly and talks directly to its database — no daemon to supervise, no web application beside it, no process manager tying the two together.

It began as a rewrite of an older server built that way, and for its first releases it kept wire compatibility with that server's clients. That compatibility was dropped on purpose in favour of Silo's own protocol and object format — `/api/silo/v1/`, content-defined chunking, per-library E2EE — and no client of the old kind can talk to a current Silo server. See [`docs/target.md`](docs/target.md) for why, and [`docs/protocol.md`](docs/protocol.md) for the wire contract that replaced it.

Silo also ships with `silo`, a terminal UI built on [Bubble Tea](https://github.com/charmbracelet/bubbletea) for interactive file management without a browser.

## Architecture

```
  Client (TUI / silo-drive / …)
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
                 chunks / objects)
```

- One process. No RPC, no Python, no controller.
- One embedded SQLite database, `silo.db`, in the data directory: users, groups, libraries, shares and tokens together.
- Content-addressable object store under `{data-dir}/storage/` with two trees: `chunks/` for content and `objects/` for the manifests, directories and commits that describe it.
- One writer per data directory, enforced: the server takes an `flock` on `{data-dir}/silo.lock` for its lifetime, and a second server — or a `gc -delete` — refuses to start and names the pid holding it.

## Features

- Single-admin bootstrap via environment variables
- Revocable credential rows for the management API — no JWT anywhere
- Library create / list / delete
- File operations: upload, download, mkdir, rename, move, delete
- Directory listing via `/api/silo/v1/libraries/{id}/dir/`
- Content-defined chunking, SHA-256 content addressing, per-library end-to-end encryption — see [`docs/storage.md`](docs/storage.md)
- In-process notification server (WebSocket `/notification`) so a client gets push events on library updates instead of polling
- Embedded SQLite backend (WAL mode, read/write connection split)

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
docker run -d --name silo -p 8082:8082 -v /path/to/silo-data:/data silo
docker logs silo          # the setup token is here
```

Multi-arch images (linux/amd64, linux/arm64) are also published to GitHub Container Registry on each release:

```bash
docker run -d --name silo -p 8082:8082 -v /path/to/silo-data:/data \
  ghcr.io/dkam/silo:latest
docker logs silo          # the setup token is here
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
```

Then `docker compose up -d`, and `docker compose logs silo` for the setup
token. No credentials go in this file: there is nothing to put there, which is
the point — a bootstrap password is needed for exactly one boot and would sit
in your compose file, your shell history and `docker inspect` output for the
life of the deployment.

### Run the server

```bash
./silo serve -d /path/to/silo-data
```

The server listens on `127.0.0.1:8082`. On first run it creates the SQLite database and the storage directory. It does **not** create an account — you choose that, and the setup token is how the server knows it is you asking:

```
[WARNING] This server has no accounts. Create the first one with this setup token:
[WARNING]     setup token: SILO-685Y-9Y0R-8ANQ-CRWX
[WARNING] Run `silo tui`, enter the email and password you want, and paste it in.
[WARNING] It stops working the moment an account exists. `silo setup-token` reprints it.
```

Run `silo tui`, type the email and password you want, paste the token, and you have an account. Nothing about the address is fixed by the server, and no password ever passes through a log line or an environment variable.

The token is the same on every boot until it is claimed, so scrolling past it costs nothing — `silo setup-token` prints it again without a restart:

```bash
./silo setup-token -d /path/to/silo-data
docker exec silo silo setup-token          # or, under Docker
```

It stops working the instant the first account exists, and `silo setup-token` says so rather than printing a token that would be refused.

Two things worth knowing. The token is printed to the log at every boot until it is claimed, so if you ship logs off the box, it goes with them — claim it before exposing the port, and treat an unclaimed server's logs as sensitive. And a server reachable from off the machine advertises `setup_required` on `GET /api/silo/v1/server-info`, which is what lets the TUI show a setup screen instead of a login form that could not work; the token is what stands between that and anyone else claiming it first.

No config file is required — Silo runs on compiled defaults plus environment variables. If you want to tweak low-level settings (quota defaults, cache limits, cluster options) you can pass a `silo.conf` with `-C /path/to/silo.conf`.

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
export SILO_EMAIL=admin@example.com
export SILO_PASSWORD=changeme
```

With that loaded, `./silo serve -d /tmp/silo-data` and `./silo tui` both pick up the same host, port, and credentials — no flags needed. The account itself is made once, through the setup screen; these two variables only say who to log in as afterwards.

From the TUI: `n` to create a library, `enter` to open it, `u` to upload a local file or directory, `v` to move, `r` to rename, `x` to delete, `q` to quit. Lists scroll: `j`/`k` or the arrow keys move the cursor, `g`/`G` jump to the top and bottom, and page up/down move a screen at a time.

`a` opens the account menu, which is where you change your password. It asks for the current one as well as the new one — the request is already authenticated, but a credential you handed to a device must not be able to turn itself into the account. Changing it signs your other sessions out and leaves mounted devices alone; the TUI you did it from signs itself back in. An operator who needs to reset a password nobody holds any more uses `silo user passwd <email>`, which revokes everything.

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
silo gc -delete                     # reclaim it — refuses while a server holds the data dir
silo backup-db <dir>                # snapshot the database; see Backups below
silo migrate [-n]                   # bring the database to this build's schema; -n only lists what would run
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
| `SILO_LOG_LEVEL` | Log level: debug, info, warn, error | — |
| `SILO_SYNC_OBJECT_WRITES` | fsync objects before publishing them | `true` |
| `SILO_VERIFY_FS_OBJECT_HASHES` | Check uploaded fs objects hash to their id (costs a decompress each; chunks and commits are always checked) | `true` |
| `SILO_ENABLE_NOTIFICATIONS` | Serve the WebSocket notification endpoint. `false` turns it off, and `/notification` then answers `404` | `true` |
| `SILO_LOGIN_RATE_LIMIT` | Throttle failed logins per address and per account | `true` |
| `SILO_TRUST_PROXY_HEADERS` | Believe `X-Forwarded-For` / `X-Real-Ip` — **set this behind a reverse proxy** | `false` |
| `SILO_TRUSTED_PROXY_HOPS` | How many proxies stand in front, deciding which `X-Forwarded-For` entry is the client's. Raise it only if something sits in front of your proxy: too high reads an entry the client wrote | `1` |
| `SILO_ALLOW_USER_CREATE_LIBRARY` | Let accounts with the `user` role create libraries (`[libraries] allow_user_create_library`). `false` is the curated install: only an admin makes libraries, and everybody else syncs what they are shared. Never applies to an admin, and never promotes a guest | `true` |
| `SILO_SENTRY_DSN` | Send errors, panics and request timings to Sentry, [Splat](https://github.com/dkam/splat) or GlitchTip (`SENTRY_DSN` also works) | — (send nothing) |
| `SILO_SENTRY_ENVIRONMENT` | Environment name on reported events | `production` |
| `SILO_SENTRY_RELEASE` | Release name on reported events | `silo@<version>` |
| `SILO_SENTRY_TRACES_SAMPLE_RATE` | Share of requests timed as performance transactions, `0` to `1` | `0.1` |
| `SILO_SENTRY_SERVER_NAME` | Name distinguishing this instance from others reporting to the same project | hostname |
| `SILO_PACK_SIZE` | Size at which a pack is sealed | `512mb` |
| `SILO_PACK_MAX_AGE` | How long a pack may stay open before it is sealed regardless of size — see [configuration.md](docs/configuration.md) | `5m` |
| `SILO_PACK_SWEEP` | How often the age sealer looks | `30s` |
| `SILO_ORPHAN_AGE` | How long an unreferenced object must sit before `gc -orphans` considers it | `24h` |
| `SILO_COMPACT_THRESHOLD` | Dead fraction at which a pack is worth rewriting | `0.5` |
| `SILO_COMPACT_MIN_AGE` | How long ago a pack must have sealed before `gc -compact` touches it | `24h` |
| `SILO_COMPACT_BUDGET` | Live bytes one compaction run may copy (`0`: no cap) | `0` |
| `SILO_COMMIT_ATTEMPTS` | Head compare-and-swap retries per commit | `5` |
| `SILO_LIBRARY_FAULT_INTERVAL` | How long a library fault is held before being logged again | `5m` |
| `SILO_URL` | Server base URL (client/TUI) | `http://localhost:8082` |
| `SILO_EMAIL` | Account email (client/TUI) | — |
| `SILO_PASSWORD` | Account password (client/TUI) | — |

Env vars take precedence over `silo.conf`, so the same binary can be pointed at different deployments without editing files. `silo.conf` itself is optional — if you don't pass `-C`, Silo uses compiled defaults.

The storage knobs above are the `[storage]` section of `silo.conf`; [docs/configuration.md](docs/configuration.md) documents them and what to weigh when changing them.

### CLI flags

| Flag | Purpose |
|---|---|
| `-d <dir>` | Data directory (default: `$SILO_DATA_DIR` or `~/.local/share/silo`) |
| `-b <addr>` | Bind address, `serve` only — a host (`0.0.0.0`) or a host and port (`0.0.0.0:8003`). Default: `$SILO_HOST:$SILO_PORT` or `127.0.0.1:8082` |
| `-C <file>` | Path to `silo.conf` (optional; only needed to override compiled defaults) |
| `-l <file>` | Log file path |
| `-P <file>` | PID file path |
| `-debug` | Log every HTTP request |

Flags beat environment variables, which beat `silo.conf`, which beats the
compiled defaults. So `-b` overrides `SILO_HOST` for one invocation without
disturbing whatever the service normally runs with.

`-b` sets the port only when you give it one: `-b 0.0.0.0` moves the host and
leaves `SILO_PORT` alone, `-b 0.0.0.0:8003` moves both. An IPv6 host needs
brackets to carry a port — `-b [::1]:8082` — because a bare `::1` is all host.

A bare `-b :8003` is refused rather than guessed at. Most Go servers read it as
every interface, but silo binds loopback by default, so it reads just as
naturally as "same host, new port" — and quietly picking the first meaning
would put a server on the network nobody asked to put there. Say which you
meant: `-b 127.0.0.1:8003` or `-b 0.0.0.0:8003`. To change only the port and
leave the host wherever it was, use `SILO_PORT`.

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

    location /debug/pprof {
        return 404;
    }

    location / {
        proxy_pass http://127.0.0.1:8082;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    }
}
```

Then set `SILO_TRUST_PROXY_HEADERS=true` so per-address rate limiting sees the
real client rather than the proxy. Without it every client arrives as the
proxy and shares one bucket, so ten failures from anywhere throttle everybody
— and the warning that says so is attached to the TLS check, which is silent
on the loopback bind that is the correct configuration. Nothing will remind
you.

If anything sits in front of your proxy — Cloudflare, say — set
`SILO_TRUSTED_PROXY_HOPS` to the number of proxies. `X-Forwarded-For` is a
path and Silo counts from the end of it; the entries at the front are the ones
the client wrote.

Setting `SILO_HOST` to a non-loopback address logs a warning on every start,
since from there Silo cannot tell whether anything is terminating TLS for it.
Under Docker, publish the port as `127.0.0.1:8082:8082` — a bare `8082:8082`
puts Silo on every host interface, in front of whatever firewall rule you
wrote.

[`docs/deployment.md`](docs/deployment.md) is the full checklist for the day
the port opens: a Caddyfile, blocking `/debug/pprof`, and claiming the setup
token before any log shipping starts.

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

Silo speaks one protocol, `/api/silo/v1/`, and nothing else. The legacy sync
lanes that early releases carried for compatibility with an older server's
clients were deleted in 0.5.0, and every route they used now answers 404 — see
[`docs/target.md`](docs/target.md).

Tested clients:

- **Silo TUI** (`cmd/silo`) — full CRUD and browse
- **silo-drive**, as a FUSE mount and as the macOS File Provider client —
  `server-info`, `auth/login`, `libraries`, `account/usage`, `entries`,
  `changes`, the chunk surface and the notification socket; see
  [`docs/protocol.md`](docs/protocol.md#writing-a-client) § Writing a client

## What's not implemented

Silo is a lean rewrite focused on the sync path and a minimal management API. The following are **not** available:

- No user management API — the first account is created by claiming the setup token, and any further account needs `silo user add` on the host
- No groups — a library is shared to one account at a time, through
  `POST /api/silo/v1/libraries/{id}/shares`
- No `is_staff` / admin privilege check in the API layer — all authenticated users have equal permissions
- No web UI — use the TUI
- No trash / restore or history / revision endpoints
- Encrypted libraries are new and thin: a client can create one and fetch its
  wrapped content key, but sharing one, rotating its key and recovering it are
  not built. See [`docs/encryption.md`](docs/encryption.md) for the scheme and
  [`docs/storage.md`](docs/storage.md) for what is left

See [`docs/roadmap.md`](docs/roadmap.md) for the rough roadmap.

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

Silo began as a fork of an existing AGPL server. Early releases reused its
on-disk object format and its wire protocol so that its clients kept working;
both have since been replaced by Silo's own (see
[`docs/target.md`](docs/target.md)), and the database schema diverged early and
was never part of that promise.

Licensed under **AGPLv3**, inherited from the upstream project. [`NOTICE`](NOTICE)
names it, as the licence requires; [`LICENSE.txt`](LICENSE.txt) has the full
text and the additional permission that travels with it.
