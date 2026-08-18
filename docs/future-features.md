# Future Features

Rough roadmap for Silo. Ordered loosely by priority, but nothing here is committed.

## User Management

Currently users can only be created via env vars on startup
(`SEAFILE_ADMIN_EMAIL` / `SEAFILE_ADMIN_PASSWORD`) or by writing directly to
the `EmailUser` table. We need a proper admin-gated API.

### Endpoints

- `POST   /api/silo/v1/users`              — create user
- `GET    /api/silo/v1/users`              — list users (paginated)
- `GET    /api/silo/v1/users/{email}`      — show a single user
- `PUT    /api/silo/v1/users/{email}`      — update password, active flag, is_staff
- `DELETE /api/silo/v1/users/{email}`      — delete user (and their owned repos? or
                                         refuse if non-empty?)
- `POST   /api/silo/v1/users/{email}/password` — admin password reset
- `POST   /api/silo/v1/auth/change-password`   — self-service password change

### Prerequisites

- **Admin check helper**: query `is_staff` from `EmailUser` for the authed user.
- **Admin middleware** (or per-handler guard): gate `/api/silo/v1/users/*` behind
  `is_staff = 1`. Cache the flag on the JWT claim so we don't hit the DB on
  every request.
- Decide: should `is_staff` also grant all-repo visibility in `CheckPerm` and
  `ListReposHandler`? Upstream conflates "admin" with "can see everything";
  we may want a cleaner split.
- Decide: soft-delete vs hard-delete. Upstream keeps orphaned repos around
  after a user is removed; we should pick a deterministic policy.

### Prevent Repo Creation For Non-Admin Users

Seafile has no built-in way to stop a regular user from creating libraries.
For "consumer" deployments where only the admin curates libraries and other
users sync shared ones, we need a flag (per-user or global) that makes
`CreateRepoHandler` return 403 for non-staff.

Likely shape: a `role` column on `EmailUser` (`admin` / `user` / `guest`) and
a config key `allow_user_create_repo = true|false`. Guest == can't create, can
only access shared repos.

## Repo Sharing

Share tables (`SharedRepo`, `SharedRepoV2`, `RepoGroup`) already exist in the
schema — they're just not exposed over HTTP. Once user management lands,
sharing should be straightforward.

### Endpoints

- `POST   /api/silo/v1/repos/{id}/shares`              — share to a user
- `GET    /api/silo/v1/repos/{id}/shares`              — list shares on a repo
- `DELETE /api/silo/v1/repos/{id}/shares/{email}`      — revoke a user share
- `POST   /api/silo/v1/repos/{id}/group-shares`        — share to a group
- `GET    /api/silo/v1/repos/{id}/group-shares`
- `DELETE /api/silo/v1/repos/{id}/group-shares/{gid}`
- `GET    /api/silo/v1/shared-with-me`                 — repos shared *to* the caller

### Permissions

Seafile supports three permission levels: `r` (read), `rw` (read-write), and
`admin`. `CheckPerm` already knows how to resolve these from the share tables
— the missing piece is the write path plus exposing the result in
`ListReposHandler` (so a shared repo shows up alongside owned ones).

### Public / link shares

Later: public download links (`/d/{token}/`) and upload links. These need a
new token type in `tokenstore`, a generated short-code, and optional password
protection. Lower priority than user-to-user sharing.

## Groups

Silo already has a `Group` table inherited from ccnet but no way to manage it.
To make group sharing useful we need:

- `POST   /api/silo/v1/groups`                          — create group
- `GET    /api/silo/v1/groups`                          — list groups the caller is in
- `GET    /api/silo/v1/groups/{id}`
- `DELETE /api/silo/v1/groups/{id}`                     — owner only
- `POST   /api/silo/v1/groups/{id}/members`             — add member
- `DELETE /api/silo/v1/groups/{id}/members/{email}`     — remove member
- `PUT    /api/silo/v1/groups/{id}/members/{email}`     — promote/demote

Group membership should participate in `CheckPerm` via the existing
`RepoGroup` table.

## File Locking

