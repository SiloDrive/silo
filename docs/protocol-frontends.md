# Protocol Frontends

Silo speaks one HTTP API, `/api/silo/v1/`. This document is a survey of what
*else* could be bolted onto the front of the same store, what each one buys, and
what it costs.

Nothing here is committed. It exists so the decision is made once, on the
architecture, rather than re-argued per protocol.

## The constraint that decides every answer

Silo is a git-shaped store: commits point at directory trees, which point at
file manifests, which are lists of content-addressed chunks. Every mutation
ends in `updateBranch` (`fileserver/commit.go`) moving the head by
compare-and-swap; a writer that loses the race is refused, not merged, and
rebuilds on the new root ([`protocol.md`](protocol.md)). There is no
partial-file update anywhere in the model: changing one byte means re-chunking
the whole file and minting a new commit.

Reads are the easy direction. `serveFile` (`fileserver/entries.go`) honours
`Range`, and a manifest carries every chunk's plaintext size, so ranged reads
don't require materialising a whole file.

That asymmetry sorts every candidate protocol into two piles:

- **Whole-file, handle-oriented** — WebDAV, S3, SFTP. The client opens, writes,
  closes; you commit once at close. Natural fit.
- **Offset-oriented** — NFS, SMB, FUSE. The client writes at arbitrary offsets
  and expects the write to land. You are emulating a filesystem on top of a
  version control system, and paying a commit per flush.

Pick from the first pile unless there's a specific reason not to.

## Prerequisite: build a back before adding a front

The operations are welded to `http.ResponseWriter`: the `entries` handlers in
`fileserver/entries.go` and the batch handler take `(w, r)` and write status
codes inline. The reusable layer beneath them — the tree lookups by path and
the diffing in `fileserver/objmgr` — sits one level too low to build a
protocol on.

Any second frontend means extracting a protocol-neutral core first:

```go
package core

func Stat(libraryID, path, user string) (*Entry, error)
func List(libraryID, path, user string) ([]Entry, error)
func Open(libraryID, path, user string, off, n int64) (io.ReadCloser, error)
func Create(libraryID, path, user string, r io.Reader) error
func Mkdir(libraryID, path, user string) error
func Move(libraryID, from, to, user string) error
func Remove(libraryID, path, user string) error
```

Taking IDs and paths, returning typed errors rather than status codes. That
refactor is most of the work of the *first* frontend and then close to free for
every one after it. It also makes the existing HTTP handlers testable without
standing up a server, which is worth doing on its own merits.

## Tier 1 — worth building

### WebDAV

The highest ratio of reach to effort. `PROPFIND`, `GET`, `PUT`, `MKCOL`,
`MOVE`, `COPY` and `DELETE` map essentially one-to-one onto operations that
already exist. It buys macOS Finder, Windows Explorer, GVFS/Dolphin, iOS file
apps, davfs2 and rclone in a single step.

Upstream shipped this as a separate Python daemon; Silo would have it in-binary,
which is the whole point of the rewrite.

Gotchas:

- `LOCK`/`UNLOCK` can start as an in-memory no-op that satisfies clients. It
  becomes real once file locking lands (see [`plans/locking.md`](plans/locking.md)).
- Finder sprays `._*` and `.DS_Store` at any mount. `shouldIgnoreFile`
  (`fileserver/api_handlers.go`) already exists for exactly this.
- The Windows client is fussy about Basic auth and `Depth` handling. Budget a
  day for its quirks alone.

### S3 (a subset)

The best conceptual fit, because Silo is already content-addressed and
immutable. Bucket maps to library, key maps to path. `GET`, `PUT`, `HEAD`,
`DELETE` and `ListObjectsV2` cover the great majority of clients, and multipart
upload lands naturally on the existing block-upload path.

It buys restic, rclone, Cyberduck, Arq, Duplicati — effectively the entire
backup-tool ecosystem — without writing an integration for any of them.

Two real costs:

- **SigV4 needs a secret to sign with.** A PBKDF2 hash cannot be used for HMAC
  signing, so this requires a genuine access-key/secret-key pair per user. See
  *App passwords* below.
- **`ETag` semantics are MD5-shaped.** Silo's object IDs are SHA-1 over
  different input, so they can't be reused. restic and rclone do check `ETag`,
  so either compute and store an MD5 alongside each file at index time, or
  document `--ignore-checksum` and accept the caveat.

Object versioning mapping onto the commit DAG is a freebie available later, and
would be the first real exposure of history to outside tools.

### SFTP

`github.com/pkg/sftp` over `golang.org/x/crypto/ssh` is close to drop-in, and
it brings something neither of the others does: **SSH public-key auth**. Today
every credential Silo uses is a bearer token in a header; public keys are a
categorical improvement.

