# Plan: an account-scoped notification socket

Owns the build of [#54](https://git.booko.info/Silo/silo/issues/54). The wire
it lands is normative in [`protocol.md`](../protocol.md) § Change notifications
once built; until then this file is where the shape is argued.

## The premise

Everything about a library's *contents* pushes. Nothing about the **set of
libraries** does, and there are four ways that set moves: a library is created
elsewhere, one is shared with the account, one is renamed, one is deleted or
unshared. None of them rings.

The rename is the one worth pausing on, because it is unpushed even for a
library the client is already watching. `renameLibrary`
(`fileserver/api_handlers.go`) is one UPDATE and mints no commit — the name
is catalog data, and under E2EE the server could not write that commit at all.
So the head does not move, `changes` carries nothing, and the socket says
nothing. silo-drive-macos verified it against 0.5.0 and now carries each
library's display name in its sync anchor purely to notice renames on its own.

The ask is a second subscription mode: an authenticated socket subscribes to
the account rather than to a list of libraries, and gets a bare ring for
anything the account can see.

## What is already true, and worth not re-deciding

- **The visible set is one function call.** `share.PrincipalsFor`
  (`fileserver/share/grant.go`) expands an account into its user and group
  principals, and `share.LibrariesFor` answers which libraries those
  principals hold a whole-library grant on. `ListLibrariesHandler`
  (`fileserver/api/api.go`) is owned ∪ granted, and that union is exactly
  what a subscription needs. No new index, no new query shape.
- **There is one producer of events.** `notif.NotifyLibraryUpdate` is called
  from `onBranchUpdated` in `fileserver/commit.go` and nowhere else.
- **Catalog mutations are all in-process on the server.** Create
  (`CreateLibraryHandler`), delete (`DeleteLibraryHandler`) and rename
  (`renameLibrary`) are HTTP handlers; the CLI and the TUI
  reach them through `client.APIClient`, not by writing the database beside a
  running server.
- **Grants have no writer yet.** `share.Add` and `share.Remove` have no
  production caller. "Shared with the account" is [#14](https://git.booko.info/Silo/silo/issues/14)'s
  to produce, and this plan builds the receiver for it rather than waiting.
- **A hook pattern exists for the packages that sit below the fileserver.**
  `libmgr.OnLibraryDeleted` (`fileserver/libmgr/libmgr.go`) is a package
  variable the server registers, because `libmgr` cannot import upwards.
- **A ring costs one request, not one per library.** `GET /libraries` answers
  with `head_commit_id` on every row (`protocol.md:237`), so a client that is
  told only "something moved" makes one round trip and compares that column
  against its anchors to learn which libraries moved. The listing is an
  account-wide changes feed in all but name, and it is what makes a payload-free
  frame viable for a client holding thirty libraries rather than three.

## Decision 1 — the visible set is resolved at subscribe, not looked up per event

An account socket registers itself in the existing per-library index
(`addSubscription`) for every library it can see. `NotifyLibraryUpdate` does
not change, does not learn about accounts, and does not grow a database query.

That matters because it runs inside the commit write. A reverse index consulted
per event would put a query on the hot path to serve a subscription mode that,
today, one client uses.

**The frame is then chosen per client, not per event.** The fanout is one set
of subscribers and one branch at the point of writing: a per-library socket
gets `library-update` exactly as it does today, an account-scoped socket gets
the bare `account-update` of decision 4. The two modes share a delivery path
and differ in what they are told.

That branch is not cosmetic. Without it an account socket receives frames
naming a library and a commit id, and the argument in decision 3 — that this
mode carries nothing authorization protects, and so needs no lease — is simply
false. The socket would carry the same facts the leased lane does, with none of
its bound.

The cost of this choice is that the registration is a snapshot, and the set
moves. Decision 2 is how it is kept true.

## Decision 2 — the resync loop is the mechanism, and hooks are only latency

Each account socket re-resolves its visible set on an interval, and diffs:
libraries that appeared are subscribed and rung for, libraries that went away
are unsubscribed and rung for. That single loop delivers all four events, needs
no producer anywhere else, and is the only thing that can cover a change made
by a process that is not this one.

Hooks then make the common cases immediate rather than eventual: create, delete
and rename ring the account directly, because all three are in-process. A grant
change will ring when #14 writes one.

Build the loop first and the hooks second. In the other order the hooks look
like the mechanism, and the first change that happens outside the server —
an operator's `silo` command against a future admin path, a repair script, a
second server against one database — is a library that never appears.

## Decision 3 — the lease goes, because a doorbell has nothing to lease

Today the 72h notification JWT is the only re-authorization a live socket ever
gets. The credential is resolved once, at the upgrade
(`middleware.OptionalCredential` in `newHTTPRouter`), and never again;
`sweepSubscriptions` checks per-library JWT expiry, and re-asks the permission
question hourly on the credential lane; revoking a credential closes no
sockets. When the token expires the subscription is
dropped, and the way back on is `POST notify-token`, which runs
`share.CheckPerm` against a live credential. So a revocation reaches a socket
within 72 hours, and it reaches it that way and no other.

That lease earns its keep on the per-library lane because that lane *carries*
something: a subscription minted against a named library, delivering commit ids
for it. Stale authorization there means a revoked device goes on learning true
facts about a real library.

**A bare ring carries nothing authorization protects**, and that is the
argument for decision 4 as much as tidiness is. What a stale account socket
learns is that something in an account it could once see has moved: not which
library, not its name, not a commit id. Acting on it means `GET /libraries` or
`GET changes`, and every one of those re-resolves the credential and re-checks
`share.CheckPerm` on the spot. The pull was always the authorization boundary;
the doorbell just stops pretending the push is a second one.

So the account mode ships with no lease and no token, which is the whole of
what #54 asks for on the client side — connect with the credential, say
"everything", listen. The resync loop of decision 2 re-resolves the credential
at the same tick it re-resolves the set, and drops the socket when the
credential is gone, disabled, or has since been narrowed, but it does that as
**hygiene rather than as the security boundary**: a revoked device should not
be told the account is active — a timing side channel, real and small — and
should not hold a socket for free.

The consequence is that the interval is a freshness number, not an
authorization one. **Five minutes**, matching the sweep silo-drive-macos
already does client-side, and a missed tick is a late ring rather than a
breach.

This inverts the moment a ring grows a payload. A frame naming a library is a
fact about that library, and the lease would have to come back with it. Bare is
not a client convenience here; it is what keeps the mode cheap to authorize.

Whether the per-library lane should also re-check is a real question and
deliberately not this plan's: it is bounded at 72h today, it carries a payload
that justifies a bound, and narrowing it is a change to a shipped lane.

## Decision 4 — a bare ring, in its own event type

The frame carries nothing:

```json
{"type": "account-update", "content": {}}
```

A client re-reads `GET /libraries` to learn a new library's id and name
anyway, so a frame describing the change would be a second, partial answer to a
question the client already asks — and it would make the server decide what a
client may be told about a library it has not been granted. A bare ring avoids
the question. It is also already how the frame is treated: silo-drive-macos
reads `type` and discards the payload.

And it is what decision 3 rests on. An empty frame is a frame that needs no
lease, because there is nothing in it to have been authorized; the fetch it
provokes is authorized on its own. Every field added here adds one back.

What a payload would buy is targeting, and it buys less than it looks like.
`GET /libraries` carries `head_commit_id` per row, so the ringed client makes
one request and compares a column — not one `changes` call per library. And
under a burst the ring is the better frame: thirty commits across an account
collapse into one ring and one sweep, where the payload lane sends thirty
frames. The payload is already lossy in exactly that case — `noteMissed` keeps
the newest commit id per library and discards the rest — so precision was never
the property it had.

Its own type rather than a `library-update` with no `commit_id`, because a
`library-update` for a library that was just deleted is a lie in the one field
that frame exists to carry.

## Decision 5 — a narrowed credential is refused the account ring, because it cannot answer it

First, what a narrowed credential is, because the word suggests something wider
than the encoding allows. `credential.Scope`
(`fileserver/credential/scope.go`) has three shapes and no others:

```
""                      every library the account reaches
"<library-id>"          one library, entirely
"<library-id>:<path>"   one library, at <path> and below
```

There is no "these three libraries" — the encoding cannot express a subset. A
narrowed credential reaches one library, or one folder in one library, and it
is a ceiling rather than a grant: it only ever subtracts from what the account
already has. `POST /auth/login` passes a scope the client asks for straight
through `ParseScope` (`fileserver/api/api.go`), so these exist in the
field, not only in tests.

**The account ring is refused to one because it is unusable by one, not
because it is dangerous.** `scopeReachesRoute`
(`resolveCredential`) refuses a scoped credential every
route that names no library — the comment there records the bug that put the
check at that layer, which was a credential cut to one library enumerating
every library its account could see. `GET /libraries` is therefore `403` for
this holder. A ring says "something moved, go look"; looking is the one call
they cannot make. All they could do is re-read their one library on every ring,
including the rings caused by libraries they cannot see and could not fetch —
a wakeup with no possible response.

The confidentiality argument is real but thin, and it is not what this rests
on. A bare ring to a stolen scoped credential leaks a timing channel: that
activity exists elsewhere in the account, and roughly when. No library id, no
name, no count, no commit. Against a holder who already has a working
credential on that account, that is inference about working hours rather than
disclosure of data, and it would not on its own justify a refusal.

Refused loudly — a frame back saying so — rather than silently serving the
per-library lane instead, on the same rule as `OptionalCredential`: a
credential that is offered and not good enough is never quietly downgraded,
because the caller believes it is authorized and would learn otherwise only
from what it stopped receiving.

This needs `notif.Handler` to carry the credential. It takes only the account
today (`fileserver/notif/handler.go`), so `middleware.GetCredential` joins
`middleware.GetAccount` there and `Client` grows the scope beside its account.

## Decision 6 — the scoped ring, so the refusal costs the client nothing

A narrowed credential subscribes to the library its scope names and gets the
same bare ring for it. No token, no payload: the credential presented at the
handshake already says which library it reaches, and `Scope.LibraryID` is that
answer. Refusing the account ring then withholds nothing a client wanted — it
redirects it to the bell it can actually answer, which rings only when its own
library moves rather than on every commit in the account.

This is why the decision belongs in this plan rather than in the follow-up that
retires the token lane. Without it, decision 5 reads as taking something away.
With it, the pair says: **the credential decides what may be subscribed, and
every mode is a ring.**

**A path-scoped credential gets its library's ring anyway, and that is a
decision rather than an oversight.** The ring may be reporting a commit outside
the folder the credential covers, and the server will not find out: deciding
otherwise means diffing every commit against a path prefix per subscriber, on
the commit path, which decision 1 exists to keep clear. What the holder learns
is that something moved somewhere in a library they are already mounted inside
— a timing signal about a thing they know exists — and what they gain is a
wakeup they can act on, since `GET entries` and `GET changes` inside the scope
are calls they can make. The trade is worth it in the direction of ringing.

It is also the piece that makes the token lane removable. `POST notify-token`
exists because `/notification` predates the `Authorization` header and could
not ask a credential what it was allowed to watch, so a per-library JWT was
minted to answer instead. Once the handshake carries the credential, the
credential answers directly — and the notification JWT is the only one Silo
still issues (`utils.AudNotif`; `option.JWTPrivateKey` has
exactly two consumers, and `golang-jwt` two non-test importers). Retiring the
lane is its own issue, sequenced after both silo-drive clients migrate, but it
is this decision that unblocks it.

## The work, in order

### 0. The socket authorizes from the credential — **done**

Not in this plan when it was written, and underneath all of it: steps 1, 3 and
5 each assume the client holds the credential that opened the socket, and none
of them builds that.

`Handler` now reads `middleware.GetCredential` alongside the account and hands
both to `NewClient`. A subscribe entry with no `jwt_token` is authorized by
that credential through `middleware.PermFor` — which is `Perm` split so that a caller holding a
credential rather than a request asks the same question, rather than a second
place remembering `CheckPerm` and forgetting the narrowing. Refusals are a new
`subscribe-denied` frame, because `jwt-expired` means "re-mint", which is false
of a permission answer and sends a client into a loop. `tokenExpiryLoop` became
`sweepSubscriptions`, and re-asks the authorizer hourly for credential-lane
subscriptions: a token bounded a withdrawn share at 72h by expiry alone, and a
device credential need never expire at all. Advertised as
`notifications-credential`.

The hook rather than a direct `share` import is not about a cycle — there is
none — it is that `share.CheckPerm` needs a database and this package has never
needed one, in its tests least of all.

**And the socket admits a scoped credential.** `resolveCredential` applied
`scopeReachesRoute` to `/notification` too, so any credential with a non-empty
scope was 403'd *before the upgrade* — a mount cut to one library had no push at
all, and found out in the one way a client cannot fall back from. The narrowing
now applies per subscribe rather than at the door
(`resolveOpts.skipDoorCheck`), which is where it belongs: the upgrade
answers nothing, and `PermFor` checks each library the credential asks for.

That is decision 6's first half, arriving early and on the per-library lane. It
grants a library-scoped credential its library and refuses a path-scoped one
even that, because `Perm`'s documented rule says a folder scope cannot be
answered about the library as a whole. Decision 6's trade — ringing a
path-scoped credential anyway — stays with the ring, where the frame carries
nothing. It should not be smuggled in on a lane whose frames carry a commit id.

Test 8 changes shape as a result: the path-scoped case is now pinned twice, in
`TestPermForAppliesTheNarrowing` as a refusal on this lane and, when the ring
lands, as a grant on that one. The contrast is the decision, so both should
name each other.

**What it leaves for step 3.** `credential` exports only `Resolve(r, kinds…)`,
which needs the presented secret. There is no lookup by id, so "re-resolve the
credential; drop on revoked, disabled, or newly narrowed" has nothing to call.
The hourly sweep covers a withdrawn *share*; a revoked *credential* still holds
its subscriptions until the socket drops. Step 3 needs that lookup built.

### 1. The subscribe frame, and the visible set — **done**

`subscribeFrame` grew `Account bool`, and `handleMessage` routes it to
`subscribeAccount`: refuse when there is no credential, refuse when the scope
is narrowed — step 5 is what turns that second refusal into the scoped ring,
and until it lands a narrowed credential keeps using the token lane exactly as
it does today — otherwise resolve owned ∪ granted through `visibleLibraries`
and subscribe to each. The owned half is `libmgr.OwnedLibraryIDs`, ids only;
the listing handlers keep their own joins because they need the columns.

Both refusals are `subscribe-denied` with `{"account": true}` as the content,
naming the mode where a per-library refusal names the library, so a client
reads one type for "no" on either lane.

A subscription now records its lane as a three-valued `lane` rather than a
flag, and `laneAccount` is the third. The sweep leaves that lane alone: its
re-check is the resync loop of step 3, not the per-library question the
credential lane asks, and a sweep that asked `PermFor` per library for the
whole set would be the reverse index decision 1 refused, on a timer.

### 2. The frame branch, and a one-bit debt — **done**

`NotifyLibraryUpdate` writes `account-update` to an account-scoped subscriber
and `library-update` to every other. One `if` on `Client.accountScoped` where
the message is handed to `wch`, and the only change the commit path sees. The
flag is atomic, because the fanout takes no lock of the client's.

The deferred-delivery machinery does not follow it across. A per-library socket
keeps `missed`, `noteMissed` and collapse-into-newest, because a commit id per
library is a thing that can be stale and has to be reconciled. An account
socket's debt is one bit — `ringOwed` — so a full `wch` sets it and the writer
sends one ring when the queue moves, on the same wake the per-library resync
uses. Two mechanisms, and the second is a handful of lines, because there is
nothing in a ring to collapse.

### 3. The resync loop — **done**

A second ticker in `sweepLoop`, at `resyncPeriod` (five minutes), that runs
`resyncAccount` on an account-scoped client. Re-resolve the credential through
`credential.ByID` — Resolve without the proof and without the last-used stamp,
for a holder that proved it once and kept it — and close the socket on
revoked, expired, disabled, or newly narrowed. Re-resolve the set and diff it
against the account-lane subscriptions; subscribe what appeared, unsubscribe
what left, ring once if either was non-empty.

A store that cannot be read is none of those and does not close the socket:
that tick logs and the next one asks again, because dropping every account
socket on a slow query would be a reconnect storm asking the same database.

The token half of `sweepSubscriptions` stays exactly as it is — a socket can
hold both kinds of subscription, and the per-library lane is unchanged.

### 4. `account-update`, and the three hooks

The event type and `notif.NotifyAccountUpdate(account.ID)`, fanned out over a
new account-keyed index. Rung from create, delete and rename. Delete already
has `libmgr.OnLibraryDeleted`; the other two are direct calls, since
`fileserver/api` may import `notif` without a cycle.

Rename rings the owner and every principal holding a grant — `share.ForLibrary`
is the inverse lookup, and it is off the commit path so it may query.

### 5. The scoped ring

A narrowed credential's subscribe takes no library id and no token: the scope
names the library, so the frame is the same `{"account": true}` and the server
answers it with a subscription to `Scope.LibraryID` rather than to the set.
Same `account-update` frame, same one-bit debt, same resync tick re-resolving
the credential.

It lands after the account ring rather than beside it because it shares every
part of that machinery and adds one branch to it. Built first, it would be the
same code with the interesting half missing.

### 6. The feature name and the docs

`notifications-account` in `features()` (`fileserver/api/api.go`),
conditional on `EnableNotification` beside `notifications`. Without it a client
cannot tell "this server has no account mode" from "this account is quiet",
and those two look identical from the outside.

One name covers both rings. A client does not choose between them — it sends
the same frame and the credential decides what it gets — so a second name would
advertise a distinction no caller can act on.

## The tests, and the order they go in

1. An account subscribe with no credential is refused, and a scoped credential
   asking for the account's set gets its own library's ring and not the
   account's. Both before anything is built: the first is the rule, the second
   is the whole of decisions 5 and 6 in one assertion.
   **Done, in its step 1 form:** an anonymous account subscribe is refused,
   and a narrowed one is refused rather than rung. The scoped ring's half is
   step 5's to write.
2. A commit to a library the account owns reaches an account-scoped socket that
   never named that library. **Done.**
3. A commit to a library the account cannot see reaches it not at all.
   **Done.**
4. A rename rings, with no commit and no head movement — the regression the
   issue was filed for, and it must fail against today's server.
5. A library created after the socket was up rings, and its next commit is
   delivered. **Done**, as a change to the set the resync tick finds — the
   create hook of step 4 is what makes it immediate. And the inverse: a
   library that leaves the set is unsubscribed and rings, and an unchanged
   set rings nothing.
6. A credential revoked under a live account socket closes it on the next tick.
   **Done**, and a store that cannot be read does not.
7. A narrowed-after-connect credential closes it likewise. **Done.**
8. A path-scoped credential is rung for a commit elsewhere in its library —
   pinning decision 6's trade as deliberate, so that narrowing it later is a
   change to a test rather than a silent one.

## What this does not do

- **It does not replace the per-library lane.** `notify-token`, the
  library-scoped JWT, its hourly sweep and `jwt-expired` all keep working
  exactly as they do, for the clients already using them. Decision 6 means a
  scoped credential no longer *needs* them, which is what makes retiring the
  lane a later cleanup rather than a break — but nothing here retires it.
- **It does not push content.** A ring says the set moved; `GET /libraries` and
  `GET changes` remain the truth, exactly as `library-update` is a hint.
- **It does not widen what an account can see.** Every ring is gated on the
  same union the listing already answers with.
- **It does not produce share and unshare.** Nothing writes a grant yet; those
  two rings land with #14, and the loop of decision 2 covers them in the
  meantime by noticing the set changed.

## Doc changes

`protocol.md` § Change notifications gains the subscribe frame, the new event
type, and the rule that the credential decides which ring answers it; §
Writing a client / Push loses the advice to re-mint on `expires_at` for a
client that can subscribe this way. The feature-name list gains
`notifications-account`. `roadmap.md` gains an entry under Independent work
pointing here.

## Open

- **The interval.** Five minutes, as `resyncPeriod`, a freshness number — see
  decision 3 for why it is not an authorization one. A compiled-in constant
  first; a `[notifications]` key in [`configuration.md`](../configuration.md)
  if a deployment ever wants it different.
- **Retiring the token lane.** Its own issue, and decisions 5 and 6 are what
  make it reachable: once the credential decides what may be subscribed,
  `notify-token` answers a question nobody asks. What goes with it is larger
  than the endpoint — `parseNotifToken`, the token half of `sweepSubscriptions`,
  `jwt-expired`, the per-subscription expiry map, `provisionalGrace` and `dropIfUnproven` (which
  exist only because a socket can arrive anonymous and prove itself with a
  token later), and then JWT itself: `AudNotif` is the audience on the one JWT
  Silo still issues, `option.JWTPrivateKey` has two consumers, `golang-jwt` has
  two non-test importers, and `SILO_JWT_SECRET` and `LoadJWTConfig` exist for
  no other reason. It also retires a roadmap item rather than doing it — the
  persistent JWT keyfile at mode 0600 is wanted only so a restart stops
  invalidating notification tokens. Sequenced after both silo-drive clients
  migrate, and it has to argue two things: that `notifications` may keep its
  feature name while `notify-token` stops answering (`blocks` and
  `blocks-fetch` are the only precedent, and that was a rename), and that
  withdrawing an endpoint silo-drive asked for in 0.4.4
  ([`feature-req/notify-token-on-the-silo-lane.md`](../feature-req/notify-token-on-the-silo-lane.md))
  is the same request taken further rather than a reversal.
- **Whether `library-update` should become a doorbell too.** Distinct from the
  above: this is about the frame, not the token. The payload is already
  advisory — `protocol.md` § Push forbids using the pushed `commit_id` as an
  anchor without fetching — and dropping it would collapse the deferred-delivery
  apparatus into the one-bit debt of step 2. Against: it is a shipped lane,
  `library_id` is the only targeting any client has, and our own
  `client/notify.go` puts both fields on a public channel. Note that a client
  wanting rings can simply subscribe the new way, so this buys the deletion of
  code rather than a capability.
- **Whether per-library sockets get a credential re-check.** They are bounded
  at 72h today and they carry a payload that justifies a bound. Narrowing that
  is a change to a shipped lane.
