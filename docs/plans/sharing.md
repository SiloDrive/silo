# Plan: accounts, public libraries, and share links

Date: 2026-08-22
Status: **proposed** — depends on [`../storage.md`](../storage.md) (E2EE model,
convergent chunk keys, manifests) and on [`auth.md`](../auth.md)'s credential
table, which has landed — including the `library_id:path` scope extension this
plan needs. Build order at the end sequences against both.

Scope: user accounts and roles, public-read-only libraries, and share links —
including links out of E2EE libraries, which is the part that needed design
rather than plumbing. Upload/drop-box links and signed URLs are explicitly
out (deferred, not rejected; see the end).

## Decisions

| # | Decision |
|---|---|
| 1 | Three principals — account, anonymous-via-public-grant, anonymous-via-link — resolve through **one permission path**. No ad-hoc checks in read handlers. |
| 2 | Public-read-only is a **permanent, listed grant to the anonymous principal** — the same grant machinery as a share link scoped to the library root, differing only in discovery. |
| 3 | `public_read` ⇒ server-readable library. Exclusive with E2EE at creation; converting an E2EE library to public is `silo convert`, the client-side re-encryption operation [`storage.md`](../storage.md) defines — and conversion to public always starts a new history root. |
| 4 | Anonymous read on public libraries covers the **chunk surface** (manifests + chunks), not just `entries/` — silo-drive can mount a public library with no account. |
| 5 | A share link is a **Credential row**: `kind=link`, path-extended scope, `perm` ceiling `r`. Revocation, listing, labels, `last_used`, expiry, and the `is_active` account join all come from the existing model. |
| 6 | Content is encrypted **once**; link flavors differ only in where the share key SK comes from. Three flavors on E2EE libraries: **e2e** (SK in URL fragment — default), **password** (SK wrapped under a password-derived key, `curl -u`), **compatible** (SK wrapped to the server — plain `curl`). |
| 7 | **No password is ever stored.** The password flavor stores a salt and a sealed blob — the SK wrap; the compatible flavor stores SK wrapped under a server key; the e2e flavor stores no key material at all. |
| 8 | A minimal **share page ships with links** — one embedded HTML file at `/s/{code}`, WebCrypto decrypt path for e2e and password flavors. |
| 9 | Accounts are **invite-only**. An invite is a credential (`kind=invite`, single-use, expiring, **bound to an address**); redemption claims that address's account and is where E2EE bootstrap happens. Roles: `admin` / `user` / `guest`. |
| 10 | Signed URLs are **not** built here. They arrive when the share page renders media inline, per [`capability-urls.md`](../capability-urls.md), and the tokenstore redirect is never resurrected. |

## Accounts

The credential model is auth.md's; this plan adds what sits around it.

**Roles.** A `role` column on the account: `admin` / `user` / `guest` — built;
the older `is_staff` flag went rather than gaining a neighbour, since a flag
and a role are one fact written twice. The role is on the resolved account
(`credential.Resolve` joins it). What an `admin` may then *do* is a set of
capabilities rather than a second flag, designed in
[`admin.md`](admin.md), which also owns the middleware and the HTTP
surface. Guest cannot create
libraries and sees only what is shared to them — the "consumer deployment"
shape, where the admin curates the libraries and everybody else syncs what
they have been given. A config key
`allow_user_create_library` gates creation for `user` as well, for installs where
the admin curates everything.

**Invites.** `POST /api/silo/v1/invites` (admin) mints a credential row:
`kind=invite`, single-use, `expires_at` default 7 days, `label` = who it's
for. The invite URL is the token in the standard `silo_invite_…` format.
Redemption creates the account and is deliberately the only registration
path — and it is where E2EE bootstrap lives, because it is the one moment a
client is guaranteed present:

1. client generates the X25519 identity keypair
2. client picks the password, runs the argon2id split ([`storage.md`](../storage.md)): `authKey`
   goes up as the account password, `wrapKey` never leaves
3. client uploads the wrapped private key, the published public key, the KDF
   salt, and a recovery-code wrap

**An invite binds the address it was sent to.** The row carries the intended
email (`label` is display, not binding), and the redeemer does not choose
their address — delivery of the invite to that inbox *is* the verification
step. When an account for that address already exists — the identity split
mints an inactive account for every address that appears in a share without
a user row — redemption **claims** it: activates the account, attaches the
password and keys, and inherits whatever was shared to the address before
its person arrived. Minting a second account beside the tombstone would
recreate the double-mint bug the split made unrepresentable; claiming an
*active* account is refused outright. This is the enforcement site for
auth.md's tombstone-inheritance warning: a lapsed address reassigned to a
new person inherits the old shares, so the admin minting the invite is
vouching that the inbox and the person still match.