Being handle-based, writes buffer to a temp file and commit once on `CLOSE` —
exactly one commit per file, which is the behaviour we want. Random writes and
appends degrade to read-modify-write of the whole file; in practice clients
write sequentially from zero. `SETSTAT`/`chmod`/times are safely ignorable.

## Tier 2 — situational

- **NFSv3** (via `go-nfs`, userspace, no kernel module required) or a FUSE
  export. Gives a real mount point, but it is the worst semantic fit in this
  document. Viable if scoped **read-mostly**: mount a library read-only and
  write through another protocol.
- **Nextcloud/ownCloud chunked upload API.** WebDAV plus extensions. Only worth
  it to pick up the Nextcloud mobile and desktop client fleet.
- **Public share links.** Designed in [`plans/sharing.md`](plans/sharing.md)
  as credential rows served at `/s/{code}`. The "protocol" is just a browser;
  signed URLs, per [`capability-urls.md`](capability-urls.md), arrive only
  when the share page renders media inline.
- **An MCP server.** A small surface over the core ops layer (list, read,
  write, search a library) that makes libraries directly available to coding
  agents. Cheap once `core` exists.
- **tsnet.** Not a protocol, but the most practical thing to put in front given
  that Silo binds loopback and speaks plaintext by default. Embedding Tailscale
  gives Silo its own tailnet identity and TLS cert, and supplies authenticated
  user identity rather than requiring one to be invented.

## Tier 3 — interesting, and hard

**Read-only git smart-HTTP.** `git clone http://silo/git/{library-id}` is
philosophically almost free: the commits, trees and blobs already exist. The
catch is that Silo object ids and git object ids hash different bytes, so
none of the existing IDs can be reused. It means computing git SHA-1s, building
packfiles, and caching the ID mapping — real work.

The payoff is that every git tool becomes a history browser for Silo libraries.
`GET /commits` and `entries/{path}?at=` already expose the commit log and a
file at a past commit; what the roadmap still lists as missing is per-path
history, a deleted-but-reachable listing, and restore.

## Explicitly not

Federation, Syncthing's block exchange protocol, CalDAV/CardDAV, gRPC and
GraphQL. None buy reach that the tier-1 three don't already cover, and the
first two contradict the "peer-to-peer anything" non-goal in
[`target.md`](target.md) § Deliberately not.

## Cross-cutting work every frontend needs

### Issued credentials

Login runs 600,000 rounds of PBKDF2 (`authmgr.PBKDF2Iterations` in
`fileserver/authmgr/authmgr.go`). WebDAV and S3 authenticate *every
request*, so reusing the account password means running the login KDF per
request — a self-inflicted denial of service that the login rate limiter cannot
help with, because these are all successful verifications.

Both frontends need credentials Silo issues rather than the account password:
app passwords (random, stored as a fast hash) for WebDAV and SFTP, and an access
key ID plus a derived secret for S3, which cannot hash its secret because SigV4
requires the server to recompute an HMAC with it.

This is the single largest prerequisite in this document, and it has its own
write-up: [`auth.md`](auth.md).

### Commit coalescing

A Finder copy of 500 files becomes 500 commits and 500 rounds of `updateBranch`
contention unless writes are batched per session or per time window. This has
to be decided before the first frontend ships, not retrofitted after a user's
history is already 500 commits deep for one drag-and-drop.

Half of it now exists: `POST libraries/{id}/batch` applies a list of operations as
one commit, so a frontend has somewhere to put a coalesced window rather than
having to invent the mechanism. What it does not supply is the *policy* — when
to close a window, and what to do about a client that goes away mid-drag — and
that is still a decision per frontend. A handle-based protocol commits on
`CLOSE`, which is already one commit per file; WebDAV and S3 have no session to
hang a window on, which is where this bites.

### Encrypted libraries are opaque

The server never holds the keys to an E2EE library — names and content are
ciphertext to it, by design ([`storage.md`](storage.md)). A server-side gateway
therefore cannot serve one at all, and must exclude E2EE libraries with a
clear error rather than rendering ciphertext as if it were a tree. Only plain
libraries are reachable through any frontend in this document.

### Permissions

`share.CheckPerm` (`fileserver/share/share.go`) exists, but the grant model in
[`plans/sharing.md`](plans/sharing.md) is not built, so every frontend
inherits "owner-only, all users equal". That's acceptable for now. What matters is that no protocol
frontend *implies* a permission model richer than the one actually enforced.

## Recommendation

**WebDAV first.** It forces the `core` extraction, which is the real
prerequisite, and it is the shortest path from "our own clients only" to
"mounts on every operating system".

**S3 second.** It unlocks the entire backup-tool ecosystem and fits the
content-addressed store better than anything else on this list.
