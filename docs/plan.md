# Migration record and compatibility constraints

> **Stale as of 2026-08-24. Do not read the constraints below as binding.**
>
> This file said "these hold for anything added from here on", and then three
> of the four stopped holding without anything here changing. An agent reading
> it to learn what binds its work would have been bound to a sync lane that no
> longer exists, an on-disk format that has been replaced, and a source file
> that has been deleted.
>
> Each false claim is annotated below with the commit that ended it. What
> *should* bind new work is not decided here and is deliberately not invented:
> `storage.md` is the document that knows, and the one live
> constraint is marked as such.
>
> This is a **record** in the sense `docs/README.md` uses the word — kept
> because the reasoning explains why the code looks the way it does, not
> because it describes the server.

The plan this file used to hold — eliminate the C daemon and the Python web
layer, extend the Go fileserver into a standalone single-binary server, and
build a TUI on top of it — has landed. What follows is the record of what
replaced what, and the constraints that bound every change *at the time it was
written*.

## What landed

| Upstream dependency | Replacement |
|---|---|
| RPC: mint a web access token | ~~`fileserver/tokenstore/`~~ — **gone**; deleted with the routes that redeemed its tokens |
| RPC: fetch a library's decrypt key | ~~`fileserver/keycache/`~~ — **gone**; the package was deleted with the legacy lanes (5d4baa0) |
| RPC: publish an event | logrus, plus `fileserver/notif/` over WebSocket |
| RPC client, `-p` flag, Unix socket | removed outright |
| Web-layer login | `POST /api/silo/v1/auth/login` → a `Credential` row, `fileserver/credential/` |
| Web-layer API tokens for sync clients | ~~the legacy token endpoint~~ — that lane is **gone** (5d4baa0), and `apitokenstore` with it; every credential is a `Credential` row now ([`auth.md`](auth.md)) |
| 174 C RPC handlers for library operations | `/api/silo/v1/` handlers in `fileserver/api/` |
| Separate notification-server binary | in-process, `/notification` |
| Separate controller process | none needed — one process |

The management API is in `docs/protocol.md`; it is not a full replacement for
the 174 RPC handlers and was never meant to be. Sharing, user management, quota
administration, trash/restore and history remain unimplemented — see
`docs/roadmap.md`.

## Standing constraints

**Three of these four no longer hold.** They are kept, struck through, with the
commit that ended each, because deleting them would lose the reason the code
was shaped around them for as long as it was.

- ~~**Client compatibility.**~~ **Ended by 5d4baa0.** The legacy sync lanes were
  deleted outright; `fileserver/server.go` registers no such route. No upstream
  client is required to keep working, and the request header the original text
  named is one this server no longer reads. Original text:
  <br>**Client compatibility.** The upstream desktop and virtual-drive clients
  must keep working. The sync HTTP API (`/repo/{id}/commit`, `/repo/{id}/block`,
  `/repo/{id}/fs-id-list`, and the rest) does not change, and neither does the
  library-token header. New surface goes in the Silo lane (`/api/silo/v1/`),
  which is free to differ — see `docs/sync-design.md`.
- ~~**Data compatibility.**~~ **Ended twice.** The object layout was replaced by
  the current store format — SHA-256 ids, `fastcdc-gear64/v1` content-defined chunking, binary
  manifests (c9d5b92, 2efc3dd) — and the two-database guard this bullet
  describes was deleted in d644f8f, so no server refuses to start on anything.
  Table definitions have since been renamed wholesale (dee67bd). The standing
  licence is the opposite of this bullet: there are no installs, so there is no
  migration to preserve. Original text:
  <br>**Data compatibility.** Same table definitions, same on-disk object layout.
  No migrations of row contents. The one exception is where the tables *live*:
  the two inherited SQLite files became a single `silo.db`. No table was
  redefined, so the two files concatenate — a server that finds the old pair
  refuses to start and prints the commands, rather than silently creating an
  empty database beside them. See `docs/backup.md`.
- **Password compatibility.** — **the one constraint here that still holds.**
  Verified against `fileserver/authmgr/authmgr.go`, which still dispatches all
  three formats. Every hash format an existing install may hold has
  to validate: `PBKDF2SHA256$…` at whatever iteration count it records, legacy
  SHA256-with-fixed-salt, and unsalted SHA1. Old formats are flagged for rehash
  on successful login rather than rejected.
- ~~**Encryption compatibility.**~~ **Ended by 5d4baa0.** `fileserver/crypt.go`
  does not exist. Encryption at rest is the store's per-library E2EE, which is a
  different scheme with different guarantees — see `storage.md`,
  not the text below. Original text refers to the inherited scheme:
  <br>**Encryption compatibility.** AES-CBC for library versions 1, 2 and 4, and
  AES-128-ECB for version 3, matching what clients already write
  (`fileserver/crypt.go`). See `docs/encryption.md`.
