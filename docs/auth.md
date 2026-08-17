# Credentials and Authentication

What Silo stores, why each kind is stored the way it is, and what has to change
before a protocol frontend (WebDAV, S3, SFTP — see
[`protocol-frontends.md`](protocol-frontends.md)) can authenticate against it.

## What is actually stored today

| Credential | Where | Form on disk | Entropy |
|---|---|---|---|
| Account password | `EmailUser.passwd` (ccnet) | PBKDF2-SHA256, 600k rounds, 32-byte random salt | Low — human-chosen |
| API token | `ApiToken.token` (seafile) | **Cleartext**, 160-bit random | High |
| Sync token | `RepoUserToken.token` (seafile) | **Cleartext**, random | High |
| Access token | `tokenstore`, memory only | Cleartext, never persisted | High |
| Session JWT | Not stored — signed with `option.JWTPrivateKey` | n/a | n/a |
| Encrypted-repo key | `keycache`, memory only | Derived key, never persisted | Low — human-chosen |

Two things follow from this table, and they point in opposite directions.

**Passwords are not stored in the clear.** `hashPassword`
(`fileserver/authmgr/authmgr.go:208`) writes
`PBKDF2SHA256$600000$<salt>$<derived>`, and `needsRehash` /`upgradeHash`
(`authmgr.go:227`, `authmgr.go:246`) silently upgrade any legacy Seafile hash —
unsalted SHA-1, or SHA-256 with the public constant salt at `authmgr.go:34` — the
next time the user successfully logs in. The password column is the
best-protected thing in the database.

**The token columns are the weak link.** `apitokenstore.Create`
(`apitokenstore.go:62`) inserts the raw token, and `repomgr.go:859` does the
same for sync tokens. A stolen `seafile.db` yields immediately usable
credentials for every device, with no cracking required.

## Why not just compare raw passwords?

This is the obvious question once you notice the tokens are in cleartext: if the
database already contains working credentials, what is 600,000 rounds of PBKDF2
buying?

The answer is that the two kinds of secret face completely different attacks,
and the asymmetry is entropy.

An API token is 160 bits from `crypto/rand` (`apitokenstore.go:51`). There is no
dictionary for it, no rainbow table, no "they probably used their dog's name",
and no chance the same value exists anywhere else in the world. An attacker
holding the hash of one cannot work backwards; brute force is 2^160. Slowing
down a guess that will never succeed accomplishes nothing, which is why a slow
KDF on a token is pure waste.

An account password is maybe 30–40 bits in practice, drawn from a distribution
attackers have measured precisely. With a fast hash, a stolen `EmailUser` table
is cracked at billions of guesses per second and most of it falls in minutes.
At 600k PBKDF2 rounds the same attack runs at a few thousand guesses per second
per core — roughly a millionfold tax, paid by the attacker on every guess and by
Silo exactly once per login.

And the consequence of losing a password is not confined to Silo. Users reuse
passwords. A cracked Silo password is plausibly the user's email password, which
is the account that can reset everything else they own. A leaked sync token
grants access to one Silo library and nothing else, ever. The slow KDF is
protecting the human as much as the service.

So storing passwords in cleartext would be strictly the worst option available:
it converts a database read into every user's plaintext password, reused
elsewhere, permanently — with no work factor and no recovery short of a global
password reset.

**The correct conclusion runs the other way.** Not "stop hashing passwords
because tokens are in the clear", but "start hashing tokens — cheaply, because
they don't need more than that."

### Fixing the token columns

A 160-bit random value needs a *fast* hash, not a KDF. Store `SHA-256(token)`
and match on that:

```go
// apitokenstore.Lookup, today:
"SELECT email, expires_at FROM ApiToken WHERE token = ?", token
// after:
"SELECT email, expires_at FROM ApiToken WHERE token_hash = ?", sha256hex(token)
```

One hash per lookup, the index still works, no behaviour change, and a stolen
database stops being a list of working credentials. The same applies to
`repomgr.go:348` and `repomgr.go:374` for sync tokens.

The awkward part is migration: the hashes cannot be derived without the
plaintext tokens, which by definition only the clients still hold. Two options —
invalidate every existing token and make everyone re-authenticate once, or carry
`token` and `token_hash` side by side, write only the hash for new rows, and
drop the plaintext column after a deprecation window. The second is kinder and
means the cleartext column lives on for a release; that is a deliberate choice,
not an oversight, and should be written down as one.

This work is worth doing on its own merits, independent of whether any protocol
frontend is ever built.