**Deactivation.** `is_active=false` on the account kills every lane through
the `Resolve()` join — including share links the account minted, which is a
property worth a test, not a hope: a departed user's public links must die
with the account. The anonymous principal needs the same rule stated:
`CheckPerm` honours an `anon` grant only while the account that owns the
library is active, so a departed user's *public library* goes dark with them
exactly as their links do.

## The grant model

One table answers "may this principal do `op` at `(library, path)`":

```sql
CREATE TABLE Grant (
  id         INTEGER PRIMARY KEY,
  principal  TEXT NOT NULL,   -- 'user:<account>' | 'group:<gid>' | 'anon'
  library_id    TEXT NOT NULL,
  path       TEXT NOT NULL DEFAULT '/',   -- subtree scope
  perm       TEXT NOT NULL,   -- 'r' | 'rw'
  created_by TEXT NOT NULL,
  ctime      INTEGER NOT NULL
);
```

The existing inherited share tables (`SharedLibrary`, `LibraryGroup`) fold into this
or are read through it — decided at implementation, but the invariant is that
**`CheckPerm` consults one model**, and the credential's `perm` column remains
a *ceiling* over the grant, never a grant itself (auth.md's rule).

- **User/group shares** are `user:`/`group:` grants, exposed on the endpoints
  this plan defines below (`/libraries/{id}/shares`, `shared-with-me`).
- **Public-read-only** is `('anon', library, '/', 'r')` plus a `listed` column
  on the grant row (meaningful only for `anon` principals) for
  discovery: `GET /api/silo/v1/public-libraries` enumerates listed anonymous
  grants, no auth required. Unlisted-but-public is a root share link instead —
  same grant, different discovery, which is the whole point of unifying.
- **Share links** mint a real Grant row — `principal = 'link:<credential-id>'`
  — created and deleted in the same transaction as the credential. The
  credential's `perm` stays a ceiling over that grant, the invariant above
  survives literally, and `CheckPerm` consults one model with no
  carried-by-credential special case.
- The anonymous principal is **never writable** in this plan. Upload links
  will change that deliberately, later, with their own abuse story.

**Write paths check too.** `chunks/missing`, chunk PUT, and manifest commit
must consult the same model — read-only means the sync surface refuses
writes, not just `entries/`.

**Anonymous lanes are rate-limited per IP** (token bucket, config caps), and
anonymous traffic is attributed to the library owner if quotas ever grow a
bandwidth dimension. Public libraries are unauthenticated bandwidth; say so in
the admin docs.

### Anonymous mount

Confirmed goal: silo-drive mounts a public library from a bare URL, no account.
The read path is identical to the authenticated one — library params, `head_commit_id`,
`changes?since=`, manifests, chunks — gated by the anonymous grant. Server
enforces read-only; the client mounts `ro`. This makes a public library a
syncable distribution channel, not just a browsable page, and it costs almost
nothing once decision #4 holds.

## Share links

### The row

A link is a credential: `silo_link_<id>_<secret><check>`, presented as
`https://host/s/{code}`. High-entropy only — no short codes, no vanity slugs;
the code is unguessable *and* recognisable to secret scanners, like every
other silo credential.

Additions to the credential model:

- `scope` extends from a library id to `library_id:path` — one format change,
  useful beyond links (a device credential scoped to a subtree becomes
  expressible for free). For E2EE libraries that path is a **ciphertext
  path** (Option A names) — consistent with `entries/`, and exactly the
  property that keeps E2EE *folder* links deferred.
- a `link_flavor` column (or a small `LinkDetail` table keyed by credential
  id): `plain` | `e2e` | `password` | `compatible`, plus `salt`,
  `wrapped_sk`, `share_manifest_id`, and an access counter.

`label`, `last_used`, `expires_at`, listing and revocation are inherited.
Endpoints:

```
POST   /api/silo/v1/libraries/{id}/links        {path, flavor, password?, expires?}
GET    /api/silo/v1/libraries/{id}/links
GET    /api/silo/v1/links                    -- all links the caller minted
DELETE /api/silo/v1/links/{link-id}
```

### One encryption, many doors

Content chunks are encrypted once, at write time, per [`storage.md`](../storage.md). A link never
re-encrypts anything; it wraps keys:

```
chunks        encrypted once under per-chunk keys K_c        (never touched)
share manifest = the file's (chunk_id, size)* public skeleton plus its K_c
                list sealed under SK — the store's manifest container with
                a different sealed payload — stored as an object the GC
                treats as a root (below), referenced by the link row
SK            = the flavor decision:
   e2e         SK travels in the URL fragment (#...) — never sent to the server
   password    SK wrapped under KEK = argon2id(password, salt) — the wrap is
               decision 7's sealed blob; SK itself stays random
   compatible  SK wrapped under link.key — a dedicated server key — in the row
```

The container inherits the store's zero-nonce discipline **structurally, not
by convention**: the sealing key is
`HKDF-SHA256(SK, salt="silo/share/v1", info=SHA-256(AD), L=32)` — AD being
the store's pinned byte range, header through the end of the public
skeleton, which carries `seal_hash = SHA-256(sealed plaintext)` per the
uniform sealing rule — zero nonce, that same range bound as AD. The same
shape as the CK path: the reader derives the key from bytes it holds
(deriving from the *sealed* plaintext would be circular — you cannot hash
what you cannot yet decrypt), and because the key hashes everything the
tag covers, seal_hash included, two distinct payloads can never meet the
same (key, nonce) pair even if an SK were ever reused.

The guardrail is unconditional: **SK is freshly random per link, in every
flavor.** The password flavor wraps SK under the password-derived KEK
rather than deriving SK itself — which also makes changing a link's
password a rewrap of sixteen-odd bytes instead of a rebuild of the share
manifest. Deriving SK from CK, a file id, or a path stays forbidden for
its original reasons. Under the uniform
derivation a reused SK is no longer a cryptographic break — two K_c lists
give two seal_hashes, two public skeletons, two keys — so the real
objection to derived SKs is blast radius and revocation: a stable share
URL means the same fragment opens every share of that file, past and
future, so one leaked link is unrevocable by construction and re-sharing
after a leak is a no-op. If stable URLs are ever wanted, they are a lookup
over link rows, never a key derivation.

The share manifest is built and uploaded by the **sharer's client** (it holds
CK; the server cannot build this for an E2EE library). It travels in the
body of the mint request, and the server holds it to the store's manifest
byte ceiling — at ~67 bytes per chunk that is generous for any single
file, and a bound stated is a bound enforced. For the compatible
flavor the client sends SK in the creation request over TLS — acceptable by
definition, since that flavor's meaning is "the server may read this file
while the link exists" — and the server wraps and stores it under
`link.key`, generated and backed up like `storage.key` but deliberately
**not derived from it**: pack encryption and content-readable link keys are
different blast radii, and coupling them would make `storage.key`'s already
impractical rotation also break every compatible link.

