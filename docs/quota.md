# Quota

**Plan, partly landed.** Enforcement, the CLI and the self-lookup endpoint are
built and described here as built. The charge those three agree on — logical
size at head — is the part this document proposes to change, and the change
pulls history retention in behind it. Read the *What is built* table first;
*The charge* and the server-level ceiling are argued and not yet written, and
the reclaimers under *History has a price* are built.

Quota is the one number a user sees that the server can refuse them over, so
what it counts is a product decision before it is an implementation one. Three
questions settle it: **whose** ceiling, **what** it counts, and **when** the
things it counts stop counting. This file answers all three in one place
because answering any of them alone produces a number nobody can act on.

## What is built

| | where | state |
|---|---|---|
| Per-account ceiling (`UserQuota` row) | `libmgr.AccountQuota` | built |
| Config fallback for accounts with no row | `[quota] default`, `option.DefaultQuota` / `option.parseQuota` | built |
| Admission on the write path, serialized per owner | `checkQuota` / `refuseOverQuota`, `quota.go` | built |
| Refusal status **507 Insufficient Storage** | `checkQuotaLocked` in `fileserver/quota.go` | built |
| Setting a cap from the shell | `silo user quota <email> [size\|none]` | built (`5f5ed23`) |
| Self lookup `GET /account/usage` → `{usage, quota, kind}` | `AccountUsageHandler` in `fileserver/api/api.go` | built |
| Per-library size on the libraries listing | `libraryInfo.Size`, `withUsage` in `fileserver/api/api.go` | built |
| Quota answering `df` on a porter-fuse mount | `internal/vfs/statfs.go` (porter-fuse) | built |
| History enumerable and readable at a point in time | `GET …/commits`, `entries/{path}?at=` | built (`6943a16`) |
| The three numbers: head, history, unreferenced | `objmgr.Census`, `silo df` | built (`fc6e846`) |
| Collecting objects no commit reaches | `silo gc -orphans`, `objmgr.Unreferenced` | built (`e56a270`) |
| Date-based history expiry | `silo gc -expire-history`, `expireHistory` | built (`c99b745`) |
| Server-wide ceiling | — | not built |
| Charging chunks rather than logical size | — | not built |
| Admin API to read or set another account's cap | — | not built, and gated on there being an admin role at all (`plans/sharing.md` § Accounts) |

## Whose ceiling

### Account level — the one that exists

The cap is **per account, not per library**. A user's ceiling applies to the
total of every library they *own*. A library shared with you appears in your
listing and is charged to whoever owns it; it contributes nothing to your
number. That is why per-library sizes live on the libraries listing rather than
under an account total — an account total that included shared libraries would
be a figure whose rows do not sum to it, and one that excluded them would leave
a client with rows it cannot size.

Two sources, and they are not rivals. A `UserQuota` row is the account's own
ceiling; `[quota] default` in `silo.conf` is the fallback for an account
without one. The row wins where it exists, so the config sets the floor for
everybody and the CLI sets the exceptions.

`quota <= 0` means no ceiling was ever set. `option.InfiniteQuota` is `-2`, and
it never reaches the wire: `accountUsageResponse.Quota` is a pointer and is
omitted rather than sent as a sentinel, because a capacity widget rendering
"-2 bytes" is the predictable end of putting one there.

### Server level — wanted, not built

An account ceiling bounds what any one user may hold. Nothing bounds what
*everybody together* may hold, so a server with ten uncapped accounts, or with
capped accounts whose caps sum to more than the disk, fills the disk and finds
out at the point where writes start failing for reasons that are not quota's.

The gap is worth closing at the same admission point, with the same status. A
write is admitted when it is under the owner's ceiling **and** under the
server's. What "the server's" means has two candidate readings and the honest
answer is both:

- **A configured ceiling** — `[quota] server = 900gb`. Predictable, and it is a
  policy an operator chose rather than a fact about a disk they may share with
  something else.
- **Actual free space, minus a reserve.** Truthful, and it is the only one that
  catches the disk filling from outside Silo. A filesystem driven to zero free
  bytes takes the database down with it, so the reserve is not a courtesy.

Refuse at the *lower* of the two. Neither alone is sufficient: the configured
number does not know about the other tenant on the volume, and free space does
not know the operator meant to keep 100 GB for something else.

The distinction from `[quota] default` matters and is easy to lose: that key is
the default *account* ceiling, applied per user. It is not, and has never been,
a statement about the server.