Seafile supports per-file advisory locks so two clients editing the same
document don't clobber each other. SeaDrive and Seafile Desktop both honour
the lock state when present. Tables `FileLocks` and `FileLocksTimestamp` exist
in the schema.

### Endpoints

- `PUT    /api2/repos/{id}/file/?p=/path&operation=lock`
- `PUT    /api2/repos/{id}/file/?p=/path&operation=unlock`
- `GET    /api2/repos/{id}/locked-files/`

### Behaviour

- Lock owner is the authenticated user; a lock blocks writes from any other
  user until released or expired.
- Two lock types upstream: **manual** (no expiry, explicit unlock) and
  **auto** (short TTL, refreshed on each save). Start with manual, add auto
  later.
- Enforcement point: `put-file` / `update-file` / `commit` upload path in
  `fileop.go` — reject with 403 if the target path is locked by someone else.
- Surface lock state in directory listings (`is_locked`, `lock_owner`,
  `lock_time`) so clients render a padlock icon.

### Ties into the notification server

Locks are the clearest case for pushing events over the notification
WebSocket instead of relying on poll-then-list. When a lock is taken or
released, publish a `file-lock-changed` event (same frame shape upstream
uses, so SeaDrive handles it without changes) to every subscriber of the
repo. Clients repaint the padlock icon immediately rather than waiting for
the next directory refresh.

This means file locking should land *after* the notification server is
working — or at least the server-side publish hook should be added at the
same time so we don't ship a half-real-time feature.

## Trash, History, Revisions

Commits are already content-addressable and immutable, so "history" is mostly
a matter of exposing what's already on disk. Upstream endpoints to port:

- `GET  /api2/repos/{id}/history/`                  — commit log for the repo
- `GET  /api2/repos/{id}/file/revision/?p=/path`    — revisions of a single file
- `POST /api2/repos/{id}/file/revert/`              — revert a file to a commit
- `GET  /api2/repos/{id}/trash/`                    — deleted-but-reachable entries
- `POST /api2/repos/{id}/trash/restore/`            — restore from trash
- `DELETE /api2/repos/{id}/trash/`                  — empty trash

Trash is interesting because Silo currently has no GC — "deleted" files are
still reachable via old commits forever. A real trash needs a retention
window and a GC pass that prunes commits older than the window.

## Garbage Collection

Related to trash: there's no block GC. If you delete a 10 GB file, the blocks
stay on disk indefinitely under `{data-dir}/storage/blocks/`. Need a
`silo gc` subcommand (or background job) that:

1. Walks reachable commits per repo (`commitmgr.Load` from each repo's head,
   following parents), collecting the live fs-object and block set via
   `fsmgr`.
2. Scans `storage/blocks/{store_id}/` and removes anything not in the live
   set. Same pass for `storage/fs/` and `storage/commits/` for entries older
   than the head chain.
3. Respects a retention window so trash/history still works — a block
   referenced by any commit within the window is live.

