# Credentials and Authentication

What Silo stores today, what is wrong with it, and the design that replaces it.

Silo has **no deployments**. Every credential in every table can be discarded
and reissued. That removes the constraint that shapes most auth rewrites — the
deprecation window where an old format and a new one have to coexist — and it
is the reason this document proposes replacing the credential model outright
rather than patching it. The window closes the first time someone else runs
this server, so the decision is worth making now.

## What is stored today

| Credential | Where | Form on disk | Lifetime | Revocable |
|---|---|---|---|---|
| Account password | `EmailUser.passwd` | PBKDF2-SHA256, 600k rounds, 32-byte random salt | — | n/a |
| Session JWT | not stored — signed with `option.JWTPrivateKey` | HS256, `aud=silo:session` | 24h | **no** |
| API token | `ApiToken.token` | **cleartext**, 160-bit random | 30d, sliding | yes |
| Sync token | `RepoUserToken.token` | **cleartext**, SHA1(uuid) | **never expires** | yes |
| Access token | `tokenstore`, memory only | uuid v4, cleartext | 1h | n/a |
| Notification JWT | not stored — same key | HS256, `aud=silo:notif` | 72h, per repo | no |
| Encrypted-repo key | `keycache`, memory only | derived key, never persisted | process | n/a |

Three lanes, three credentials, and a client that wants all of Silo needs all
three: `Authorization: Bearer <jwt>` on `/api/silo/v1`, `Authorization: Token
<hex>` on `/api2` and `/api/v2.1`, and `Seafile-Repo-Token: <hex>` on `/repo`.
That shape is inherited from Seafile, not chosen, and the cost lands on the
client — `porter-brief.md` has to spend a section explaining which credential
goes where.

## What is right, and worth keeping

Credit where it is due, because the password column is the best-protected thing
in the database and that did not happen by accident.

`hashPassword` (`authmgr.go:208`) writes `PBKDF2SHA256$600000$<salt>$<derived>`
at OWASP's current work factor, sixty times Seafile's 10,000. `needsRehash` /
`upgradeHash` (`authmgr.go:227`, `authmgr.go:246`) silently replace any legacy
hash — unsalted SHA-1, or SHA-256 with the public constant salt at
`authmgr.go:34` — the next time the user successfully logs in, which is the
only moment the plaintext is in hand and known good.

`ValidateSessionToken` passes `jwt.WithValidMethods` and `jwt.WithAudience`, so
a token cannot select its own algorithm and a notification token cannot be
replayed as a session token even though both are signed with the same key.

`allowLoginAttempt` (`api/loginlimit.go:45`) limits per address *and* per
account, charges only failures, and clears the account bucket on success.

`tokenstore.QueryToken` redeems a one-time token with a single `LoadAndDelete`,
so two requests racing on the same capability URL cannot both be served.

`apitokenstore.Lookup` distinguishes `ErrNotFound` from a database error, so an
outage does not present itself to a client as "your credential is invalid".

None of that changes below. What changes is everything around it.

## What is wrong

### 1. The token columns are cleartext

`apitokenstore.Create` (`apitokenstore.go:62`) inserts the raw token;
`repomgr.GenerateRepoToken` (`repomgr.go:987`) does the same. A read of
`seafile.db` — a backup, a snapshot, a stray `SELECT` through some future admin
surface — yields immediately usable credentials for every device of every user,
with no cracking required.