### Per library — deliberately not

"This shared team library may grow to 500 GB regardless of who owns it" is a
real request, and it stays an extension rather than a third level: a
`LibraryQuota` consulted alongside the owner's, taking the smaller of the two.
It is additive to everything below and nothing here has to anticipate it.

## The charge: what quota should count

### What it counts today

Logical size at head — the sum of the sizes of the files the current commit
reaches (`objmgr.Usage`, `libmgr.Usage`). The wire says so in as many words:
`kind: "logical-at-head"` on `/account/usage`, so a client can name which
number it is displaying instead of arguing about it.

The reasoning, recorded in the header of `checkQuota` in `fileserver/quota.go` and worth stating before it is
overturned: dedup and compaction move stored bytes under the user's feet, and a
number that changes because the server ran a background job is not one anybody
can act on. Under logical-at-head, deleting a file frees exactly its size,
immediately, and that property is what `TestDeletingAFileMakesRoomImmediately` in
`fileserver/quota_test.go` exists to pin.

### What it should count

**The chunks you actually occupy.** A user's charge is the unique chunks their
libraries hold, which is very nearly the bytes their libraries cost the disk.

The argument for it is that logical-at-head charges for something the user does
not consume. Store two copies of the same 4 GB video and content-defined
chunking stores one; logical-at-head bills 8 GB. The dedup that Silo's whole
object model exists to provide accrues to the operator and is invisible to the
person paying for the space. Charging chunks hands the saving to the user who
earned it, and — more importantly — makes the number the user sees and the
number the operator has to buy disk for the same number. Two accounting systems
that disagree is how a server runs out of space while every account is under
quota.

**Cross-library sharing is not a problem here, and that is what makes this
clean.** Objects live at `storage/{type}/{storeID}/`, one store per library
(`objstore.LibraryDir`), and E2EE libraries have per-library keys anyway,
so identical bytes in two libraries are two chunks. A chunk therefore belongs
to exactly one library, which is owned by exactly one account. There is no
question of who pays for a shared chunk, no split that changes when somebody
else deletes their copy, and no first-writer rule to be arbitrary about. Every
chunk has one payer.

### What it costs to change

Three consequences, and none is a surprise once stated:

1. **Deleting a file no longer frees space immediately.** The chunks stay
   reachable from older commits, so they stay charged until history expires.
   This is exactly ZFS and NetApp semantics, and users of both already expect
   it, but it is a behaviour change and it must be *said* — a client that
   deletes a file and watches the number not move will otherwise be filed as a
   bug. It is also the strongest argument for retention being built in the same
   stretch of work rather than after.
2. **The number moves when a background job runs**, which is the original
   objection and it is real. Packing and compaction (`chunking.md`) change
   stored bytes without the user touching anything. The mitigation is that the
   movement is bounded and downward — a job never makes a user cost more — and
   that the accounting reports the components separately, so "it went down
   because the server packed your library" is answerable rather than eerie.
3. **Compression means the charge is not the sum of file sizes**, in either
   direction, and a client cannot compute it locally from a listing. It has to
   ask. That is fine — it already asks — but it kills any client-side
   optimistic "will this fit" check that works by adding up sizes.

### On the wire

`kind` was put on `/account/usage` for exactly this day. The new charge gets a
new kind — `chunks-occupied` — rather than quietly redefining `logical-at-head`
under clients that already parse it. Reporting both together is worth doing:
`logical-at-head` is what the user's files add up to and is the number that
matches what they see in a file manager, `chunks-occupied` is what they are
billed for, and the gap between them is dedup and compression doing their job.
A client that shows both can explain itself. A client that shows one can pick.

## How a client gets this

The endpoint is `GET /api/silo/v1/account/usage`, and `server-info`'s features
list carries `usage` (`features` in `fileserver/api/api.go`) to advertise it alongside `size` and
`file_count` on the libraries listing.

**Do not gate the read on the feature flag.** The flag says what a server
claims; a server too old to have the list claims nothing while being perfectly
able to answer, so a client that consults the list first reports invented
numbers to a server that would have told it the truth. Ask, and treat "no such
route" the same as "the server is down" — porter-fuse's `AccountUsage` records
this reasoning in full.

That said, the two cases are not equally live. **There are no old installs, so
there is no version to be compatible with.** A client does not need a path for
"this server predates the endpoint" today, and writing one now is writing for a
world that does not exist. What it does need — permanently — is a path for *the
server did not answer*: unreachable, slow, mid-restart. That is not
back-compatibility, it is the network, and it never goes away.