Should run per repo (one repo can be GC'd without locking the whole server)
and must coordinate with in-flight uploads so a block that's written but not
yet committed isn't reaped. Upstream does this via a "fs-mgr freeze" flag;
we'd do something similar.

## Quota

Quota is **per user**, not per repo — a user's cap applies to the total size
of every repo they own. The logic is in `fileserver/quota.go` but isn't
enforced on the upload path today, and there's no API to set a user's cap.

### How it works

- `UserQuota(user, quota)` table holds the cap in bytes. No row → use
  `option.DefaultQuota` (settable via `seafile.conf`, `fileserver/option/`).
- `-2` (`InfiniteQuota`, `quota.go:14`) means unlimited.
- `getUserUsage` (`quota.go:83-105`) sums `RepoSize.size` across every repo
  the user owns via a join on `RepoOwner`, **excluding virtual repos**
  (`AND v.repo_id IS NULL`) so subdirectory-shares don't double-count.
- `checkQuota(repoID, delta)` (`quota.go:17-62`) is called with the
  projected upload size. For a virtual repo, it first resolves to the
  origin repo and charges the origin's owner — so uploading to a shared
  subdirectory counts against whoever created the parent library, not the
  uploader.
- `RepoSize` is maintained asynchronously by `size_sched.go` → the
  `updateSizePool` worker, which recomputes after each commit. Quota
  decisions are therefore eventually consistent; a fast series of uploads
  can momentarily overshoot.

### What's missing

- **Enforcement wiring**: `checkQuota` is defined but the upload path in
  `fileop.go` doesn't consistently short-circuit on a quota violation with
  the right HTTP status. Needs to return `443 QUOTA_FULL` (Seafile-specific
  code already present in `http_code.go`) before the block write, not
  after.
- **Admin API**: no endpoints to read or set quota. Wanted:
  - `GET /api/silo/v1/users/{email}/quota` — returns `{quota, usage}`
  - `PUT /api/silo/v1/users/{email}/quota` — set cap (admin only)
  - `GET /api/silo/v1/account/quota` — self lookup, no admin needed
- **Default quota config**: surface `option.DefaultQuota` as an env var
  (`SEAFILE_DEFAULT_QUOTA`) so it's settable without editing
  `seafile.conf`.
- **Per-repo quota** (extension, not upstream-compatible): there's no
  `RepoQuota` table in the schema. For "this shared team library can grow
  to 500 GB regardless of who owns it" we'd need to add one and have
  `checkQuota` consult it alongside the user cap, taking the smaller of
  the two.
- **Grace behaviour**: decide what happens *at* the cap — upstream refuses
  any further writes outright. A soft-limit / hard-limit split would be
  friendlier but is more work.

## Encrypted Libraries

Dropped as written. This section used to propose teaching the TUI Seafile's
key derivation so it could browse encrypted repos. We are not adopting that
format: 1000 PBKDF2 iterations, a published offline-crackable password
verifier, one key and IV for the whole library forever, and no authentication
on the ciphertext.

Silo has never been able to create an encrypted library, so there is no
installed base to stay compatible with. The replacement — X25519 identity keys,
a per-library content key wrapped per member, chunked AEAD — is sketched in
[`docs/encryption.md`](encryption.md), along with the list of things not to
build if we want to keep it reachable.

Note the server-side decrypt-key cache (`keycache/`) is **not** working
support: nothing ever calls `SetKey`, so `parseCryptKey` can only return its
400. It reads as a feature and is dead code.

## Web UI

Not planned in the short term. Upstream's Seahub is a Django app and we
explicitly walked away from that. If a web UI happens, it should be a small
SPA (HTMX or similar) served from the same Go binary, talking to `/api/silo/v1/`.

A web UI is the one thing that brings back a consumer which cannot set an
`Authorization` header — a `<video>` src, an `<img>` thumbnail, a download
link. That is the point at which signed URLs become worth building, and
[`capability-urls.md`](capability-urls.md) records what they should look like
(stateless and signed) versus the stateful one-time token the Seafile lane
still uses.

## Admin / Ops

- **Structured logs**: switch from stdlib `log` to `slog` with a JSON handler
  behind a flag. Useful for shipping to Loki/ES.
- **Metrics**: `fileserver/metrics/` already exists; expand coverage to
  include upload/download throughput, commit rate, and auth failures. Expose
  on `/metrics` in Prometheus format.
- **Healthcheck**: a real `/healthz` that pings both SQLite handles and
  returns 503 if either is wedged.
- ~~**Backup story**~~: done — `silo backup-db` snapshots both SQLite files
  with `VACUUM INTO`, and [`backup.md`](backup.md) documents the ordering the
  object-store copy has to follow.

## Protocol / Client Compat Gaps

Things SeaDrive or Seafile Desktop call that Silo currently stubs or 404s.
Track them here as they're discovered:

- `/api2/events/` — activity feed (returns empty list today)
- `/api2/starred-files/`
- `/api2/repos/{id}/commits/` — richer commit metadata than `/history/`
- Avatar endpoints (`/api2/avatars/…`) — cosmetic, clients tolerate 404

## Credentials

How passwords and tokens are stored today, why the token columns are the weak
link rather than the password column, and what has to be issued before any
per-request-authenticated protocol can work: [`auth.md`](auth.md).

## Additional Protocol Frontends

Separate question from the compat gaps above: not "what does a Seafile client
expect", but "what *other* protocols could front the same store". WebDAV, S3,
SFTP and friends, with the architectural constraints that rule each in or out,
are surveyed in [`protocol-frontends.md`](protocol-frontends.md).

## A Native Silo Client

The CLI and TUI drive the management API one file at a time, which is not
sync: `silo put` takes a single file, always uploads the whole thing, and has
no way to ask the server what it already holds. Three tiers close that gap —
recursive put, dedup-aware upload via `check-blocks`, and a full headless sync
agent — in [`native-client.md`](native-client.md).

The middle tier is the interesting one, and is much cheaper than it sounds:
Silo chunks at fixed 8 MiB offsets rather than content-defined boundaries, so
any client can compute the server's block ids with stdlib SHA-1 and a loop.

## Search (Filename Index)

Motivated by macOS's File Provider search pushdown (`NSFileProviderSearching`,
macOS 26+ — see [`porter-brief.md`](porter-brief.md) /
[`macos-fileprovider-plan.md`](macos-fileprovider-plan.md)), but useful on its
own independent of any one client. Today Silo has **no search of any kind**:
files live as content-addressed `SeafDirent`/`SeafDir` objects inside per-repo
commit trees (`fileserver/fsmgr/fsmgr.go`), not rows in a table, so there is
nothing to `WHERE filename LIKE` against — finding a file means the *client*
walking the current tree itself. Seafile Pro's answer is an external
Elasticsearch-backed indexer (`seafevents`); that's commercial-only and not a
dependency we want to force on a single self-hosted binary.

