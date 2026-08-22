# Plan: accounts, public libraries, and share links

Date: 2026-08-22
Status: **proposed** — depends on [`store-v2.md`](store-v2.md) (E2EE model,
convergent chunk keys, manifests) and on [`auth.md`](../auth.md)'s credential
table, which store-v2 phase 3 assumes is landed or landing. Build order at the
end sequences against both.

Scope: user accounts and roles, public-read-only libraries, and share links —
including links out of E2EE libraries, which is the part that needed design
rather than plumbing. Upload/drop-box links and signed URLs are explicitly
out (deferred, not rejected; see the end).

## Decisions

| # | Decision |
|---|---|
| 1 | Three principals — account, anonymous-via-public-grant, anonymous-via-link — resolve through **one permission path**. No ad-hoc checks in read handlers. |
| 2 | Public-read-only is a **permanent, listed grant to the anonymous principal** — the same grant machinery as a share link scoped to the library root, differing only in discovery. |
| 3 | `public_read` ⇒ server-readable library. Exclusive with E2EE at creation; converting an E2EE library to public is `silo convert`, the client-side re-encryption operation store-v2 defines — and conversion to public always starts a new history root. |
| 4 | Anonymous read on public repos covers the **chunk surface** (manifests + chunks), not just `entries/` — porter can mount a public library with no account. |
| 5 | A share link is a **Credential row**: `kind=link`, path-extended scope, `perm` ceiling `r`. Revocation, listing, labels, `last_used`, expiry, and the `is_active` account join all come from the existing model. |
| 6 | Content is encrypted **once**; link flavors differ only in where the share key SK comes from. Three flavors on E2EE libraries: **e2e** (SK in URL fragment — default), **password** (SK derived from a password, `curl -u`), **compatible** (SK wrapped to the server — plain `curl`). |
| 7 | **No password is ever stored.** The password flavor stores a salt and a sealed blob; the compatible flavor stores SK wrapped under a server key; the e2e flavor stores no key material at all. |
| 8 | A minimal **share page ships with links** — one embedded HTML file at `/s/{code}`, WebCrypto decrypt path for e2e and password flavors. |
| 9 | Accounts are **invite-only**. An invite is a credential (`kind=invite`, single-use, expiring); redemption is where E2EE bootstrap happens. Roles: `admin` / `user` / `guest`. |
| 10 | Signed URLs are **not** built here. They arrive when the share page renders media inline, per [`capability-urls.md`](../capability-urls.md), and the tokenstore redirect is never resurrected. |

## Accounts

The credential model is auth.md's; this plan adds what sits around it.

**Roles.** A `role` column on the account: `admin` / `user` / `guest`, plus
the existing `is_staff` semantics folding into `admin`. Guest cannot create
libraries and sees only what is shared to them — the "consumer deployment"
shape from [`future-features.md`](../future-features.md). A config key
`allow_user_create_repo` gates creation for `user` as well, for installs where
the admin curates everything.

**Invites.** `POST /api/silo/v1/invites` (admin) mints a credential row:
`kind=invite`, single-use, `expires_at` default 7 days, `label` = who it's
for. The invite URL is the token in the standard `silo_invite_…` format.
Redemption creates the account and is deliberately the only registration
path — and it is where E2EE bootstrap lives, because it is the one moment a
client is guaranteed present:

1. client generates the X25519 identity keypair
2. client picks the password, runs the argon2id split (store-v2): `authKey`
   goes up as the account password, `wrapKey` never leaves
3. client uploads the wrapped private key, the published public key, the KDF
   salt, and a recovery-code wrap

**Deactivation.** `is_active=false` on the account kills every lane through
the `Resolve()` join — including share links the account minted, which is a
property worth a test, not a hope: a departed user's public links must die
with the account.

## The grant model

One table answers "may this principal do `op` at `(repo, path)`":

```sql
CREATE TABLE Grant (
  id         INTEGER PRIMARY KEY,
  principal  TEXT NOT NULL,   -- 'user:<account>' | 'group:<gid>' | 'anon'
  repo_id    TEXT NOT NULL,
  path       TEXT NOT NULL DEFAULT '/',   -- subtree scope
  perm       TEXT NOT NULL,   -- 'r' | 'rw'
  created_by TEXT NOT NULL,
  ctime      INTEGER NOT NULL
);
```

