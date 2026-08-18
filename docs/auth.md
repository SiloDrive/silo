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

### 7. Smaller things

`utils.GetAuthorizationToken` (`utils/http.go:14`) splits the header on a space
and returns field 1 — it ignores the scheme entirely, so `Bearer`, `Token` and
`Basic` are indistinguishable on the sync lane. Harmless today only because the
three stores are disjoint; it forecloses ever telling two schemes apart on one
lane, and it turns "you sent the wrong credential" into "invalid token".

`SHA1(uuid)` for sync tokens is copied from the C implementation. The entropy
is fine — a uuid through SHA-1 is still a uuid's worth of randomness — but the
hash accomplishes nothing except producing 40 characters to fit `CHAR(41)`.

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

## The design

One table, one verification path, several kinds of credential.

### Token format

```
silo_<kind>_<id>_<base32(32 random bytes)>
```

The `id` is public, indexed, and how the row is found; only `SHA-256(secret)`
is stored, compared with `subtle.ConstantTimeCompare`. Looking a credential up
by id rather than by secret means the comparison is constant-time over a fixed
32 bytes, and it means the secret never appears in a query, a query log, or a
slow-query trace.

The `silo_` prefix and the kind make a leaked credential identifiable on sight
— in a log, a bug report, or a secret scanner — and let a handler reject a
credential meant for another lane before touching the database.

### Kinds

| Kind | Held by | Lifetime | Scope |
|---|---|---|---|
| `device` | Porter, the File Provider extension, any long-lived client | absolute, default 90d | optional repo + permission ceiling |
| `session` | the TUI, the CLI, a browser | 24h | account |
| `sync` | the upstream Seafile client, via `Seafile-Repo-Token` | absolute | one repo |
| `access` | capability URLs (`/files/`, `/zip/`) | 1h, memory only | one object, one op |
| `s3` | an S3 frontend, if it is ever built | absolute | see [S3](#s3-needs-a-master-key-not-a-column) |

### The table

```sql
CREATE TABLE Credential (
  id          TEXT    PRIMARY KEY,   -- public, travels in the token
  kind        TEXT    NOT NULL,      -- 'device' | 'session' | 'sync' | 's3'
  secret_hash BLOB,                  -- SHA-256; NULL for kinds that derive
  account_id  BLOB    NOT NULL REFERENCES Account(id),
  label       TEXT    NOT NULL,      -- "dan's macbook, porter-fuse"
  scope       TEXT,                  -- NULL = all libraries; else a repo id
  perm        TEXT    NOT NULL,      -- 'r' | 'rw' — a ceiling, never a grant
  client_id   TEXT,                  -- device identity, when the lane has one
  ctime       INTEGER NOT NULL,
  expires_at  INTEGER,               -- absolute; NULL = no expiry
  last_used   INTEGER
);
CREATE INDEX credential_account_idx ON Credential (account_id);
```

`label` and `last_used` are not decoration. They are what turns revocation from
a guess into a decision.

`expires_at` is **absolute and does not slide**. The current `ApiToken`
behaviour renews any token more than halfway through its life
(`apitokenstore.go:102`), which means an always-on mount polling constantly can
never age out — the 30-day TTL is unreachable in the one case it was written
for. Absolute expiry plus `last_used` gives an operator the same information
without the credential quietly becoming permanent.

### One verification path

Every lane resolves its credential through a single function:

```go
func Resolve(kind Kind, presented string) (*Credential, error)
```

which parses the prefix, looks up by id, compares the hash in constant time,
checks `expires_at`, **joins `Account` and checks `is_active`**, and stamps
`last_used`. One place. That join is the property that cannot be retrofitted
onto three separate stores: disabling a user has to kill every lane at once, or
it does not mean anything.

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

### Passwords: Argon2id

Since every password can be reset, use the better primitive. Argon2id is
memory-hard; PBKDF2 is not, which is why a GPU eats it. `validatePasswd`
(`authmgr.go:72`) already dispatches on a stored prefix, so `argon2id$...`
slots in beside the existing formats with no special casing, and `needsRehash`
already upgrades anything weaker on next login.

Parameters worth writing down when chosen: 64 MiB, t=3, p=4 is a reasonable
starting point, measured on the target hardware rather than copied.

### Rate limiting stops applying to credentials

The login limiter exists to protect a low-entropy secret. A 256-bit credential
does not need it, and applying it causes harm: a wedged mount retrying a stale
credential in parallel empties the **account** bucket in about two seconds, and
that bucket is shared with the TUI and every sync client the same user owns.

So: the password path keeps exactly the limits it has. `Resolve` bypasses
`allowLoginAttempt` entirely.

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

`GET /api/silo/v1/server-info` (`server.go:700`, already unauthenticated)
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

When OIDC is not configured the protocol is unchanged: Silo's `/device/code`
returns a code whose approval path is a password login rather than an IdP
redirect, or Porter uses `POST /api/silo/v1/auth/login` (`server.go:699`)
directly. Porter does not know or care how the human was authenticated, which is
the point of brokering.

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
   the `is_active` join that closes findings 1, 2, 5 and 6 at once.
3. **A real user CLI** (`silo user add | disable | passwd`), which step 1 makes
   possible and `future-features.md`'s admin API then builds on.
4. **Persistent JWT keyfile.** Closes finding 4 for notification tokens; login
   no longer depends on it.
5. **Argon2id**, since every password can be reset from scratch.
6. **Permission ceilings in `CheckPerm`.** Read-only, single-library
   credentials.
7. **OIDC** — Silo's `/device/code` enrolment endpoint for Porter, the device
   grant run against the IdP, ID-token verification, `AccountIdentity` binding
   with verified-email recovery, and backchannel logout. Adds no browser
   surface, and step 2's `Credential` is the artefact it produces.
8. **Master key and S3 derivation.** Only gates S3; defer until S3 is wanted.

Steps 1 and 2 are the ones with a deadline: they are free only while there are
no deployments.