### One genuine cleartext password

`SILO_ADMIN_EMAIL` / `SILO_ADMIN_PASSWORD` are read from the environment
(`EnsureAdmin`, `authmgr.go:265`), so the admin password sits in cleartext in
`docker-compose.yml`, in `.envrc`, and in the process environment of a running
container. It is hashed the moment it reaches the database, but everything
before that point is plaintext. Worth knowing; worth a `SILO_ADMIN_PASSWORD_FILE`
variant eventually.

## Why per-request protocols break the current design

The 600k work factor is justified in a comment at `authmgr.go:198`:

> That is a cost worth paying at login — logins are rare here, since both kinds
> of token are durable.

That is true today. `ValidatePassword` has exactly three call sites
(`api/api.go:70`, `api/seadrive.go:36`, and the TUI through the first), all of
them login endpoints that mint a durable token and never verify a password
again. Nothing caches a password verification because nothing needs to.

WebDAV and S3 authenticate **every request**. There is no session. At the
measured ~80ms per verification (`authmgr.go:197`), Basic auth over WebDAV gives
roughly 12 requests/sec/core — from legitimate traffic. A Finder `PROPFIND` walk
is dozens of requests; a folder drag-and-drop is thousands. One user mounting
one library can saturate the server.

The login rate limiter cannot help, by design: only failures spend a token
(`api/loginlimit.go:64`), and these are all successes.

The tempting fix — cache successful password verifications keyed by
`email:password` — reintroduces exactly the offline-guessing economics the work
factor exists to prevent, and holds password-derived material in memory. Don't
cache the password. Issue a different credential.

## Yes, we can issue credentials

Both frontends can have credentials minted for them rather than reusing the
account password. The mechanisms differ, because the protocols demand different
things.

### WebDAV: app passwords

Straightforward. Generate a random secret, show it once, store only its hash.

```
silo cred create --kind app --label "macbook finder"
  → silo_app_7f3c9a1e5b2d8f4a6c0e9b7d3a5f1c8e   (shown once, never again)
```

The client puts it in Basic auth, which is what every WebDAV client already
does. Verification is one SHA-256 against `Credential.secret_hash` — microseconds,
not 80ms. It is not the account password, so it can be revoked individually
without disturbing anything else, and it can carry a scope.

`apitokenstore` is already 90% of this: `Create` / `Lookup` / `ListByEmail` /
`Delete` all exist. What's missing is a user-facing create path (today tokens
are only minted by the login handler at `seadrive.go:45`), a label, and a scope.

### S3: access key ID plus a derived secret

Harder, because of one unavoidable protocol constraint: **SigV4 is an HMAC the
server must recompute**, so the server must be able to obtain the secret in
usable form. There is no asymmetric variant of S3 request signing, and presigned
URLs need the same secret. Hashing is not an option here.

That doesn't mean storing a secret in the database, though. Derive it:

```
access_key_id  = "SILO" + base32(random(10))        // public; travels in the
                                                    // Authorization header
secret_key     = HKDF-SHA256(master_key,
                             info = "silo-s3-v1|" + access_key_id)
```

The `Credential` row stores the access key ID, owner, label, scope and expiry —
**no secret material at all**. To verify a request: look the ID up, confirm the
row is live, re-derive the secret, recompute the SigV4 chain, compare. To create
one: derive it once and show it to the user. Revocation is deleting the row, at
which point the derivation is never reached again.

This has a real advantage over encrypting a stored secret: there is no
per-row ciphertext to manage and no re-encryption pass on key rotation. The
trade-off is that rotating `master_key` invalidates every S3 credential at once,
where encrypt-at-rest would let you re-encrypt in place. For a single-node
server where S3 credentials are cheap to reissue, invalidate-all is the right
trade.

**The master key is a new, hard requirement.** Silo has no persistent key
management today, and the closest thing — `SILO_JWT_SECRET` — is *ephemeral by
default*, auto-generated when unset. Deriving S3 secrets from an ephemeral key
would invalidate every credential on every restart. So S3 support forces a
persistent secret (`SILO_MASTER_KEY`, or a keyfile in the data directory created
on first run with mode 0600) and a decision about what happens when it is
missing: refuse to start if S3 credentials exist, rather than silently breaking
every one of them.

Note the honest security consequence: with derived secrets, compromise of the
master key compromises every S3 credential, and access key IDs are public. The
master key becomes the single most sensitive value in the deployment. That is
inherent to serving S3 at all — the protocol requires a recoverable shared
secret — and the mitigation is keeping it out of the database and out of the
backup set, not pretending it isn't there.