This is the worst single property of the current system and the cheapest to
fix. See [why a fast hash is the right one](#why-tokens-want-a-fast-hash-and-passwords-do-not).

### 2. `is_active` is never read

The column is in the schema (`schema.go:17`), `EnsureAdmin` writes it
(`authmgr.go:278`), and **nothing in the codebase ever selects it**.
`ValidatePassword` fetches `passwd` alone. There is no way to disable an
account.

Worse, no token lookup joins back to the user at all. Deleting a row from
`EmailUser` does not stop that user's sessions, API tokens, or sync tokens —
every one of them keeps working, because each store answers "who is this
token's email" without ever asking whether that email is still anybody.

Compounding it: the only account-creation path is `SILO_ADMIN_EMAIL` /
`SILO_ADMIN_PASSWORD` read from the environment. There is one account and no
lifecycle.

### 3. The lane Porter uses cannot log out

`/api2` has `SeaDriveLogoutHandler`, which deletes the row. `/api/silo/v1` has
no logout endpoint, no `jti`, and no deny list. A session JWT is valid for its
full 24 hours no matter what happens to the account behind it — password
changed, user deleted, laptop stolen.

The only kill switch is rotating `SILO_JWT_SECRET`, which also invalidates
every notification token in flight.

### 4. The JWT secret is ephemeral by default

`LoadJWTConfig` (`option.go:585`) generates a random key when
`SILO_JWT_SECRET` is unset and logs a line about it. Every restart invalidates
every session simultaneously.

Because there is also no refresh endpoint, the documented client workaround is
to **keep the account password in the Keychain** and re-login on 401
(`porter-brief.md`). So the default configuration pushes the highest-value
secret in the system into long-term storage on every device, to work around a
session that cannot be renewed. That is the wrong secret in the wrong place for
the wrong reason.

### 5. Nothing has a scope

Every credential grants the whole account. A read-only, single-library
credential — the thing you actually want to hand a backup tool, or a mount you
do not fully trust — is not expressible. `share.CheckPerm` has no notion of a
ceiling that a credential could lower.

### 6. Nothing has a name

`silo token list` prints indistinguishable 40-char hex strings with no label,
no last-used timestamp, and no client identity. Deciding which one to revoke is
a guess. And `GenerateRepoToken` mints a fresh row per call with no expiry, so
`RepoUserToken` only ever grows.

### 7. Login says which accounts exist

`ValidatePassword` fetches `passwd` and returns immediately when there is no
row. An address that exists costs 600,000 PBKDF2 rounds — around 80 ms
(`authmgr.go:197`) — and one that does not costs a database round trip. That
gap is not noise; it is a directory listing for anyone willing to time the
endpoint.

The login limiter does not help, because enumeration needs one attempt per
address rather than ten, and Argon2id widens the gap rather than closing it.

### 8. The credential model was inherited, not chosen

`RepoUserToken.token` is `CHAR(41)` because the C daemon hashed a UUID to forty
hex characters and left room for a NUL — the hash accomplishing nothing, since
a UUID through SHA-1 is still exactly a UUID's worth of randomness. There are
three credentials on three headers because Seahub and `seaf-server` were
separate programs that had to authenticate callers to each other. Sync tokens
are per repo, never expire, and only ever accumulate, because that was the
cheapest thing for a daemon holding no session state.

Not one of those is a decision Silo made. They are the seams of a distributed
system Silo does not have, and they currently reach all the way into the
schema. What to do about it is
[below](#compatibility-is-an-adapter-not-a-shape).

### 9. Smaller things

`utils.GetAuthorizationToken` (`utils/http.go:14`) splits the header on a space
and returns field 1 — it ignores the scheme entirely, so `Bearer`, `Token` and
`Basic` are indistinguishable on the sync lane. Harmless today only because the
three stores are disjoint; it forecloses ever telling two schemes apart on one
lane, and it turns "you sent the wrong credential" into "invalid token".

The 5-minute `tokenCache` / `permCache` (`sync_api.go:101`) means sync-lane
revocation lags. `entries` deliberately does not cache. Both choices are
defensible; having both is not.

`SILO_ADMIN_PASSWORD` arrives in cleartext through the environment, so it sits
in `docker-compose.yml`, in `.envrc`, and in the process environment of a
running container. It is hashed the moment it reaches the database; everything
before that point is plaintext.

## Why tokens want a fast hash, and passwords do not

The obvious objection to hashing tokens is that it looks like the same problem
as passwords, so it should want the same answer. It does not, and the asymmetry
is entropy.

An API token is 160 bits from `crypto/rand`. There is no dictionary for it, no
rainbow table, and no chance the value exists anywhere else in the world. An
attacker holding its hash cannot work backwards; brute force is 2^160. Slowing
down a guess that will never succeed accomplishes nothing, which is why a slow
KDF on a token is pure waste — one SHA-256 per lookup, the index still works,
and a stolen database stops being a list of working credentials.

An account password is perhaps 30–40 bits in practice, drawn from a
distribution attackers have measured precisely. With a fast hash a stolen
`EmailUser` table cracks at billions of guesses per second. At 600k PBKDF2
rounds the same attack runs at a few thousand per second per core — a
millionfold tax, paid by the attacker on every guess and by Silo once per
login.

And the blast radius differs. A leaked sync token grants one library, forever,
and nothing else. A cracked password is plausibly the user's email password,
which is the account that can reset everything else they own. The slow KDF
protects the human as much as the service.

## Identity: an account is not an email address

Email is not a column on the user today. It *is* the key, repeated as a foreign
key across fourteen tables and 59 SQL statements in 16 files:
`RepoOwner.owner_id`, `SharedRepo.from_email`/`to_email`, `RepoUserToken.email`,
`ApiToken.email`, `FolderUserPerm.user`, `GroupUser.user_name`,
`RepoGroup.user_name`, `UserQuota.user`, `UserShareQuota.user`,
`RepoTrash.owner_id`, `OrgRepo.user`, `OrgSharedRepo.from_email`/`to_email`,
`UserRole.email`, `Binding.email`.

Four consequences, in rising order of how much they hurt:

- Changing an address is a fourteen-table migration, and a half-applied one is
  a permission bug rather than a cosmetic one.
- An account cannot hold two addresses, so there is nowhere to put "what they
  log in with" beside "what is on their older libraries".
- An OIDC subject has no home. It is not an email and never will be.
- "Is this account still active" has no single place to be asked — finding 2
  restated as a schema problem rather than a missing `WHERE` clause.

### The shape

The account gets an opaque key; email becomes an attribute of it.

```sql
CREATE TABLE Account (
  id         BLOB    PRIMARY KEY,        -- UUIDv7, 16 bytes
  display    TEXT,
  is_active  INTEGER NOT NULL DEFAULT 1,
  is_staff   INTEGER NOT NULL DEFAULT 0,
  ctime      INTEGER NOT NULL
);

CREATE TABLE AccountEmail (
  email       TEXT    PRIMARY KEY,       -- an address belongs to one account
  account_id  BLOB    NOT NULL REFERENCES Account(id),
  is_primary  INTEGER NOT NULL DEFAULT 0,
  verified_at INTEGER
);
CREATE INDEX account_email_account_idx ON AccountEmail (account_id);

CREATE TABLE AccountIdentity (            -- external identities, one row each
  issuer     TEXT    NOT NULL,
  subject    TEXT    NOT NULL,
  account_id BLOB    NOT NULL REFERENCES Account(id),
  ctime      INTEGER NOT NULL,
  PRIMARY KEY (issuer, subject)
);

CREATE TABLE AccountPassword (            -- separate: not every account has one
  account_id BLOB    PRIMARY KEY REFERENCES Account(id),
  hash       TEXT    NOT NULL,            -- argon2id$...
  changed_at INTEGER NOT NULL
);
```

**UUIDv7, not v4.** Both are 128 bits of identifier, but v7 leads with a
millisecond timestamp, so consecutive inserts land beside each other in the
index instead of scattering across it. `uuid.NewV7` ships in `google/uuid
v1.6.0`, which `go.mod` already requires — no new dependency.

**A separate password table, because not every account has a password.** An
OIDC-only account has none, and a nullable `passwd` column is how you end up
with a code path that reads "no password" as "any password will do". Seafile's
guard against exactly that is the sentinel string `"!"`, checked in two places
(`authmgr.go:39`, `authmgr.go:73`). A row that does not exist cannot be
compared against.

### What SeaDrive actually needs

The Seafile and SeaDrive clients never send an account id and never will. What
they send and receive is email:

- `POST /api2/auth-token/` — email and password in, token out.
- `GET /api2/account/info/` — returns `email` and `name` (`api/seadrive.go:248`).
- `GET /api2/repos/{id}/download-info/` — returns `email` (`api/seadrive.go:265`).

All three keep working, because email survives exactly where it belongs: **in
the API responses, not in the schema.** Each becomes a join.

So the legacy tables take `account_id` and the API layer resolves the address
on the way out. 59 SQL sites across 16 files, and no migration, because there
are no deployments. It is a week of mechanical work, and it should land as one
change — half-normalised is worse than either end, because a query written
against the half that has not moved yet is silently wrong rather than broken.

### The one place email is permanent

`Commit.CreatorName` is fed into the commit hash (`commitmgr.go:77`) and
`SeafDirent.Modifier` into an fs object's serialisation (`fsmgr.go:124`). Both
are content-addressed: the string is part of the object id. Rewriting them
means rewriting every object in every library.

That is not a problem, and it is worth naming so nobody tries to fix it. It is
the git model — an author string on a commit is historical display data, not an
identity key, and it is *correct* for it to record the address in use at the
time. Silo must simply never resolve a permission from it.

## Compatibility is an adapter, not a shape

Silo keeps working with SeaDrive and Seafile Desktop. What it stops doing is
letting them choose the credential model — and the difference between those two
positions is only *where the translation happens*: in four handlers at the
edge, or in the schema.

At the edge. What the legacy clients actually require is small:

| What a legacy client needs | What it costs |
|---|---|
| `POST /api2/auth-token/`, form-encoded | one handler |
| Forty hex characters in `Authorization: Token` | an *encoding* — 8 hex of credential id, 32 hex of secret. 128 bits, still an id-first lookup, still one `Resolve` |
| `Seafile-Repo-Token` per library | one `kind`, minted only by `/api2/repos/{id}/repo-tokens/` |
| An email address in `/api2` responses | a join, already required by [the identity split](#identity-an-account-is-not-an-email-address) |

Whether SeaDrive genuinely enforces forty characters is an assertion in a
comment (`api/seadrive.go:17`), not a tested fact. The Ruby harness in `test/`
against a 3.0.21 client would settle it in an afternoon, and it is worth
settling — but it gates nothing, because the answer only picks an encoding.

### What we stop carrying

- **Per-repo sync tokens, for anything that is not a legacy client.** Look at
  what they are: no expiry, no label, no last-used, one row per (repo, device)
  forever, and a README paragraph explaining that changing your password does
  not revoke them. A device credential reaches every library the account
  reaches, and narrowing is `scope` and `perm` on the row — not a second token
  type with its own table and its own header.
- **Three headers.** `Authorization` on every Silo-native lane.
  `Seafile-Repo-Token` becomes an input the legacy handlers translate, not a
  concept `Resolve` knows about.
- **`CHAR(41)` and `SHA1(uuid)`**, which existed only to fit a column width
  chosen in C.
- **The schema-compatibility promise.** The README used to offer "the sync
  protocol, block storage layout, and database schema are unchanged, so ...
  clients work against Silo without modification". The causal claim was never
  true — a client cannot see the schema — and the identity split breaks the
  literal one anyway. `README.md:11` now promises the wire only: *Seafile and
  SeaDrive clients keep working; the schema and the on-disk layout are Silo's
  own, and are free to change.* Left in place, that sentence would have been a
  compat claim quietly acting as a veto on every decision in this document.

### A legacy client is a legacy trust level

This should be visible rather than discovered. A `legacy` credential is a
bearer secret, is account-wide or repo-wide with no ceiling, and cannot
participate in [proof of possession](#proof-of-possession). That is not a
defect awaiting a fix; it is what an unmodifiable client can support. So `silo
credential list` labels it as such, and an operator who wants the stronger
guarantee knows that it means moving that device to Porter.

## The design

One table, one verification path, several kinds of credential.

### Token format

```
silo_<kind>_<id>_<base32(32 random bytes)><check>
```

The `id` is public, indexed, and how the row is found; only `SHA-256(secret)`
is stored, compared with `subtle.ConstantTimeCompare`. Looking a credential up
by id rather than by secret means the comparison is constant-time over a fixed
32 bytes, and it means the secret never appears in a query, a query log, or a
slow-query trace.

The `silo_` prefix and the kind make a leaked credential identifiable on sight
— in a log, a bug report, or a secret scanner — and let a handler reject a
credential meant for another lane before touching the database.

`check` is six base32 characters of SHA-256 over everything before it. It costs
nothing to compute, and it means a truncated paste or a mistyped character is
rejected as *malformed* before the database is touched rather than as *invalid*
after a lookup. It also lets a secret scanner confirm a match instead of
guessing at one, which is why `ghp_` tokens carry the same thing.

### Kinds

| Kind | Held by | Presented as | Lifetime | Scope |
|---|---|---|---|---|
| `device` | Porter, the File Provider extension | a signature — [proof of possession](#proof-of-possession) | absolute, default 90d | optional repo + permission ceiling |
| `session` | the TUI, the CLI | a signature, or a bearer secret where there is no key store | 24h | account |
| `access` | capability URLs (`/files/`, `/zip/`) | bearer, memory only | 1h | one object, one op |
| `legacy` | SeaDrive, Seafile Desktop | bearer, forty hex characters | absolute | account on `/api2`, one repo on `Seafile-Repo-Token` |
| `s3` | an S3 frontend, if it is ever built | SigV4 | absolute | see [S3](#s3-needs-a-master-key-not-a-column) |

`legacy` is what was going to be a `sync` kind. One kind rather than two,
because what distinguishes it is not which header it arrives on but that it is
[unmodifiable, and therefore bearer](#a-legacy-client-is-a-legacy-trust-level).

### The table

```sql
CREATE TABLE Credential (
  id          TEXT    PRIMARY KEY,   -- public, travels in the token
  kind        TEXT    NOT NULL,      -- 'device'|'session'|'access'|'legacy'|'s3'
  secret_hash BLOB,                  -- SHA-256 of the secret, for bearer kinds
  public_key  BLOB,                  -- SPKI, for proof-of-possession kinds
  account_id  BLOB    NOT NULL REFERENCES Account(id),
  label       TEXT    NOT NULL,      -- "dan's macbook, porter-fuse"
  scope       TEXT,                  -- NULL = all libraries; else a repo id
  perm        TEXT    NOT NULL,      -- 'r' | 'rw' — a ceiling, never a grant
  client_id   TEXT,                  -- device identity, when the lane has one
  ctime       INTEGER NOT NULL,
  expires_at  INTEGER,               -- absolute; NULL = no expiry
  last_used   INTEGER,
  CHECK (secret_hash IS NULL OR public_key IS NULL)
);
CREATE INDEX credential_account_idx ON Credential (account_id);
```

`label` and `last_used` are not decoration. They are what turns revocation from
a guess into a decision.

A row carries a secret hash or a public key, never both. An `s3` row carries
neither and derives its secret from the master key.

`expires_at` is **absolute and does not slide**. The current `ApiToken`
behaviour renews any token more than halfway through its life
(`apitokenstore.go:102`), which means an always-on mount polling constantly can
never age out — the 30-day TTL is unreachable in the one case it was written
for. Absolute expiry plus `last_used` gives an operator the same information
without the credential quietly becoming permanent.

### One verification path

Every lane resolves its credential through a single function:

```go
func Resolve(r *http.Request, kind Kind) (*Credential, error)
```

which parses the prefix, verifies the checksum, looks up by id, **joins
`Account` and checks `is_active`**, checks `expires_at`, and stamps
`last_used`. Proving the credential is one branch inside it: compare
`SHA-256(secret)` against `secret_hash` in constant time, or verify the
request's signature against `public_key`.

It takes the request rather than a string because verifying a signature means
seeing the method, the target and the headers; a bearer kind reads its one
header and ignores the rest.

One place. That join is the property that cannot be retrofitted onto three
separate stores: disabling a user has to kill every lane at once, or it does
not mean anything.

### Proof of possession

A bearer credential is a secret that authenticates whoever holds it, and every
finding in [What is wrong](#what-is-wrong) is a variation on someone else
coming to hold it: a database snapshot, a log line, a backup of a laptop, a
token pasted into a bug report. Hashing the stored side fixes half of that. The
other half is that the client still has to keep a usable copy.

It does not have to. Enrolment can register a **public key** instead of handing
out a secret.

```
Enrolment — password login or the OIDC device grant, either one:

  the client generates a keypair, non-exportable where the platform allows it
  (Secure Enclave on macOS, TPM on Windows, a 0600 file for the TUI)
  → the public key goes up with the login
  → the Credential row stores public_key, and no secret exists to steal

Every request after that:

  Authorization: Silo <credential-id>
  Signature-Input: sig=("@method" "@target-uri" "date" "nonce");created=…
  Signature: sig=:<base64>:
```

Nothing else in this document moves. Kinds, ceilings, `is_active`, the
generation counter, revocation, the legacy adapter: all unchanged.

**It is not a JWT, and it is not a token.** A JWT is a bearer assertion — a
signed statement you carry and present, which anyone else who holds it can also
present. Here nothing is carried. The credential id is a public name, the
private key never leaves the device, and what is signed is *this request*, so a
captured signature is a receipt for a request that already happened rather than
a credential. The close relatives are SSH `publickey` auth, mTLS, and AWS
SigV4; of those SigV4 is the right mental model, because it signs the request
rather than asserting an identity. The wire format is RFC 9421 HTTP Message
Signatures, which is the standardised version of what SigV4 does by hand.

#### The body is mostly not in the signature

Signing a digest of the body would mean buffering a five-gigabyte upload before
deciding whether the request is authentic. That is unacceptable, and it turns
out to be unnecessary, because of what Silo's writes already are.

Almost every write is content-addressed, idempotent, and carries the content's
own hash in the request line:

- `PUT /repo/{id}/block/{block-id}` — the block id *is* the hash of the bytes.
- `PUT /repo/{id}/commit/{commit-id}` — likewise.
- `recv-fs` carries a batch of fs objects, each verified against its own id on
  the way in.

Signing the request line therefore binds the payload already: bytes that hash
to something else are rejected by the write path regardless of who sent them.
And replaying one of these is harmless — it stores bytes that are already
stored, under an id they already have.

What is left is the small set of requests where replay means something:

- `PUT /repo/{id}/commit/HEAD?head=<commit-id>` — the branch update, and the
  one genuine state change on the sync lane. No body at all; the target is in
  the query string, so the request line covers it.
- The mutations on `/api/silo/v1` — mkdir, rename, move, delete, and writes to
  `entries` — which have small bodies and already accept `If-Match` /
  `If-None-Match` (`entries.go:305`). A conditional write is replay-proof by
  construction: the second attempt fails its precondition.

So: **sign the request line, the date and a nonce always; add a content digest
only where the body is small and is not already named by its hash.** The
expensive requests do not need the protection, and the ones that need it are
cheap.

#### Nonce and digest are two different jobs

They look like one job, and conflating them produces something that does
neither. Replay protection needs *uniqueness* — a value never seen before — and
a content hash is not unique: uploading the same block twice is legitimate and
common, and two clients writing identical bytes produce identical hashes.
Content binding needs the digest, which does not need to be unique at all.

They are separate signed components, and on the content-addressed lanes the
digest comes free because it is already in the path. A nonce is sixteen random
bytes; verification keeps a cache of recently seen ones for the width of the
accepted clock skew — sixty seconds is generous — so the cache stays bounded
with no cleanup logic beyond expiry.

#### Cost, and who cannot play

Ed25519 verification is around fifty microseconds and P-256 about twice that. A
directory walk issuing a few hundred requests pays single-digit milliseconds in
total, comfortably under the storage reads it is making anyway.

The clients that can do this are the ones we control: Porter, the File Provider
extension, the TUI, the CLI, and any future SFTP frontend by way of SSH keys.
The ones that cannot are SeaDrive and Seafile Desktop — and, for a different
reason, S3, whose SigV4 needs a shared secret the server can recompute with.
Those stay bearer, and say so.

### Permission ceilings

`share.CheckPerm(repoID, user)` keeps answering what the *user* may do.
`Credential.scope` and `Credential.perm` intersect with it:

```
effective = min(CheckPerm(repo, cred.account_id),
                cred.perm  if cred.scope in (NULL, repo) else "")
```

A credential can only ever narrow. That is what makes a read-only,
single-library credential safe to hand out: it cannot outlive or exceed the
account behind it, and if the account's own permission is withdrawn the
credential follows immediately.

### JWTs keep exactly one job

Statelessness is load-bearing in precisely one place: the notification tokens
that cross to another process, which has no access to Silo's database. Those
stay JWTs.

Everything a client presents *to Silo* becomes a `Credential` row, because
revocable beats stateless when the thing being revoked is a laptop. Session
JWTs go away, and with them findings 3 and 4 — a session that can be revoked
and survives a restart needs neither a logout workaround nor a persistent
signing key to be useful.

`SILO_JWT_SECRET` should still become a keyfile in the data directory, created
on first run at mode 0600, because notification tokens should not all die on
restart either. But it stops being load-bearing for login.

### Rate limiting stops applying to credentials

The login limiter exists to protect a low-entropy secret. A 256-bit credential
does not need it, and applying it causes harm: a wedged mount retrying a stale
credential in parallel empties the **account** bucket in about two seconds, and
that bucket is shared with the TUI and every sync client the same user owns.

So: the password path keeps exactly the limits it has, and `Resolve` bypasses
`allowLoginAttempt` entirely. Once a password is presented only at enrolment,
those buckets stop colliding with legitimate traffic altogether.

### Revocation takes effect now

Cache by credential id, and hold a per-account generation counter that a
revocation, a password change, or a deactivation bumps. A cache hit checks the
generation, so revocation is immediate rather than lagging `AuthCacheTTL`.
`invalidateRepoAuth` (`sync_api.go:1453`) already does the repo-scoped version
of this; the account-scoped version is the same idea one level up.

## What Porter does

Today: log in with email+password, get a 24h JWT, keep the password in the
Keychain because there is no refresh. Also hold an API token for `/api2`, also
hold a sync token per library.

After: log in once, get a **device credential**, store *that* in the Keychain,
never the password. One credential, one header — `Authorization: Bearer
silo_device_…` — on every lane. `Seafile-Repo-Token` survives only for the
upstream Seafile client, which cannot be changed.

The credential is durable, so there is no refresh problem and no re-login
stampede on restart. It is labelled, so a lost laptop is one identifiable row.
It is revocable, and revocation takes effect on the next request. It can be
scoped read-only, which is the difference between mounting a library and
trusting a mount.

With a key in the Secure Enclave there is nothing in the Keychain to steal at
all: a backup of the laptop, or of Silo's database, yields a public name and a
public key. See [proof of possession](#proof-of-possession).

## Password login

Password login and OIDC are two enrolment paths that produce one artefact.
Nothing downstream can tell them apart, which is the point of
[brokering](#silo-brokers-login-it-does-not-federate-every-request).

### It is not a device grant

The device grant exists for one reason: the client cannot host the user agent
that has to talk to the authenticator. A local password has no third party —
Porter collects it in its own window — so the code-and-approval dance would buy
nothing, at the cost of the HTML approval page the OIDC design specifically
declined to build.

So `POST /api/silo/v1/auth/login` stays, and becomes the password half of
enrolment.

```
POST /api/silo/v1/auth/login
{ "email": "dan@…", "password": "…",
  "kind": "device",                       // default "session"
  "client_name": "Porter 1.2 (macOS)",    // becomes label
  "public_key": "<base64 SPKI>",          // optional; bearer secret if absent
  "perm": "r", "scope": "<repo-id>" }     // optional, narrowing only

201 { "credential": "silo_device_…", "expires_at": …, "email": "…" }
```

`perm` and `scope` follow the [ceiling rule](#permission-ceilings): asking for
`rw` on an account that has `r` yields `r`, not a 403. A field that can only
narrow needs no validation branch.

### The password is an enrolment credential, not a request credential

It is presented once, exchanged, and forgotten. Porter stores the credential —
or, with a key, stores nothing that can be stolen — and never holds the
password again. That closes
[finding 4](#4-the-jwt-secret-is-ephemeral-by-default)'s Keychain workaround at
the source rather than by making sessions renewable.

It also changes the cost model. Password verification drops from "every session
refresh on every device" to "once per device, ever", and that is what makes the
next decision affordable.

### Argon2id, and the memory it costs the server

Since every password in the system can be reset from scratch, use the better
primitive. Argon2id is memory-hard; PBKDF2 is not, which is why a GPU eats it.
`validatePasswd` (`authmgr.go:72`) already dispatches on a stored prefix, so
`argon2id$…` slots in beside the existing formats with no special casing, and
`needsRehash` upgrades anything weaker on the next successful login.

The trap is that memory-hardness is a cost paid by *the server*, and unlike CPU
it is not self-limiting. PBKDF2 under load queues on the scheduler and the box
stays up. Argon2id at 64 MiB with twenty verifications in flight is 1.25 GiB
resident, and the login limiter does not bound concurrency — it bounds attempts
per address and per account, and a hundred addresses arrive together quite
happily.

So password verification runs behind a semaphore, four wide or thereabouts,
returning 429 with `Retry-After` when it is full rather than allocating. The
semaphore width and the memory parameter multiply, so they are chosen together
and measured on the target hardware rather than copied from a blog post.
64 MiB, t=3, p=4 is a reasonable place to start measuring.

### Where the hash lives

```sql
CREATE TABLE AccountPassword (
  account_id BLOB    PRIMARY KEY REFERENCES Account(id),
  hash       TEXT    NOT NULL,
  updated_at INTEGER NOT NULL
);
```

A row that does not exist means an account is *incapable* of password login
rather than configured against it. That retires Seafile's `"!"` sentinel
(`authmgr.go:39`, `authmgr.go:73`), which exists precisely because a nullable
column invites a code path that reads "no password" as "any password will do".

Login resolves the address through `AccountEmail`, case-folded, so every
address a user owns works. The asymmetry with
[identity binding](#binding-an-external-identity-to-a-silo-account) is
deliberate: an unverified address is fine for password login, where the
password is the proof and the address is only a lookup key, and is not fine for
linking an external identity, where the address *is* the proof.

### Close the enumeration oracle

[Finding 7](#7-login-says-which-accounts-exist): verify against a fixed dummy
hash when there is no account, so a miss costs what a hit costs. A handful of
lines, and it belongs with the Argon2id change rather than after it — the gap
gets wider, not narrower, when the KDF gets slower.

### Changing a password, and resetting one

Changing requires the *current* password, even when the request is
authenticated by a credential. Otherwise a stolen device credential upgrades
itself into account takeover, and the point of a scoped, revocable credential
is that it cannot become the account.

What gets revoked has two answers, because the obvious "everything" is wrong:

- **A user changes their own password** → bump the account generation, revoking
  every `session` credential at once. `device` credentials survive unless the
  request asks for them too. Unmounting somebody's laptop as a side effect of
  routine hygiene teaches them to stop doing hygiene.
- **An administrator resets a password** → revoke everything. The reason an
  administrator resets a password is that the user has lost control of
  something, and which something is not knowable from here.

### Coexisting with OIDC

Both can be true of one account: an `AccountPassword` row and an
`AccountIdentity` row are independent. A deployment that wants the IdP to be
the only path sets `SILO_PASSWORD_LOGIN=off`.

The failure mode to answer before that switch exists is the IdP being down with
nobody able to administer the server. Resolve it with the CLI on the host —
`silo user passwd` — rather than a standing exception for staff accounts.
Anyone who can run the CLI already owns the data directory, so it grants
nothing they did not have, and it avoids a permanently-enabled password path
that exists only for an emergency.

### Bootstrap without a password in the environment

`SILO_ADMIN_PASSWORD` is the one genuine cleartext password in the system
([finding 9](#9-smaller-things)): it sits in `docker-compose.yml`, in `.envrc`,
and in the environment of a running container.

Replace it. On first run with no accounts, mint a single-use `setup` credential
and print it to the log, valid for fifteen minutes or until used; the operator
creates the first account with it. `SILO_ADMIN_PASSWORD_FILE` stays for
automated deployments that need one. Either way the password leaves the
environment.

Half of that has landed already, in the cheapest form it can take:
`authmgr.BootstrapAdmin` creates `admin@silo.local` with a generated password
and logs it once when the user table is empty and no password was supplied. It
is a real account password rather than a single-use setup credential — that
part waits on credentials existing at all — but it means the default path to a
running server no longer goes through a cleartext password in the environment.

## Running with no authentication

Discovery, exploration, a test harness, a fresh checkout at 11pm — all of it is
faster when there is no credential to obtain first. Silo should support that
directly, so that nobody arrives at it by disabling a check.

```
SILO_AUTH=none          # every request is the configured account
SILO_AUTH=none:r        # …read-only, which is what exploring actually needs
```

### It grants a credential; it does not skip one

The implementation that matters: **no-auth is not a bypass.** `Resolve` is
still called on every request and still returns a `*Credential` — a synthetic
one, in memory, belonging to a real account, carrying a real `perm`. Nothing
downstream learns that the mode exists.

That is the whole design. A bypass means every handler grows a branch, and one
of those branches is eventually wrong in a build where the mode is off. A
synthetic credential means the authorization path has exactly one shape, is
exercised identically in development and production, and `share.CheckPerm` and
the [ceilings](#permission-ceilings) keep applying — `SILO_AUTH=none:r` really
is read-only, because it is the same ceiling code every other credential uses.

Which account: `SILO_AUTH_USER`, or the only account if there is exactly one.
If there are several and none is named, refuse to start. An ambiguous answer to
"who is everybody?" is not one to guess at.

### Making it hard to run by accident

The mode is safe in the case it is for and catastrophic in every other, so the
guards are about the boundary rather than the feature:

- **Environment or flag only**, never a value read from the database or from a
  file that a user of the server can write.
- **Refuse to start when the listener is not loopback**, unless a second,
  differently-named acknowledgement is also set. Silo already warns about a
  non-loopback `SILO_HOST`; with authentication off, a warning is not enough.
- **Say so, repeatedly.** A banner at startup, a line on every request log, and
  a field in `GET /api/silo/v1/server-info` — which is already unauthenticated
  — so the TUI and Porter can show it and skip login rather than inventing a
  credential.
- **Never in a release container's default configuration**, and it should be
  visible in `docker-compose.yml` only as a commented line explaining itself.

Encrypted libraries are unaffected: the server still cannot read one without
the password in `keycache`, and no-auth does not change that. Turning
authentication off gives away everything the account can see, which is the
point — it does not give away what the server itself cannot decrypt.

## OIDC

`future-features.md:301` lists LDAP/SAML/OIDC under Non-Goals. Committing to
this reverses that line, which should be updated rather than left to
contradict.

### Silo brokers login; it does not federate every request

The tempting design is to treat the IdP's access token as Silo's credential:
Porter sends `Authorization: Bearer <idp-token>`, Silo validates it per request.
It is wrong for this server, for reasons that hold whichever IdP is used.

A mounted filesystem issues requests at the rate of `ls`, not at the rate of
page loads. `stat` on a directory of two hundred entries is two hundred chances
to consult the IdP. Whatever the validation mechanism, the IdP is now in the hot
path of a filesystem — and when it is down, the filesystem is down. That is a
worse availability posture than Silo has today, bought for no gain.

The access token is also the wrong shape. Its lifetime is the IdP's to choose,
typically an hour, so Porter must hold a refresh token and re-mint constantly —
a long-lived secret on the device anyway, just one Silo cannot see, label,
scope, or revoke. And it carries the IdP's audience and scopes, not Silo's:
nothing in it says "read-only, library X, this laptop".

So Silo brokers. The IdP authenticates the human, once. Silo verifies that
result, mints its own `Credential` row — labelled, scoped, revocable, durable by
design — and every subsequent request is a local lookup. The authentication
decision is federated; the authorization state is Silo's.

This is what makes the rest of this document survive the change. Adding OIDC
adds one enrolment path and one `kind`. Nothing about `Resolve` moves, and an
OIDC deployment and a password deployment differ only in how a credential is
obtained, never in how it is checked.

### Only Silo can hold the registration

Porter is a compiled, signed application shipped to whoever installs it. Every
deployment it meets has a different identity provider. There is no `client_id`
it can be built with, because there is no value that would be right for more
than one server.

RFC 7591 dynamic client registration exists for exactly this, and some IdPs
implement it — Clinch among them — but it is commonly disabled and unevenly
supported. A desktop client's only login path cannot depend on a feature that
may not be present.

That settles the topology. **Silo is the OIDC client; Porter never speaks to the
IdP at all.** One registration per deployment, performed by the administrator
who is already writing a config file. Porter ships knowing nothing but the
server address it is given.

### Silo is the device in the device flow

RFC 8628 is usually described as the flow for televisions and consoles. The
requirement it actually encodes is narrower: *the client cannot host the user
agent*. Silo qualifies — it serves JSON and bytes, has no HTML anywhere, no
templates, no cookie sessions, and adding them would introduce a class of
vulnerability it currently cannot have.

So Silo runs the device grant as the client, against the IdP:

```
1. Porter → Silo     POST /api/silo/v1/device/code
                     { client_name: "Porter 1.2 (macOS)", perm: "r" }

2. Silo  → IdP       POST /oauth/device_authorization
                     client_id=silo, client_secret=…, scope=openid email profile
   IdP   → Silo      user_code=WDJB-MJHT
                     verification_uri=https://id.example.com/device
                     device_code=<opaque>, interval=5, expires_in=600

3. Silo  → Porter    { user_code, verification_uri, verification_uri_complete,
                       poll_token, interval, expires_in }

4. Porter displays the code, or opens verification_uri_complete locally.

5. The user approves at the IdP's device page.

6. Silo polls the IdP's token endpoint with device_code, honouring interval and
   slow_down: authorization_pending … then id_token + access_token.

7. Silo verifies the id_token against JWKS, resolves the account, mints a
   Credential, and discards the IdP's tokens.

8. Porter's poll returns the credential once, and Porter stores it.
```

**Silo's browser surface is nothing at all.** No form, no callback, no redirect,
no `redirect_uri`, no `state`, no HTML. The browser never touches Silo. This is
the flow `gh auth login` and `az login --use-device-code` use, for the same
reason.

Two codes exist and only one is human-visible. Silo mints an opaque `poll_token`
for Porter to poll with, and relays the IdP's `user_code` and URL for the
person. Porter never holds an IdP secret and never sees the IdP's `device_code`.

Three properties fall out of Silo being the client rather than Porter:

- The ID token's `aud` is Silo's `client_id`, which is what the spec requires.
  There is nothing to validate out-of-audience and no reason to introspect.
- `sub` is Silo's own pairwise subject, so `AccountIdentity` stores an
  identifier that belongs to Silo. A second Silo client added later changes
  nothing.
- The approval step is the IdP's device page, which requires the code to be
  typed and approved **on every enrolment** — unlike a consent screen, which is
  remembered after the first authorization and would wave the second device
  through unseen.

Silo asks for `openid email profile` and **not** `offline_access`. After step 7
Silo never acts on the user's behalf again, so a refresh token would be one more
long-lived secret to store and lose. The access token is discarded in the same
breath.

The one dependency this introduces is an IdP that implements RFC 8628. Clinch,
Keycloak, Authentik, Okta, Entra and Auth0 all do. For one that does not, the
fallback is an authorization-code flow with a Silo-hosted approval page — two
redirect handlers and a single HTML form, perhaps 150 lines. It should not be
built until an IdP is met that needs it.

### Which token is verified, and how

Exactly one, exactly once: the **ID token**, at step 7.

It is a JWT by specification — `id_token_signing_alg_values_supported` is a
required discovery field — signed by a key published at `jwks_uri`, and it is
the purpose-built assertion "this issuer authenticated this subject at this
time". Verification is offline against cached JWKS, RS256, about a millisecond,
once per device enrolment. Checked: signature, `iss`, `aud` equals Silo's
`client_id`, `exp`, `iat`, and `at_hash` when an access token accompanies it.

Deliberately not used:

- **Introspection (RFC 7662), cached or otherwise.** It is the right mechanism
  when a resource server must validate opaque tokens it did not mint. Silo has
  no such need: it mints its own. Using it per request would put a network call
  in the filesystem path, add a hard runtime dependency on the IdP, and bound
  revocation latency below by the cache TTL. A five-minute cache gives *worse*
  revocation than deleting a `Credential` row, which takes effect on the next
  request. Reaching for a cache to make a mechanism fast enough is a sign the
  mechanism is wrong.
- **Access-token JWTs.** Some IdPs sign access tokens for offline validation.
  That trades revocability for statelessness, the opposite of what a filesystem
  server wants, and it is not universal.
- **`userinfo` per request.** Same objection, and it is specified as an identity
  endpoint rather than an authorization one.

`userinfo` is useful on a slow schedule, out of the request path, to notice that
an email or group membership changed. Minutes to hours, never per request.

### Configuration

```
SILO_OIDC_ISSUER=https://id.example.com
SILO_OIDC_CLIENT_ID=silo
SILO_OIDC_CLIENT_SECRET=...
SILO_OIDC_TRUSTED=1     # this issuer's verified emails may claim an account
```

Silo is a confidential client — a server with a config file, capable of keeping
a secret — and authenticates at the device authorization and token endpoints
with `client_secret_post`. PKCE (S256) on top of that, on every flow: it costs a
SHA-256 and closes code interception independently of secret confidentiality.

Everything else — authorization, token, JWKS, and device endpoints — comes from
`/.well-known/openid-configuration`. Hardcoding those URLs is how integrations
break on an IdP upgrade.

`GET /api/silo/v1/server-info` (`server.go:724`, already unauthenticated)
advertises whether OIDC is configured, so Porter shows the right first screen
without being told.

### Binding an external identity to a Silo account

`AccountIdentity(issuer, subject)` is the join. On a verified ID token:

1. Look up `(iss, sub)`. Found → that account. Done.
2. Not found, `email_verified` is true, and the address is in `AccountEmail` →
   link `(iss, sub)` to that account and continue. **Only when the issuer is
   trusted** (`SILO_OIDC_TRUSTED`), because against an IdP you do not control
   this is account takeover by whoever can set an email claim. Against your own,
   it is the difference between a working system and a support ticket.
3. Otherwise → provision `Account`, `AccountEmail`, `AccountIdentity`.

Never key on email alone, and never treat an unverified email as identifying.

**Why step 2 exists.** OIDC requires `sub` to be stable for the relying party —
"never changes for that End-User" (Core §2). A good many IdPs derive it from a
row that is deleted when the user revokes the application's access, so
re-authorizing mints a fresh `sub`; Silo would see a stranger, provision a
second account, and orphan every library the first one owned. A verified address
lets Silo recover such a user instead of stranding them.

This is worth testing against the actual IdP before deployment, because the
failure is silent and the recovery — merging two accounts afterwards — is not
something the schema in this document supports. Clinch has this behaviour today:
`sub` is `OidcUserConsent#sid`, and "Revoke Access" destroys the consent
(`app/controllers/active_sessions_controller.rb`), which its own
`docs/backchannel-logout.md` documents as intended. "Logout" preserves the
consent and is safe; "Revoke Access" sits one click away and is not. The durable
fix belongs on the Clinch side — soft-delete the consent, or move `sid` to a
record that outlives it — since a pairwise subject is specified to be stable and
this affects every relying party, not only Silo.

### Logout and revocation

Backchannel logout is a push channel: the IdP POSTs a signed logout token to
`backchannel_logout_uri` when a session ends or access is revoked. It is
strictly better than any poll — lower latency, no steady-state cost — and it is
the right way to hear about revocation. It is also the one endpoint in this
design that the IdP calls directly, and it is JSON in, 200 out; still no HTML.

Verify it like any other assertion: RS256 against JWKS, `iss` matches, `aud` is
Silo's `client_id`, `events` contains the backchannel-logout event, `jti` not
replayed, and no `nonce` — its presence means someone handed you an ID token.

Then decide what it *means*, which the spec leaves open:

- A logout token should revoke `session` credentials for that account.
- Whether it revokes `device` credentials is a judgement. Signing out of a web
  session on a phone should probably not unmount a laptop; revoking the
  application's access probably should.

Most IdPs send the same token shape for both, Clinch included, so the
distinction has to be drawn on the Silo side. The defensible default is to
revoke `session` on any logout token and leave `device` to explicit revocation
in Silo's own device list, where the labels make it clear what is being killed.

Independently of all of this, `is_active` on `Account` is read on every
`Resolve`. That is the local kill switch, and it works when the IdP is
unreachable — which is exactly when it is most likely to be needed.

### What Porter ships with

Nothing but a server address field. No `client_id`, no issuer, no IdP
configuration of any kind. It learns whether OIDC is in play from `server-info`,
receives a code and a URL from Silo, polls Silo, and stores one credential.

When OIDC is not configured, `server-info` says so and Porter collects an email
and a password itself, posting them to `POST /api/silo/v1/auth/login`
(`server.go:699`). There is no device code on that path and no approval page —
see [Password login](#password-login). Porter does not otherwise know or care
how the human was authenticated, which is the point of brokering: the response
is a credential either way.

### Dependencies

`github.com/coreos/go-oidc/v3` and `golang.org/x/oauth2`. Neither is in
`go.mod`. They cover discovery, JWKS fetching and rotation, and ID token
verification — three things that are subtly wrong when hand-rolled. The device
grant itself is a POST and a polling loop; `oauth2` has `DeviceAuth` but the
loop is small enough to own.

## Deferred

### S3 needs a master key, not a column

SigV4 is an HMAC the server must recompute, so an S3 frontend needs the secret
in usable form. There is no asymmetric variant of S3 request signing and
presigned URLs need the same secret, so hashing is not available.

Derive rather than store:

```
access_key_id = "SILO" + base32(random(10))     // public; the Credential id
secret_key    = HKDF-SHA256(master_key, info = "silo-s3-v1|" + access_key_id)
```

The row holds no secret material at all. Verification re-derives; revocation
deletes the row so the derivation is never reached. The trade against
encrypting a stored secret is that rotating `master_key` invalidates every S3
credential at once — correct for a single-node server where reissuing is cheap.

This forces a persistent master key (`SILO_MASTER_KEY`, or a 0600 keyfile in
the data directory created on first run) and a decision about a missing one:
refuse to start if S3 credentials exist, rather than silently breaking every
one of them. The honest consequence is that the master key becomes the most
sensitive value in the deployment. That is inherent to serving S3 at all; the
mitigation is keeping it out of the database and out of the backup set.

### Encrypted libraries have nowhere to put the second password

Three different things are called "password":

- **Account password** — `EmailUser`, grants everything.
- **Encrypted-library password** — client-side key derivation, cached in
  `keycache`, never leaves the client in usable form. See `encryption.md`.
- **Device credential** — proposed above.

A WebDAV or S3 mount of an encrypted library needs two of them, and those
protocols have nowhere to carry the second. Either frontends refuse encrypted
libraries with an explicit error, or an out-of-band step primes `keycache`
before mounting. The failure mode if nobody decides is that the mount silently
serves ciphertext — the same bug the TUI has today. This is a paragraph of
policy, not a project, but it should be written before the first frontend
ships.

## Order of work

1. **The identity split.** `Account` (UUIDv7), `AccountEmail`,
   `AccountIdentity`, `AccountPassword`, and `account_id` through the fourteen
   legacy tables, with the API layer joining to produce the email SeaDrive
   expects. Everything else assumes this, and it is the one step that wants to
   land whole rather than in pieces.
2. **`Credential`, device credentials, hashed secrets, one `Resolve`** — with
   the `is_active` join that closes findings 1, 2, 5, 6 and 8 at once, and the
   legacy adapter that keeps SeaDrive working through it rather than beside it.
3. **A real user CLI** (`silo user add | disable | passwd`), which step 1 makes
   possible and `future-features.md`'s admin API then builds on.
4. **Password login as enrolment** — the credential-minting login response, the
   `AccountPassword` table, Argon2id behind a concurrency semaphore, the
   dummy-hash fix for finding 7, and setup credentials in place of
   `SILO_ADMIN_PASSWORD`.
5. **`SILO_AUTH=none`** — a synthetic credential from `Resolve`, the
   non-loopback refusal, and the `server-info` field. Cheap, and worth having
   early, because it is what makes the next three steps pleasant to develop
   against.
6. **Persistent JWT keyfile.** Closes finding 4 for notification tokens; login
   no longer depends on it.
7. **Permission ceilings in `CheckPerm`.** Read-only, single-library
   credentials — and the thing that makes `SILO_AUTH=none:r` mean something.
8. **Proof of possession** — public keys registered at enrolment, RFC 9421
   signatures on the Silo-native lanes, bearer retained for legacy clients and
   capability URLs. Independent of OIDC; whichever is wanted first.
9. **OIDC** — Silo's `/device/code` enrolment endpoint for Porter, the device
   grant run against the IdP, ID-token verification, `AccountIdentity` binding
   with verified-email recovery, and backchannel logout. Adds no browser
   surface, and step 2's `Credential` is the artefact it produces.
10. **Master key and S3 derivation.** Only gates S3; defer until S3 is wanted.

Steps 1 and 2 are the ones with a deadline: they are free only while there are
no deployments.
