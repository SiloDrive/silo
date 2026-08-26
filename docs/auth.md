# Credentials and Authentication

The account model and the credential design: what has landed, what is built
but not yet wired, and what remains. The historical findings against the
inherited Seafile model are kept lower down; most are closed or mooted now,
and each says which.

Silo still has **no deployments**. Every credential in every table can be
discarded and reissued, which is what licensed replacing the credential model
outright rather than patching it. That freedom expires the first time someone
else runs this server.

## Where this stands

**Landed:**

- **The identity split.** `Account` (UUIDv7), `AccountEmail`,
  `AccountIdentity`, `AccountPassword`; `account_id` is the only user key on
  the live tables and `EmailUser` is gone. `is_active` is checked on **every
  authenticated request** — the join inside `credential.load` asks once,
  unskippably, for every lane, which is the check the split existed to make
  possible.
- **A user lifecycle**: `silo user list | add | passwd | disable | enable`,
  over `account.Create` / `SetPassword` / `SetActive`. `BootstrapAdmin`
  replaces the cleartext `SILO_ADMIN_PASSWORD` path with a generated,
  logged-once password when the account table is empty.
- **One lane.** Everything outside `/api/silo/v1/*` and `/notification` is a
  404 since `5d4baa0`. The three-credential, three-header shape this document
  was written against no longer exists to authenticate to.

- **`Credential`, and one `Resolve` reading it.** Every route under
  `/api/silo/v1` and the notification socket authenticate through
  `credential.Resolve`; `middleware.RequireCredential` is the only thing
  mounted on the API subrouter. Login mints a `session` credential instead of
  signing a JWT, `credential.Issue` is the one way a row comes into being, and
  `silo token` lists and revokes out of that single table.

  The three stores it replaced are gone with it: the session JWT
  (`GenerateSessionToken`/`ValidateSessionToken` deleted), the `ApiToken` table
  and its package, the `LibraryUserToken` table and `libmgr`'s functions over
  it, and `middleware.RequireAPIToken`. The `Credential` swap is what finally
  made "revoke everything this person holds" a statement the command could make
  good on.

**What a client actually presents today:**

| Credential | Where | Form on disk | Lifetime | Revocable |
|---|---|---|---|---|
| Account password | `AccountPassword.hash` | self-describing prefix, PBKDF2-SHA256 600k today | — | n/a |
| Session credential | `Credential`, kind `session` | `SHA-256(secret)` only; id is the public half | 24h, absolute | **yes**, immediately |
| Access token | `tokenstore`, memory only | uuid v4, cleartext | 1h | one-time redeem |
| Notification JWT | not stored — signed with `option.JWTPrivateKey` | HS256, `aud=silo:notif` | 72h, per library | no |