### One table for both

```sql
CREATE TABLE Credential (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  kind        TEXT    NOT NULL,   -- 'app' | 's3'
  public_id   TEXT    UNIQUE,     -- access key ID for s3; NULL for app
  secret_hash BLOB,               -- SHA-256 for app; NULL for s3 (derived)
  email       TEXT    NOT NULL,
  label       TEXT    NOT NULL,
  scope       TEXT,               -- NULL = all libraries; else a repo id
  perm        TEXT    NOT NULL,   -- 'r' | 'rw'
  ctime       INTEGER NOT NULL,
  expires_at  INTEGER,            -- NULL = no expiry
  last_used   INTEGER
);
CREATE INDEX credential_email_idx ON Credential (email);
```

`label` and `last_used` are not decoration. `silo token list` today prints
indistinguishable 40-char hex strings, which means nobody can safely decide
which one to revoke. A name and a last-seen timestamp turn revocation from a
guess into a decision.

`scope` and `perm` are what makes a read-only, single-library credential
possible — the thing you actually want to hand to a backup tool. They depend on
`share.CheckPerm` (`fileserver/share/share.go:40`) growing a notion of a
credential-level ceiling that intersects with the user's own permission, so a
credential can never grant more than its owner has.

## Interactions to get right

### The login limiter will lock out real users

`loginIPLimiter` allows 10 attempts per 30s and `loginAccountLimiter` 10 per
minute (`api/loginlimit.go:32-33`) — correctly sized for a human typing a
password.

Give Finder a stale password and it does not ask once. It retries per request,
in parallel, across a directory walk. Ten failures in about two seconds empties
the **account** bucket. The user corrects it in Keychain and still sees 429s,
because the mount is still hammering with the old credential. Since that bucket
is per account, one wedged WebDAV mount also locks the same user out of the TUI
and their sync clients.

Behind a reverse proxy without `SILO_TRUST_PROXY_HEADERS`, every request shares
one address bucket, so a single misconfigured mount can 429 the whole
deployment.

The fix is not to loosen the limits. It is that issued credentials are
high-entropy and therefore need no rate limiting at all — the limiter exists to
protect low-entropy secrets. Frontends authenticating with `Credential` rows
should bypass `allowLoginAttempt` entirely, and the account-password login path
should keep exactly the limits it has.

### Sliding expiry makes long-lived mounts permanent

`Lookup` slides a token's expiry once it is more than halfway through its life
(`apitokenstore.go:102-107`). An always-on WebDAV mount polls constantly, so the
30-day TTL (`option.go:235`) never elapses. That is probably the behaviour you
want for a mounted filesystem — but it should be a decision recorded here rather
than an emergent property. `last_used` plus an absolute `expires_at` that does
*not* slide is the alternative if credentials should genuinely age out.

### Revocation lag

`AuthCacheTTL` defaults to 5 minutes (`option.go:236`), caching token and
permission lookups in `tokenCache` / `permCache` (`sync_api.go:101-102`). A
revoked credential keeps working for up to that long, which the README already
documents for the CLI. Every new credential type adds another cache surface that
revocation has to purge; the `Credential` lookup path should either share the
existing cache discipline or deliberately skip caching.

### Three different things called "password"

- **Account password** — `EmailUser`, PBKDF2, grants everything.
- **Encrypted-library password** — client-side key derivation, cached in
  `keycache`, never leaves the client in usable form. See
  [`encryption.md`](encryption.md).
- **App password / access key** — proposed above.

A WebDAV mount of an encrypted library needs two of them, and **WebDAV has
nowhere to put the second**. Either frontends refuse encrypted libraries with an
explicit error, or there is an out-of-band step that primes `keycache` before
mounting. The failure mode if nobody decides is that the mount silently serves
ciphertext, which is the same bug the TUI has today.

## Suggested order

1. **Hash the token columns.** Small, self-contained, improves the system
   whether or not a frontend is ever built. Decide the migration strategy first.
2. **The `Credential` table and app passwords.** Dissolves both the PBKDF2
   cost problem and the rate-limiter lockout, and is the prerequisite for
   WebDAV and SFTP alike.
3. **Scope and permission on credentials.** Read-only backup credentials are
   most of the practical value, and they need `CheckPerm` to learn about
   ceilings.
4. **Master key and S3 derivation.** Only gates S3; defer until S3 is actually
   wanted.
5. **Encrypted-library policy.** A paragraph, not a project — but write it
   before the first frontend ships, not after.
