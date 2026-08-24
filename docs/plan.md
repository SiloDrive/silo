# Migration record and compatibility constraints

The plan this file used to hold — eliminate the C daemon and the Python web
layer, extend the Go fileserver into a standalone single-binary server, and
build a TUI on top of it — has landed. What follows is the record of what
replaced what, and the constraints that still bind every change.

## What landed

| Upstream dependency | Replacement |
|---|---|
| searpc `seafile_web_query_access_token` | `fileserver/tokenstore/` — `sync.Map` + TTL |
| searpc `seafile_get_decrypt_key` | `fileserver/keycache/` — `sync.Map` + TTL |
| searpc `publish_event` | logrus, plus `fileserver/notif/` over WebSocket |
| searpc client, `-p` flag, Unix socket | removed outright |
| Web-layer login | `POST /api/silo/v1/auth/login` → JWT, `fileserver/authmgr/` |
| Web-layer API tokens for sync clients | `POST /api2/auth-token/`, `fileserver/apitokenstore/` |
| 174 C RPC handlers for library operations | `/api/silo/v1/` handlers in `fileserver/api/` |
| Separate notification-server binary | in-process, `/notification` |
| Separate controller process | none needed — one process |

The management API is in `docs/protocol.md`; it is not a full replacement for
the 174 RPC handlers and was never meant to be. Sharing, user management, quota
administration, trash/restore and history remain unimplemented — see
`docs/future-features.md`.

## Standing constraints

These hold for anything added from here on.

- **Client compatibility.** SeaDrive and Seafile Desktop must keep working. The
  sync HTTP API (`/repo/{id}/commit`, `/repo/{id}/block`, `/repo/{id}/fs-id-list`,
  and the rest) does not change, and neither does the `Seafile-Repo-Token`
  header. New surface goes in the Silo lane (`/api/silo/v1/`), which is free to
  differ — see `docs/sync-design.md`.
- **Data compatibility.** Same table definitions, same on-disk object layout.
  No migrations of row contents. The one exception is where the tables *live*:
  `ccnet.db` and `seafile.db` became a single `silo.db`. No table was
  redefined, so the two files concatenate — a server that finds the old pair
  refuses to start and prints the commands, rather than silently creating an
  empty database beside them. See `docs/backup.md`.
- **Password compatibility.** Every hash format an existing install may hold has
  to validate: `PBKDF2SHA256$…` at whatever iteration count it records, legacy
  SHA256-with-fixed-salt, and unsalted SHA1. Old formats are flagged for rehash
  on successful login rather than rejected.
- **Encryption compatibility.** AES-CBC for library versions 1, 2 and 4, and
  AES-128-ECB for version 3, matching what clients already write
  (`fileserver/crypt.go`). See `docs/encryption.md`.