The library CK is never in any link's blast radius. The honest scope of a
leaked link is **one file's content, plus any chunk that file shares with
other files in the library** — K_c values are convergent, so given the
ciphertext they decrypt a shared chunk wherever it appears. The chunk route
being code-gated to the link's manifest narrows that in practice, but that
is access control, not cryptography, and the claim must not quietly upgrade.
For the compatible flavor it means the server durably holds keys that can
reach shared chunks — one more reason that flavor is the labeled exception,
never the default. (On the measured workload, cross-file dedup inside media
libraries is near zero, so the gap between "one file" and the honest claim
is usually empty — but usually is not a security property.)

**Plain-library links** need none of this: the server reads the content
anyway, so the link is purely a grant. Every plain-library link is curl-able.
The password option remains available as a plain access check: store an
argon2id verifier in the row, compare per request. Same UX, no key wrapping.

### Share manifests are GC roots

The store's mark phase traces from live commits; a share manifest hangs off a
credential row, not a commit. Without this rule the first GC after a link is
minted collects the share manifest and the link 404s. So the mark's root
set is **live commits ∪ the manifests referenced by live link rows** — the
share manifest for E2EE flavors, and for plain-library links the file's
ordinary manifest id, recorded on the row at mint time so the pin survives
the file's deletion (a plain link has no share manifest, and without this
it would break the moment the file was removed). Share manifests use the
store container (chunk ids public, K_c list sealed) precisely so the
server can trace what they pin without reading what they carry.

The converse is decided here rather than discovered in phase 5: **a live
link pins its content.** Deleting the file from the library does not break
the link — its chunks stay reachable until the link is revoked or expires,
visible in the links listing the whole time. Deleting the library deletes
its links in the same transaction.