The existing Seafile share tables (`SharedRepo`, `RepoGroup`) fold into this
or are read through it — decided at implementation, but the invariant is that
**`CheckPerm` consults one model**, and the credential's `perm` column remains
a *ceiling* over the grant, never a grant itself (auth.md's rule).

- **User/group shares** are `user:`/`group:` grants, exposed on the endpoints
  future-features.md already lists (`/repos/{id}/shares`, `shared-with-me`).
- **Public-read-only** is `('anon', repo, '/', 'r')` plus a `listed` bit for
  discovery: `GET /api/silo/v1/public-repos` enumerates listed anonymous
  grants, no auth required. Unlisted-but-public is a root share link instead —
  same grant, different discovery, which is the whole point of unifying.
- **Share links** mint a real Grant row — `principal = 'link:<credential-id>'`
  — created and deleted in the same transaction as the credential. The
  credential's `perm` stays a ceiling over that grant, the invariant above
  survives literally, and `CheckPerm` consults one model with no
  carried-by-credential special case.
- The anonymous principal is **never writable** in this plan. Upload links
  will change that deliberately, later, with their own abuse story.

**Write paths check too.** `blocks/missing`, chunk PUT, and manifest commit
must consult the same model — read-only means the sync surface refuses
writes, not just `entries/`.

**Anonymous lanes are rate-limited per IP** (token bucket, config caps), and
anonymous traffic is attributed to the repo owner if quotas ever grow a
bandwidth dimension. Public repos are unauthenticated bandwidth; say so in
the admin docs.

### Anonymous mount

Confirmed goal: porter mounts a public library from a bare URL, no account.
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

- `scope` extends from a repo id to `repo_id:path` — one format change,
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
POST   /api/silo/v1/repos/{id}/links        {path, flavor, password?, expires?}
GET    /api/silo/v1/repos/{id}/links
GET    /api/silo/v1/links                    -- all links the caller minted
DELETE /api/silo/v1/links/{link-id}
```

### One encryption, many doors

Content chunks are encrypted once, at write time, per store-v2. A link never
re-encrypts anything; it wraps keys:

```
chunks        encrypted once under per-chunk keys K_c        (never touched)
share manifest = the file's (chunk_id, size)* public skeleton plus its K_c
                list sealed under SK — the store-v2 manifest container with
                a different sealed payload — stored as an object the GC
                treats as a root (below), referenced by the link row
SK            = the flavor decision:
   e2e         SK travels in the URL fragment (#...) — never sent to the server
   password    SK = argon2id(password, salt) — derived per use, stored nowhere
   compatible  SK wrapped under link.key — a dedicated server key — in the row
```

The container inherits store-v2's zero-nonce discipline **structurally, not
by convention**: the sealing key is
`HKDF-SHA256(SK, salt="silo/share/v1", info=SHA-256(AD), L=32)` — AD being
store-v2's pinned byte range, header through the end of the public
skeleton, which carries `seal_hash = SHA-256(sealed plaintext)` per the
uniform sealing rule — zero nonce, that same range bound as AD. The same
shape as the CK path: the reader derives the key from bytes it holds
(deriving from the *sealed* plaintext would be circular — you cannot hash
what you cannot yet decrypt), and because the key hashes everything the
tag covers, seal_hash included, two distinct payloads can never meet the
same (key, nonce) pair even if an SK were ever reused.

The guardrail stands with its reason updated: **SK is freshly random per
link, never derived from CK, a file id, or a path.** Under the uniform
derivation a reused SK is no longer a cryptographic break — two K_c lists
give two seal_hashes, two public skeletons, two keys — so the real
objection to derived SKs is blast radius and revocation: a stable share
URL means the same fragment opens every share of that file, past and
future, so one leaked link is unrevocable by construction and re-sharing
after a leak is a no-op. If stable URLs are ever wanted, they are a lookup
over link rows, never a key derivation.

The share manifest is built and uploaded by the **sharer's client** (it holds
CK; the server cannot build this for an E2EE library). For the compatible
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
reach shared blocks — one more reason that flavor is the labeled exception,
never the default. (On the measured workload, cross-file dedup inside media
libraries is near zero, so the gap between "one file" and the honest claim
is usually empty — but usually is not a security property.)

**Plain-library links** need none of this: the server reads the content
anyway, so the link is purely a grant. Every plain-library link is curl-able.
The password option remains available as a plain access check: store an
argon2id verifier in the row, compare per request. Same UX, no key wrapping.

### Share manifests are GC roots

store-v2's mark phase traces from live commits; a share manifest hangs off a
credential row, not a commit. Without this rule the first GC after a link is
minted collects the share manifest and the link 404s. So the mark's root
set is **live commits ∪ the manifests referenced by live link rows** — the
share manifest for E2EE flavors, and for plain-library links the file's
ordinary manifest id, recorded on the row at mint time so the pin survives
the file's deletion (a plain link has no share manifest, and without this
it would break the moment the file was removed). Share manifests use the
store-v2 container (chunk ids public, K_c list sealed) precisely so the
server can trace what they pin without reading what they carry.

The converse is decided here rather than discovered in phase 5: **a live
link pins its content.** Deleting the file from the library does not break
the link — its chunks stay reachable until the link is revoked or expires,
visible in the links listing the whole time. Deleting the library deletes
its links in the same transaction.

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
salt, derives SK client-side (argon2id WASM), and decrypts with WebCrypto —
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
                                          409 pointing at /meta (e2e — the server
                                          cannot serve plaintext; the client must)
GET /s/{code}/meta            flavor, filename, size, salt, sealed blob / manifest id
GET /s/{code}/manifest        the sealed share manifest        (e2e, password)
GET /s/{code}/chunks/{id}     ciphertext chunks, code-gated    (e2e, password)
```

`Content-Disposition` set on byte responses. The chunk route reuses the
ordinary chunk-serving path with the link as the credential — no second
implementation.

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
(store-v2), link redemption is the **only** argon2id the server ever runs —
a pool sized to protect login now exists entirely to serve unauthenticated
share redeemers, and its sizing should be revisited under that identity.

### The share page

One embedded static HTML file — no framework, no build step, served for every
flavor at `/s/{code}`. Filename, size, a download button; for e2e it reads SK
from the fragment, for password it prompts; decrypts via WebCrypto (AES-GCM
and HKDF are native — the store-v2 AEAD choice paying off) with argon2id as a
small WASM blob used only by the password flavor. Download only — no inline
preview, because preview is what drags in signed URLs, and those wait.

The known caveat gets written down rather than hidden: a browser recipient of
an e2e link is running crypto JS served by the same server the flavor
distrusts. Native clients (`silo get`, porter) don't have this hole; the page
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
  `entries/` for E2EE repos — this is the keycache guardrail restated for the
  one place we deliberately punch through it.
- **No short or vanity codes.** Credential format or nothing.
- **The anonymous principal never writes** until upload links arrive with
  their own plan.
- **No per-handler permission checks.** Anything not answered by `CheckPerm`
  over the grant model is a bug, not a shortcut.
- **SK is never derived — random per link, always.** A derived SK makes
  every future share of the file open under a leaked fragment and makes
  revocation structurally impossible; dedupe links in the database, never
  in the KDF.

## Build order

1. **Grant model + roles + invites.** Grant table, `CheckPerm` unification,
   role column, invite kind + redemption flow with E2EE bootstrap. Requires
   auth.md's credential table; sequence with store-v2 phase 3.
2. **Public libraries.** Anonymous grants, `public-repos` listing, anonymous
   read across entries + manifests + chunks, per-IP rate limiting, read-only
   enforcement on the write surface. porter learns credential-less `ro`
   mounts.
3. **Links, plain libraries.** `kind=link`, scope extension, mint/list/revoke
   endpoints, `/s/{code}` byte serving, password-as-verifier option.
4. **Links, E2EE flavors.** Share manifest build in the shared client
   package, the three SK sources, `/meta` + `/manifest` + `/chunks`
   redemption, `silo get`, lockouts.
5. **The share page.** Embedded HTML + WebCrypto + argon2id WASM.

Deferred, deliberately: upload/drop-box links (design the `perm=rw` directory
scope so it's expressible; ship nothing), signed URLs (arrive with inline
media on the share page), folder links from E2EE libraries (need name keys
and per-file share manifests; file links first).

## Doc changes

- `future-features.md` — the sharing and public-links sections point here;
  the `/d/{token}/` + tokenstore sketch under "Public / link shares" is
  superseded by the credential-backed design.
- `capability-urls.md` — status note: the predicted browser-shaped consumer
  arrived (the share page); signed URLs remain deferred and the old mechanism
  remains dead.
- `auth.md` — kind table gains `link` and `invite`; `scope` format gains the
  `:path` extension.
- `encryption.md` — add the share-manifest / SK-wrapping pattern alongside
  the CK member-wrapping section; same indirection, one level down.
- `backup.md` — `link.key` joins `storage.key` in the must-not-lose set.