### porter-fuse: `df`

Built. `internal/vfs/statfs.go` is the reference implementation and the shape it
settled on is the shape any client wants:

- **A TTL cache behind its own lock.** `Statfs` is not only called by a person
  at a terminal — desktop file managers poll it while a window is open — so one
  answer serves 30 seconds of them. The lock is its own and not the listing's:
  `df` has no business waiting behind a library listing, and a listing has no
  business waiting behind `df`.
- **A short timeout, shorter than the client's own.** `df` with no arguments
  walks every mounted filesystem, so a mount that blocks for a full HTTP timeout
  does not hang the client — it hangs `df` for the whole machine, including the
  local disks the person was actually asking about.
- **Failures are cached too.** Otherwise a server that is down is asked again by
  every single call, and the reply to all of them is the same anyway.
- **Clamp the remainder at zero.** An account over its ceiling — the cap was
  lowered, or the server counts something the client does not — otherwise wraps
  an unsigned subtraction to the size of the universe, and `df` reports an
  exabyte free on a filesystem that will refuse the next write.

The columns:

| | with a quota | without one |
|---|---|---|
| used | `usage` | `usage` |
| total | `quota` | invented, but *above* what is stored |
| free | `quota - usage`, clamped | invented |

**`df` will not match `account/usage` to the byte, and that is not a bug.**
`statfs(2)` reports block *counts*, not bytes, so every column is divided by a
block size on the way out, and used is `floor(quota/B) - floor((quota-usage)/B)`
— two independent truncations, within one block of the real figure on either
side. The block size is the client's choice, not the kernel's; porter reports
4096 so its row lines up with every local filesystem's. porter-fuse's `statfs`
implements this, and the arithmetic is its concern rather than the server's.

The half worth noticing is that **the used column is real either way**. That is
what makes an uncapped account's `df` useful rather than decorative: a backup
tool deciding whether a copy will fit is reading used, and an invented used
column would tell it half a terabyte was in use on an account holding thirty
megabytes.

Two mismatches survive, and both are the better of the available errors:

- **The figure is per account; `df` is asking about a mount.** Two mounts of one
  account report the same number. That is right about the constraint — the
  account really is the ceiling both are writing against — and wrong about the
  attribution.
- **The mount is not the sum.** It shows libraries you own *plus* libraries
  shared with you, and `usage` covers only what you own, so writing into a
  shared library does not move `df`. Owner headroom on the listing row would
  close it, at the cost of telling everyone a library is shared with something
  about its owner's account. NFS has the same wart and it has not been the thing
  that sinks a filesystem.

### macOS File Provider: not `df`

Porter-mac has no `Statfs` to answer. A File Provider extension does not get to
declare a volume size — Finder reports the disk backing the domain — so the
quota cannot be surfaced the way it is on a FUSE mount. **This is worth
confirming against the current API before anything is designed around it**, but
if it holds, the quota surfaces in two other places instead:

- **The container app**, which is where the account already lives: a plain
  "5.0 GB of 6.0 GB used" read from the same endpoint, refreshed on open rather
  than on a timer, since nothing is polling it.
- **507 handling**, which matters more. On a FUSE mount an over-quota write
  fails a `write(2)` and the program above decides what to do. In a File
  Provider the write has already been accepted into the local replica and the
  refusal arrives during upload, so it has to become a user-visible item state
  rather than an error return — the file stays, marked, with an actionable
  message. `porter-brief.md` already says 507 is a user-facing condition and not
  a transport error; this is what that means in practice.

### Refreshing

A TTL is sufficient and is what is built. The better trigger is already
available: porter subscribes to `/notification`, and a `LibraryUpdateEvent`
(`fileserver/notif/event.go`) is exactly the moment the account total may have moved. A
client that re-reads usage on that event, with the TTL as a floor rather than
the only clock, gets a number that changes when the user's own writes land
instead of up to thirty seconds later.

### What changes under `chunks-occupied`

The client-side shape does not, which is the point of having put `kind` on the
wire. Three things do:

- The used column becomes what the account is **billed** for rather than what
  its files add up to, and those are the same number only when nothing is
  deduplicated or compressed.
- A client can no longer estimate the charge locally by summing listing sizes,
  so anything doing an optimistic "will this fit" check has to ask instead.
