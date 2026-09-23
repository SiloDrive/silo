# Protocol Compatibility

This document is the server's **contract with the clients**. It lists the
HTTP endpoints the Go fileserver implements.

**The legacy sync lane — `/repo/*`, `/api2/*`, `/api/v2.1/*`,
`/files/{token}/*` and their prefixed variants — was deleted in `5d4baa0` (0.5.0).** Every
one of those paths answers 404 now; nothing here still stubs or shims them.
See [`docs/target.md`](target.md) for why. This document covers the one lane
that remains.

It says which endpoints exist, and — under [Writing a client](#writing-a-client)
at the end — what a client built on them has to get right. For what the *status codes* mean — and which are
already spoken for — see [`responses.md`](responses.md), which is the file to
check before a new handler picks one. For what a client would want that is *not*
here, and why, see [`protocol-gaps.md`](protocol-gaps.md).

Treat this as the source of truth when a new client release starts hitting
an endpoint we don't support — find it in the "not implemented" list and
decide whether to shim it.

## Who reads what

| if you are | read |
|---|---|
| writing a new client | the endpoint reference below, then [Writing a client](#writing-a-client), then [`responses.md`](responses.md) |
| choosing a status code for a new handler | [`responses.md`](responses.md). Always, and before you write the handler |
| wondering why something is missing | [`protocol-gaps.md`](protocol-gaps.md) |

## Tested clients

- **silo** — our own Go TUI (`cmd/silo`)
- **silo-drive** — our file-access client, as a macOS File Provider extension
  and as a FUSE mount. Speaks `/api/silo/v1` and nothing else: `server-info`,
  `auth/login`, `libraries`, `account/usage`, `entries`, `changes`,
  the notification socket, and the chunk surface —
  `chunks/missing` and `PUT chunks/{id}` going up, `GET objects/{id}` and
  `GET chunks/{id}` coming back down on the FUSE build. The list summarises what
  has a client and grows as the client does; `/api/silo/v1` is the half of the
  sentence that is a limit.

## Authentication

One scheme, one table, one verification path.

| Scheme | Header | Used by | Validated against |
|---|---|---|---|
| Bearer credential | `Authorization: Bearer silo_<kind>_<id>_<secret><check>` | silo (TUI), silo-drive, `/api/silo/v1/*` | `credential.Resolve` against the `Credential` table |

The middleware is `RequireCredential`, in `fileserver/middleware/credential.go`.

`POST /api/silo/v1/auth/login` does double duty, and **the request decides
which response comes back**:

```
{"email":…, "password":…}
  → 200 {"token": "silo_session_…"}          a 24h session; unchanged, byte for byte

{"email":…, "password":…, "kind":"device",   enrolment: any of kind, client_name,
 "client_name":"SiloDrive 1.2 (macOS)",         public_key, perm or scope makes it one
 "perm":"r", "scope":"<library-id>"}
  → 201 {"credential": "silo_device_…", "expires_at": …, "email": …}
```

A device credential lives 90 days, absolute. `client_name` becomes the label an
operator revokes by and is required for enrolment. `perm` and `scope` can only
narrow, so asking for more than the account has yields what the account has
rather than a 403. `public_key` answers `501` — proof of possession is designed
and not built.

The password is an *enrolment* credential rather than a request credential:
presented once, exchanged, and not held afterwards.

Only `SHA-256(secret)` is stored, and a row is found by the public `id` in the
middle of the token, so the secret never reaches a query or a query log. The
last six characters are a checksum over everything before them, so a truncated
paste is refused as *malformed* before the database is touched. Revocation is
a row delete and takes effect on the next request; `silo token list|revoke` is
the operator's side of it.

`Authorization: Silo` is reserved for
[proof of possession](auth.md#proof-of-possession) and answers `501` until the
RFC 9421 verifier exists. `Authorization: Token` (the forty-hex legacy form)
parses but is mounted on no route.

The notification socket takes the same credential and accepts requests without
one — see `/notification` below.

A credential can carry a **ceiling**: a permission (`r` or `rw`) and a scope
(every library, one library, or one folder and everything beneath it). What a
caller may do is the account's permission intersected with that ceiling, never
the account's alone, so a narrowed credential answers `403` where the account
behind it would have been allowed. Library-wide operations — `changes` and
`commits` — and the id-addressed chunk and object surfaces are
refused to a folder-scoped credential outright, since neither can be answered
partially. A plain login mints one unscoped `rw` session; an enrolment request
mints whatever narrowing it asks for, since `perm` and `scope` can only take
access away.

A scoped credential is refused every route that names no library, because such
a route answers about the account and that is wider than the scope. `POST
/auth/logout` and `POST /auth/renew` are the two exceptions, and neither is a
special case: the subject of each is the row presenting it rather than the
account, so both are mounted on `RequireOwnCredential`, which is
`RequireCredential` without the scope check. Nothing else uses that lane.

See [`docs/auth.md`](auth.md) for the model.

### Renewing a credential

```
POST /api/silo/v1/auth/renew    Authorization: Bearer silo_device_…
  → 201 {"credential": "silo_device_…", "expires_at": …, "email": …}
```

Enrolment's shape exactly, because it answers enrolment's question — here is
your credential and here is when it dies — and one decoder should serve both.
Feature name `credential-renew`.

**No body.** `kind`, `label`, `scope`, `perm` and `client_id` are copied from
the credential presenting the request, so there is nothing a ceiling could be
widened with. A client that wants a different perm or scope is asking for a
different credential and should enrol.

**A new row, not a moved expiry.** The credential that asked goes on working
and dies on its original `expires_at`; nothing slides. Renew before the cliff
rather than after it — a credential that has already expired is refused here
like anywhere else, and the way back from that is the password.

Only a `device` credential may renew; a `session` gets `403`. A session lasts a
day because a person walked away from a terminal, and the client with nobody to
ask is the device.

### Discarding a credential, and changing a password

```
POST /api/silo/v1/auth/logout             → 200 {"revoked": 1}   this credential
POST /api/silo/v1/auth/logout/others      → 200 {"revoked": n}   all but this one
POST /api/silo/v1/auth/logout/everywhere  → 200 {"revoked": n}   all of them
POST /api/silo/v1/auth/password           → 200 {"revoked": n}   n credentials signed out
{"current_password": "…", "new_password": "…", "revoke_others": false}

GET    /api/silo/v1/account/credentials       → 200 {"credentials": [...]}
DELETE /api/silo/v1/account/credentials/{id}  → 200 {"revoked": 1}
```

**A password change revokes nothing by default, and never the credential that
asked.** `revoke_others` defaults to `false`: rotating a password is not
evidence that anything was stolen, and a client should not have to warn people
that improving a password will unmount their laptop. `true` signs the other
hosts out and keeps the caller's own row, which is the same operation
`auth/logout/others` performs.

This changed. Older servers revoked **every** credential the account held, the
caller's included, and a client that needs to know which behaviour it is
talking to should check for the `logout-others` capability rather than the
version. A client that does not see that name is talking to a server where
changing a password still signs everything out, and should say so rather than
let somebody find out.

The three logout verbs each say what they do: `logout` is this credential,
`logout/others` is the rest, `logout/everywhere` is all of them including this
one. `others` is the one a person wants after losing a laptop while sitting at
a desktop they intend to keep using.

The listing is what makes the revoke aimable: one object per credential, with
`id`, `kind`, `label`, `scope`, `perm`, `client_id`, `created`, `expires_at`,
`last_used`, `last_ua`, and `"current": true` on the row the request
authenticated with. A client renders "this device" from that flag; without it
the person is one misread row away from signing themselves out.

`expires_at` and `last_used` are **absent** rather than zero when they do not
apply — a credential that never expires, and one never yet used. Zero is a real
instant and a client rendering it says 1970. The absence of `last_used` is
worth showing: a device credential minted and never seen again is the
interesting row on the page.

The listing omits `kind: invite`. The only invite row a self-service caller can
hold is their own spent one, kept because the `Invite` table references it as
the record of an arrival; it opens nothing, and the `DELETE` answers `404` for
it. `silo token list` still shows it, because an operator is reading the record.

The `DELETE` answers `{"revoked": 1}`, plus `"current": true` when the row was
the caller's own — which is allowed, and is `auth/logout` reached by another
name. An id that is unknown, belongs to another account, or is the spent invite
is `404`, indistinguishably: the delete is owner-scoped in SQL and cannot tell
them apart, and telling them apart would answer "does this id exist on another
account". Neither route needs write permission, for the reason logout does not.
Feature name `credentials`.

Logging out needs no write permission — revocation only ever takes access away.
Changing a password needs `rw`, needs the **current** password even though the
request is authenticated, and signs out every `session` credential on the
account including the one that asked, while leaving `device` credentials
mounted. A wrong current password answers `401` and charges the login rate
limiter.

### The account's key material

Four things per account that the server stores and cannot read: the published
X25519 public key, the identity private key wrapped under a password-derived
key, that same key wrapped once per recovery code, and the argon2id parameters
the password is stretched under. See
[`auth.md`](auth.md#the-accounts-key-material).

```
GET    /api/silo/v1/account                     → 200 {account_id, email}
GET    /api/silo/v1/account/keys                → 200 {account_id, public_key, wrapped_key, kdf_params, recovery:[…]}
                                                  404 if nothing has been published
PUT    /api/silo/v1/account/keys                → 200 {"updated_at": …, "recovery": 10}
{"public_key": "<b64>", "wrapped_key": "<b64>", "kdf_params": "$argon2id$…",
 "recovery": [{"ordinal": 0, "wrapped_key": "<b64>"}, …]}

DELETE /api/silo/v1/account/keys/recovery/{n}   → 200 {"remaining": 9}

POST   /api/silo/v1/auth/kdf                    → 200 {"kdf_params": "$argon2id$…"}
{"email": "…"}
```

`account_id` is served but not accepted: it is the **holder**, the string every
wrap here is bound to as associated data, and without it the blobs beside it
cannot be opened. It rides on this response because it is only ever wanted with
them, and because it is the one fact about itself an account cannot otherwise
learn over HTTP — a new device has an address and a password, and the holder is
neither. On `PUT` it is the server's to know and not the client's to assert.

Every blob is base64. `PUT` replaces the whole set rather than merging into it,
in one transaction, because a password change re-wraps all of them at once and
a stale blob is one that still opens with the old secret. The server refuses a
publish whose `kdf_params` disagree with the parameters sealed inside
`wrapped_key` — they are one fact stored twice, and a pair that can drift makes
a blob nobody can open.

`POST auth/kdf` is **unauthenticated**, because a client needs the parameters
before it can turn a password into anything. It never answers `404`: an address
nobody holds gets plausible parameters derived from a stored server secret, the
same ones every time, because the difference between two answers would be an
account-enumeration oracle. `POST` rather than `GET` so the address does not
travel in a URL and into every log along the way. Rate limited per address at
sixty a minute.

Nothing sends the derived `authKey` yet — `POST auth/login` still takes the
password. The endpoint ships with the account model rather than after it; see
[`storage.md`](storage.md)'s split-derivation login.

### Creating an encrypted library

A different request from creating a plain one, because the server cannot build
any of it. The root directory and the initial commit are sealed under a content
key the server never holds, so both arrive with the request; and the library id
arrives with them, because `store.WrapCK` binds the library id into the wrap as
associated data and so the id must exist before the key can be wrapped.

```
POST /api/silo/v1/libraries                     → 201 {"id": …, "name": …}
{"name": "…", "e2ee": true,
 "library_id":  "<canonical lower-case hyphenated UUID>",
 "root":        "<b64 of the sealed empty root directory object>",
 "commit":      "<b64 of the sealed initial commit object>",
 "wrapped_key": "<b64 of the content key wrapped to your identity key>"}

GET /api/silo/v1/libraries/{libraryid}/key      → 200 {"wrapped_key": "<b64>"}
                                                  404 on a plain library
```

The alternative — the server mints the id, the client publishes the key
afterwards — leaves a window holding a library whose content key nobody stored.
One request, or none of it.

**The account must have published an identity key first** (`PUT account/keys`),
or the request is `409`: a content key wrapped to nothing lives on the device
that made it and dies with it.

What the server checks, and it is everything it can check without a key: each
object's id is the SHA-256 of its bytes, each decodes, both are sealed rather
than plain, the root is empty, the commit has no parents and names that root,
and the id is free. `400` names the specific failure; `409` means the id is
taken. Sending `root`, `commit`, `library_id` or `wrapped_key` **without**
`"e2ee": true` is `400` rather than ignored — a client that sent a wrapped key
and got a server-readable library would not find out until somebody read the
data.

`GET libraries/{libraryid}/key` needs read permission and nothing more: whoever
can read the ciphertext and holds this can read the library, and whoever cannot
gains nothing from a blob they cannot open. The library id is in the route, so
a scoped credential reaches its own library's key and no other.

## Endpoints

### Native management API — `/api/silo/v1/*`

JSON request/response bodies. Used by the silo TUI, the CLI in `client/`, and
the sync clients. Protected by `RequireCredential`, except the five marked
**No auth** below — they are registered above the authenticated subrouter
(`NewServer` in `fileserver/server.go`) because they are what a client needs *before* it
has a credential: one to learn what it is talking to, one to get the parameters
that turn a password into what it sends, one to get a token, one to create
the first account on a server that has none, and one to turn an invitation into
an account. `auth/logout` and `auth/renew`
are registered there too, but are authenticated: see the lane note above.

| Method | Path | Purpose |
|---|---|---|
| GET | `/api/silo/v1/server-info` | **No auth.** `{"version":"0.7.0","features":[…]}` — semver with no leading `v`, and the capability list a client should branch on instead of the version. Carries `"setup_required": true` on a server that has no accounts yet, and omits the key entirely otherwise, so a claimed server's body is unchanged from before the field existed. No chunker parameters: they belong to the library, and the libraries listing carries them. The `libraries` name says this server serves `/libraries/…`; a client that does not find it is talking to a build that predates the word and should say so rather than read the 404 that follows as an empty account |
| POST | `/api/silo/v1/auth/login` | **No auth.** Email + password → a `session` credential, or an enrolled one. Not a JWT: it names a row in `Credential` that can be revoked, labelled and narrowed |
| POST | `/api/silo/v1/auth/renew` | Mint the presenting credential's successor: no body, a full fresh lifetime, every field inherited. `device` only — a `session` gets `403`. The old credential is untouched and expires when it always would have. A scoped credential may reach it. Feature name `credential-renew` |
| POST | `/api/silo/v1/auth/logout` | Discard the credential that made the request. No write permission needed, and a scoped credential may reach it |
| POST | `/api/silo/v1/auth/logout/others` | Discard every credential the account holds **except** the one that asked. Account-wide, so a scoped credential is refused. No write permission needed. Feature name `logout-others` |
| POST | `/api/silo/v1/auth/logout/everywhere` | Discard every credential the account holds, including this one |
| POST | `/api/silo/v1/auth/password` | `{"current_password":…,"new_password":…}` — change the password. Needs `rw` and the current password. Revokes **nothing** by default, and never the credential that asked; `"revoke_others": true` signs the account's other hosts out, which is `auth/logout/others` by another name. It has meant all three things — sessions only, then everything, now nothing-unless-asked — so branch on the `logout-others` capability rather than assuming. With `"client_kdf_params":…` it is the split-derivation crossover instead: `new_password` carries an `authKey`, and hash and parameters are written together. Feature name `split-login` |
| GET | `/api/silo/v1/account/credentials` | Every credential the account holds, newest first, with `"current": true` on the one that asked. `expires_at` and `last_used` are absent rather than zero when they do not apply. Omits `kind: invite`. No write permission needed; a library-scoped credential is refused, because the subject is the account rather than the row presenting it. Feature name `credentials` |
| DELETE | `/api/silo/v1/account/credentials/{id}` | Revoke one of them → `200 {"revoked": 1}`, with `"current": true` when it was the caller's own, which is allowed. `404` — not `403` — for an id that is unknown, another account's, or the spent invite; the three are deliberately indistinguishable. No write permission needed |
| POST | `/api/silo/v1/auth/setup` | **No auth**, because it is the request that creates the first account — there is nothing to authenticate it against yet. `{"email":…,"password":…,"setup_token":…}` → `201 {"token":…}`, login's shape exactly. The address and password are the operator's choice; the setup token, printed at boot and by `silo setup-token`, is what proves they own the host. `401` for a wrong *or* malformed token, indistinguishably; `409` once any account exists. Guarded by `setup_required` above rather than by trying it |
| POST | `/api/silo/v1/auth/redeem` | **No auth**, for setup's reason: the account this activates opens no lane until it does. `{"invite_token":…,"password":…}` → `201 {"token":…}`, login's shape. The invite binds the address — there is no `email` field, and delivery to that inbox is the verification — and the password is the redeemer's choice, or an `authKey` when `"client_kdf_params"` is sent alongside, exactly as on `auth/password`. `401` for a token that is malformed, unknown, withdrawn or lapsed, indistinguishably; `409` for one already redeemed. Publish key material next, with the session this hands back. Feature name `invites` |
| POST | `/api/silo/v1/auth/device` | **No auth**, for login's reason. Starts a sign-in through the identity provider: the enrolment half of a login request — `{"kind":…,"client_name":…,"client_id":…,"perm":…,"scope":…}`, `client_name` required — and no address or password, because the IdP is who the person proves themselves to. → `200 {"user_code","verification_uri","verification_uri_complete","poll_token","interval","expires_in"}`. Show the code and the URL (or open `verification_uri_complete` locally, or render it as a QR code); keep the `poll_token`, which is the only thing that collects the result. The client never speaks to the IdP. `404` when this server has no IdP configured, `409` while it has not been set up, `429` past ten starts a minute from one address, `503` when the IdP cannot be reached or too many sign-ins are pending. Feature name `oidc` |
| POST | `/api/silo/v1/auth/device/poll` | **No auth**; the 256-bit `poll_token` is the guard. `{"poll_token":…}`. Poll at `interval` seconds. `202` still waiting for the person; `429` with `Retry-After` for polling faster than that; `200 {"credential","expires_at","email"}` — the enrolment response, **once** — when they approved and the server accepted who they are; `403` when they denied it at the IdP or the server refused the identity, with a body written for the person (no account and no invite, a disabled account, an address outside the allowed domains, an address the IdP did not vouch for); `410` when it expired, was already collected, or never existed. A `403` or `410` ends the sign-in; start again |
| POST | `/api/silo/v1/auth/kdf` | **No auth.** `{"email":…}` → the argon2id parameters that address's password is stretched under, client-side. Never `404`: an address with no account gets plausible, stable, per-address parameters, so this cannot be used to ask which addresses exist |
| GET | `/api/silo/v1/account` | `{"account_id","email"}` — who this credential belongs to. Behind any credential; nothing here is a secret. It answers before enrolment, which `account/keys` does not, and that is what it is for: `store.WrapIdentity` binds the `account_id`, so a client cannot wrap an identity key until it knows one |
| GET | `/api/silo/v1/account/keys` | The account's `account_id` — the holder its wraps are bound to — with its published X25519 public key, its wrapped identity private key, its recovery wraps and its `kdf_params`. `404` before anything is published. Readable with a `perm: "r"` credential |
| PUT | `/api/silo/v1/account/keys` | Publish all of it, replacing what was there. Needs `rw`. `400` names the specific refusal — every one is a client bug whose symptom otherwise appears on a device months later |
| DELETE | `/api/silo/v1/account/keys/recovery/{n}` | Redeem one recovery wrap; the rest of the set stands. Needs `rw`. `404` if that ordinal is already spent |
| GET | `/api/silo/v1/account/usage` | `{"usage": n, "quota": n, "kind": "logical-at-head"}` — the account's total. `quota` is absent when there is no ceiling. Feature name `usage`; per-library `size` and `file_count` are on the libraries listing, not here. See [Size and quota](#size-and-quota). `AccountUsageHandler` in `fileserver/api/api.go` |
| GET | `/api/silo/v1/libraries` | List the caller's libraries — owned, plus any granted to them through `LibraryGrant` — each with `head_commit_id`, the anchor `changes` starts from. `[]`, never `null`, for an empty account |
| POST | `/api/silo/v1/libraries` | Create a new library. `{"name":…}` for a plain one; add `"e2ee": true` and the four fields above for an encrypted one |
| GET | `/api/silo/v1/libraries/{libraryid}/key` | The library's content key, wrapped to the calling account. `404` on a plain library, and on an encrypted one nobody has shared with you |
| DELETE | `/api/silo/v1/libraries/{libraryid}` | Delete a library |
| PATCH | `/api/silo/v1/libraries/{libraryid}` | `{"name":"New name"}` — rename a library. `PATCH` because the body names only what changes |
| GET | `/api/silo/v1/libraries/{libraryid}/shares` | Who this library is shared with: the principal, the address it was shared at, `perm`, and the subtree (always `/`). Owner only — `403` for anybody else, including a grantee holding `rw`, because being able to write in a library is not being able to hand it on. Feature name `shares` |
| POST | `/api/silo/v1/libraries/{libraryid}/shares` | `{"email":…,"perm":"r"` or `"rw"}` → `201` with the grant. Owner only, and needs `rw` on the credential. Sharing again at a different `perm` replaces rather than adds — a correction is not a second fact. An address nobody has enrolled is held for them: the share mints the inactive account it will belong to, and redeeming an invite to that address inherits it. `400` for an unspellable `perm` or for sharing with yourself; `501` on an end-to-end encrypted library, where a grant with no key wrap beside it would hand somebody a library they cannot read |
| DELETE | `/api/silo/v1/libraries/{libraryid}/shares/{principal}` | Take a grant away — `user:<account-id>`, as the listing spells it. `204`, and idempotent: the caller asked for a state. Owner only, needs `rw`. The library leaves the other account's listing and its reads stop on the next request |
| POST | `/api/silo/v1/libraries/{libraryid}/batch` | `{"ops":[…]}` — many operations, one commit. See the batch surface below |
| GET | `/api/silo/v1/libraries/{libraryid}/commits` | `{"commits":[{id, created_at, author?, message?},…]}`, newest first. Pages with `?limit=`. `author` and `message` are absent under E2EE. The list ends where history ends — a collected commit stops the walk rather than erroring. Feature name `history`. `CommitsHandler` in `fileserver/api/history.go` |

#### The entries surface

One addressable noun with the HTTP methods as its verbs. This is what new
clients speak, and what `client/` speaks; the traps are under
[Writing a client](#writing-a-client), and the status codes are in
[`responses.md`](responses.md).

| Method | Path | Purpose |
|---|---|---|
| GET | `/api/silo/v1/libraries/{libraryid}/entries/{path}` | Read a file's bytes, or list a directory. Ranged; `ETag`/`304`; `Cache-Control: private, no-cache`, because both are mutable and a validator with no expiry is a cache's licence to guess one |
| GET | `/api/silo/v1/libraries/{libraryid}/entries/{path}?type=manifest` | The file's manifest — the same object `objects/{id}` serves, reachable with a path-scoped credential. See the chunk surface below |
| QUERY | `/api/silo/v1/libraries/{libraryid}/entries/{path}` | `{"ranges":[[offset,length],…]}` → the manifest **and** the chunks covering those bytes, in one framed response. The round trip that `?type=manifest` followed by `chunks/fetch` cannot avoid, because the second cannot name its ids until the first lands. Feature name `entries-ranges`; see [The range read](#the-range-read-one-round-trip) |
| GET, HEAD | `/api/silo/v1/libraries/{libraryid}/entries/{path}?at={commit}` | The same read, resolved against that commit's tree instead of the head's. `400` if `at` is not a commit id or is sent with `PUT`, `POST` or `DELETE` — history refuses writes rather than silently taking them; `410` if the commit is no longer reachable; `404` if the path is absent in that commit. Feature name `history`. `rootFor` in `fileserver/entries.go` |
| GET | `/api/silo/v1/libraries/{libraryid}/entries/{path}?type=history` | `{"versions":[{commit, created_at, id, type, size?, author?, message?},…]}`, newest first, and **only the commits where the entry's id changed** — three edits are three rows however long the history. `id` is `null` for a deletion between two versions; the commit is the one that wrote the version. `?limit=` bounds versions, not commits, and a page may come back short with a `Link` if the scan hit its cap. `400` with `at`; `403` on an E2EE library. Feature name `history`. `EntryHistoryHandler` in `fileserver/api/history.go` |
| HEAD | `/api/silo/v1/libraries/{libraryid}/entries/{path}` | The same headers as `GET`, no body. On a directory `Content-Length` is the size of the listing, not of its contents |
| PUT | `/api/silo/v1/libraries/{libraryid}/entries/{path}` | Store a file — body is the content |
| PUT | `/api/silo/v1/libraries/{libraryid}/entries/{path}?type=dir` | Create a directory (a trailing slash also works; prefer the parameter). `201` and `{id, name, type}`, the same shape every other create returns |
| PUT | `/api/silo/v1/libraries/{libraryid}/entries/{path}?type=chunks` | Store a file from chunks already uploaded — body is `{"chunks":[sha256,…]}`, no content. See the chunk surface below |
| POST | `/api/silo/v1/libraries/{libraryid}/entries/{path}` | `{"op":"move","to":"/dst"}` — moving covers renaming. `200`, no body. A destination file is **replaced**; a destination directory is `409` |
| POST | `/api/silo/v1/libraries/{libraryid}/entries/{path}` | `{"op":"copy","to":"/dst"}` — server-side copy; `201` and the source's `ETag`, no content transferred. Same replacement rule as `move` |
| DELETE | `/api/silo/v1/libraries/{libraryid}/entries/{path}` | Delete a file or directory |
| GET | `/api/silo/v1/libraries/{libraryid}/changes?since=` | Changes since an anchor (`410` when the anchor is too old) |
| GET | *any of the above* `?limit=N` | Page the answer. `Link: …; rel="next"` until the last page. See pagination below |

`{path}` is relative to the library root and never repeats the library name;
`entries/` with nothing after it is the root. Every mutating method honours
`If-Match` and `If-None-Match`; on a `move` or a `copy` the precondition is
about the *source*, which is the thing the caller looked at before deciding.

**`Range` is one range, and only on a plain library.** Feature name
`ranged-reads`. A single `bytes=` range answers `206` with `Content-Range`;
`bytes=N-`, `bytes=N-M` and the suffix form `bytes=-N` are all read, and a
range past the end is clamped to it. A **multi-range** request — two or more
ranges in one header — is `416`, not a `multipart/byteranges` response
(`parseRange` in `fileserver/entries.go`): that response format has no consumer
here, and half-implementing it is worse than not offering it. `Accept-Ranges:
bytes` is on every file response. The ranged path is arithmetic rather than
I/O planning — chunk sizes in a manifest are plaintext lengths, so the run of
chunks a range touches is computable without reading any of them.

**A read of an encrypted library through `entries/{path}` is `403`**, the
mirror of the write refusal under the chunk surface below and with the same
body shape (`errE2EEReadByID` in `fileserver/commit.go`, raised by the walk as
`objmgr.ErrNoContentKey`). The server holds no content key, so it cannot
resolve a path through names it cannot read, and what it could stream is
ciphertext a client has no way to tell from the file. Such a library is read
through [the id-addressed surface](#the-id-addressed-surface), where the client
opens the chunks itself. `GET entries/{path}?type=manifest` is refused there
for the same reason; `objects/{id}` is the route that answers.

Copy is cheap in a way worth stating plainly: the destination dirent points at
the object the source already names, so a copy costs one dirent and one commit
whether it is an empty file or a hundred-gigabyte subtree, and reads no content
at all. A client emulating it with `GET` then `PUT` pays for the content twice.

Directory creation is `PUT …?type=dir` rather than a `MKCOL`-style verb or an
RPC: `PUT` already means "make the resource at this URI have this state", and
the marker resolves the one ambiguity — a directory has no body, so without it a
bodiless `PUT` is indistinguishable from writing an empty file. Parents are not
created implicitly; a `PUT` into a missing directory is a `404`, for the same
reason WebDAV's `MKCOL` answers `409` rather than building the tree.

#### The batch surface

Feature name `batch`.

```
POST /api/silo/v1/libraries/{library}/batch
{"ops":[
  {"op":"mkdir",  "path":"/reports"},
  {"op":"create", "path":"/reports/q3.txt", "chunks":["<sha256>", …]},
  {"op":"move",   "path":"/old.txt", "to":"/reports/old.txt"},
  {"op":"copy",   "path":"/tpl.txt", "to":"/reports/tpl.txt"},
  {"op":"delete", "path":"/stale.txt"}
]}
→ 200 {"commit_id":"…","ops":5,"changed":true}   + ETag of the new root
```

**All or nothing.** The operations apply to a working tree that exists only for
the duration of the request. If any fails, nothing is written and the library is
untouched — the reply names the index, the op and the path that stopped it, with
the status code that operation would have answered on its own. Half-applied is
the one outcome a client cannot recover from, because it has no way to find out
which half.

**Ordered.** Each operation sees the ones before it, which is what makes a
`mkdir` followed by writes into it a single request rather than two.

**`create` takes chunks, not bytes.** Upload them to the chunk surface first;
this is the call that makes them a file. That pairing is the point: five hundred
files become five hundred chunk uploads — only for content the server does not
already hold — and one commit, instead of five hundred commits and five hundred
rounds of branch-head contention.

**`mkdir` of a directory that already exists is not a failure.** A batch
describes where the library should end up, and "make sure this folder exists"
has to be expressible in one request or a client is back to asking first and
racing the answer. A *file* at that path is still a `409`.

**`If-Match` applies to the library root**, whose id is the ETag `GET entries/`
already returns — so "apply only if the library is still what I read" is spelled
here the way it is everywhere else. On contention the answer is `503` with
`Retry-After` rather than a merge: merging is right for uploads, which only add,
but a batch can delete and move and a three-way merge of those against an unseen
commit is a guess.

`changed: false` means the operations left the tree exactly as it was, so no
commit was minted. Limits: 1000 operations, 4 MiB of body.

#### Pagination

Feature name `pagination`. Two things page: `GET changes` and a directory
listing from `GET entries/{path}`.

**It is opt-in and it is a header.** No `limit`, no paging — the response is
whole, exactly as before. With `limit`, the body keeps the shape it always had
and the next page arrives as an RFC 8288 header:

```
Link: </api/silo/v1/libraries/{library}/changes?cursor=eyJ2Ijox…&limit=1000>; rel="next"
```

Follow it verbatim. The cursor is opaque; it carries an offset today and may
carry a key tomorrow, and no client should need changing for that. The last
page carries no `Link`.

There is no default page size, deliberately. A truncated answer that looks
complete is the worst failure available here — a sync client would apply half a
diff and record the anchor for all of it — so a client that never asks to page
is never paged.

**A cursor pins the version it started on.** Objects are immutable, so the
cursor names the exact commit or directory object the first page was computed
from, and every later page is served from that one. Items do not skip or repeat
across a page boundary while the library is written to; the writes show up on
the client's next pass, which is the bargain the delta feed already makes.

**On `changes`, `anchor` is absent until the last page.** That is the contract
rather than an omission: a client that records the anchor whenever it is present
is correct by construction, where one told "record it only at the end" has to
remember to. Recording it early marks the client up to date for changes it has
not seen.

**A paged listing carries no `ETag` or `Last-Modified`.** A window is not the
representation the id names, and a validator there would answer `304` to a
request for a different page.

`limit` is capped at 10,000, and a bad `limit` is a `400` rather than a clamp.

What paging bounds is the response and the client's apply loop, not the
server's work: a diff is recomputed per page, because a Merkle diff is
proportional to what changed and cannot be resumed part-way. A client that wants
the server to do less should ask more often, not for smaller pages.

`GET /libraries` does not page. A library count is bounded by how many an account
has, which is tens, not by anything a client can grow without noticing.

#### The chunk surface

Feature name `chunks`. Three calls, and the shape of every resumable upload:

| Method | Path | Purpose |
|---|---|---|
| POST | `/api/silo/v1/libraries/{libraryid}/chunks/missing` | `{"chunks":[id,…]}` → `{"missing":[id,…]}` — which of these do you not already have? |
| PUT | `/api/silo/v1/libraries/{libraryid}/chunks/{id}` | Upload one chunk. `201` when stored, `200` when it was already there — re-sending is what a resumed upload does, so it succeeds rather than conflicts. `400` if the bytes do not hash to the id |
| PUT | `/api/silo/v1/libraries/{libraryid}/entries/{path}?type=chunks` | `{"chunks":[id,…]}` — create the file from them. `201` and an `ETag`, as any other write |
| GET, HEAD | `/api/silo/v1/libraries/{libraryid}/chunks/{id}` | One chunk, as stored. `ETag` is the bare id and `Cache-Control` is `private`, a year and `immutable`, because the id *is* the content hash and this representation can never change. `private` rather than `public` because the route is library-scoped: a shared cache that stored it could serve it to a requester who never passed that check |
| POST | `/api/silo/v1/libraries/{libraryid}/chunks/fetch` | `{"chunks":[id,…]}` → a chunk stream: many chunks in one framed response. Feature name `chunks-fetch` |
| POST | `/api/silo/v1/libraries/{libraryid}/chunks` | A chunk stream in, `{"stored":N,"present":M}` out: many chunks in one framed request. Feature name `chunks-upload` |
| GET | `/api/silo/v1/libraries/{libraryid}/entries/{path}?type=manifest` | A file's chunk list, addressed by path. Feature name `entries-manifest` |

The first three are the upload half and were built first. The next three are
the download half, and the gap between them was worth naming because it was the
one a client felt: a write has been able to ask "which of these do you hold?"
and send only the answer since 0.4.5, while a read had no equivalent, so a
client holding a previous version of a 1 GiB file uploaded a few chunks and
downloaded the whole file to build them.

The last is the upload half catching up on the other axis. `chunks/fetch`
closed the asymmetry in *what* could be asked for; `POST chunks` closes the one
in *how many at a time*, which was the asymmetry left: the read side had
answered many-at-once since `chunks-fetch`, and the write side, whose whole
purpose is bulk, still took one request per chunk.

An id is the SHA-256 of the chunk's bytes, so a client computes the names the
server would without asking. Where the cuts fall is the other half, and that
one is per library: the chunker is content-defined, and its parameters come
from the `chunker` object on the libraries listing. Chunking under anything else
still uploads correctly and still reads back — the server verifies bytes
against the id it was given — but the ids match nothing already in the store,
so nothing dedups. **A client that cannot read a library's parameters must
upload whole files rather than guess them**: every id would be well-formed,
every request would succeed, and the failure would be invisible.

Content-defined boundaries are what make the question worth asking at all.
Under the fixed offsets this surface started with, inserting one byte near the
front of a file shifted every boundary after it, so a re-upload matched nothing
and sent the whole file again.

The id is not taken on trust in either direction. The server hashes what
arrives and refuses a chunk that does not match the id it was offered under
(`400`), which makes a successful `PUT` an end-to-end integrity check of the
transfer as well as a store.

**Nothing exists until the last call.** Chunks are immutable and addressed by
content, so uploading them commits to nothing: no path changes, no commit is
minted, and the destination is untouched. Upload in any order, in parallel,
across restarts, over days. An interrupted upload leaves the library exactly as
it was, and the retry is the same three calls — `chunks/missing` simply returns
a shorter list the second time. That is what makes it resumable; there is no
session, no offset and no upload id to keep.

A commit naming a chunk the server does not hold is `424 Failed Dependency`,
with the missing ids in the body. It is not `400`: the request is not wrong and
the identical one succeeds once the chunks are up.

A path-addressed write on an encrypted library — `PUT entries/{path}` with
content or with `?type=chunks`, and a `create` inside `batch` — answers `403`
with a body naming the id-addressed surface (`errE2EEWriteByID` in
`fileserver/commit.go`). The server holds no content key there, so it cannot
chunk what it is given or name the entry; the client seals the chunks and
writes the objects itself. See [The E2EE client shape](#the-e2ee-client-shape).

Edits in the middle of a file are cheap. Chunking is `fastcdc-gear64/v1`
(`store/params.go`) — content-defined, 256 KiB minimum, 1 MiB target, 4 MiB
maximum. An edit shifts the boundaries around it and the rolling hash re-syncs
within a chunk or two, so a change in the middle of a 1 GB file re-transfers
single-digit megabytes rather than the file. Appends and unchanged regions cost
nothing.

Two limits remain. Dedup depends on both sides cutting at the same
places, so a client that uploads without reading the library's `chunker`
parameters produces ids that match nothing already stored — see above. And no
amount of chunk negotiation helps a file whose bytes are rewritten wholesale on
every save, which is what [`protocol-gaps.md`](protocol-gaps.md) means when it
separates a photo library from a library of VM images.

#### The chunk stream — `POST chunks/fetch`

Feature name `chunks-fetch`. `{"chunks":[id,…]}` in, and a body of framed
chunks out, `Content-Type: application/vnd.silo.chunks`. One frame is

```
id      32 bytes   the chunk's id
status   1 byte    0 present, 1 absent, 2 the terminator
length   4 bytes   big-endian, the chunk's stored length; 0 when absent
bytes    length    the chunk exactly as stored
```

Every complete body ends with a terminator frame — a zero id, status 2 and a
zero length — and a body that stops without one is truncation rather than a
short answer. The same rule going up is the first of the three refusals under
`chunks/upload` below.

The framing is `store.WriteChunkFrame` / `store.DecodeChunkFrames`
(`store/chunkstream.go`), in the shared module for the same reason the chunker
and the manifest codec are: one expression of the format, so two sides cannot
name the same bytes differently. A JSON envelope with base64 bodies would have
been the second expression.

Three things the layout decides, each on purpose:

- **The id leads, so a frame is self-describing.** A reader matches frames by
  id and not by position, which is what lets the server drop a repeated id —
  a file with a run of zeroes names one chunk many times, and sending it once
  per mention would spend exactly the bandwidth this endpoint saves.
- **Absence is a status byte, not a zero length.** A zero-length chunk cannot
  occur today, since a file with no bytes inlines instead. Inferring absence
  from a length is a rule that holds until it does not, and it fails silently.
- **The length is fixed-width.** Everything else in this format that counts
  uses a varint; a frame header wants to be a fixed offset a port can read
  without a loop, and the saving would be two bytes per megabyte.

The framing has vectors: `chunk_stream` in
[`objects.json`](../store/testdata/vectors/objects.json) commits five bodies —
a present frame, an absent one, the mixed case, a sealed chunk, and an empty
stream — as hex, with each frame's payload described rather than stored.
`chunk_stream_refused` commits seven bodies a decoder must refuse, which is the
half a port passes every valid stream without: the length checked against what
remains, the status byte against the three this version defines, the bytes
against the id they arrived under, and the body against the terminator that
says it is whole. The `ends-without-a-terminator` row is the one a port is
likeliest to pass by accident, since a decoder that simply stops at the end of
its buffer reads a cut-off response as a complete one. The sealed row is the
one worth reading twice — the bytes on the wire are the sealed frame and the id is the sealed id,
so a decoder hashing what it thinks is plaintext rejects every frame it is
sent.

At most **256 chunks** in one request, and over that is `400` rather than a
short answer — for the reason pagination is opt-in with no default, sharpened
by the fact that a client here cannot tell a chunk it was not sent from one the
store does not hold. There is no cursor: the frames are id-labelled and
verifiable, so a stream that dies part-way is resumed by asking for what did
not arrive.

Read permission, which is the opposite of `chunks/missing` beside it. That one
takes **write** because answering "yes, I hold that" to any reader is an oracle
for whether a given file exists somewhere on the server. This one hands over
the bytes, so a caller who can use the answer can already read the library and
there is no oracle left to close — `GET chunks/{id}` makes the same disclosure
one chunk at a time.

`GET chunks/{id}` remains the right call for one chunk: it is cacheable by any
intermediary, immutable, and needs no body. This is the same answer for many.

#### The batched upload — `POST chunks`

Feature name `chunks-upload`. The same framing as above, read rather than
written: `Content-Type: application/vnd.silo.chunks` going up, and
`{"stored":N,"present":M}` coming back.

It exists because the chunker targets 1 MiB, so a 1 GB file is roughly a
thousand `PUT chunks/{id}` calls — a thousand round trips to move content one
connection could stream in one. On a link with any latency that is the dominant
cost of an upload; the bytes were never the complaint.

`stored` is what the server did not already hold and `present` is what it did.
The second is not an error and is worth reading: it is a `chunks/missing`
answer that went stale under the client, which is ordinary on a library more
than one client writes. `stored + present` always equals the number of frames
sent, so the pair is also the receipt — a client that gets a smaller total has
found a server it cannot reason about and should not go on to name those ids in
an entry.

At most **256 chunks** and **256 MiB** in one request, whichever binds first,
and over either is `413` (`maxUploadChunks` and `maxUploadBody` in
`fileserver/chunks_upload.go`) — the request was too large, and the fix is to
split it. A library chunking at the format's 4 MiB maximum therefore reaches
the size limit at 64 frames rather than the count limit at 256, which is the
intended behaviour: the limit that binds should be whichever comes first, and a
client batching by bytes never meets either.

Three refusals are worth stating because each is a decision:

- **A body without its terminator is `400`,** not a short success. A sender cut
  off mid-transfer emits whole frames and stops, and nothing in the bytes says
  the count was not the intended one — only the terminator says that. The
  message distinguishes truncation from a malformed frame, because the first
  should simply be retried and the second will fail identically forever.
- **A frame marked absent is `400`.** Absence is how the *read* side says "I do
  not hold this"; going up it would have to mean something new, and a frame
  that means nothing is refused rather than skipped, so a client cannot come to
  believe it uploaded a chunk it did not.
- **A missing or wrong `Content-Type` is `415`.** The framing is not guessable
  from the bytes, so an unlabelled body would be read as an id, a status and a
  length, and would fail much further in with an error about a chunk nobody
  sent.

Chunks written before a failure stay written, deliberately. They are
content-addressed, so storing one twice is storing it once, and nothing
references them until an entry does — a client that retries asks
`chunks/missing` and gets a shorter list. That is this surface's whole resume
story, and it is why this endpoint creates nothing: the entry still arrives
through `PUT entries/{path}?type=chunks` or through `batch`.

`PUT chunks/{id}` remains the right call for one chunk, and remains the shape a
retry takes. This is the same answer for many.

#### The manifest — `GET entries/{path}?type=manifest`

Feature name `entries-manifest`. The body is the encoded `store.Manifest` —
byte for byte what `GET objects/{id}` returns for the same id, which is the
point: a client implements `store.DecodeManifest` once and reaches it two ways.

**The reason both spellings exist is scope**, not convenience. The id-addressed
surface is library-level, because an id says nothing about where it is linked
and so cannot be checked against a narrowed credential. Without this route a
folder-scoped credential could read a file's bytes and not its chunk list —
able to download a gigabyte to change a byte, and not able to avoid it.

**The `ETag` is the bare id**, not the `v1-` prefixed tag the rest of the
entries surface carries, and that is deliberate. The prefix versions the
*representation*: a listing's JSON shape can change under a fixed id, so a
cached listing needs a tag that moves with it. A manifest cannot — the id is
the hash of exactly these bytes, so a different encoding is a different id — and
the same object at `objects/{id}` has to validate identically or a client
caching both spellings holds two entries for one thing.

`Cache-Control` is `private, no-cache`, and the `immutable` the id-addressed
spelling carries is deliberately withheld here. `immutable` is a claim about
the URL, not about the bytes: replace the file and `entries/{path}?type=manifest`
means something else, and RFC 8246 `immutable` tells a cache not to revalidate
even on an explicit reload — so promising it on a path strands a stale manifest
whose chunks GC may since have reclaimed. Revalidating costs one dirent lookup
and reads no chunks, so `no-cache` buys the correctness for almost nothing.

**The trap, stated because it is silent:** do not feed this tag back as
`If-Match` on `entries/{path}`. That precondition compares against the `v1-`
form, so it would fail every time for a reason nothing in a log would explain.
Send back only tags you were given for the resource you are writing.

A directory answers `400` — it is a real entry that has no manifest, and `404`
would be a claim about the library rather than about the request. Under 64 KiB
the bytes *are* the manifest (`store.Inlined`), so the fetch that would have
been a round trip of overhead is the read.

#### The range read, one round trip

**`QUERY entries/{path}` answers the manifest and the covering chunks
together.** Feature name `entries-ranges`.

```
QUERY /api/silo/v1/libraries/{library}/entries/{path}
Content-Type: application/json

{"ranges": [[0, 4096], [5011283968, 4096]]}
```

```
200 OK
Content-Type: application/vnd.silo.ranges

<manifest frame> <chunk frame> … <terminator>
```

The body is [the chunk stream](#the-chunk-stream--post-chunksfetch) exactly — the same
framing `POST chunks/fetch` answers with, the same `store.DecodeChunkFrames`
reading it, the same per-frame hash check — with **the manifest as the first
frame**. That works without a new frame type because a manifest id is the same
SHA-256 over the same kind of bytes a chunk id is, so the manifest arrives
self-describing and verifiable like everything behind it. It is the one frame
you cannot match by id, because its id is the thing you did not have; take it
by position, then match the rest by id as usual.

The media type is `application/vnd.silo.ranges` rather than
`application/vnd.silo.chunks`, and the difference is the contract rather than
the format: the chunks type promises chunks, and a client that read this
response under that name would file the manifest away as one.

**Why it exists.** Reading part of a file you do not hold takes two requests
that cannot overlap: `GET entries/{path}?type=manifest` to learn which chunks
cover the bytes, then `POST chunks/fetch` to get them. The second cannot name
its ids until the first lands, and the first fetches nothing — it asks where to
look. On the host that motivated this, that is 165/312/738 ms: about a third of
a second of pure latency in front of every cold file, spent before anything
appears on screen. A preview making three cold range reads goes from four round
trips to three; a thumbnailer reading one header goes from two to one.

**The rules.**

- **Ranges are `[offset, length]`, half-open, and in bytes of the file** — not
  the inclusive first/last pair a `Range` header carries. They may overlap and
  need not be sorted; the answer is each covering chunk once, in the order the
  manifest lists them.
- **At most 256 ranges, touching at most 256 chunks.** Both are `400` rather
  than a short answer, for the reason `chunks/fetch` caps rather than
  truncating: a response that looks complete and is not is worse than one that
  did not happen. The chunk cap is counted after the ranges resolve.
- **An offset or length below zero, and a length of zero, are `400`.** A client
  that computed one has a bug, and an empty answer is how that bug reaches
  production.
- **A range running past the end is clamped; one wholly past it is answered
  with the manifest and no chunks, not `416`.** The manifest in the same
  response is what says how long the file actually is, so the client learns the
  answer rather than a status — which nothing else in the response could have
  told it.
- **A file small enough to inline answers in one frame.** Its bytes are in the
  manifest, so a `QUERY` on it is the whole file in one round trip.
- **A directory is `400`**, not `404`: it has no manifest, so this is the wrong
  question rather than a missing path.
- **`?at={commit}` works**, resolving against that commit's tree exactly as the
  `GET` does.
- **An E2EE library is `403`**, for the reason the `GET` beside it is —
  the server cannot resolve a path through names it has no key to read.
- **A manifest too large to frame is `501`.** A frame carries 64 MiB and a
  manifest may legally reach 1 GiB, so at roughly 67 bytes per chunk the two
  part company somewhere around a terabyte of file. Nothing is broken and a
  retry will not help: read that file's manifest with `?type=manifest`, which
  streams, and its chunks with `POST chunks/fetch`.

**`QUERY`, not `POST`.** `POST entries/{path}` is taken and means mutate — it
is the move and copy verb. A safe, idempotent read on that same route,
distinguished only by its body, is what `QUERY` exists to avoid; the method
went to RFC in June 2026. It is not a `Range` header either: that header is
scoped to the representation being returned, so here it would have to mean
*part of the manifest*. Asking for two representations of one resource is what
makes this a method and a body.

**It is path-addressed on purpose.** The obvious shape takes a manifest id, and
would be unusable by the client that asked for this: `objects/{id}` is refused
to a credential narrowed to a subtree, because an id says nothing about where
it is linked. Same reasoning that put `entries/{path}?type=manifest` beside
`objects/{id}`.

**What it deliberately does not do** is bundle "the first chunk" with the
manifest. That is a bet on the read being at offset 0 — fair for a header,
wrong for a seek into the middle or for an MP4 whose `moov` atom sits at the
tail, which are exactly the reads this exists for. The covering chunks are the
answer; the first chunk is a guess.

**If the server is too old to have it,** issue `GET entries/{path}` with a
`Range` and `GET objects/{id}` **concurrently** and pay `max()` rather than
`sum()`. `ranged-reads` has always worked. What that loses is not verification
— it is reuse, since the ranged bytes arrive under no id and cannot be used for
the next range. Check the feature name rather than calling and reading the
`404`: probing costs the round trip you were trying to save.

**On the second range of the same file you already hold the manifest**, so
`POST chunks/fetch` is the call — this endpoint is for the cold read.

#### The id-addressed surface

Feature name `objects`. How a client reads and writes a library the server
cannot read — and, on a plain library, the second of the
[two read paths](chunking.md#two-read-paths-both-correct).

| Method | Path | Purpose |
|---|---|---|
| GET, HEAD | `/api/silo/v1/libraries/{libraryid}/objects/{id}` | One manifest, directory or commit, exactly as stored |
| PUT | `/api/silo/v1/libraries/{libraryid}/objects/{id}` | The same, verified against its id and checked to decode. `201` stored, `200` already there |
| GET, HEAD | `/api/silo/v1/libraries/{libraryid}/chunks/{id}` | One chunk, as stored |
| PUT | `/api/silo/v1/libraries/{libraryid}/chunks/{id}` | One chunk, verified against its id |
| PUT | `/api/silo/v1/libraries/{libraryid}/head` | Compare-and-swap on the branch head; `If-Match` the current commit id |

It exists because of one fact: the server holds no content key for an E2EE
library, so it cannot chunk a file, build a manifest or name an entry. Every
write there is something the client computes and the server merely stores. The
surface is deliberately the same for both library types, so a client that
implements it needs no second implementation for the plain case.

What the server verifies is short because it is everything it can: that an
object's id is the SHA-256 of the bytes offered under it, and that those bytes
decode as one of the store's object kinds. It cannot check that a manifest
describes a real file or that a commit says anything true. The decode check
earns its place by refusing to store something that would break the server's
own later walks — GC's mark and `changes?since=` both read public sections.

`ETag` is the bare id and `Cache-Control` is `private`, a year and `immutable`
on both `GET`s: the id is the content hash, so the representation under that URL
can never change. `private` rather than `public` because reaching the route
required library authorization, and `public` would license a shared cache to
hand the stored response to anyone who asks.

**Refused to a folder-scoped credential**, as every id-addressed call is. Such a
client reads structure through `entries/{path}` and a chunk list through
`?type=manifest`.

#### Reading `changes`

`{"anchor": "<commit>", "changes": [...]}`, where each change is
`{op, path, old_path?, id?, size, is_dir}` and `op` is `create`, `delete`,
`modify` or `move`. The anchor comes back even when nothing changed, so a
polling caller can always advance — with the one exception that if you asked to
[page](#pagination), it is absent on every page but the last. `old_path` is set
for moves, and a rename is a move — compare the parent directories if you need
to tell them apart.

It is a **net diff between two trees**, not a replay of what happened. A file
created and then renamed twice arrives as one create at its final path; a file
created and deleted between the two anchors does not arrive at all. That is the
right shape for reconciliation and the wrong shape for an audit log.

The consequence worth designing around: **one batch can carry two operations for
the same path**, distinguishable only by `is_dir`. Deleting a directory `/Z` and
creating a file called `Z` between two anchors produces

```json
{"op": "create", "path": "/Z", "is_dir": false}
{"op": "delete", "path": "/Z", "is_dir": true}
```

A client keyed on path alone will apply those in some order and can delete the
file it just created. Key on `(path, is_dir)`, or carry stable identifiers of
your own.

#### Removed in 0.4.4

`dir/`, `mkdir`, `file`, `download`, `rename` and `move` were the pre-entries
shape: verbs in paths, one resource under two names, the path passed as a query
parameter. Every one of them had an equivalent on the entries surface, so they
were duplicates rather than capabilities, and they are gone.

| removed | call instead |
|---|---|
| `GET /libraries/{libraryid}/dir/?path=` | `GET libraries/{libraryid}/entries/{path}` on a directory |
| `POST /libraries/{libraryid}/mkdir` | `PUT libraries/{libraryid}/entries/{path}?type=dir` |
| `DELETE /libraries/{libraryid}/file?path=` | `DELETE libraries/{libraryid}/entries/{path}` |
| `GET /libraries/{libraryid}/download?path=` | `GET libraries/{libraryid}/entries/{path}` — streams on the same response |
| `POST /libraries/{libraryid}/rename` | `POST libraries/{libraryid}/entries/{path}` with `{"op":"move"}` |
| `POST /libraries/{libraryid}/move` | `POST libraries/{libraryid}/entries/{path}` with `{"op":"move"}` |

`download`'s `302` to `/files/{token}/{name}` is the one behaviour that does not
survive verbatim, and its removal was already the plan: this lane has no
browser-shaped consumer, and for a client that sets a bearer header anyway a
capability URL is overhead — for a FUSE client, two round trips per read. See
[`capability-urls.md`](capability-urls.md), which is the decision; this removes
the last route that still contradicted it.

`POST /api/silo/v1/access-tokens`, which minted the token that redirect used,
is gone too. It outlived the route that redeemed it and went on handing clients
a string nothing would accept. If a browser-usable URL is ever wanted here, the
thing to build is a signed URL rather than a stateful token —
[`capability-urls.md`](capability-urls.md) is the decision and the design.

### Change notifications — `WS /notification`

A WebSocket, served in-process when `EnableNotification` is set (it is, by
default). Clients subscribe per library and receive `library-update` when a commit
lands, which is what lets a sync client react in about a second instead of
polling.

#### Subscribing with the credential you already have

Since 0.5.0 this is the only way to subscribe, and the subscribe frame carries
no token. It is authorized by the `Authorization` header the handshake
presented — the same credential, the same permission check
(`middleware.PermFor`) and the same ceiling as every other route. Advertised as
`notifications-credential`. There is no token to fetch, so there is nothing to
re-mint and nothing to expire.

```json
{"type": "subscribe", "content": {"libraries": [{"id": "<library>"}]}}
```

A subscribe this lane will not grant is answered
`{"type": "subscribe-denied", "content": {"library_id": …}}` — the frame that
replaced `jwt-expired`, because there is no token to renew and a client told
otherwise would re-mint in a loop. It names the library and not the reason;
"you may not reach it", "no such library" and "you sent no credential" are one
answer on the wire, as they are on the HTTP lane.

Access is re-checked about once an hour for the life of the socket. A share
withdrawn under a live subscription ends it with the same `subscribe-denied`.
That check is what replaces the token's expiry: a credential need never expire,
so nothing else would bound how long a subscription outlives the access that
authorized it.

The socket must have authenticated, and it must do so at the handshake: a
`/notification` upgrade that presents no credential is answered `401` before
it becomes a socket, and one that presents a bad credential is answered `401`
rather than downgraded to anonymous. There is nothing an anonymous socket
could subscribe to, so there is nothing for it to hold open.

A **scoped credential may open the socket**, unlike every other route that
names no library: the upgrade answers nothing, and each subscribe is checked
against the credential on its own. A credential cut to one library is granted
that library and denied the rest. A credential cut to a *path* inside a library
is denied even its own on this lane, because a `library-update` names a commit
and "what changed in this library" cannot be answered partially — the same rule
that refuses it `changes`. What such a credential gets instead is the account
ring below.

#### Subscribing to the account

Since 0.5.0 one frame subscribes a socket to every library its account can
see — owned, or shared whole with it or with a group it is in, which is exactly
the set `GET /libraries` answers with. Advertised as `notifications-account`.

```json
{"type": "subscribe", "content": {"account": true}}
```

It is authorized by the credential the handshake presented, which by then is
the only thing the socket has: an anonymous connection never reaches this
frame, having been refused at the upgrade.

What arrives is a **bare ring**:

```json
{"type": "account-update", "content": {}}
```

It rings for a commit to any library in the set, and for the four things the
per-library lane never pushed: a library created, one shared with the account,
one renamed, one deleted or unshared. The rename matters most — it is one
catalog UPDATE and mints no commit, so no head moves and `library-update` has
nothing to say — and a client that carried each library's name in its anchor
to notice renames on its own can stop.

The frame carries nothing on purpose. The ring says the set moved;
`GET /libraries` says what, and its `head_commit_id` per row is what a client
compares against its anchors to learn which libraries moved — one request,
not one `changes` call per library. Because the frame carries nothing, it
needs no lease: nothing in it was authorized, and the fetch it provokes is
authorized on its own. Under a burst the ring is the better frame too: thirty
commits across an account collapse into one ring and one listing.

**The credential decides which ring answers the frame.** An unscoped
credential gets the account's set. A credential cut to one library — or to a
path inside one — gets that library's ring and nothing else, for the same
frame: the scope already names the one library it may watch, and a ring tells
it nothing but "look". A client does not choose between the two rings, which
is why one feature name covers both.

The socket re-checks itself every five minutes: the credential is re-read and
the set re-resolved, so a library that appears or leaves by a path this server
did not see — an operator's command, a second server on the same database —
rings within that interval rather than never. A credential revoked, expired,
disabled, or whose scope has changed closes the socket at the same tick; the
client reconnects and is answered by the credential it now holds. That is
hygiene rather than the authorization boundary, which is the pull the ring
provokes.

#### Removed in 0.5.0 — the minted token

`POST /api/silo/v1/libraries/{id}/notify-token` and the `jwt_token` field in a
subscribe entry are gone. The endpoint answers `404`, a `jwt_token` in a
subscribe entry is ignored, and no `jwt-expired` frame is ever sent.

It was the original lane, added in 0.4.4: one call minted a per-library HS256
token, `aud=silo:notif`, 72 hours, and the client carried it inside each
subscribe entry. It existed because the socket could not ask a credential what
it was allowed to watch. `middleware.PermFor` answers that question directly,
so the token was minted from a permission check and then checked instead of
the permission — a copy of an answer the server already had, with an expiry
bolted on to bound how stale the copy could get.

Withdrawing it is the request that asked for it
([`feature-req/notify-token-on-the-silo-lane.md`](feature-req/notify-token-on-the-silo-lane.md))
taken one step further rather than reversed. What that request wanted was to
stop speaking a second protocol to get a socket: one lane, one credential, one
call. Removing the mint is the last call on that lane going away.

`notifications` keeps its name. The name means the socket exists and is served
— which is what a client branches on, and it is still true. `notifications-credential`
keeps its name too, and stays worth checking: it is what tells a client the
tokenless subscribe will be understood, and a server old enough to lack it is
a server that still wants a token.

**If you still mint tokens:** stop, and delete the code. Check
`notifications-credential`, send `{"libraries": [{"id": …}]}` with no
`jwt_token`, and set `Authorization` on the upgrade request itself — the
handshake now requires it. `subscribe-denied` replaces `jwt-expired`, and it
means re-check access, never re-mint.

#### Frames

Inbound frames are `{"type": "library-update", "content": {"library_id": …, "commit_id": …}}`,
`{"type": "account-update", "content": {}}` and
`{"type": "subscribe-denied", "content": {"library_id": …}}` or
`{… "content": {"account": true}}`; unknown types are ignored rather than
closing the socket. `unsubscribe` takes the same frame shape as `subscribe`.
The server pings every 30s and drops a client that has not ponged within 90s;
most WebSocket libraries answer pings for you.

An update that cannot be delivered because a client is behind is **deferred,
not dropped**: the server remembers the latest commit id per library and sends
it as an ordinary `library-update` as soon as the client's queue moves.
Repeated misses for one library collapse into the newest. Nothing extra is
needed on the client side to receive this — it is the same frame.

### Debug middleware

The `-debug` flag wraps the whole server in `middleware.DebugLogger`,
which logs `HTTP <METHOD> <PATH> -> <STATUS> (<DURATION>) from <REMOTE>`
for every request and flags 404s as `WARN`. Off by default.

## Not implemented (intentionally)

Anything outside `/api/silo/v1/*` and `/notification` is a 404, including
every path listed in older revisions of this document under the
legacy sync lane — see the note at the top. That includes
`/protocol-version` itself: no route mounts it and no handler answers it.

## Divergence policy

We track our own **protocol**, not any upstream codebase. When
`/api/silo/v1` gains an operation, it lands here in the same commit — see
[`docs/plan.md`](plan.md) for the migration record and
[`docs/target.md`](target.md) for what this server is aiming at now.

## Writing a client

The reference above says what each call does. This part says what a client
built on it has to get right — the things the wire does not say, ordered
roughly as a client meets them. The status codes are in
[`responses.md`](responses.md); nothing here restates a table.

### Start with `features`, not the version

`GET server-info` is the first request, before any credential exists. Branch on
a name in `features`, never on a version range and never by probing behaviour:
a name is added in the release its capability ships and is never removed or
reused, so `has("entries-copy")` stays a safe question forever, and an older
server that sends no list reads as "no features", which is the correct answer
— absent means do not call it. `notifications` is the one name that depends on
how the server was started rather than on which build it is; seeing it is how
a client knows the socket exists at all.

The whole vocabulary this build publishes, in the order `features()` emits it
(`fileserver/api/api.go`). A name here is a promise; the endpoints it covers
are documented above.

| name | what it says this server has |
|---|---|
| `libraries` | `/libraries/…`, and `library_id` in every payload |
| `entries` | the entries surface — one addressable noun, HTTP methods as its verbs |
| `entries-copy` | `POST entries/{path}` `{"op":"copy","to":…}` |
| `conditional-writes` | `If-Match` / `If-None-Match` on every mutating method |
| `ranged-reads` | `Range` on `GET entries/{path}`, plain libraries only |
| `changes` | `GET libraries/{id}/changes?since=` |
| `library-rename` | `PATCH libraries/{id}` |
| `chunks` | `chunks/missing`, `PUT chunks/{id}`, `PUT entries/{path}?type=chunks` |
| `objects` | `GET`/`PUT objects/{id}`, `GET`/`HEAD chunks/{id}`, `PUT head` |
| `chunks-fetch` | `POST chunks/fetch` — many chunks, one framed response |
| `entries-manifest` | `GET entries/{path}?type=manifest` |
| `chunks-upload` | `POST chunks` — many chunks, one framed request |
| `pagination` | `?limit` on `changes` and directory listings, `Link: …; rel="next"` |
| `batch` | `POST libraries/{id}/batch` — many operations, one commit |
| `usage` | `GET account/usage`, and `size`/`file_count` on the libraries listing |
| `logout` | `POST auth/logout`, `POST auth/logout/everywhere` |
| `password-change` | `POST auth/password` |
| `credentials` | `GET account/credentials`, `DELETE account/credentials/{id}` |
| `logout-others` | `POST auth/logout/others`, and `revoke_others` on `auth/password` — its absence means a password change still signs everything out |
| `credential-renew` | `POST auth/renew` |
| `account-keys` | `GET`/`PUT account/keys`, `DELETE …/recovery/{n}`, `POST auth/kdf` |
| `split-login` | an `authKey` on `POST auth/login`, `kdf_params` on `POST auth/password` |
| `e2ee-libraries` | `POST /libraries` with `"e2ee": true`, `GET libraries/{id}/key` |
| `setup` | `POST auth/setup`, and `setup_required` on `server-info` |
| `invites` | `POST auth/redeem`, and the admin invite routes behind it |
| `oidc` | `POST auth/device`, `POST auth/device/poll` — **only when the server has an identity provider configured** |
| `shares` | `GET`/`POST libraries/{id}/shares`, `DELETE …/shares/{principal}` |
| `history` | `GET commits`, `GET entries/{path}?at=`, `GET entries/{path}?type=history` |
| `entries-ranges` | `QUERY entries/{path}` — the manifest and the chunks covering a byte range, one framed response |
| `notifications` | `WS /notification` — **only when the server was started with it** |
| `notifications-credential` | subscribe with no `jwt_token` |
| `notifications-account` | subscribe with `{"account": true}`, and `account-update` |

`oidc` is conditional on `[oidc]` being configured — not on the IdP answering,
so a client shows the sign-in screen during an outage and reports the outage
when it meets it. The last three are conditional on `option.EnableNotification`.
Every other name is a property of the build.

Check `notifications-credential` before subscribing without a `jwt_token`.
This is the one place where guessing wrong is expensive rather than merely
wrong: a pre-0.5.0 server reads a missing token as a bad one and answers
`jwt-expired`, which a client cannot tell from the token it did send having
lapsed — so it mints a fresh one, sends it, and is told the same thing again.
Seeing the name is what lets a client delete its notify-token code; not seeing
it is what tells it to keep it, because that server still mints them.
`notifications-account` is the same shape one
step further: seeing it is what lets a client send `{"account": true}` and
stop subscribing per library at all, and not seeing it is the difference
between a server with no account mode and an account with nothing happening.

`version` is semver with no leading `v`. A build from an untagged or dirty tree
keeps its suffix — `0.5.0-3-gabc1234`, `0.5.0-dirty` — so parse the leading
`major.minor.patch` and ignore the rest, and strip a leading `v` anyway; it
costs one line and turns a silent failure into a clear one.

### The credential on the device

Ask for a `device` credential at domain setup, not a session: it lasts 90 days
against 24 hours and carries the label an operator revokes by. Call
`POST auth/logout` when the user removes the account, or the credential stays
live for the rest of its 90 days on a machine that has stopped using it.

**Do not architect around a password at rest.** The password is an enrolment
credential, presented once and discarded, and the identity key in the platform
key store is what a device holds; [`auth.md`](auth.md#proof-of-possession) and
[`storage.md`](storage.md)'s split derivation go on from there. Keep the
credential behind a narrow interface — something that answers *authenticate
this request*, not *give me the password* — so the swap is a new implementation
rather than an unwind.

**`POST auth/renew` is what makes that advice affordable**, and a client with no
window has to check for it. Ninety days is not long enough to hold a credential
for the life of an install, and until renewal existed the only way to the next
one was the password — so a File Provider extension, which has no UI to ask in
and does not own its own lifecycle, had to keep the password on the device
forever or stop working on day 90. Renew somewhere inside the lifetime rather
than on a `401`: past the expiry there is no live credential left to ask with.
A server that does not name `credential-renew` in `features` is one where the
password is still the only route, and that is a decision to make at enrolment
rather than on the morning it lapses.

If several requests are in flight when a credential expires they all `401` at
once. Collapse that into one re-login rather than a stampede; `client/` does it
by recording which token a caller observed and re-logging in only if it has not
already been replaced.

### Paths on the wire

`{path}` is part of the URL, not a query parameter. Escape it **per segment**:
separators must survive as separators or the route stops matching, and
everything else must be escaped or a file named `awkward name?.txt` truncates
the request at the `?`. In Swift, build it with `URLComponents` and set `path`
(not `string`), or percent-encode each component with a character set that
excludes `/`. Do not use a query-string escaper: it encodes a space as `+`,
which is wrong in a path.

Prefer `?type=dir` to a trailing slash for the same reason. `path.Join`,
`path.Clean` and `url.PathEscape` all lose or mangle the slash, proxies
normalise it, and a bare `PUT` that has lost its marker stores an empty *file*
— silently, with a `201`. A query parameter survives all of that and says the
same word `type` that the listing says back.

The library root cannot be moved or deleted (`400`), and creating it is a
`409`. Deleting a library is `DELETE libraries/{libraryid}`, a different
operation from emptying one.

### An id is not an identity

A directory listing:

```json
[
  {"name":"a.txt", "type":"file",
   "id":"f7eef0341b337c218dce12bb06c55e52bf98f863a0a749097682016e3ae84cb1",
   "size":2,"mtime":1787578220},
  {"name":"empty", "type":"dir",
   "id":"fb50dc0717ff266cf9baf82b1ce7a1c2ef6d9247859680b11a19fb7077f5f222",
   "mtime":1787578220},
  {"name":"sub",   "type":"dir",
   "id":"fb50dc0717ff266cf9baf82b1ce7a1c2ef6d9247859680b11a19fb7077f5f222",
   "mtime":1787578220}
]
```

Those are the only keys: a file row carries `id`, `mtime`, `name`, `size` and
`type`; a directory row the same without `size`. Pinned by a test, so the
example and the wire cannot drift apart.

**Two of those rows share an id, and that is the format working.** `empty` and
`sub` are both empty directories, so they are the same object. An id names
content, never an item: two paths holding identical bytes share one, and a
file edited back to its previous content returns to the id it had. If you need
a handle that survives a rename and stays distinct between two identical files
— and macOS requires exactly that — the id cannot be it. Keep your own mapping.
As a *cache* key it is not merely safe but ideal, since two identical objects
sharing one entry is the point.

Objects are typed, so an empty directory and a zero-byte file hash
differently; there is no sentinel id for either, and a client should carry no
constant for one. An empty library's root listing reports the empty
directory's id as its `ETag`, because the root *is* an empty directory —
consistent rather than special.

### `HEAD`, and how to get attributes cheaply

`HEAD entries/{path}` on a **file** gives `getattr` everything it needs:
`Content-Length` is the size, `Last-Modified` the mtime, `ETag` the content
hash. On a **directory** `ETag` and `Last-Modified` are correct but
**`Content-Length` is the size of the listing JSON**, not of the directory.
Never take a size from a directory `HEAD`. The library root has no dirent of
its own, so it carries an `ETag` and no `Last-Modified`.

Two things matter more than `HEAD` itself:

- **`HEAD` on a directory is not cheaper than `GET`.** The listing is still
  built; only the body is discarded. What makes it cheap is `If-None-Match`,
  which answers `304` before any of that work happens.
- **Do not `HEAD` each child to populate attributes.** The parent's listing
  already carries `name`, `type`, `id`, `size` and `mtime` for every entry, so
  one readdir fills the whole attribute cache. Revalidate the parent with
  `If-None-Match` and N `HEAD`s collapse into one conditional `GET` that
  usually `304`s.

Every `GET` and `HEAD` sets `ETag: "v1-{id}"`. Send it back as `If-None-Match`
and an unchanged object answers `304` having read no chunks — the check costs
one dirent lookup in the parent. This is the cheapest question in the API; ask
it often. Treat the whole quoted string as opaque and never parse the id out of
it: the `v1-` prefix versions the representation, and it changes if the listing
JSON ever changes shape, which is exactly what stops a client validating a
cache entry against a body format that no longer exists. The id maps straight
into `NSFileProviderItemVersion.contentVersion`.

**Revalidate; do not assume.** Every authenticated response carries a
`Cache-Control`, and unless the route says otherwise it is `private, no-cache`.
That is not a request to stop caching — `no-cache` means store it and ask
before reusing it, which with an `ETag` that is a content hash costs one dirent
lookup and no body. It is stated explicitly because the alternative is not "no
caching" but a cache inventing a freshness lifetime of its own: RFC 9111 §4.2.2
licenses a heuristic whenever there is a validator and no explicit expiry, and
the conventional one is a tenth of the document's age. A listing served that
way once hid a newly added file for three days — the delta named it correctly,
the client listed its parent to get an mtime, CFNetwork answered from a body
stored days earlier, and the child was absent from it, so the change was
dropped as a path the listing does not mention and the anchor advanced past the
commit. Nothing asks for that delta twice.

The exception is the id-addressed surface. `objects/{id}` and `chunks/{id}` are
content-addressed, so they carry `private, max-age=31536000, immutable` and a
client that holds those bytes need never ask again.

### What a write hands back, and how to check it

A `PUT` answers `201` with the new `ETag` and a body naming the entry:

```json
{"id":"b8d0fa06c1e4a2f7d9b3e5c8a1f4d7b0e3c6a9f2d5b8e1c4a7f0d3b6e9c2a5f8","name":"greeting.txt","size":14,"type":"file"}
```

**That id is the file's manifest id, not a hash of the bytes you sent.** A
client that hashes as it uploads computes the wrong thing and mismatches on
every file, including the ones that transferred perfectly — so do not read a
mismatch as corruption until the check itself is right.

Computing it means building the manifest, which a client holding `store/` can
do: chunk the content under the library's **own** `chunker` parameters from
its listing row, take each chunk's id as the SHA-256 of its bytes, encode the
manifest, and hash that. A file under 64 KiB never reaches the chunker — its
bytes are inlined into the manifest — so the id is still the hash of a
manifest and never of the bytes alone. Done that way it is a complete
end-to-end integrity check for the transfer, and it costs a comparison;
`fileserver/manifest_id_test.go` runs exactly this against a live server for
an inline and a chunked file.

**Every create answers the same shape.** A client can decode one struct for
all of them rather than special-casing the verb it used:

| call | status | body | `ETag` |
|---|---|---|---|
| `PUT entries/{path}` | `201` | `{id, name, type, size}` | the new manifest id |
| `PUT entries/{path}?type=chunks` | `201` | `{id, name, type, size}` | the new manifest id |
| `PUT entries/{path}?type=dir` | `201` | `{id, name, type}` | the new directory's id |
| `POST {"op":"copy"}` | `201` | `{id, name, type}` | the *source's* id, which the copy shares |
| `POST {"op":"move"}` | `200` | empty | — |
| `DELETE entries/{path}` | `200` | empty | — |

`size` is absent on a directory for the reason the listing omits it there: a
directory object has no size of its own, and the sum of what is under it is a
different question with a different endpoint. Everything else is present on
every create, including `id` — which is the field a client is most likely to
have built its decoding around, because a listing row always carries one.

The two `200`s hand back nothing because there is nothing new to name: a move
and a delete both act on an entry the caller already has the id of. What
changed is the library root, and its new `ETag` comes back on the next read.

### The three write refusals a sync client meets

**`503` on a write means nothing was applied, retry unchanged.** Concurrent
writers race for the branch head and the loser is told `503` with
`Retry-After: 1` and a body of `write contention; retry`. The identical request
will usually succeed on the retry; retry it rather than surfacing a failure.
This is the case where a wrong status code costs data: a `500` means the server
hit an unexpected condition and *may have applied part of the request*, so the
only safe handling there is to stop and surface it — `EIO` from a FUSE client,
which to the application that already wrote the bytes is data loss.

**`409` means exactly one thing: the state here is not what your request
assumed.** A `mkdir` over a file, an attempt to create the root, a path
component that is not a directory, and the two destination collisions below.
Change something — rename, usually — and send it again. It is never a request
to retry unchanged; that is `503`'s job.

**Not every collision is one.** A move or a copy onto an existing *file*
replaces it and answers an ordinary success, exactly as `PUT entries/{path}`
does — replacement is the contract for a file, not an accident, and there is
no header that opts out of it. What is refused is the collision that would
take something the caller did not name:

| destination holds | `move`/`copy` of a file | of a directory |
|---|---|---|
| nothing | written | written |
| a file | **replaced** | `409 Destination exists and is a file` |
| a directory | `409 Destination exists and is a directory` | `409` likewise |

Where the table says written or replaced, the status is the verb's ordinary
one — `200` for a `move`, `201` for a `copy` — whether or not anything was
there before. A directory is refused in both columns because replacing it
drops its whole subtree in one commit: data loss the caller never asked for,
reported as a success. A file is not, because the only thing lost is the one
entry the request named.

**And replacing a file does not destroy its bytes.** The commit before the
replacement still holds them, so `GET entries/{path}?at={commit}` reads what
was there — which is the reason replacement is an acceptable default rather
than an unrecoverable one. It is bounded: history is reclaimed by `silo
retention` and the expiry pass behind it, so the window is a deployment's
policy and not a guarantee this endpoint makes.
`fileserver/api_handlers_test.go` pins the recovery.

So **do not read "no `409`" as "nothing was there."** On a `PUT` a client that
wants to know sends `If-None-Match: *`, which asks exactly that about the path
being written and answers `412` when something is there. On a `move` or a
`copy` that header asks about the **source** — see the note under the endpoint
table — so it cannot be used to ask about the destination, and sending it
there just fails against a source that by definition exists. A client that
needs to know before replacing looks first with `HEAD`, and accepts that
looking and writing are two requests with a gap between them.

This is sharpest for a File Provider extension, which believes it knows the
destination's state: a replace it did not hear about leaves the replica
disagreeing with the server, and nothing signals it until the next
`GET changes`. Treat that feed as the authority on what the destination holds
rather than inferring it from a write's status code.

**`412` is the mechanism working, not an error.** Someone else wrote first.
Re-read, reapply, write again. Note the header changes meaning with the
method: on `GET`, `If-None-Match` asks "skip the body if unchanged" and yields
`304`; on a write it asks "fail if it exists" and yields `412`. Same header,
different question, as RFC 9110 specifies.

Both preconditions are opt-in. A request with neither header is last-writer-
wins, which is still available — it just has to be chosen rather than arrived
at by accident. For a File Provider extension, send `If-Match` on every
`modifyItem`: the `baseVersion` you were handed is precisely the tag to send.

#### What a `412` means inside `modifyItem`

Not an error. `modifyItem` reports a conflict on the **success** path.

The system hands `modifyItem` a `baseVersion` — the version it believes is on
disk. Send its `contentVersion` as `If-Match`. On a `412` someone else moved
the entry underneath you, so re-read it and call the completion handler with
the **server's** item, carrying the server's new `contentVersion`, and
`shouldFetchContent: true`. The system sees the content version move, calls
`fetchContents`, and replaces the local copy. Returning an error instead gets
the whole modification retried from the top, against the same stale version,
forever.

That resolution discards the local edit — the right default for a first cut
and the wrong one in general. *Which* version wins is a policy decision the
extension makes item by item, and the API is built to let it make that
decision rather than to make it for you.

Two traps in the same completion handler:

- The second argument is `stillPendingFields`, the subset of `changedFields`
  you did **not** apply. Since macOS 12, returning a set *identical* to the
  fields you were passed does not mean "try me again later" — the system reads
  it as "this provider does not support these fields" and stops sending them
  until the item changes again. Never return the whole set to signal a
  temporary failure.
- `NSFileProviderError.localVersionConflictingWithServer` does mean exactly
  this conflict, but only under the `failUploadOnConflict` policy, which needs
  `NSExtensionFileProviderSupportsFailingUploadOnConflict` in the extension's
  `Info.plist` — and it is macOS 26.0 and later. Under it the provider *does*
  fail the call and the system merges and re-calls with a fresh `baseVersion`.
  Below 26.0 the paragraph above is the only route.

Outside `modifyItem` — a conditional write the extension issues on its own
behalf — a `412` is just a `412`: re-read, reapply, write again.

### What a `404` on the library means, and what it does not

A `404` whose body names the library is a positive assertion: it is gone, and
removing your copy is the correct handling — it is how a library deleted from
the web UI reaches you. The server says it only when the library has no row. A
library the server holds but cannot read — its head commit object is missing
from the store — answers `500`, and a database it cannot reach answers `503`.
Both mean *something on the server is broken, nothing has been deleted, do not
act on it*: `EIO` for a FUSE client, a transient error for a File Provider
extension, never an `NSFileProviderItem` removal. Neither is worth a full
re-enumeration; retry, and surface it if it persists.

### Size and quota

Feature name `usage`. Each row of `GET libraries` carries `size` and
`file_count` for that library, and the account's total lives on its own
endpoint:

```
GET /api/silo/v1/account/usage
{"usage": 5000000, "quota": 6000000, "kind": "logical-at-head"}
```

**`quota` is absent when there is no ceiling** — not `-1`, not `0`, absent.
Render a missing key as "no limit", never as a number. The same rule holds on
the listing: `size` and `file_count` are absent, not zero, when the server
could not work out a library's size. Zero means an empty library, and showing
"0 bytes" for "unknown" states a fact you were never told.

**Per-library sizes are on the listing and not under `account/usage`.** A
library shared with you appears in your listing but is charged to its
*owner's* quota, so the two surfaces do not add up and are not meant to. Do
not sum the listing to reproduce `usage`.

**`kind` says which number this is.** `logical-at-head` is the sum of the
sizes of the files the library currently holds — what you get back by deleting
them, and not bytes on disk: dedup and deferred compaction make those diverge
by multiples in both directions. If you show a figure next to anything a user
might compare with `du`, label it. `kind` is on the wire because
[`quota.md`](quota.md) argues for charging the chunks an account occupies
instead, and when `chunks-occupied` arrives the number becomes the bill rather
than the file total, cannot be estimated locally by summing listing sizes, and
stops dropping the moment a file is deleted, because the chunks stay reachable
from history. **Branch on `kind` or display it; never hard-code the string.**
A client that ignores it shows an account halving overnight and calls it a
server bug.

**Over quota is `507`**, from `PUT entries/{path}` (before the body is read if
you sent a `Content-Length`, and again after), from `POST batch` (before
anything is applied) and from `PUT chunks/{id}`. It is a user-facing condition
with an actionable message — free some space — not a transport error to retry
blindly. A single write is admitted against the figure before it, so the last
one through can cross the line and the account can sit marginally over; do
not build a client that depends on `usage <= quota` holding.

**If you are filling in `df`:** `statfs(2)` reports block counts, and you pick
the divisor. Dividing the account figures by a block size truncates twice —
once for the total, once for what is free — so the used column lands within
one block of the real number, on whichever side depends on where the quota
falls relative to a block boundary; it is not reliably a round up, and it is
not per-file occupancy. Report it as "the aggregate divided by the block size".
silo-drive-linux uses 4096 because that is what every local filesystem on the
machine reports, which is a tradeoff rather than a property of the interface.
The macOS build fills in no `df` at all — a File Provider extension does not
get to declare a volume size, and Finder reports the disk behind the domain
instead — so on that side the quota has to surface in the container app and in
what a `507` is made to look like. See [`quota.md`](quota.md) § macOS File
Provider.

### Chunks: what the reference does not say

**If the library's row carries no `chunker`, upload whole files.** Do not
substitute defaults: chunking under the wrong parameters uploads *correctly*
and dedups against nothing, and nothing detects it. It is the one failure on
this surface with no error to see.

**Re-sending a chunk the server already holds costs the bytes.** `PUT
chunks/{id}` answers `200` rather than `201` when the chunk was already there,
but it answers it *after* reading the body: the existence check happens once
the bytes are in hand, so `Expect: 100-continue` saves nothing here — the
server sends the `100`, the client uploads, and the `200` arrives at the end.
Ask `chunks/missing` first, which is the call that exists for this, and accept
that its answer can go stale under a library more than one client writes.
`POST chunks` reports that staleness rather than hiding it: `present` counts
the frames the server already had, and those bytes were spent.

**The manifest id is already in your hand.** A listing's file `id` *is* the
manifest id, unprefixed, so `?type=manifest` is a fetch you make when you
decide to read the file, not a lookup you make first. Decode it with
`store.DecodeManifest`, not a JSON parser: an ordered `[]ChunkRef` of
`{ID, Size}` with the file's size, where `Size` is the **plaintext** length —
specified that way because mapping a read offset to a chunk needs it.

**Key your content cache on the chunk id, not on the file.** A cache keyed by
`(file-id, window)` collects nothing: one byte changing invalidates every
window of that file, and two files sharing a gigabyte share nothing. Keyed by
chunk id, an append invalidates one chunk and identical content is stored once
however many files hold it.

**Ask by id rather than by range for anything you mean to keep.** A range is
addressed by `(path, offset, length)`, which only means anything relative to a
version — between fetching a manifest and its four-hundredth chunk the file can
change, and the read assembles a file that matches no version of anything.
`If-Match` on every range defends against it; content addressing does not have
the problem. For a file read once with nothing cached, `GET entries/{path}` is
still the right call: one request, and the server assembles.

**Verify every chunk on arrival.** `store.DecodeChunkFrames` does it for a
stream. The hash check is what makes a source other than this server — a cache
of uncertain provenance, a peer, a mirror — legal to read from at all.

**Splicing an edit is where this pays, and it is the one dangerous part.**
Fetch the chunks around an edit, re-chunk from there until the boundaries
re-sync with the manifest you hold, and reuse every id on either side. A
re-sync that is wrong by one chunk produces *valid chunks* and a manifest that
reproduces nothing, and it surfaces at `close(2)` with nothing naming the
cause. Ship it with a verifier that re-chunks the whole file and asserts the
result — on in tests, behind a flag in production. Two cheaper guards worth
having anyway: assert the manifest's `FileSize` equals the sum of its
`ChunkRef.Size` before you `PUT` it, and treat `chunks/missing` returning a
*shorter* list than the ids you spliced in as free evidence the reused ids
were real.

### Reading `changes`

```json
{
  "anchor": "9f016e4b3e5c4ffb99776ab6e155b10bce15e6542c8d7a1f0b3e6c9d2a5f8b1e",
  "changes": [
    {"op":"create","path":"/deep/nested/z.txt",
     "id":"cb01668407c71277b010774ffdceb5fe2a85a72a4d1e7f0b3c6a9d2e5f8b1c4a",
     "size":2,"is_dir":false}
  ]
}
```

`id` is absent on deletes; `changes` is always an array, never null. The
first anchor is `head_commit_id` from `GET libraries` — listing libraries is
the first thing a sync client does, and without it the opening enumeration
would have no name for the state it just read. Use it for `currentSyncAnchor`.

**A subtree is reported in full.** Every directory that appeared is reported
in its own right, alongside every path inside it:

```
mkdir /deep, /deep/nested, put z.txt → create d /deep
                                       create d /deep/nested
                                       create f /deep/nested/z.txt
delete a non-empty /d                → delete d /d
                                       delete f /d/z.txt
```

`is_dir` says which is which; you do not have to infer parents, though
creating them anyway is harmless. Renames are emitted server-side because the
server holds both trees; the inference is imperfect — deleting one file and
creating another with identical bytes looks like a rename — but the failure is
benign, an identifier following the "wrong" copy of byte-identical content.

`410` is not an error. The anchor is no longer reachable and the recovery is
to enumerate from scratch — the natural source for
`NSFileProviderError.syncAnchorExpired`.

### History, and building `.history/` on top

`GET libraries/{libraryid}/commits` lists history newest first as
`{"commits":[{"id","created_at","author","message"},…]}`, paged like
everything else. `author` and `message` are **absent under E2EE**, not empty
— they are sealed under a key the server never holds — while `id` and
`created_at` stay public because the walk needs them; decode them from the
commit object if you hold the key. The listing ends where history ends: when
retention collects old commits, the walk meeting one that is gone is reported
by the list stopping, not by an error. That is the difference from
`changes?since=`, where *you* named a commit and are owed a `410`.

`?at={commit}` on `entries/{path}` is the ordinary read with a different
starting id — files, listings, `Range`, `If-None-Match`, all unchanged. A file
deleted three commits ago is still there in the commit that had it. `400` if
`at` is not a commit id or is sent on a `PUT`, `POST` or `DELETE` — writes
carrying `at` are refused, never ignored, because a client that believes it
is editing the past while silently editing the present is the worst outcome
available; `410` if it is well-formed and no longer reachable; `404` if the
path does not exist in that commit.

The `.history/` directory is yours to synthesize; the server will not invent
one. A synthetic entry has no id and appears in no manifest, so it would have
to be excluded from GC's mark, from `changes?since=`, from size accounting and
from every other walk, and the whole store rests on an id naming content. Two
properties fall out of that: reading history costs nothing against quota under
`logical-at-head` (under a `chunks-occupied` charge the history being read is
part of what its owner is billed for, which is what makes retention a lever),
and what the commits list returns *is* the retention window, so the user can
see how far back they can go. ETags work across time: an unchanged file has
the same ETag at an old commit as at the head, and a `.history/` copy of a
file you already hold revalidates to `304`.

#### One file's versions — `GET entries/{path}?type=history`

`.history/` answers *what did the library look like then*. The question a
file manager asks is *what has this file been*, and from the client side the
only route from one to the other is to read the path at every commit and drop
the repeats — hundreds of requests to find three versions, because most
commits did not touch the file. The server holds every tree, so it does the
collapse in one pass:

```
{"versions":[
  {"commit":"…","created_at":1788786009,"id":"…","type":"file","size":4784725402,
   "author":"…","message":"…"},
  {"commit":"…","created_at":1788700000,"id":null},
  …
]}
```

Newest first, and **only the commits where the id changed**. Each row names
the commit that *wrote* the version — the oldest of the run of commits that
carried it — so `created_at` and `message` are the ones a person wants beside
it. `id` is the manifest's id, which is absolute: the hash of the encoded
manifest, identical at every commit the file survived unchanged. Take it to
`objects/{id}` and the chunk surface with no `?at=` — the id has already
resolved the tree — and a chunk cache keyed on Silo's ids already holds
whatever an old version shares with the current one. `type` says whether that
id is a manifest or a directory, because you have to know before you decode
it; `size` is present on files only.

**`limit` bounds versions, not commits.** Ten versions may mean scanning the
whole history to find them, so one request reads at most a thousand commits
before it hands back a `Link: …; rel="next"` — a page may hold fewer rows
than you asked for and still have more after it. Follow `Link` until it is
absent, exactly as for any other paged listing; the cursor carries the walk
position and nothing is repeated or skipped across the boundary.

**A deletion is a row with `id: null`.** A path deleted and recreated is two
lives of one name, and a list that omitted the gap would read as one
continuous file; drop the null rows if you do not care. A file deleted at the
head starts with one. The one absence that is *not* a row is the time before
the file first existed, so a path that never existed is an empty list, not a
`404`.

`at` does not combine with `type=history` — `400`, never silently ignored —
and an E2EE library is `403` for the reason `entries/{path}` itself is: the
server cannot resolve a path through names it cannot read. Restoring a
version needs nothing new: the ordinary write path, fed the old bytes.

### The E2EE client shape

An end-to-end encrypted library splits the API, and the split is worth
designing for before the first plain-library code is written. On a plain
library everything above applies. On an E2EE library the server holds no
content key, so it cannot chunk a file, build a manifest, name an entry — or
resolve a path: every request on `entries/{path}` answers `403` with a body
naming the id-addressed surface, in both directions. What still answers by
path-free means is `changes?since=`, which reads public directory sections and
carries names as unpadded base64url (RFC 4648 §5) of their AES-SIV ciphertext.

So on an E2EE library the client reads and writes the object graph itself,
through `objects/{id}`, `chunks/{id}`, `chunks/fetch`, `POST chunks` and
`PUT head`. Build one interface with two implementations — the thing that
answers *list this directory*, *read this file*, *write these bytes* — rather
than branching on `e2ee` at each call site. The id-addressed surface is the
same for both library types, so the E2EE implementation is also a complete
plain-library implementation.

`client.LibraryFS` is that interface in the reference client, and
`Account.Open` is the one place the choice is made: the listing says whether a
library is encrypted, and the answer is a handle that behaves the same either
way. The one capability the two halves do not share is preserving a file's
mtime through a write, which the path surface has no room for —
[`protocol-gaps.md`](protocol-gaps.md).

**Reading** is a walk down the object graph from the head:

```
GET  libraries                                → head_commit_id for the library
GET  libraries/{library}/objects/{commit}     → decode → root directory id
GET  libraries/{library}/objects/{dir}        → decrypt names → child ids and types
GET  libraries/{library}/objects/{manifest}   → chunk list, or the inline bytes
POST libraries/{library}/chunks/fetch         → chunk ciphertext → decrypt
```

`client.EncryptedLibrary` is that walk in the reference client:
`OpenEncryptedLibrary` from an opened account, then `List`, `Stat`,
`ReadFile` and `ReadAt` by plaintext path.

**Resolving a depth-N path costs N sequential fetches the first time**, and
no cleverness removes it: each segment's name key is derived from its parent
directory's salt, so the parent has to be read before the child can be named.
Cache the salt map beside your local index — a cold resolve is a tree walk,
which is why the local index earns its keep here in a way it does not on a
plain library.

**Writing** is the same shape for both library types:

```
POST libraries/{library}/chunks/missing       {"chunks":[id,…]} → {"missing":[id,…]}
POST libraries/{library}/chunks               many chunks, one framed request
PUT  libraries/{library}/objects/{id}         manifest, then each directory up the spine,
                                              then the commit
PUT  libraries/{library}/head                 If-Match: <current head commit id>
```

The server verifies that each object's id is the SHA-256 of its bytes and that
it decodes, and nothing else, because there is nothing else it can check.

`client.EncryptedLibrary` writes with `WriteFile`, `WriteFrom`, `MkdirAll` and
`Remove`. Each publishes one commit and returns the new head, retrying the
compare-and-swap below on its own.

**A write rewrites the spine, and that is your job.** Changing one file means
a new manifest, a new parent directory, a new grandparent, up to a new root
and a new commit. Three rules the server cannot enforce for you:

- **Carry the salt forward.** A rewritten directory keeps the salt of the
  object it replaces. Mint a fresh one and its id changes, so every ancestor's
  does, so the root does — and `changes` reports the entire library modified
  on every commit.
- **Only the directory whose entry list changed gets a new mtime**, and that
  mtime lives one level up, in its parent's entry for it. Stamping the whole
  spine makes every commit look like it touched everything between the change
  and the root.
- **The mutation's timestamp is not the file's mtime.** You preserve a file's
  mtime, which may be years old; the directory it lands in changed just now.

All three have vectors: [`spine.json`](../store/testdata/vectors/spine.json)
commits six rewrites as chains in and sealed bytes out, with each level's
resulting mtime stated beside it so the second rule can be checked without
decoding anything. It is the one file here that pins a *rewrite* rather than an
object — no single-object vector can catch a writer that stamps every level,
because each object it produces is individually well-formed. In Go the rewrite
itself is `Keyring.RewriteSpine` (`store/spine.go`), which takes the chain and
returns the sealed bytes; fetching the chain and storing what changed stay in
the client, and that split is why the rules are now executable off the network.

**There is no server-side merge on `PUT head`.** Merging trees means reading
names. `PUT head` is a compare-and-swap, and a writer that loses gets `412`,
not a merge: re-read the head, rebuild your change on the new root, retry.
Write that loop deliberately — it is not the `503`-and-retry above, because
the root you built on is stale rather than the server being busy.

**Libraries are created with a client-supplied UUID.** `store.WrapCK` binds
the library id into the wrap as associated data, so the client mints the id
and the server accepts it; `409` means mint another and rebuild the wrap.

Four facts about the format that shape the client:

- **Ids are 64 hex characters**, SHA-256 of the stored bytes, everywhere:
  chunks, objects, commits, `head_commit_id`, `since` anchors, ETags.
  `store.ParseID` refuses any other width at the door.
- **Chunking is per library and keyed.** The seed is derived from the
  library's content key, so the same file cut in two encrypted libraries lands
  on different boundaries — deliberately, so the server cannot confirm what a
  file is from its boundary fingerprint. **The boundary cache is per-library**,
  never shared across them.
- **Small files inline.** Below `store.Inlined`'s threshold the bytes live
  inside the manifest and there are no chunks. Whether a file inlines is
  decided by its size and never by the writer: two clients disagreeing about a
  30 KB file would mint two ids for identical content. Do not carry a second
  answer.
- **Import `store/` rather than reimplementing it** if you are writing Go. It
  is a package in this module with no server dependencies — the chunker, the
  ids, the codecs, the content crypto, name encryption and key wrapping —
  built to ship inside a client. A Swift port conforms when it reproduces the
  vectors in `store/testdata/vectors`, per
  [`spec/store-format.md`](spec/store-format.md).

#### Where the encryption boundary is

**What is in a library is private; that the library exists and what it is
called is not.** E2EE covers a library's content and the names of the files
inside it. A library's own display name and description are server-plaintext,
permanently and by decision: the server has to sort them, search them and put
them in `GET libraries` for a client that has not unlocked anything. Sealing
them would produce a listing of untitled libraries, or a second name kept in
the clear beside the sealed one, which is the same disclosure with an extra
step.

Two more things the server knows: **who wrote last, and when.** The account is
the one that authenticated when the head moved and the timestamp is the
server's clock at that moment, recorded outside the commit. They may differ
from the author and `created_at` you sealed inside it — you may seal whatever
attribution you like — and that is the intended relationship. Surface the
sealed pair where you show history and the server pair where you show sync
state; do not try to reconcile them.

#### Keys, in one paragraph

The password splits client-side: an auth key that goes to the server and a
wrap key that never leaves the device. The wrap key unwraps the X25519
identity key; the identity key unwraps each library's content key
(`GET libraries/{libraryid}/key`). **Store the identity key in the platform
key store and discard both the password and the wrap key.** `store/` has the
whole path: `DeriveCredentials`, `OpenIdentityWithPassword`, `UnwrapCK`.

#### Things that will never be added, so do not wait for them

- **No password-to-the-server endpoint for an encrypted library.** The content
  key reaches a new device wrapped to that account's identity key, never by
  the server holding it.
- **No path-addressed write on an E2EE library.** The server cannot chunk what
  it cannot read.
- **No pack ids or offsets on the wire.** Compaction moves chunks between
  packs, so a pack id in a response would be a lie by the time it was used.
  Address chunks by id and nothing else.
- **No chunk-parameter renegotiation.** A library's parameters are frozen at
  creation; changing them is a full rewrite, done deliberately or not at all.

### Push

If the server advertises `notifications-account`, open the socket with your
credential, send `{"account": true}`, and treat every `account-update` as
"list the libraries": one `GET /libraries`, compare `head_commit_id` per row
against your anchors, and fetch `changes` for the ones that moved. That is the
whole push loop, it covers renames and new libraries, and there is no token to
mint or renew.

On the per-library lane, `commit_id` in a `library-update` is exactly the
anchor `changes` wants, so the frame translates directly into
`GET changes?since=<your last anchor>`. Do not treat the pushed `commit_id` as
your new anchor without fetching — you may have missed events; push is a hint
and the pull is the truth.

There is no token on either lane since 0.5.0. Set `Authorization` on the
upgrade request, subscribe with no `jwt_token`, and treat `subscribe-denied`
as "re-check access" — a server that still wants a token does not advertise
`notifications-credential`, which is the one thing to branch on. A server with
no `notifications` at all means fall back to polling, not an error.

### If you are building a FUSE client

FUSE serves `read(fd, buf, len, offset)`, and `GET entries/{path}` with a
`Range` header is one authenticated round trip, repeatable on the same URL.
Build the read path directly on it; there is no block lane to reassemble
from, and no whole-file local cache is needed for correctness, though you may
still want one for latency. What is worth doing:

1. **Cache attributes**, keyed by path, revalidated with `If-None-Match`. A
   `304` reads no chunks, so `getattr` storms are nearly free.
2. **Invalidate from `changes`** rather than by polling paths.
3. **Read through to `entries/` with `Range`** for anything read once, and go
   through the manifest for anything you mean to keep, keyed on the chunk id.

### The request inventory

Every row has been exercised against a running server.

| Callback | Method + path |
|---|---|
| domain setup | `POST auth/login`, `GET server-info` |
| `enumerateItems` (root) | `GET libraries` — rows carry `size`/`file_count` |
| show storage used | `GET account/usage` |
| `enumerateItems` (dir) | `GET libraries/{id}/entries/{path}` |
| `currentSyncAnchor` | `head_commit_id` from `GET libraries` |
| `enumerateChanges` | `GET libraries/{id}/changes?since={commit}` |
| `item(for:)` | *none* — local `IdMap` ⋈ `WorkingSet` |
| `fetchContents` | `GET libraries/{id}/entries/{path}` — bytes on the response |
| revalidate a cached item | the same `GET` with `If-None-Match` → `304` |
| read at an offset | the same `GET` with `Range` → `206`, repeatable |
| `createItem` (dir) | `PUT libraries/{id}/entries/{path}?type=dir` |
| `createItem` (file) | `PUT libraries/{id}/entries/{path}` — body is the file |
| `modifyItem` (contents) | the same `PUT` — it replaces |
| `modifyItem` (rename, reparent) | `POST libraries/{id}/entries/{path}` `{"op":"move",…}` |
| duplicate an item | `POST libraries/{id}/entries/{path}` `{"op":"copy",…}` — no content transferred |
| upload a large file | `POST chunks/missing`, then `POST chunks` with the ones it named (or `PUT chunks/{id}` each, without `chunks-upload`), then `PUT entries/{path}?type=chunks` |
| download a large file you hold a version of | `GET entries/{path}?type=manifest`, then `POST chunks/fetch` for the ids your chunk cache lacks |
| read a byte range of a file you hold nothing of | `QUERY entries/{path}` `{"ranges":[[off,len],…]}` — manifest and covering chunks, one request |
| read another range of that same file | `POST chunks/fetch` — you have the manifest now, so this one is already a single request |
| read a file once, nothing cached | `GET entries/{path}` — one request, the server assembles |
| enumerate a huge directory | `GET entries/{path}?limit=1000`, then follow `Link: …; rel="next"` |
| write many things at once | `POST libraries/{id}/batch` — one commit, all or nothing |
| `deleteItem` | `DELETE libraries/{id}/entries/{path}` |
| list history | `GET libraries/{id}/commits` |
| read at a past commit | `GET libraries/{id}/entries/{path}?at={commit}` |
| the versions of one file | `GET libraries/{id}/entries/{path}?type=history` — only the commits where it changed |
| push invalidation | `WS /notification` |

Identifiers never cross the wire: every request is `(library_id, path)`,
resolved client-side from `IdMap`.

### Checking your work

The Go CLI speaks exactly this surface, so it is a reference implementation to
diff against — `client/client.go` is the transport, and `silo libraries`,
`ls`, `put`, `get`, `mkdir`, `mv`, `rm` and `changes` each map to one call
above. Server-side Sentry shows anything a client sends that the server does
not expect, with the route template as the transaction name, so
`entries/{path}` groups rather than fragmenting per file.

The server is ours and additive endpoints are cheap. If a client wants
something else, ask rather than working around it: reimplementing server logic
client-side is what makes sync clients enormous.