The notification JWT is the last unstored bearer token, and deliberately: it is
verified in a process with no database. It is why [finding 4](#4-the-jwt-secret-is-ephemeral-by-default)
survives the swap when findings 3, 5 and 6 did not.

## What is right, and worth keeping

Credit where it is due, because the password column is the best-protected thing
in the database and that did not happen by accident.

`hashPassword` writes `PBKDF2SHA256$600000$<salt>$<derived>` at OWASP's
current work factor, sixty times Seafile's 10,000. `needsRehash` /
`upgradeHash` (`authmgr.go:249`, `authmgr.go:268`) silently replace any legacy
hash the next time the user successfully logs in, which is the only moment the
plaintext is in hand and known good.

`ValidateSessionToken` passes `jwt.WithValidMethods` and `jwt.WithAudience`, so
a token cannot select its own algorithm and a notification token cannot be
replayed as a session token even though both are signed with the same key.

`allowLoginAttempt` (`api/loginlimit.go:45`) limits per address *and* per
account, charges only failures, and clears the account bucket on success.

`tokenstore.QueryToken` redeems a one-time token with a single `LoadAndDelete`,
so two requests racing on the same capability URL cannot both be served.

Distinguishing `ErrNotFound` from a database error, so an outage does not
present itself to a client as "your credential is invalid". `apitokenstore`
had this right and is gone; `credential.Resolve` kept the property, and
`middleware.credentialRefused` is where it turns into 401 or 500.

None of that changes below. What changes is everything around it.

## The findings, and where each stands

The numbered findings this design was argued from, kept because later sections
cite them — each now carries its status.

### 1. The token columns are cleartext — *closed*

`apitokenstore.Create` inserted the raw token and `libmgr` did the same for
sync tokens. Both tables and both packages are deleted, and the one table left
stores `SHA-256(secret)` and looks a row up by its public id, so the secret
never reaches a query, a query log or a slow-query trace. A read of `silo.db`
now yields no usable credential at all. See
[why a fast hash is the right one](#why-tokens-want-a-fast-hash-and-passwords-do-not).

### 2. `is_active` is never read — *closed*

Closed by the identity split, and now closed at a lower level still: the check
is a join inside `credential.load`, so it costs nothing extra and no lane can
be written that skips it. The account-lifecycle half is closed too: `silo user`
exists, and disabling an account stops every credential it holds on the next
request — without deleting them, so re-enabling restores the user's devices
rather than making everyone log in again.

### 3. Sessions cannot be revoked — *closed*

A session is a row now, so revoking one is deleting it: `silo token revoke
<email> <id>` stops that credential and leaves the account's others working,
and `silo token revoke <email>` stops all of them. `Resolve` reads the row on
every request, so there is no cache and no window — the next request after the
delete is a 401.

What is still absent is a *logout endpoint*: a client cannot revoke its own
credential over HTTP, only an operator can at the CLI. That is a smaller hole
than this finding described and it belongs with the enrolment work, since the
same route table has to grow a way to mint before it grows a way to discard.

### 4. The JWT secret is ephemeral by default — *open, and narrowed*

`LoadJWTConfig` generates a random key when `SILO_JWT_SECRET` is unset and
logs a line about it. What that costs has shrunk: sessions are rows and survive
a restart, so the only thing an ephemeral key still invalidates is the
**notification tokens** in flight. A client re-mints one per subscription, so
the cost is a reconnect rather than a re-login.

The keyfile is still worth having — a restart should not disconnect every
watching client — but it no longer stands between a user and their account. The
part of this finding that pushed **the account password into the Keychain** as
the documented workaround is gone with it: a device holds a revocable
credential, and the password is needed once, at enrolment.

### 5. Nothing has a scope — *closed on the server; nothing mints one yet*

`middleware.Perm(r, libraryID, path)` is the one place the ceiling is applied,
and every handler that used to call `share.CheckPerm` now calls it instead. A
read-only credential cannot write, a library-scoped one 403s on every other
library, and a path-scoped one reaches its subtree and nothing else — measured
through the real router in `fileserver/ceiling_test.go`.

Three consequences worth stating, because each is a deliberate refusal rather
than an oversight:

- **Library-wide operations pass `""` for the path**, so a path-scoped
  credential is refused `changes`, `commits` and `notify-token`. There is no
  way to answer "what changed in this library" partially without telling the
  holder about paths it may not reach.
- **The chunk and object surfaces are library-level** for the same reason from
  the other end: they are addressed by content hash, and an id says nothing
  about where it will be linked.
- **A batch is library-level**, so a path-scoped credential cannot use it. A
  batch is ordered and all-or-nothing over a working tree, and a move or copy
  names two paths, so "does this stay inside the scope" is a question about the
  resulting tree rather than about each path in isolation. Refusing is the
  honest answer until that is worked out.

`Perm` **fails closed on a nil credential** and logs it: every route is mounted
under `RequireCredential`, so no credential means a route registered outside
the authenticated subrouter, which is a mistake rather than an anonymous
caller.

What remains is the enrolment half — **no route mints a scoped or read-only
credential**. Login mints one unscoped `rw` session; anything narrower has to
come from `credential.Issue` in a test or a future device-enrolment endpoint.
The mechanism is done and honoured; what is missing is a way to ask for it.

### 6. Nothing has a name — *closed*

`silo token list` prints, per credential, its id, its kind, its label, when it
was created, when it expires and when it was last used. Login names the
credential after the `User-Agent` that asked for it, and `credential.Issue`
refuses a row with no label at all, so the column cannot quietly go back to
being empty. Deciding which one to revoke is a decision rather than a guess,
which is the whole reason `label` and `last_used` are columns.

`last_used` is stamped at five-minute granularity, not per request: a write
transaction in front of every read would serialise an otherwise concurrent
workload behind an always-on mount's polling, and five minutes answers "is
anybody still using this?" exactly as well.

### 7. Login says which accounts exist — *closed on login; open on the salt endpoint*

`ValidatePassword` returned immediately when there was no account: an address
that existed cost 600,000 PBKDF2 rounds and one that did not cost a database
round trip. Measured before the fix, that was 240ms against 109µs — a factor of
two thousand, and a directory listing for anyone willing to time the endpoint.

A miss now verifies against `dummyHash`, derived once at whatever work factor
`HashPassword` currently uses, so raising the iteration count raises this too
and the gap cannot quietly reopen. An account that exists with no
`AccountPassword` row takes the same path, since that is the same question
asked a different way. The test measures the timing rather than the error
string, because timing is the property — a test that checked only the message
would have passed on the code this replaces.

Still open in one place: **the pre-login KDF parameters endpoint** is the same
oracle in a new shape, and it does not exist yet. When it does, an unknown
address has to receive plausible parameters rather than a 404, and the same
ones every time — see
[the client's KDF](#the-clients-kdf-is-not-this-one-and-it-needs-four-columns).

### 8. The credential model was inherited, not chosen — *history*

`LibraryUserToken.token` was `CHAR(41)` because the C daemon hashed a UUID to
forty hex characters and left room for a NUL. There were three credentials on
three headers because Seahub and `seaf-server` were separate programs that had
to authenticate callers to each other. Sync tokens were per library, never
expired, and only ever accumulated, because that was the cheapest thing for a
daemon holding no session state. None of those was a decision Silo made — and
the question this finding used to open, how much of that shape to keep for
legacy clients' sake, was answered by deleting the legacy lanes whole.

### 9. Smaller things

`utils.GetAuthorizationToken` (`utils/http.go:14`) splits the header on a
space and ignores the scheme entirely. The sync lane that made this matter is
gone and the function now has no callers — it should be deleted before
something finds it. `credential.tokenFromRequest` dispatches on the scheme
properly, and answers `Bearer`, `Token` and `Silo` differently.

`SILO_ADMIN_PASSWORD` in the environment is *half-closed*: `BootstrapAdmin`
generates and logs a password when the table is empty and none was supplied,
so the default path no longer requires a cleartext password in the
environment. The variable still works when set, and the single-use setup
credential that would retire it fully waits on credentials existing at all.

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

And the blast radius differs. A leaked token grants what the token grants and
nothing else. A cracked password is plausibly the user's email password, which
is the account that can reset everything else they own. The slow KDF protects
the human as much as the service.

## Identity: an account is not an email address — *landed*

This section is the argument that produced the schema now in
`dbutil/schema.go`; it is kept as the record of why the shape is what it is.

Before the split, email was not a column on the user — it *was* the key,
repeated as a foreign key across fourteen tables and 59 SQL statements.
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

**Nine tables carry an account id, not fifteen.** Several of the tables listed
above are queried by no Go code at all: `Binding`, `UserRole` and `LDAPUsers`
by nothing, `LibraryTrash`, `FileLocks`, `FolderUserPerm` and `FolderGroupPerm`
because trash, locking and folder-level permissions are unimplemented,
`UserShareQuota` because only `UserQuota` is read, and all six `Org` tables
because both callers of the org-aware share functions pass `orgID = -1` and the
branch reading them is unreachable. They are dropped along with the branch,
rather than carried. What remains: `LibraryOwner`, `LibraryGroup`, `GroupUser`,
`Group`, `UserQuota`, `SharedLibrary` (both ends) and `Credential`.
(`LibraryUserToken` and `ApiToken` were on this list and have since been
dropped outright — see [Where this stands](#where-this-stands).)

**A separate password table, because not every account has a password.** An
OIDC-only account has none, and a nullable `passwd` column is how you end up
with a code path that reads "no password" as "any password will do". Seafile's
guard against exactly that is the sentinel string `"!"`, checked in two places
(`authmgr.go:39`, `authmgr.go:73`). A row that does not exist cannot be
compared against.

### Email lives in API responses, not in the schema

That was the rule the split was executed under, and it held: tables key on
`account_id`, and the API layer joins to produce an address on the way out
wherever a response wants one. There was no migration because there was
nothing to migrate — `EmailUser` was deleted, not drained, and a database from
before the split is recreated, not upgraded. (The subsection here used to
enumerate the `/api2` endpoints whose responses needed the email join for
SeaDrive's sake; that lane is deleted and the constraint is gone with it.)

### The one place email is permanent

`Commit.CreatorName` is fed into the commit hash (`commitmgr.go:77`) and
`SeafDirent.Modifier` into an fs object's serialisation (`fsmgr.go:124`). Both
are content-addressed: the string is part of the object id. Rewriting them
means rewriting every object in every library.

That is not a problem, and it is worth naming so nobody tries to fix it. It is
the git model — an author string on a commit is historical display data, not an
identity key, and it is *correct* for it to record the address in use at the
time. Silo must simply never resolve a permission from it.

## The legacy adapter — retired unbuilt

A long section here designed the compatibility story for SeaDrive and Seafile
Desktop: a `legacy` credential kind, an edge adapter translating
`Authorization: Token` and `Seafile-Repo-Token` into `Resolve` calls, and a
"legacy client is a legacy trust level" rule for labelling what an
unmodifiable client cannot support. The lane deletion (`5d4baa0`) removed the
clients before the adapter was built, so none of it is needed: there is one
lane, one header, and no bearer-only client class to carve out. The design
below sheds the `legacy` kind with nothing else moving — which is itself the
best evidence the adapter really was an adapter and not a shape.

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
| `device` | Porter, the File Provider extension | a signature — [proof of possession](#proof-of-possession) | absolute, default 90d | optional library + permission ceiling |
| `session` | the TUI, the CLI | a signature, or a bearer secret where there is no key store | 24h | account |
| `access` | capability URLs (`/files/`, `/zip/`) | bearer, memory only | 1h | one object, one op |
| `s3` | an S3 frontend, if it is ever built | SigV4 | absolute | see [S3](#s3-needs-a-master-key-not-a-column) |

(A `legacy` kind for SeaDrive existed in this table until the legacy lanes
were deleted; see [the retired adapter](#the-legacy-adapter--retired-unbuilt).)

### The table — *landed; read on every request*

The DDL lives in `dbutil/schema.go` now and differs from the first draft here
in one deliberate way: `scope` is `TEXT NOT NULL DEFAULT ''` rather than
nullable — empty means "every library", so there is one spelling of unscoped
rather than two, and `credential.ParseScope` owns the encoding of the narrower
forms (which can name a folder inside a library, not just a library).

```sql
CREATE TABLE Credential (
  id          TEXT    PRIMARY KEY,   -- public, travels in the token
  kind        TEXT    NOT NULL,      -- 'device'|'session'|'access'|'s3'
  secret_hash BLOB,                  -- SHA-256 of the secret, for bearer kinds
  public_key  BLOB,                  -- SPKI, for proof-of-possession kinds
  account_id  BLOB    NOT NULL REFERENCES Account(id),
  label       TEXT    NOT NULL,      -- "dan's macbook, porter-fuse"
  scope       TEXT    NOT NULL DEFAULT '',  -- '' = all libraries
  perm        TEXT    NOT NULL,      -- 'r' | 'rw' — a ceiling, never a grant
  client_id   TEXT,                  -- device identity, when the lane has one
  ctime       BIGINT  NOT NULL,
  expires_at  BIGINT,                -- absolute; NULL = no expiry
  last_used   BIGINT,
  CHECK (secret_hash IS NULL OR public_key IS NULL)
);
```

`label` and `last_used` are not decoration. They are what turns revocation from
a guess into a decision.

A row carries a secret hash or a public key, never both. An `s3` row carries
neither and derives its secret from the master key.

`expires_at` is **absolute and does not slide**. The `ApiToken` behaviour this
replaced renewed any token more than halfway through its life, which meant an
always-on mount polling constantly could never age out — the 30-day TTL was
unreachable in the one case it was written for. Absolute expiry plus
`last_used` gives an operator the same information without the credential
quietly becoming permanent.

Expired rows are swept hourly by `credential.StartCleanup`. That is table
space only — `Resolve` refuses an expired credential whether or not the sweeper
has run — but a client re-logs in when its credential lapses, so an always-on
mount leaves one dead row behind per day and nothing else would collect them.
A row with no expiry has `expires_at` NULL, which fails the comparison rather
than reading as zero, so the sweep cannot reach the long-lived device
credentials.

`Issue` refuses a negative lifetime rather than treating it as none. Zero means
"no expiry" and the arithmetic that skips writing `expires_at` would otherwise
have turned the one request that most clearly means *this must not work* into
the one credential that works forever.

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
  Signature-Input: sig=("@method" "@target-uri" "date");created=1755950400
                       ;nonce="AAECAwQFBgcICQoLDA0ODw"
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

#### The wire format, pinned

RFC 9421 is a framework, not a format: it leaves the algorithm, the signature
encoding, the covered components and the parameter set to the profile using it.
Every one of those is a place where a signer and a verifier are each correct
and do not interoperate, so each is pinned here rather than chosen twice.

- **Algorithm: `ecdsa-p256-sha256`.** One algorithm, not a negotiation. The
  cost note below compares P-256 with Ed25519; that comparison is settled and
  P-256 wins on platform key stores — the Secure Enclave and most TPMs hold
  P-256 and will not hold Ed25519, and a non-exportable key is the whole point
  of this lane.
- **Signature encoding: raw `r ‖ s`, 64 bytes.** Not DER. Go's
  `ecdsa.SignASN1` and CryptoKit's `derRepresentation` both default to DER, and
  a DER signature verified as raw fails; a raw signature parsed as DER fails
  differently. `rawRepresentation` on the Swift side, `r` and `s` fixed to 32
  bytes each and left-padded on the Go side.
- **The stored SPKI is authoritative for the key *and* the curve.** The
  verifier reads `Credential.public_key`, and nothing the request says about
  which key or which algorithm to use is honored. A signature the request gets
  to describe is a signature the request gets to weaken.
- **The wrong key type is rejected at enrolment, not at verification.** The
  SPKI is parsed when the credential is registered and refused unless it is
  P-256; a key that is not the one algorithm never reaches the table. Checking
  at verification instead would leave rows in `Credential` that can never
  authenticate anything, failing on every request with an error that looks like
  a signing bug on a device that is in fact holding a key the server should
  have refused to store — and the person debugging it is the one who cannot see
  the table.
- **The nonce is a signature parameter, not a covered component.**
  `;nonce="…"` per RFC 9421 §2.3 — *not* `"nonce"` in the component list, which
  would require inventing a `Nonce:` request header this profile has no other
  use for. The parameter form is the RFC's own definition, and it keeps the
  nonce out of the header-canonicalization surface entirely: parameters are
  covered by the signature regardless, through `@signature-params`. Sixteen
  random bytes, base64.
- **`created` is required, and the skew window measures against it.** A
  signature whose `created` is more than **60 seconds** from the verifier's now
  — in either direction — is rejected before the key is even loaded. The nonce
  cache therefore needs to span exactly that window and no longer, which is
  what keeps it bounded with no cleanup logic beyond expiry.
- **`created` and `nonce` are the only parameters honored.** Not `expires`, not
  `alg`, not `tag`. `expires` would let a signer widen its own replay window;
  `alg` is the algorithm-confusion door, already closed by the previous point.
- **`keyid` is not used.** The credential id travels in
  `Authorization: Silo <id>` and nowhere else. A request that carries a `keyid`
  parameter anyway is **rejected on mismatch, not ignored**: two fields naming
  the key is two fields that can disagree, and a verifier that silently prefers
  one of them is the confused-deputy bug somebody builds on later.

That list closes every degree of freedom in this lane. A signer and a verifier
written from it on different days should meet in the middle.

**When the lane is built, the signing contract gets the store-format
treatment** — a file under [`spec/`](spec/) with cross-implementation vectors,
because porter-mac has to produce byte-identical signature bases and prose has
never once been enough for that. Signature base construction, the canonical
form of each covered component, and the nonce rules are what the vectors pin.
This section is the decision record; the spec is the contract. Not now — it
waits for something to verify against, the way `store-format.md` waited for
`store/`. [`porter-brief.md`](porter-brief.md) then gets the captured request
flow, as it does for every other lane.

#### The body is mostly not in the signature

Signing a digest of the body would mean buffering a five-gigabyte upload before
deciding whether the request is authentic. That is unacceptable, and it turns
out to be unnecessary, because of what Silo's writes already are.

Almost every write is content-addressed, idempotent, and carries the content's
own hash in the request line:

- `PUT /libraries/{id}/blocks/{chunk-id}` — the chunk id *is* the hash of the
  bytes, and the server verifies what arrives against it.
- The object uploads behind `batch` — manifests, directories, commits — each
  verified against its own id on the way in.

Signing the request line therefore binds the payload already: bytes that hash
to something else are rejected by the write path regardless of who sent them.
And replaying one of these is harmless — it stores bytes that are already
stored, under an id they already have.

What is left is the small set of requests where replay means something:

- The commit/head advance — the one genuine state change in a sync. Its
  target travels in the request line, so the signature covers it, and the
  head move is a compare-and-swap: a replayed advance fails its precondition.
- The mutations on `/api/silo/v1` — mkdir, rename, move, delete, and writes to
  `entries` — which have small bodies and already accept `If-Match` /
  `If-None-Match`. A conditional write is replay-proof by construction: the
  second attempt fails its precondition.

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

P-256 verification is around a hundred microseconds — roughly twice Ed25519's
fifty, which is the price paid for a key the Secure Enclave will actually hold.
A directory walk issuing a few hundred requests pays single-digit milliseconds
in total, comfortably under the storage reads it is making anyway.

The clients that can do this are the ones we control — which, since the lane
deletion, is all of them: Porter, the File Provider extension, the TUI, the
CLI, and any future SFTP frontend by way of SSH keys. The one exception left
is S3, whose SigV4 needs a shared secret the server can recompute with;
capability URLs also stay bearer, because a URL cannot sign anything.

### Permission ceilings — *landed*

`share.CheckPerm(libraryID, user)` keeps answering what the *user* may do.
`Credential.scope` and `Credential.perm` intersect with it:

```
effective = min(CheckPerm(library, cred.account_id),
                cred.perm  if cred.scope in (NULL, library) else "")
```

A credential can only ever narrow. That is what makes a read-only,
single-library credential safe to hand out: it cannot outlive or exceed the
account behind it, and if the account's own permission is withdrawn the
credential follows immediately.

It is applied in one function, `middleware.Perm(r, libraryID, path)`, and not
as two calls at each site, because the failure it prevents is precisely a
handler that remembers `CheckPerm` and forgets the narrowing. `share.CheckPerm`
takes an account and knows nothing about credentials, which is why the
intersection lives beside the request rather than inside it.

The encoding is a superset of what the table comment describes — `ParseScope`
also reads `<library-id>:<path>`, one folder and everything beneath it, which
is the case a scoped mount actually wants. See
[finding 5](#5-nothing-has-a-scope--closed-on-the-server-nothing-mints-one-yet)
for which operations a path scope is refused, and why.

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
generation, so revocation is immediate rather than lagging a cache TTL. (The
sync lane once had a library-scoped version of this; the account-scoped one is
the same idea a level up, and with one lane there is only one cache to get
right.)

## What Porter does

Today: log in with email+password, get a 24h JWT, keep the password in the
Keychain because there is no refresh — the workaround finding 4 describes.

After: log in once, get a **device credential**, store *that* in the Keychain,
never the password. One credential, one header — `Authorization: Bearer
silo_device_…` — everywhere.

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

### It is not a device grant — *landed*

The device grant exists for one reason: the client cannot host the user agent
that has to talk to the authenticator. A local password has no third party —
Porter collects it in its own window — so the code-and-approval dance would buy
nothing, at the cost of the HTML approval page the OIDC design specifically
declined to build.

So `POST /api/silo/v1/auth/login` stays, and becomes the password half of
enrolment.

**The request decides which response comes back.** A body carrying any of
`kind`, `client_name`, `public_key`, `perm` or `scope` is an enrolment and gets
the shape below; a body carrying none of them gets the `200 {"token": …}` it
always got, byte for byte. The old shape cannot be widened — adding a number to
a token body broke a client once already, on the day the field it had asked for
shipped ([the note](bugs/fixed/adding-a-number-to-a-token-response-breaks-clients.md))
— and two spellings of one secret in one body is one spelling too many.

```
POST /api/silo/v1/auth/login
{ "email": "dan@…", "password": "…",
  "kind": "device",                       // default "session"
  "client_name": "Porter 1.2 (macOS)",    // becomes label
  "public_key": "<base64 SPKI>",          // optional; bearer secret if absent
  "perm": "r", "scope": "<library-id>" }     // optional, narrowing only

201 { "credential": "silo_device_…", "expires_at": …, "email": "…" }
```

`perm` and `scope` follow the [ceiling rule](#permission-ceilings--landed):
asking for `rw` on an account that has `r` yields `r`, not a 403. A field that
can only narrow needs no validation branch — only a check that it is spellable,
because `minPerm` reads an unrecognised permission as no access at all and a
typo would otherwise mint a credential that authenticates and permits nothing.

Three refusals, all `400` except the last:

- **`kind` outside `session` and `device`.** `access` and `s3` are real kinds
  and are not minted by presenting a password: one belongs to a capability URL
  and lives in memory, the other derives its secret from the master key.
- **Enrolment with no `client_name`.** A client asking for a 90-day credential
  and declining to say what it is leaves an operator a row they cannot decide
  about, which is the thing `label` exists to prevent. A plain login still
  falls back to the `User-Agent`.
- **`public_key`, which answers `501`.** Proof of possession is designed and
  the RFC 9421 verifier is not built, so the row would resolve to
  `ErrSignatureNotImplemented` forever. It matches what `RequireCredential`
  answers for `Authorization: Silo`, and beats handing back a credential that
  can never work.

The request is validated before the password is checked, so a malformed
enrolment is a `400` whether or not the password was right — the shape of the
answer says nothing about the account — and the rate limiter goes on charging
password attempts rather than typos.

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

There is no algorithm column, and that omission is load-bearing. Every hash
written here names its own format — `PBKDF2SHA256$iterations$salt$hash` today,
`argon2id$…` next — because `validatePasswd` dispatches on the prefix and falls
back to guessing by length. A bare hash with no prefix would be unreadable to
it. Anything that ever writes this column has to keep that true.

Login resolves the address through `AccountEmail`, case-folded, so every
address a user owns works. The asymmetry with
[identity binding](#binding-an-external-identity-to-a-silo-account) is
deliberate: an unverified address is fine for password login, where the
password is the proof and the address is only a lookup key, and is not fine for
linking an external identity, where the address *is* the proof.

### The client's KDF is not this one, and it needs four columns

Once [`plans/store-v2.md`](plans/store-v2.md)'s split-derivation login lands
there are **two** argon2id derivations per password and they are constantly
mistaken for one. The client stretches the password into `authKey` under
parameters it fetches before logging in; the server stretches the `authKey` it
receives into `AccountPassword.hash` under its own. Two KDFs, two parameter
sets, one living in each schema — neither is vestigial, and raising one does
not raise the other. (The server's half also gets *cheaper* when this lands: by
this document's own rule a 256-bit `authKey` needs no memory-hard KDF at all,
so the semaphore above stops being the constraint it is today.)

Silo has designed the client half twice — [`spec/store-format.md`](spec/store-format.md)
pins the wire format, this document pins the account model — and connected them
nowhere. Nothing in the schema stores any of the key material, and the endpoint
that hands out the client's parameters has no route and no backing column. That
is a gap between two finished designs rather than an unfinished one, which is
why it is written down here before something implements against its absence.

**Four schema items.** The shapes below are a starting point, not settled DDL:

```sql
ALTER TABLE AccountPassword
  ADD COLUMN client_kdf_params TEXT;      -- $argon2id$v=19$m=...,t=..,p=..$<salt>

CREATE TABLE AccountIdentityKey (         -- exactly one per account
  account_id  BLOB PRIMARY KEY REFERENCES Account(id),
  public_key  BLOB NOT NULL,              -- X25519, 32 bytes, published
  wrapped_key BLOB NOT NULL,              -- store wrap kind 1, sealed under wrapKey
  updated_at  INTEGER NOT NULL
);

CREATE TABLE AccountRecoveryWrap (        -- ten per account
  account_id  BLOB    NOT NULL REFERENCES Account(id),
  ordinal     INTEGER NOT NULL,           -- which of the set; never the code
  wrapped_key BLOB    NOT NULL,           -- store wrap kind 2
  ctime       INTEGER NOT NULL,
  PRIMARY KEY (account_id, ordinal)
);
```

- **`client_kdf_params` sits on `AccountPassword`** because it governs how a
  password becomes `authKey`, and an account with no password has none. It is
  the PHC string `store.KDFParams.String()` writes, carrying the per-user salt,
  so there is no separate salt column — the same self-describing argument the
  hash column already makes. Deliberately adjacent to `hash`, with a comment,
  because two argon2 parameter sets far apart and unexplained is how the next
  reader concludes one of them is dead.
- **`public_key` is a column, not a derivation.** It is what another member's
  client wraps a content key to, so it is read by people who are not its owner
  and must be servable without unwrapping anything.
- **Recovery wraps are rows, and the row granularity is forced.** Redeeming a
  code deletes its blob and leaves the rest of the set standing — the spec is
  explicit that regenerating the set on redemption is the tidier-looking rule
  and the worse one, because it invalidates the codes a person is still holding
  at the moment they have proved they lost their device. One deletable row per
  code is that rule expressed as a schema constraint. A single blob column
  holding a serialised set would make partial redemption a read-modify-write.
  `ordinal` names the row; the server never learns a code, so redemption is the
  client fetching the set and trying each blob.
- **`holder` is `Account.id` in canonical lower-case hyphenated UUID text**,
  which is exactly what `account.ID.String()` already renders. It is bound into
  every wrap as associated data and the store package now refuses any other
  spelling, so nothing has to remember the rule — but note that it makes the
  account id load-bearing in a new way. It was already immutable; now changing
  it would make every blob unopenable rather than merely breaking joins.

**The pre-login parameters endpoint** is the sharp one, because a client cannot
log in without it: `authKey` is a function of parameters only the server knows.
Given an address it returns that account's `client_kdf_params`. Two constraints
it must satisfy, neither of which falls out of writing the obvious handler:

- **It is unauthenticated and therefore an enumeration oracle by default** —
  [finding 7](#7-login-says-which-accounts-exist) in a new place. An unknown
  address has to receive plausible parameters rather than a 404, and the same
  ones every time, or the difference between two requests answers the question.
  Derive them from `HMAC(server_secret, normalized_address)` so they are stable
  without being stored, exactly as the dummy-hash fix works.
- **The answer is attacker-influenced input to the client's KDF**, which is
  what `store`'s ceiling exists for: a server answering `m=4 GiB` does not
  weaken anything, it takes the device down. The client validates against
  `store.KDFParams.Validate` before deriving — it already does — and this is
  the request that makes that guard load-bearing rather than theoretical.

### Enrolling into an account that already exists

Sharing a library with an address nobody has enrolled under is a thing people
do, and it has to mint something for `SharedLibrary.to_account_id` to point at:
an inactive `Account` with the address claimed and no `AccountPassword`, which
cannot be signed in to. Enrolment then finds the address already taken and
claims that account rather than colliding with it, which reunites the shares
with the person they were meant for.

**That is an email-reuse attack surface, and it must be gated.** A corporate or
university address that lapses and is reassigned hands the new holder every
share the departed holder was given. So claiming an existing inactive account
must require verifying the address — a link to it, not merely typing it — and
never happen on the strength of a password chosen at the signup form. The same
rule the [identity binding](#binding-an-external-identity-to-a-silo-account)
step already applies to unverified OIDC email claims applies here, for the same
reason.

Verification bounds the damage but does not remove it: the new holder does
control the mailbox. So it is worth deciding, before this ships, whether shares
older than some threshold are surfaced to whoever granted them — "this library
is still shared with someone who has just enrolled" — rather than silently
reattached. That is a product decision, not a security control, and it is the
one that turns a quiet takeover into a visible event.

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

- **A user changes their own password** → revoke every `session` credential at
  once and leave `device` credentials standing unless the request asks for them
  too. Unmounting somebody's laptop as a side effect of routine hygiene teaches
  them to stop doing hygiene. **Not built**: there is no HTTP endpoint for a
  user to change their own password, so this case has nowhere to live yet. With
  one credential table it needs no generation column — it is a delete with a
  `kind` in the where clause.
- **An administrator resets a password** → revoke everything. The reason an
  administrator resets a password is that the user has lost control of
  something, and which something is not knowable from here. **Landed**:
  `silo user passwd` revokes every credential the account holds and says how
  many. It runs after the password is set, not before — a revocation that ran
  and then failed to change the password would sign every device out and leave
  the old password working, which is the worst of both.

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

## OIDC

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

`GET /api/silo/v1/server-info` (`server.go:534`, already unauthenticated)
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
(`server.go:533`). There is no device code on that path and no approval page —
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

### E2EE libraries have nowhere to put the second secret

Three different things authenticate something:

- **Account password** — `AccountPassword`, grants the account.
- **The library content key (CK)** — client-side, wrapped to member identity
  keys, never on the server in usable form. See `encryption.md`.
- **Device credential** — above.

A WebDAV or S3 mount of an E2EE library needs the second as well as the third,
and those protocols have nowhere to carry it — and the server-side answer
(hold a key for the client) is exactly what the E2EE guardrails forbid. So
frontends that cannot run the client crypto must refuse E2EE libraries with an
explicit error, and the listing's `encrypted` flag is how they know. The
failure mode if nobody decides is a mount silently serving ciphertext. A
paragraph of policy, not a project, but it should be written before the first
frontend ships.

### Running with no authentication

`SILO_AUTH=none` — every request resolving to a synthetic credential for a
configured account, with `SILO_AUTH=none:r` narrowing it to read — was step 5
of the list below for one reason: discovery, a test harness, a fresh checkout
at 11pm, all of it faster when there is no credential to obtain first. Silo
should support that directly, the argument went, so that nobody arrives at it
by disabling a check.

Two things are true now that were not when that was written.

**The first-boot credential exists.** `authmgr.BootstrapAdmin` mints
`admin@silo.local` with a generated password and logs it once when the account
table is empty, [above](#bootstrap-without-a-password-in-the-environment). The
case this mode was for — a server nobody can log in to without having set two
environment variables before the first boot — is now one warning line and one
`POST /auth/login`. The Ruby harness in [`test/`](../test/README.md) already
does exactly that, transparently, and gains nothing from the mode; and CI
never reaches the generated password at all, because it can set
`SILO_ADMIN_EMAIL` and `SILO_ADMIN_PASSWORD` itself and know the answer before
the server starts.

**The ergonomics argument moves to step 2.** No-auth was specified as a
credential rather than a bypass — `Resolve` still called on every request,
still returning a `*Credential`, so the authorization path keeps exactly one
shape and `share.CheckPerm` and the [ceilings](#permission-ceilings) keep
applying. That design is right, and it is also why the mode could never land
before `Resolve` mounts. But once it does, a long-lived device credential in an
environment variable buys the same absence of a login round trip through the
real code path — and unlike this mode it is revocable, nameable and
narrowable.

What is left is a mode that is safe in the case it was for and catastrophic in
every other, where the guards are the bulk of the work rather than the feature:
environment or flag only, never a value the database or a user-writable file
can supply; refuse to start off loopback unless a second, differently-named
acknowledgement is set, because the existing non-loopback warning is not enough
once authentication is off; a startup banner, a line on every request log, and
a field in the unauthenticated `server-info` so a client can show it and skip
login; never in a release container's default configuration. Plus a rule for
*which* account is everybody — `SILO_AUTH_USER`, or the sole account, or refuse
to start, because an ambiguous answer to "who is everybody?" is not one to
guess at.

That is a day of guards buying an ergonomic win a static credential already
buys. Deferred rather than rejected: reconsider when a concrete consumer asks
— porter mounting without enrolment, or a harness where the login round trip is
genuinely in the way. Neither is asking.

One thing not to lose with it. `SILO_AUTH=none:r` was the forcing function for
[permission ceilings](#permission-ceilings) — the mode is why a read-only
credential had to *mean* something rather than being a column nobody
intersects. The ceilings are worth building on their own merits and keep their
place in the list without this.

E2EE would have been unaffected either way, and the reason is worth keeping on
the record: the server holds only ciphertext, so turning authentication off
gives away everything the account can see and nothing the server itself cannot
decrypt.

## Order of work

1. **The identity split.** `Account` (UUIDv7), `AccountEmail`,
   `AccountIdentity`, `AccountPassword`, and `account_id` as the only user key
   on the nine live tables, with the API layer joining to produce the email
   SeaDrive expects. `EmailUser` is deleted, not drained. Everything else
   assumes this, and it lands whole rather than in pieces.
2. **`Credential`, device credentials, hashed secrets, one `Resolve`** — with
   the `is_active` join. Mounted 2026-08-26: every route resolves through it,
   login mints a session credential, and the session JWT, `ApiToken` and
   `LibraryUserToken` went with it. Findings 1, 3 and 6 closed; 5 is
   expressible and waits on step 6 to be enforced. Device credentials are the
   half still to come — nothing mints one yet, because what a device is handed
   at enrolment is [step 4](#order-of-work)'s question. (The legacy adapter
   this step once included is retired — the clients it served are gone.)
3. **A real user CLI** (`silo user add | disable | passwd`), which step 1 makes
   possible and `future-features.md`'s admin API then builds on. Done, with
   `list` and `enable` alongside — a `disable` with no way back is a one-way
   door, and an operator cannot use any of the others without first being able
   to see what addresses exist. Passwords are never taken as a flag, for
   [finding 9](#9-smaller-things)'s reason one scope smaller: a flag is
   readable through `/proc` by every other process on the host and kept in the
   operator's shell history afterwards. A terminal is prompted twice with echo
   off, a pipe is read from stdin, and `-generate` invents one and prints it
   once.
4. **Password login as enrolment** — *part-done 2026-08-26*: the
   credential-minting login response, the `AccountPassword` table and the
   dummy-hash fix for finding 7 have landed, as has revocation on an
   administrator reset. Argon2id behind a concurrency semaphore and setup
   credentials in place of `SILO_ADMIN_PASSWORD` have not — and the Argon2id
   half should wait, because split-derivation login makes a memory-hard
   server-side KDF unnecessary and would undo it. This is also where
   [the client's KDF](#the-clients-kdf-is-not-this-one-and-it-needs-four-columns)
   lands — the four schema items and the pre-login parameters endpoint — because
   split-derivation login *is* password login, and building the server half
   first means building it twice. It is sequenced by
   [`plans/store-v2.md`](plans/store-v2.md) phase 2, which needs it.
5. **Persistent JWT keyfile.** Closes finding 4 for notification tokens; login
   no longer depends on it.
6. **Permission ceilings.** Landed 2026-08-26, as `middleware.Perm` rather
   than as a change to `CheckPerm` — the ceiling is a property of the request,
   and `share.CheckPerm` answers about a user, so intersecting them belongs
   where the credential is rather than inside the function that knows nothing
   about one. Read-only and single-library credentials are honoured; the
   enrolment route that would hand one out is step 2's remaining half.
   `SILO_AUTH=none:r` used to be the argument for this step and is now
   [deferred](#running-with-no-authentication); the ceilings outlived it,
   because step 2's device credentials are what a client is handed and a
   credential that cannot be narrowed is one that can only be revoked.
7. **Proof of possession** — public keys registered at enrolment, RFC 9421
   signatures on the Silo-native lanes, bearer retained for legacy clients and
   capability URLs. Independent of OIDC; whichever is wanted first.
8. **OIDC** — Silo's `/device/code` enrolment endpoint for Porter, the device
   grant run against the IdP, ID-token verification, `AccountIdentity` binding
   with verified-email recovery, and backchannel logout. Adds no browser
   surface, and step 2's `Credential` is the artefact it produces.
9. **Master key and S3 derivation.** Only gates S3; defer until S3 is wanted.

Where the list stands: **steps 1, 2, 3 and 6 are done.** `Resolve` is mounted,
the three token stores it replaced are deleted, a credential can be listed,
named and revoked — with the revocation taking effect on the next request
rather than whenever a cache ages out — and a narrowed credential is now
honoured on every route.

Step 2 is finished: `POST /auth/login` mints device credentials with a label, a
permission and a scope, so the ceilings are reachable by a real client rather
than only by a test.

**Step 4 is part-done.** Its enrolment half has landed, along with
[finding 7](#7-login-says-which-accounts-exist--closed-on-login-open-on-the-salt-endpoint)'s
dummy hash and the administrator-reset revocation. What remains is the KDF, and
it is worth being explicit about the order: building **Argon2id on the server
now is work that split-derivation login later undoes**. This document already
says so — a 256-bit `authKey` needs no memory-hard KDF, and the semaphore stops
being the constraint — so the server-side KDF should be decided together with
[the client's KDF](#the-clients-kdf-is-not-this-one-and-it-needs-four-columns)
rather than before it. PBKDF2 at 600k is not the thing standing between this
system and safety in the meantime.

Two smaller gaps, both waiting on the same route table:

- **No logout endpoint.** A client cannot discard its own credential over HTTP,
  only an operator can at the CLI.
- **No self-service password change**, which is where the session-only
  revocation above belongs.

And one that is not about routes: **the single-use `setup` credential** still
has not replaced `SILO_ADMIN_PASSWORD`, though `BootstrapAdmin` already keeps
the default path out of the environment.

One thing step 3 surfaced and step 2 has now fixed: a password change still
does not revoke anything, but it *can* — `credential.RevokeAll` is the call,
and whether changing a password should sign out every device is a policy
question rather than a missing mechanism. `silo user passwd` still says
nothing is revoked, which is now a choice rather than a confession.

Still open from step 3, unchanged: `is_staff` can be set when an account is
created but not afterwards, which is fine only until
[`plans/admin-check.md`](plans/admin-check.md) makes the flag mean something.