- Deleting a file stops moving the number until history expires, so a UI that
  refreshes usage after a delete to show the space coming back needs to stop
  promising that.

## History has a price, and a date

Once quota charges chunks, history stops being free, and "how long do I keep
it" stops being a preference and becomes the lever that controls somebody's
bill. It needs an answer before the charge changes, not after.

**Date-based expiry.** A library keeps history for N days; commits older than
that are collectable, and the chunks reachable only from them come back. Days
rather than a commit count, because days are what a user can reason about — "I
can go back a fortnight" is a promise; "I can go back 200 commits" is a number
whose meaning depends on how busy the library was, and a single noisy client
can burn a hundred commits in an afternoon.

Three numbers fall out of this, and they want three different actions:

- **live at head** — reachable from the head commit. What quota charges.
- **history** — reachable from some older commit but not from head. What
  `silo gc -expire-history` reclaims.
- **unreferenced** — reachable from nothing at all. Interrupted uploads,
  abandoned commits. What `silo gc -orphans` reclaims, with no policy decision
  attached.

`objmgr.Census` computes all three in one mark phase and `silo df` prints them
per library. It is a set union over object ids rather than a sum of per-commit
`Measure` calls, because successive commits share nearly all of their objects
and summing them would report a history cost many times the disk.

`libmgr.advance` in `fileserver/libmgr/usage.go` falls back to measuring the
tree outright when a delta walk meets an object that expiry has collected out
from under it.

The reclaimers, their guards and the retention policy behind them are
[`storage.md`](storage.md) § Reclaiming's to describe, and this document does
not restate them.

## The `.snapshot` directory

NetApp's trick: every directory contains a `.snapshot` (or `.history`)
subdirectory holding the older versions of what is in it. No special client, no
restore workflow, no modal — the past is a path.

Silo should have it, and **the client builds it, not the server.** The two
endpoints the client needs are already there:

```
GET /libraries/{id}/commits[?limit=N]           → history, newest first, Link: rel="next"
GET /libraries/{id}/entries/{path}?at={commit}  → that path under that commit's tree
```

Together those are the whole of the server's contribution, and that is a
decision rather than an omission (the header of `fileserver/api/history.go`).

**No virtual nodes.** A directory the server invented has no id, appears in no
manifest, and would need excluding from GC's mark, from `changes?since=`, from
`Measure`, and from every other walk in the tree. This codebase rests on the
rule that an id names content; one synthetic entry puts an `if` into every place
that rule is relied on, and those `if`s are where the corruption bugs come from.
The client synthesizes the directory locally, where it is a display concern and
can be wrong without costing anybody data.

Consequences the client should know:

- **Reading history costs nothing against quota** under today's charge, because
  usage is logical size at head and history is by definition not at head. Under
  `chunks-occupied` it still costs nothing to *read* — but the history being
  read is now part of what the owner is charged for, which is the whole point of
  the retention lever.
- **The listing ends where history ends; it does not 410.** That is the opposite
  of `changes?since=`, and deliberate: there you named a commit and are owed a
  "that is gone, re-enumerate". Here you asked what history exists, and running
  out of it is the answer. When expiry starts collecting, the retention boundary
  shows up as the list simply stopping, and the normal case stays a 200.
- **Writes carrying `?at=` are refused with 400.** Silently dropping an
  unrecognised parameter is how you get a client that thinks it is editing the
  past while it is editing the present.

### The one endpoint still missing

`commits` plus `?at=` lets a client browse a *snapshot*. It does not let it
answer "show me the older versions of **this file**" without fetching every
commit and probing the path in each — N round trips to populate one `.snapshot`
directory the user just opened.

A per-path history — walking commits server-side and returning only those where
that path's id changed — is cheap for the server (compare ids, skip unchanged
subtrees unread) and expensive for anyone else. It is what turns `.snapshot`
from possible into usable, and it is the last server-side piece.

## What has to change

The claim that quota is logical size at head is written into code, schema
comments, the wire and four documents. Changing the charge means changing all
of them together, because the failure mode when half of them move is the one
`c4f9827` was written about — a correction lands in one place and the rest goes
on describing the world it replaced.