And the pin runs both ways in time: **a link is a snapshot.** It serves the
file as it was at mint — the share manifest seals that version's K_c list,
and a plain link's row records that version's manifest id — so editing the
file afterwards does not update the link, in any flavor. That is a
decision, not an accident of the GC rule: the E2EE flavors *cannot* track
edits (the server cannot rebuild a sealed manifest; only the sharer's
client could, and silently re-sharing new content under an old URL is a
misfeature anyway), and the plain flavor matches them so there is one
behavior to explain instead of two. Say it in the UI at mint time.
Chunks kept alive only by a link count against the owner's quota — the
owner controls the link, so the owner carries its bytes.

### What each flavor honestly claims

| flavor | plain `curl`? | server can read at rest | server can read during request |
|---|---|---|---|
| e2e | no — `silo get <url>` or the share page | never | never |
| password | `curl -u link:<password>` | no | curl path: yes · browser path: no |
| compatible | yes | while the link exists | yes |

Password mechanics: the password travels in the `Authorization: Basic` header
— never in the URL, for capability-urls.md's reasons (URLs land in history,
logs, `Referer`; headers die with the request). HTTPS-only. Server-side, the
password and derived keys live in request memory and nowhere else. Browser
recipients don't send it at all: the share page fetches the sealed blob and
salt, derives the KEK client-side (argon2id WASM), unwraps SK, and decrypts
with WebCrypto —
same row, two redemption modes, the recipient's tooling decides what the
server sees.

Revocation is non-retroactive, same honesty as member revocation in
[`encryption.md`](../encryption.md): deleting the row stops the server serving
bytes (and, for compatible, destroys the only key material the server held);
a recipient who already downloaded keeps what they have.

### Redemption surface

```
GET /s/{code}                 content negotiation:
                              browser (Accept: text/html) → the share page
                              otherwise → bytes (plain/compatible),
                                          401 + WWW-Authenticate (password),
                                          406 pointing at /meta (e2e — the server
                                          cannot serve plaintext; the client must)
GET /s/{code}/meta            flavor, filename, size, salt, sealed blob / manifest id
GET /s/{code}/manifest        the sealed share manifest        (e2e, password)
GET /s/{code}/chunks/{id}     ciphertext chunks, code-gated    (e2e, password)
```

`Content-Disposition` set on byte responses. The chunk route reuses the
ordinary chunk-serving path with the link as the credential — no second
implementation. `406` and not `409` for the e2e byte request:
[`responses.md`](../responses.md) spent a section giving `409` one meaning
(a destination collision) and this is not one — `406 Not Acceptable` says
the server holds no representation it can serve, which is literally the
situation.

**argon2id on an unauthenticated endpoint is a DoS lever.** Server-side
password verification is rate-limited per code *and* per IP, with a lockout
(e.g. 10 failures → the link stops accepting the curl path for a cooldown;
the browser path is unaffected since the server does no KDF work there).
Brute force needs the code *and* the password; the code alone is already
128-bit. Two couplings with auth.md are explicit: the link KDF draws from
the **same global argon2id semaphore** auth.md sizes for enrolment — two
independent pools would sum to an unplanned memory budget — and link
redemption never touches `allowLoginAttempt` or login lockout state; its
per-code and per-IP counters are its own, for the same reason auth.md has
`Resolve` bypass login throttling. Worth saying out loud: once enrolment
derivation moves client-side and authKey verification drops to a fast hash
([`storage.md`](../storage.md)), link redemption is the **only** argon2id the server ever runs —
a pool sized to protect login now exists entirely to serve unauthenticated
share redeemers, and its sizing should be revisited under that identity.

One honesty line the lockout must not obscure: `/meta` hands the salt and
the wrapped SK to anyone holding the code — the browser path requires it —
so a code-holder can guess passwords **offline**, at their own hardware's
pace, with no server-side counter in the loop. The lockout protects only
the curl path, where the server does the KDF work. Against a code-holder,
the password flavor's real strength is argon2id's cost times the
password's entropy, nothing else. Every client-side-decrypting share
design has this property; the failure mode is forgetting it and letting
the rate-limit paragraph above imply more than it delivers. It is also
why the KDF parameters matter even though the server does no work on the
browser path.

### The share page