### Endpoints

- `GET /api/silo/v1/search?q=...` — account-wide, filtered to repos the
  caller can see (owned + shared).
- Maybe `GET /api/silo/v1/repos/{id}/search?q=...` for a scoped variant,
  lower priority.

### Design

- **SQLite FTS5, not Elasticsearch.** Silo already has sqlite via `dbutil`;
  FTS5 gives prefix/substring matching on filenames with no new operational
  dependency. Filename-only for v1 — content search means extracting and
  indexing blob text, a much bigger lift, and not needed to match what
  Spotlight/Finder actually ask for.
- **Populating the index is the hard part, not querying it.** There's no
  existing "list every filename in a repo" call to seed from — needs a full
  tree walk once, then incremental maintenance per new commit. Reuse the
  commit-diffing machinery already backing the enumerator's change feed
  (`fileserver/api/changes.go`, `fileserver/diff/diff.go`) instead of
  re-walking whole trees on every write.
- **Permission-filtered at query time**, not via separate per-grantee
  indexes — join against the same visibility check `CheckPerm` /
  `ListReposHandler` already do, so a share revoked mid-session can't leak
  stale results.
- Ranking: prefix/substring plus maybe recency. No need for real relevance
  scoring at this scale.

### Open questions

- One FTS table across all repos (join-filtered per query) vs one per repo.
  All-repo is simpler to query; per-repo is simpler to rebuild in isolation
  and to scope alongside quota/GC.
- Whether virtual repos (subdirectory shares) need special-casing the way
  quota's `checkQuota` does for them.
- Backfill cost on existing large repos — first build is a full tree walk,
  should run as a background job (cf. `size_sched.go`'s worker) rather than
  inline on first query.

Not being built now — parked here until a client actually needs it.

## Compression

zlib appears in exactly one package (`fsmgr`) and covers metadata only —
blocks and commits are stored raw. Because an fs object's id is the SHA-1 of
its *uncompressed* JSON, the compression format is not part of object
identity, so it can be changed without rewriting a single id, and mixed
formats can coexist by sniffing magic bytes on read.

The larger prize is compressing blocks, which is possible for the same reason
one level down, but pays nothing on media workloads. Measure first.
Analysis in [`compression.md`](compression.md).

## Non-Goals

Things we're explicitly *not* going to build, to keep scope honest:

- **Federation / multi-server sync** — one binary, one node.
- **Plugin system** — if you want custom behaviour, fork.
- **LDAP / SAML / OIDC** — local password auth only. (A reverse proxy doing
  header-auth is acceptable; Silo will trust a configurable header.)
- **Mobile apps** — use the upstream Seafile mobile clients, they speak our
  protocol.