| site | what it says |
|---|---|
| `checkQuota` header, `fileserver/quota.go` | the charge, and why it is not stored bytes |
| `usageKind`, `fileserver/api/api.go` | `usageKind = "logical-at-head"` |
| `libraryInfo.Size`, `fileserver/api/api.go` | listing-row `Size`, same claim |
| `reportUserQuota`, `fileserver/user_quota_cmd.go` | what the CLI prints to an operator |
| `fileserver/chunks.go`, the delta comment | "must never reach quota, which is logical size at head" |
| `LibraryUsage` in `fileserver/dbutil/schema.go` | the table comment |
| `objmgr.Usage`, `fileserver/objmgr/measure.go` | what `Usage` is |
| `docs/porter-brief.md` § `account/usage` | the wire contract clients are written against |
| [`docs/roadmap.md`](roadmap.md) | the roadmap entry |
| [`docs/storage.md`](storage.md) § What the numbers mean | "cutting history reclaims disk, never quota" |
| `TestDeletingAFileMakesRoomImmediately`, `fileserver/quota_test.go` | the test pinning "freeing space makes room" |

That last row is the honest one. There is a passing test asserting the property
this change gives up. It should fail first, then be rewritten to assert what
replaces it — freeing space makes room *once history expires* — rather than
deleted.

## Order of work

1. **The mark phase and the three numbers** — built, `fc6e846`
   (`objmgr.Census`, `silo df`).
2. **Collect the unreferenced** — built, `e56a270` (`silo gc -orphans`).
3. **Date-based expiry and the retention policy** — built, `c99b745` and
   `7668d3f` (`silo gc -expire-history`, `silo retention`). Still open:
   nothing runs it unattended. `expireHistoryByPolicy` takes no window so a
   scheduler can call it, but an operator runs the command or schedules it
   with cron. An in-process timer is deliberately not built: it would be the
   first thing in Silo that deletes user data with nobody watching, and it
   wants a kill switch and an interval before it wants code.
4. **Switch the charge to `chunks-occupied`**, reporting both kinds, with every
   site in the table above moving together. This is fourth on purpose: it is the
   step users feel, and it should not land until the space it makes chargeable
   can also be released.
5. **Server-level ceiling**, at the same admission point, refusing at the lower
   of the configured cap and free-space-minus-reserve.
6. **Per-path history**, and the client's `.snapshot`.

## Settled: the CLI sets, it does not increment

`silo user quota <email> <size>` assigns a ceiling. An increment — `add
<email> 5gb`, meaning "give them another five" — was considered and declined.

It is the only operation in `silo user` that is not idempotent. Every other one
can be run twice with no second effect: `add` on an existing account fails,
`passwd` sets the same password again, `disable` on a disabled account is a
no-op, and `quota … 100gb` is an upsert precisely so that raising a cap does not
depend on whether a row already exists. An increment breaks that property, and
it breaks it in the place it is least affordable — an operator command recalled
from shell history, where the second run looks identical to the first and is
not.

Nothing has asked for it. If it is ever wanted it should be spelled so that it
cannot be mistaken for an assignment at a glance — `-by 5gb` rather than a
positional size — because the failure is silent: `quota alice 5gb` and
`quota add alice 5gb` differ by one word and by four orders of magnitude on an
account already holding 500 GB.

The distinction between creating a cap and changing one is deliberately absent
for the same reason. `5f5ed23` has a test for it: a plain `INSERT` worked
exactly once, so raising somebody's quota failed silently and left the old
number in force. The upsert erased the distinction on purpose, and a verb pair
that reinstates it would make the operator know the current state before
choosing how to change it.

## Open questions

- **Is `[quota] default` still the right shape** once the charge is chunks? A
  default expressed in logical bytes and enforced in stored ones is a number
  whose meaning changed under the operator who wrote it. Probably it just needs
  the units restating in the config comment, but it should be decided rather
  than inherited.
- **Grace behaviour.** Today the cap is a hard 507 at the boundary. A soft
  threshold that warns before it refuses is friendlier, and is more work: it
  needs somewhere to put the warning, which means either a field on
  `/account/usage` the client polls, or the events log (`plans/events.md`).
- **Does the account total need a headroom field on the libraries listing?**
  `/account/usage` covers libraries you own; the listing shows owned plus
  shared. A client looking at a shared library cannot tell that a write will 507
  until it does. One field on `libraryInfo` — the owner's remaining headroom —
  closes it, and it leaks a small fact about another account to anybody the
  library is shared with.
- **What `.snapshot` is called**, and whether the client or the server picks.
  NetApp's is `.snapshot`; `.history` reads better to somebody who has not used
  one. It is the client's directory, so it is the client's name — but two
  clients choosing differently is a thing users will notice across a desktop and
  a phone.