One embedded static HTML file — no framework, no build step, served for every
flavor at `/s/{code}`. Filename, size, a download button; for e2e it reads SK
from the fragment, for password it prompts; decrypts via WebCrypto (AES-GCM
and HKDF are native — the store's AEAD choice paying off) with argon2id as a
small WASM blob used only by the password flavor. Download only — no inline
preview, because preview is what drags in signed URLs, and those wait.

The known caveat gets written down rather than hidden: a browser recipient of
an e2e link is running crypto JS served by the same server the flavor
distrusts. Native clients (`silo get`, silo-drive) don't have this hole; the page
is a convenience tier, and the compatible flavor exists precisely because the
purity here was already imperfect.

## What not to build

- **No tokenstore resurrection.** The share surface is stateless per request
  against the credential row; no one-time redemption maps
  ([`capability-urls.md`](../capability-urls.md) owns the reasoning).
- **Passwords never in URLs or query strings, never stored** — header
  delivery, salt + sealed blob at rest.
- **Server-side decryption stays scoped to link redemption.** The compatible
  and password curl paths are the only places the server ever derives a
  content key, gated on the link row. That code must never generalise into
  `entries/` for E2EE libraries — the server-never-holds-CK rule of
  [`storage.md`](../storage.md), restated for the one place we deliberately
  punch through it.
- **No short or vanity codes.** Credential format or nothing.
- **The anonymous principal never writes** until upload links arrive with
  their own plan.
- **No per-handler permission checks.** Anything not answered by `CheckPerm`
  over the grant model is a bug, not a shortcut.
- **SK is never derived — random per link, always, in every flavor.** The
  password flavor wraps SK under a password-derived KEK; it does not derive
  SK. A derived SK makes every future share of the file open under a leaked
  fragment and makes revocation structurally impossible; dedupe links in
  the database, never in the KDF.

## Build order

1. **Grant model + roles + invites.** Grant table, `CheckPerm` unification,
   role column, invite kind + redemption flow with E2EE bootstrap. Requires
   auth.md's credential table; sequence with the account side of E2EE in [`storage.md`](../storage.md).

   **Built.** The `LibraryGrant` table with one `CheckPerm` over it, the `role`
   column, the invite kind with its `Invite` row, and both surfaces: the
   administrative invite routes with `POST auth/redeem` in front of them, and
   `GET`/`POST`/`DELETE libraries/{id}/shares` for the grants themselves.
   Whole-library grants only — a subtree grant needs a virtual library and
   nothing creates one — and owner-only management, because a grantee who could
   re-share would make a share a decision they can copy. The two halves join at
   the tombstone: a share to an address nobody has enrolled mints the inactive
   account it will belong to, and redeeming an invite to that address claims it
   and inherits what was shared there.

   Sharing an **encrypted** library answers `501` rather than minting a grant
   the recipient cannot use. That is step 3 of
   [`e2ee-completion.md`](e2ee-completion.md), and the grant row it hangs off
   now exists.
2. **Public libraries.** Anonymous grants, `public-libraries` listing, anonymous
   read across entries + manifests + chunks, per-IP rate limiting, read-only
   enforcement on the write surface. silo-drive learns credential-less `ro`
   mounts.
3. **Links, plain libraries.** `kind=link`, mint/list/revoke endpoints,
   `/s/{code}` byte serving, password-as-verifier option. Mint and revoke
   append `share.created` / `share.revoked` audit events, and every
   redemption appends `share.opened` (trace, deliberately un-rate-limited) —
   [`events.md`](events.md)'s rule that the share surface does not ship
   without its trail. The E2EE redemption paths in step 4 reuse the same
   emission points.
4. **Links, E2EE flavors.** Share manifest build in the shared client
   package, the three SK sources, `/meta` + `/manifest` + `/chunks`
   redemption, `silo get`, lockouts.
5. **The share page.** Embedded HTML + WebCrypto + argon2id WASM.

Deferred, deliberately: upload/drop-box links (design the `perm=rw` directory
scope so it's expressible; ship nothing), signed URLs (arrive with inline
media on the share page), folder links from E2EE libraries (need name keys
and per-file share manifests; file links first).

## Doc changes

When this plan is built, these documents change with it:

- `auth.md` — kind table gains `link` and `invite`; the tombstone-inheritance
  warning points at this plan's invite section as its enforcement site.
- `docs/plans/events.md` — the `share.*` events in its taxonomy are minted
  here; build-order step 3 names the emission points.
- `responses.md` — `406` gains its row: the e2e byte-request answer, "the
  server holds no plaintext to serve; go to `/meta`".
- `backup.md` — `link.key` joins the must-not-lose set beside `storage.key`,
  once `storage.key` exists ([`storage.md`](../storage.md) designs it; it is
  not built, and backup.md does not yet name it).
