# Future Features

Rough roadmap for Silo. Ordered loosely by priority, but nothing here is committed.

## User Management

Users can only be created at startup — `SILO_ADMIN_EMAIL` / `SILO_ADMIN_PASSWORD`,
or the bootstrap admin the server mints and logs when the user table is empty —
or by writing to the database directly. There is one account and no lifecycle.
We need a proper admin-gated API.

[`auth.md`](auth.md) puts a CLI (`silo user add | disable | passwd`) ahead of
this API, on the grounds that the identity split makes it possible and it is
what the API would be built on anyway.

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

- **Admin check helper and middleware**: read `is_staff` for the authenticated
  user and gate `/api/silo/v1/users/*` behind it. Planned in
  [`plans/admin-check.md`](plans/admin-check.md), which settles one thing worth
  not re-arguing: the flag is read per request rather than cached on the token,
  so revoking admin takes effect immediately instead of at the next expiry.
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

The share tables — `SharedRepo` for user-to-user, `RepoGroup` for groups,
`InnerPubRepo` for server-wide — already exist and are already read: a row in
any of them grants what it says, through `share.CheckPerm`. What is missing is
every way to *write* one. (There is no `SharedRepoV2`; an earlier draft of this
section named one that has never been in Silo's schema.) Once user management
lands, sharing should be straightforward.

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
the lock state when present. Tables `FileLocks` and `FileLockTimestamp` exist
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

Trash is interesting because a file deleted from a library it stays in is
still reachable via old commits forever — `silo gc` only reclaims libraries
that were deleted whole. A real trash needs a retention window and a GC pass
that prunes commits older than the window; see Garbage Collection below.

## Garbage Collection

Half of this landed. `silo gc` reclaims the object-store directories of
libraries that have already been *deleted* — `DeleteRepo` records the id in
`GarbageRepos` and the command removes that library's tree from the commit, fs
and block stores. It reports by default and needs `-delete` to remove anything,
and it deliberately never inspects a library that still exists, which is what
lets it run without reasoning about concurrent writes. Stop the server first;
nothing locks the data directory.

What is still missing is GC *within* a live library. If you delete a 10 GB file
from a library you keep, its blocks stay on disk indefinitely under
`{data-dir}/storage/blocks/`, because an old commit still references them. That
needs a pass that:

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
  (`SILO_DEFAULT_QUOTA`, following every other variable Silo added) so it's
  settable without editing `seafile.conf`.
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
- ~~**Backup story**~~: done — `silo backup-db` snapshots the database with
  `VACUUM INTO`, and [`backup.md`](backup.md) documents the ordering the
  object-store copy has to follow. It said *both* SQLite files when there were
  two of them; there is one now, `silo.db`.

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

**OIDC login is planned**, and the design is settled in that document. Silo
serves no HTML, so it cannot host a login form, a callback or a redirect —
which rules out the flow everyone reaches for first. Instead Silo runs the
device grant (RFC 8628) as the client against the IdP: Porter shows a code,
the user approves it on the IdP's own device page, and Silo hands Porter a
Silo credential once the IdP confirms. Porter never speaks to the IdP at all,
which is what makes a shipped binary workable against a different IdP per
deployment.

Login is brokered, not federated. The IdP is consulted once per enrolment, not
once per request: a mounted filesystem issues requests at `ls` rate, and a
design that asks the IdP each time means the filesystem stops when the IdP
does. What the IdP returns is verified once and discarded; the credential Silo
issues carries the session from there, so revoking access is a row delete
rather than a cache expiry.

## Additional Protocol Frontends

Separate question from the compat gaps above: not "what does a Seafile client
expect", but "what *other* protocols could front the same store". WebDAV, S3,
SFTP and friends, with the architectural constraints that rule each in or out,
are surveyed in [`protocol-frontends.md`](protocol-frontends.md).

## A Native Silo Client

The CLI and TUI drive the management API, which is not sync. Two of the three
tiers that close that gap have landed: dedup-aware upload via the block surface,
and `silo put -r`, which composes it with `batch` to write a whole directory in
one commit. What remains is a full headless sync agent — no incremental
comparison against the remote tree, and nothing in the read direction. All
three tiers are in [`native-client.md`](native-client.md).

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

## Chunking

Silo cuts files at fixed 8 MiB offsets, so a boundary is a function of where
you are rather than what is there. Insert a byte near the front of a file and
every block id after it changes; `blocks/missing` will report a file it has
held for months as entirely absent, because under the new names it is.

Content-defined chunking fixes it at the root, and the objection previously
raised against it — that it would cost cross-lane dedup — rests on a claim
about upstream client chunking that nobody has verified. The chunker itself is
under a hundred lines. The real costs are that its parameters become permanent
wire protocol, and that smaller blocks force packing, which in turn forces a
compactor.

[`chunking.md`](chunking.md) has the argument, the test that decides it, and
the pack and compaction design. It supersedes the opposite verdict in
[`protocol-gaps.md`](protocol-gaps.md).

Note that the compactor and the per-repo garbage collector above are the same
project: both need a mark phase that walks live commits, and block liveness is
a global reachability property that cannot be maintained as a running counter.
Build them together or build the mark twice.

## Replication and Parity

Pie-in-the-sky, parked deliberately. Nothing here is scheduled, and the first
step below is worth more than everything under it combined.

The store is already git-shaped. `commitmgr.Commit` carries both `ParentID` and
`SecondParentID`; `mergeTrees` (`merge.go:21`) is a real three-way merge over
base/head/remote roots; `fastForwardOrMerge` (`fileop.go:1991`) mints merge
commits with a second parent and CASes the branch forward. That engine already
runs on every concurrent client write. Pointing it at a peer rather than at a
desktop client is a smaller change than "multi-server sync" sounds.

The one genuinely missing piece is the merge base. Today the client hands in
`base`, because it knows what it last saw. Two servers have to compute their
common ancestor themselves, by walking the commit DAG — a `git merge-base` walk
over parent pointers.

### Two products, not one

- **Primary plus N secondaries is durability.** Secondaries pull the DAG and
  the objects and never mint commits. A replica that cannot write cannot
  conflict: no merge base to find, no conflict files, no clock skew. Failover
  is permission to write. This is the piece actually worth building, and it
  needs none of the merge machinery.
- **Two people mirroring each other is collaboration.** That one is genuinely
  bidirectional, and Seafile's answer is already implemented and is the right
  one: never block. Both edits survive and one is renamed
  `foo (SFConflict user time)` (`merge.go:348`) — convergence by making the
  conflict visible rather than by choosing a winner.

Bundled, they give a system that is good at neither. The replica is also the
transport for the mirror, so build it first and stop there if nothing else is
wanted.

### Parity fits here better than it fits SnapRAID

SnapRAID's parity is a snapshot: change a file and the parity is stale until
the next `snapraid sync`. That is the whole reason it is only recommended for
media libraries — the tool is confined to static data by its own design.

Content addressing removes that constraint. Blocks are immutable, so an edit
writes new blocks and leaves the old ones untouched, and parity computed over
blocks is never invalidated by a write. Only deletion disturbs it, which makes
GC rather than editing the thing parity has to be designed around.

Which lands it on the compactor. The Chunking section above already concludes
that packing and the mark-phase GC are one project; parity stripes are the same
unit at the same layer. Parity over *packs* rather than over loose blocks makes
a sealed pack the stripe and compaction the only event that recomputes
anything. Content-defined chunking pulls against this, though — shards are
uniform today only because the block size is fixed.

### What stays out

A group where everyone holds a fraction plus parity and still reads locally is
two incompatible wishes. Holding 1/N of the blocks means fetching the rest from
peers, which means being online: a cache with an exotic backing store, and a
worse experience for a media library than simply storing it. Full peer-to-peer
parity also needs agreement on which blocks sit in which stripe, and shared
mutable state across untrusting peers is consensus — at which point this is no
longer a binary anyone can explain.

The cheap version keeps an authority. The primary owns the stripe map and hands
secondaries their assignments: subset replicas, no consensus, and the durability
that matters — a member's disk can die without every member holding everything.

## Non-Goals

Things we're explicitly *not* going to build, to keep scope honest:

- **Peer-to-peer federation** — a mesh of untrusting instances agreeing on
  shared state is consensus, and consensus is a different project.
  Replication with a single authority is parked rather than ruled out; see
  Replication and Parity above.
- **Plugin system** — if you want custom behaviour, fork.
- **LDAP / SAML** — OIDC covers the same ground with far less surface to
  implement and to get wrong, and it is planned rather than ruled out; see
  Credentials above. (A reverse proxy doing header-auth also remains
  acceptable; Silo will trust a configurable header.)
- **Mobile apps** — use the upstream Seafile mobile clients, they speak our
  protocol.
