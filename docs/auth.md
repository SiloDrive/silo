# Credentials and Authentication

The authoritative account and credential model: who a user is, what a client
presents to prove it, and what that proof may reach.

**Part 1 describes the server as it runs.** If the code and Part 1 disagree, one
of them is a bug. **Part 2 is designed and not built** — nothing in it exists
unless it says so.

Silo has no deployments yet. Every credential in every table can be discarded and
reissued, which is what licensed replacing the model outright rather than
patching it. That freedom expires the first time someone else runs this server.

---

# Part 1 — as built

## Three rules

**An account is an opaque id.** Addresses, external identities and a password
hang off it as attributes, so none of them is the key.

**Every secret a client presents is a row** in one table, resolved by one
function. Revoking any of them is deleting a row. There is no second store and no
lane that authenticates some other way. Two secrets sit outside that table, both
listed below and both for reasons the table itself creates rather than for
convenience.

**A credential can only narrow.** Its `perm` and `scope` are a ceiling
intersected with what the account may do, never a grant of their own.

| What a client holds | Where it lives | Lifetime | Revocable |
|---|---|---|---|
| Account password | `AccountPassword.hash` — self-describing prefix, PBKDF2-SHA256 at 600k | — | n/a |
| Session credential | `Credential`, kind `session`; `SHA-256(secret)` only | 24h, absolute | yes, next request |
| Device credential | `Credential`, kind `device`; `SHA-256(secret)` only | 90d, absolute | yes, next request |
| Notification token | not stored — HS256 over `option.JWTPrivateKey` | 72h, per library | no |
| Setup token | `SetupToken.token`, one row, in the clear | until the first account exists | by claiming it, or `silo user add` |

The notification token is the only unstored bearer token, and deliberately: it is
verified in a process with no database.

The setup token is the only secret held in the clear, and the only one that is
not a `Credential`. Both follow from what it is for: it exists precisely while
`Credential` cannot hold it — that table's `account_id` references an `Account`
row, and there are none — and it has to be legible because the server reprints
it and `silo setup-token` prints it. It authenticates nobody and grants no
access; it authorises the single transition from no accounts to one. See
[claiming a server](#claiming-a-server-that-has-no-accounts).

## Identity

Making an address the key costs four things, in rising order of how much they
hurt: changing an address becomes a many-table migration, and a half-applied one
is a permission bug; an account cannot hold two addresses; an OIDC subject has
nowhere to live, because it is not an address and never will be; and "is this
account still active?" has no single place to be asked.

```sql
CREATE TABLE Account (
  id         BLOB    PRIMARY KEY,        -- UUIDv7, 16 bytes
  display    TEXT,                       -- what a human is called; not an identifier
  is_active  INTEGER NOT NULL DEFAULT 1,
  role       TEXT    NOT NULL DEFAULT 'user',  -- admin | user | guest; account.ParseRole is the one rule
  ctime      INTEGER NOT NULL
);

-- One row per administrative operation an account holds. Rows rather than
-- columns, so a new administrative verb is data and not a migration -- and half
-- an answer rather than the whole one: the rule is a conjunction, role = admin
-- AND a row exists, spelled once in fileserver/admin. See plans/admin.md.
CREATE TABLE AccountCapability (
  account_id BLOB NOT NULL REFERENCES Account(id),
  capability TEXT NOT NULL,             -- users | passwords | quota | tokens | retention | grant
  PRIMARY KEY (account_id, capability)
);

CREATE TABLE AccountEmail (
  email       TEXT    PRIMARY KEY,       -- lowercased; account.Normalize is the one rule
  account_id  BLOB    NOT NULL REFERENCES Account(id),
  is_primary  INTEGER NOT NULL DEFAULT 0,
  verified_at INTEGER
);
CREATE UNIQUE INDEX account_email_primary_idx ON AccountEmail (account_id) WHERE is_primary = 1;

CREATE TABLE AccountIdentity (            -- external identities, one row each
  issuer     TEXT    NOT NULL,
  subject    TEXT    NOT NULL,
  account_id BLOB    NOT NULL REFERENCES Account(id),
  ctime      INTEGER NOT NULL,
  PRIMARY KEY (issuer, subject)
);

CREATE TABLE AccountPassword (            -- separate: not every account has one
  account_id BLOB    PRIMARY KEY REFERENCES Account(id),
  hash       TEXT    NOT NULL,
  changed_at INTEGER NOT NULL
);
```

**UUIDv7, not v4.** Both are 128 bits, but v7 leads with a millisecond
timestamp, so consecutive inserts land beside each other in the index.

**The partial unique index is the one that matters.** Without it an account
accumulates two primary addresses and every query asking what it is called starts
depending on row order.

**A separate password table, because not every account has a password.** An
OIDC-only account has none, and a nullable column is how "no password" turns into
a path reading "any password will do". A row that does not exist cannot be
compared against.

**No algorithm column, and that omission is load-bearing.** Every hash names its
own format — `PBKDF2SHA256$iterations$salt$hash` today — because
`authmgr.validatePasswd` dispatches on the prefix. Anything writing this column
has to keep that true.

Every table that names an account carries an `account_id`; none keys on an
address. The API layer joins
to produce one where a response wants it, and `credential.load` joins
`AccountEmail` for the primary address on every request, because that is what a
commit records as its author.

**One place an address is permanent, and it must stay that way.**
`Commit.Author` and the directory entry's `Modifier` are fed into the object ids that
name them, so rewriting either means rewriting every object in every library.
That is correct rather than a defect — it is the git model, where an author
string is display data recording the address in use at the time. Silo must simply
never resolve a permission from one.

## The credential

### Token format

```
silo_<kind>_<id>_<base32(32 random bytes)><check>
```

Lowercase RFC 4648 base32, unpadded, decoded strictly: a spelling the encoder
would not produce is refused, because two spellings of one id is ambiguity on a
primary key.

- **`id` is public**, 10 random bytes, and is how the row is found. Only
  `SHA-256(secret)` is stored, compared with `subtle.ConstantTimeCompare`. Looking
  up by id keeps the secret out of every query, query log and slow-query trace,
  and keeps the comparison constant-time over a fixed 32 bytes.
- **The prefix and kind** make a leaked credential identifiable on sight — a log,
  a bug report, a secret scanner — and let a handler reject one meant for another
  lane before touching the database.
- **`check` is six base32 characters of SHA-256** over everything before it. A
  truncated paste becomes *malformed*, before the database is touched, rather
  than *invalid* after a lookup — and a secret scanner can confirm a match
  instead of guessing.

### Kinds

| Kind | Held by | Lifetime | Status |
|---|---|---|---|
| `session` | the TUI, the CLI, any client that just logs in | 24h | built |
| `device` | silo-drive (FUSE mount, File Provider extension) | 90d default | built |
| `access` | a capability URL, if one is ever built | — | reserved; nothing mints it |
| `s3` | an S3 frontend, if one is ever built | — | reserved; see [S3](#s3-needs-a-master-key-not-a-column) |

There is no `legacy` kind. Every client can be changed, so a scheme Silo does not
mint for is a client that has misread the model rather than one to accommodate.

### The table

```sql
CREATE TABLE Credential (
  id          TEXT    PRIMARY KEY,   -- public, travels in the token
  kind        TEXT    NOT NULL,      -- 'session'|'device'|'access'|'s3'
  secret_hash BLOB,                  -- SHA-256 of the secret, for bearer kinds
  public_key  BLOB,                  -- SPKI, for proof-of-possession kinds
  account_id  BLOB    NOT NULL REFERENCES Account(id),
  label       TEXT    NOT NULL,      -- "dan's macbook, silo-drive"
  scope       TEXT    NOT NULL DEFAULT '',  -- '' = every library
  perm        TEXT    NOT NULL,      -- 'r' | 'rw' — a ceiling, never a grant
  client_id   TEXT,                  -- device identity, when a lane has one
  ctime       BIGINT  NOT NULL,
  expires_at  BIGINT,                -- absolute; NULL = no expiry
  last_used   BIGINT,
  CHECK (secret_hash IS NULL OR public_key IS NULL)
);
```

A row carries a secret hash or a public key, never both. An `s3` row would carry
neither and derive its secret from a master key, which is why the `CHECK` permits
both being NULL. `credential.Issue` writes `client_id` when the caller supplies
one and NULL otherwise.

**`label` and `last_used` turn revocation from a guess into a decision.**
`credential.Issue` refuses a row with no label, so the column cannot quietly go
back to being empty; login names a credential after the `User-Agent` when the
client does not name itself. `last_used` is stamped at five-minute granularity,
because a write transaction in front of every read would serialise an otherwise
concurrent workload behind an always-on mount's polling, and five minutes answers
"is anybody still using this?" exactly as well.

**`expires_at` is absolute and does not slide.** A sliding expiry means a client
that polls constantly never ages out, which makes the TTL unreachable in the one
case it was written for. `Issue` refuses a negative lifetime rather than reading
it as none: zero means "no expiry", and the arithmetic that skips the column
would otherwise turn the request that most clearly means *this must not work*
into the credential that works forever.

**Expired rows are swept hourly**, for table space only — `Resolve` refuses an
expired credential whether or not the sweeper has run. A row with no expiry has
`expires_at` NULL, and NULL fails the comparison rather than reading as zero, so
the sweep cannot reach the long-lived device credentials.

### Why tokens want a fast hash, and passwords do not

The asymmetry is entropy. A credential secret is 256 bits from `crypto/rand`:
there is no dictionary for it, brute force is 2^256, and slowing down a guess
that will never succeed accomplishes nothing. One SHA-256 per lookup, the index
still works, and a stolen database stops being a list of working credentials.

An account password is perhaps 30–40 bits, from a distribution attackers have
measured precisely. At 600k PBKDF2 rounds an offline attack runs at a few
thousand guesses per second per core instead of billions — a millionfold tax,
paid by the attacker on every guess and by Silo once per login. The blast radius
differs too: a leaked credential grants what that credential grants, while a
cracked password is plausibly the user's email password, which is the account
that can reset everything else they own.

## One verification path

```go
func Resolve(r *http.Request, kinds ...Kind) (*Credential, error)
```

It parses the credential, verifies the checksum, looks the row up by id, **joins
`Account` and checks `is_active`**, proves the secret, checks expiry, and stamps
`last_used`. It takes the request rather than a string because proving a
signature means seeing the method, the target and the headers.

**The join is the property that cannot be retrofitted onto separate stores.**
Disabling an account has to kill every lane at once, and it only does if every
lane asks. One `Resolve` makes that unskippable, and it costs nothing extra — the
account comes out of the same row read.

**An empty `kinds` set matches nothing.** A caller that named no kind forgot to
say which lane it was; reading that as "any lane will do" would turn the omission
into a wrong-lane credential being accepted on whichever lane nobody tested.

**The proof is checked before expiry and `is_active`**, so a caller who cannot
prove a credential does not learn that an id they guessed belongs to a disabled
account.

```
Authorization: Bearer silo_<kind>_…    every Silo client
Authorization: Silo   <credential-id>  proof of possession — designed, answers 501
```

Any other scheme is malformed, and nothing is looked up.

| Condition | Status |
|---|---|
| No `Authorization` header | `401 Authorization header required` |
| Malformed, unknown, wrong secret, wrong lane, expired, account disabled | `401 Invalid or expired token` |
| `Authorization: Silo` | `501 Signature authentication is not implemented` |
| The store itself failed | `500` |

The 401s are deliberately indistinguishable: telling a client whether its
credential was unknown, revoked, expired, disabled or simply for another lane
answers questions about rows it has not proved it holds. The distinction survives
in the log, where the operator is the audience. The 500 matters as much — a
database outage answering 401 would tell every client at once that it had been
signed out, and they would all re-enrol against a database that is down.

`Resolve` does **not** go through the login rate limiter. That limiter protects a
low-entropy secret; a 256-bit credential does not need it, and applying it would
let a wedged mount retrying a stale credential empty the shared account bucket in
seconds.

## Permission ceilings

```
effective = min(CheckPerm(library, cred.account_id),
                cred.perm  if cred.scope covers (library, path) else "")
```

`share.CheckPerm` answers what the *user* may do and knows nothing about
credentials, which is why the intersection lives beside the request rather than
inside it. It is applied in **one** function, `middleware.Perm(r, libraryID,
path)`, not as two calls at each site, because the failure it prevents is
precisely a handler that remembers `CheckPerm` and forgets the narrowing.

A credential cannot outlive or exceed the account behind it, and if the account's
own permission is withdrawn the credential follows immediately. That is what
makes a read-only, single-library credential safe to hand out.

**`Perm` fails closed on a nil credential** and logs it: every route is mounted
under `RequireCredential`, so no credential means a route registered outside the
authenticated subrouter — a mistake, not an anonymous caller. An unrecognised
`perm` string ranks as no access rather than read-write, because a typo should
cost a user their access and be reported, not silently grant more than either
side intended.

### Scope

```
""                      every library
"<library-id>"          one library, entirely
"<library-id>:<path>"   one library, at <path> and below
```

Empty rather than NULL, so there is one spelling of "unscoped". The separator
needs no escaping: the split takes the first colon, so a library id cannot
contain one by construction and a path may contain as many as it likes. Paths are
cleaned and rooted on the way in, so `..` collapses to something inside the
library rather than reaching out of one. Comparison is bytewise, with no case
folding and no Unicode normalisation — the server must not derive behaviour from
the shape of a name.

**A path scope is refused three surfaces, each deliberately:**

- **Library-wide operations pass `""`** — `changes`, `commits`, `notify-token`.
  There is no way to answer "what changed in this library" partially without
  telling the holder about paths it may not reach.
- **Chunks and objects are library-level**, from the other end: they are
  addressed by content hash, and an id says nothing about where it will be linked.
- **A batch is library-level.** It is ordered and all-or-nothing over a working
  tree, and a move names two paths, so "does this stay inside the scope?" is a
  question about the resulting tree rather than about each path in isolation.
  Refusing is the honest answer until that is worked out.

In an E2EE library a path scope compares ciphertext, exactly as the entries route
already routes on names the server cannot read. Two things move a path out from
under such a scope and both are quiet: renaming an ancestor leaves the segment
ciphertext untouched but spells a different path, and a directory merge
re-encrypts the loser's names so an unchanged path acquires new ciphertext.
Either way the scope matches nothing rather than failing loudly, so a credential
can outlive the thing it was cut to reach.

## Enrolment: password login

The password is an **enrolment** credential, not a request credential: presented
once, exchanged for something revocable, and forgotten. There is no device grant
on this path because there is no third party — the client collects the password
in its own window, so a code-and-approval dance would buy nothing at the cost of
an HTML page Silo has no other use for.

```
POST /api/silo/v1/auth/login          (no auth)
{ "email": "…", "password": "…",
  "kind": "device",                    // optional; default "session"
  "client_name": "SiloDrive 1.2 (macOS)", // required when asking for a credential
  "perm": "r",                         // optional, narrowing only
  "scope": "<library-id>" }            // optional, narrowing only
```

**Two response shapes, and the request chooses.** Any of `kind`, `client_name`,
`perm`, `scope` or `public_key` makes it an enrolment request; a client that
sends what it always sent gets what it always got, byte for byte.

```
200 { "token": "silo_session_…" }                                    plain login
201 { "credential": "silo_device_…", "expires_at": …, "email": "…" } enrolment
```

The old shape is not widened, because adding a field to a token response has
broken a client here before
([the bug](bugs/fixed/adding-a-number-to-a-token-response-breaks-clients.md)),
and two spellings of one secret in one body is one spelling too many.

**`perm` and `scope` get no "may they ask for this?" branch.** They are a ceiling
intersected on every request, so asking for `rw` on an account that has `r`
yields `r`, not a 403. Spellable is still checked, because an unrecognised perm
would mint a credential that authenticates perfectly and permits nothing.

**`client_name` is required for enrolment**, not defaulted from the `User-Agent`.
A client asking for a durable credential that will not say what it is leaves an
operator several indistinguishable rows.

| Request | Answer |
|---|---|
| `kind` outside `session`/`device` | `400` — the other kinds are not minted by a password |
| `perm` that is not `r` or `rw` | `400` |
| A scope that will not parse | `400`, describing the string sent and nothing else |
| Enrolment with no `client_name` | `400` |
| `public_key` | `501` — [proof of possession](#proof-of-possession) is designed, not built |
| A wrong password | `401` |

The request is validated **before** the password is checked, so a malformed
enrolment is a 400 whether or not the password was right: the shape of the answer
says nothing about the account, and the rate limiter goes on charging password
attempts rather than typos.

### Two protections on the login endpoint

**Rate limiting** charges only failures, in two dimensions: ten attempts per
address per 30s, ten per account per minute. Per address stops one host working
through many accounts; per account stops many hosts working on one. The
per-account bucket does let an attacker throttle a specific user by failing on
their behalf — the standard cost of counting per account, bounded at a minute's
wait rather than a lockout.

**A dummy hash** closes the enumeration oracle. Without it an address that exists
costs 600,000 PBKDF2 rounds and one that does not costs a database round trip,
which is a directory listing for anyone willing to time the endpoint; the limiter
does not help, because enumeration needs one attempt per address rather than ten.
So a miss verifies against a fixed hash of a password nobody has, derived lazily
at whatever work factor `HashPassword` currently uses — raising the iteration
count raises this too, so the gap cannot quietly reopen.

### Password hashing and bootstrap

`PBKDF2SHA256$600000$<salt>$<derived>`, at OWASP's current work factor.
`needsRehash` / `upgradeHash` replace any weaker stored hash on the next
successful login, which is the only moment the plaintext is in hand and known
good.

Login resolves the address through `AccountEmail`, case-folded, so every address
a user owns works. An unverified address is fine here, where the password is the
proof and the address is only a lookup key. It is **not** fine for linking an
external identity, where the address *is* the proof — see
[identity binding](#binding-an-external-identity-to-a-silo-account).

### Claiming a server that has no accounts

A server with an empty account table is a server nobody can use: there is no
signup endpoint and no user-management API. The server does not invent an
account or read one from the environment — the first would hand the operator
an identity they did not choose, the second would leave a password in a
compose file for the life of a deployment, to be read once.

What it does instead is a **setup token**: sixteen Crockford base32 symbols,
eighty bits from `crypto/rand`, printed at warning level on every boot until it
is claimed and reprinted on demand by `silo setup-token`. `POST auth/setup`
takes it with an address and a password of the operator's choosing and creates
the first account, with the `admin` role. It is refused with `409` the moment any account
exists, and `GET server-info` carries `setup_required` so a client can offer the
right screen rather than a login form that cannot work.

Three things about it are worth stating because they are exceptions to rules
this document makes elsewhere.

**It is a second secret outside `Credential`.** Rule 2 says every secret a
client presents is a row in one table resolved by one function, with the
notification token as the single documented exception. This is the second, and
the reason is structural rather than convenience: `Credential.account_id`
references `Account(id)`, so on a server with no accounts the rule's own table
cannot hold it. It authenticates nobody, names no account, and authorises
exactly one transition — after which its row is gone. It lives in `SetupToken`,
a single-row table whose `CHECK (id = 1)` makes "two live tokens"
unrepresentable, and it is consumed in the same transaction that inserts the
account, so the two can never come apart.

**It is stored in plaintext**, where every `Credential` stores only
`SHA-256(secret)`. It has to be: the server reprints it at every boot and
`silo setup-token` prints it on demand, and a hash does neither. The cost is
bounded to nothing — the row exists only while the server has no accounts, and
anyone who can read that table can already `INSERT INTO Account` by hand. That
same premise is why `silo setup-token` is a local command with no remote
version: an HTTP endpoint handing out the setup token would be unauthenticated
by construction, which is the same as having no token at all.

**Its rate limit is not the defence.** Five attempts a minute per address, which
bounds the cost of the endpoint and forgives a mistyped code. Eighty bits is
what makes guessing hopeless. Do not read the limiter as load-bearing and
shorten the token.

The account predicate is deliberately unfiltered — `SELECT 1 FROM Account`, with
no `is_active` test. Under an active-only test,
disabling your last account would put the server back into setup mode and mint a
fresh token, handing a way in to anyone who could read its logs. A disabled
account is still an account; the way back from locking yourself out is
`silo user enable`, which needs the same shell access the token does.

## Discarding a credential

A client that can mint one can discard it; signing out of a laptop is not a
support request for an operator holding `silo token revoke`.

```
POST /api/silo/v1/auth/logout             this credential
POST /api/silo/v1/auth/logout/everywhere  every credential the account holds

200 { "revoked": 1 }
```

Both answer the same body, and the count is worth returning: *"signed out of
four places"* is a sentence a client can show and cannot derive from a `204`.
`logout` answers `{"revoked": 0}` if something else revoked the row first,
which is the outcome the caller asked for either way.

**Neither route asks for write permission.** Revocation only ever takes access
away, so a `perm: "r"` credential may do it — refusing would mean the client
narrowed to the point of harmlessness is also the one that cannot slam the
door.

**The two routes differ in what the request is about, and the narrowing follows
that.** `logout` is about the row presenting it, which is not wider than that
row's own scope, so a credential cut to one library may sign itself out;
`middleware.RequireOwnCredential` is the lane that skips the scope check for
exactly this reason, and nothing else uses it. `logout/everywhere` answers about
every credential the account holds, which is strictly wider than any scope, so a
scoped credential gets `403` from the ordinary rule.

That is also why "everywhere" is a second route rather than a field in the body:
a flag inside the body would put the answer somewhere the middleware cannot see,
and the scope check would have to move into the handler, where the failure it
prevents is a handler that forgets it.

## Changing a password, and what it revokes

The obvious answer — "everything" — is wrong for one of the two cases:

- **A user changing their own password** revokes `session` credentials and
  leaves `device` credentials mounted. Unmounting somebody's laptop as a side
  effect of routine hygiene teaches them to stop doing hygiene.
- **An administrator resetting a password** revokes everything, and `silo user
  passwd` does. Reaching that command means shell access and an account that is
  not yours to log in to, and the reason an administrator resets a password is
  that the user has lost control of something — which something is not knowable
  from here. The revocation runs *after* the password is set: one that ran and
  then failed to change the password would sign every device out and leave the
  old password working.

```
POST /api/silo/v1/auth/password
{ "current_password": "…", "new_password": "…" }

200 { "revoked": 2 }    how many session credentials were signed out
```

**With `client_kdf_params`, the same request is the split-derivation
crossover.** `new_password` then carries an `authKey` rather than a password,
and the hash and the parameters are written in one statement — see
[Split-derivation login, on this side](#split-derivation-login-on-this-side).

**The current password is required even though the request is already
authenticated.** Otherwise a stolen device credential upgrades itself into
account takeover, and the point of a scoped, revocable credential is that it
cannot become the account. It is checked against the account the credential
resolved to; there is no address in the body, so nothing here can name somebody
else's.

**The session that asks is revoked with the rest.** The rule is about kinds, and
a carve-out for "this one" would mean a client could not read the count as what
it says.

| Request | Answer |
|---|---|
| Either field missing or empty | `400` — the rule `silo user passwd` already holds |
| A wrong current password | `401`, and it charges the login rate limiter |
| A `perm: "r"` credential | `403` — setting the account password is the most consequential write there is, and holding the password means a fresh login is available |
| A scoped credential | `403` — the password is the account's, which is wider than one library |
| `client_kdf_params` the server cannot parse | `400`, before anything is written |
| No `client_kdf_params` on an account that has crossed over | `409` — see below |

Permission is checked before the body is read, and the current password after
the shape: a caller who may not do this at all learns nothing about their body,
and a malformed request charges no password attempt. The revocation runs
**after** the new password is stored, for the reason the administrator path
gives.

Rate limiting is the login endpoint's, on the same buckets. A credential is not
a throttle: whoever holds one could otherwise guess the password here as fast as
the server will hash.

## Split-derivation login, on this side

Built here; the client half is not, and that is the whole of what is left.
The design is in [`storage.md`](storage.md) § One password, split client-side:
the client runs argon2id once and splits the result, sending `authKey` up and
keeping `wrapKey` on the device. What follows is only what this server does
about it.

**The stored hash names its own format, and that is the crossover flag.** A
crossed-over account's `AccountPassword.hash` begins `AUTHKEY-SHA256$`, and
`authmgr.IsAuthKeyHash` is the question anything asks. It is deliberately not
`client_kdf_params`: that column is written by `account.SetKeys` to record what
the identity blob was sealed under, so it is set on every account that has
published keys, crossed over or not. A flag that is part of the hash cannot
drift from the hash.

**One SHA-256, salted.** `authKey` is 256 bits out of HKDF over an argon2id the
client already ran, so the rule in [Why tokens want a fast hash](#why-tokens-want-a-fast-hash-and-passwords-do-not)
applies unchanged: there is no dictionary, brute force is 2^256, and the
memory-hard work has been done where it protects something guessable. The salt
buys little and costs nothing; unlike a credential's hash this column is never
looked up by value, so there is no index to keep.

**The rehash path must skip it.** `needsRehash` rewrites anything that is not
PBKDF2 at the current work factor, and an authKey hash is not. Left alone, the
first successful login after a crossover would have rewritten it as 600k rounds
over the `authKey` — no stronger, and the next login would present the
`authKey` against a hash of an `authKey` run through PBKDF2 and fail. A
crossover undone by using it.

**The write is one statement.** `account.SetPasswordAndKDFParams` puts the hash
and the parameters down together, because they are one fact: the hash is of an
`authKey`, and the parameters are how a password becomes that `authKey`. A
state where one landed and the other did not is an account nobody can log in
to, and two statements have a window a crash can land in.

**A change with no parameters, on an account that has crossed over, is
`409`.** Taking it would store a hash of a raw password and clear the column —
putting the account back on password login by omission, after which every
device that had been sending an `authKey` starts failing and the server looks
wrong rather than the request that did it.

**An operator reset does put it back on password login, deliberately.**
`silo user passwd` and every other raw-password write clears
`client_kdf_params`, because parameters left beside a hash of a password
describe a stretching that no longer leads to it. What the reset does *not*
touch is `AccountIdentityKey` and `AccountRecoveryWrap`. An operator cannot
re-wrap the identity key — re-wrapping means unwrapping first, which needs the
old password, which is precisely what whoever is resetting does not have — so
the blob is left in place and unopenable, and the command says so. It says one
of two things: that the user can redeem a recovery wrap and republish, or, if
they published none, that there is no way back to that identity key. Which of
those it is, is not knowable from the prompt, so it is read from the account.

**The client half is built too.** `client.Enrol` mints an identity key,
publishes it wrapped under the `wrapKey` its password produces, and crosses the
account over in the same call; `client.OpenAccount` derives under the
parameters `POST auth/kdf` serves and logs in with the `authKey`. An account
enrolled by this client never sends its password to the server again.

`client.ChangePassword` carries the identity key across. A new password is a
new `wrapKey`, so the blob is re-wrapped under it and republished — with the
recovery wraps passed through unchanged, since those seal the identity under a
code rather than under the password — and only then does the login secret move.
Neither order is atomic; that one leaves the account reachable with the
password the caller still has, and a retry of the same call closes the window,
because the identity is opened under either password.

The derived key is tried first and the password second, because this endpoint
will not say which kind an address is. That ordering is what makes the fallback
safe: an account that has crossed over never sends its password, and one that
has not was always going to. It costs one refused login for an account that has
not crossed over, which spends a rate-limit token; the answer is to cross
accounts over rather than to reverse the order.

## Who the caller is — `GET /account`

`{"account_id", "email"}`, behind any credential. Nothing here is a secret; it
is what the credential already proves.

It exists because of a bootstrap that could not start. `store.WrapIdentity`
binds the account id as associated data, so a client cannot wrap an identity
key until it knows that id — and the id appeared nowhere but `GET
account/keys`, which answers `404` until an identity key exists. An account
that had never enrolled had no way to learn the one string it needed in order
to enrol. Separate from `account/keys` rather than folded into it because they
are two questions, and answering "who am I" only alongside "what do I hold" is
what produced the gap.

## The account's key material

End-to-end encryption needs four things per account that the server stores and
cannot read: a published X25519 public key, the private half wrapped under a
password-derived key, that same private half wrapped once per recovery code,
and the argon2id parameters the password is stretched under to get there. The
format for all four is in [`spec/store-format.md`](spec/store-format.md) and
implemented in `store/`; what follows is where they live and who may touch
them.

```
GET    /api/silo/v1/account/keys                  what this account has published
PUT    /api/silo/v1/account/keys                  publish, replacing everything
DELETE /api/silo/v1/account/keys/recovery/{n}     redeem one recovery wrap
POST   /api/silo/v1/auth/kdf                      the pre-login parameters, unauthenticated
```

**`PUT` replaces the whole set rather than merging into it.** A password change
produces a new `wrapKey`, so every blob the account holds is re-wrapped at
once. A stale recovery wrap left standing beside the new set still opens with a
code the user was told to throw away; a stale identity blob still opens with
the old password. One transaction, or none of it.

**The parameters are stored twice, and the write checks they agree.**
`AccountPassword.client_kdf_params` carries them so the pre-login endpoint can
answer without handing out the blob, and `store.WrapIdentity` seals them inside
the blob because every stretched secret in this system is self-describing.
`store.WrapIdentity`'s own comment names the hazard in storing them once: "a
pair that can drift, after which the blob is unopenable and nothing says why".
So `account.Keys.Validate` refuses a publish whose column and blob disagree.

**An account with no password is refused outright.** The parameters describe
how a password becomes the `wrapKey` that opens the identity blob, so an
account with no password has nothing for them to describe. Writing the identity
key anyway would be a publish that succeeds and a new device that can never
bootstrap — the failure surfaces months later, on the one day it cannot be
worked around. When OIDC-only accounts want E2EE they will need a different
wrapping secret, and that is a design, not a column.

**Redemption deletes one blob and the rest of the set stands.** Regenerating
the set on redemption is the tidier-looking rule and the worse one: it
invalidates the codes a person is still holding at the moment they have proved
they lost their device. The server never learns a code — it stores an
`ordinal`, never the code — so redemption is the client fetching the set,
trying each blob, and telling the server which one is spent.

**The client mints the set at enrolment, and only there.** `client.Enrol`
wraps the identity private half once per code and returns the codes to its
caller, in the same call that generates the key. It is the only moment they
can be produced: minting a set means holding the private half, and after
enrolment returns nothing does. So a set arrives with the identity key or the
account never has one, which is the shape that avoids a population of accounts
holding no codes and a migration to give them some.

**The wraps go up in the same `PUT` as the identity blob**, for the reason the
whole-set rule above gives. A second publish that added them would be a second
chance to fail with the identity key already stored — an account whose key is
recorded and whose way back to it is not.

**`client.Recover` spends the code in that same `PUT` rather than through
`DELETE`.** Publishing without a wrap removes it, so re-wrapping the identity
under the new password and retiring the spent code are one statement. The
order matters the way it does everywhere else here: deleting first would spend
a code and then, on a failed publish, leave the user holding one fewer and no
further forward. `DELETE account/keys/recovery/{n}` remains the endpoint for
retiring a code on its own, which is not what redemption is.

**A code recovers the identity key. It does not recover login, and it cannot.**
Redeeming takes an authenticated request, so an account nobody can log in to is
one no code can reach. Login comes back through the operator, who can set a
password and cannot open a blob; the code is the half the operator does not
have. `client.Recover` takes both for that reason, and what it leaves behind is
an ordinary enrolled account — identity re-wrapped under fresh parameters, the
account crossed back over to derived login, and a device holding only the new
password working afterwards with no code at all.

| Request | Answer |
|---|---|
| A publish whose blob and `kdf_params` disagree | `400`, saying which is which — every case here is a client bug whose symptom otherwise appears months later |
| A `public_key` that is not an X25519 key, a blob over 4 KiB, a duplicate or out-of-range ordinal | `400` |
| `GET` before anything is published | `404` |
| Redeeming an ordinal that is already gone | `404` |
| A `perm: "r"` credential on `PUT` or `DELETE` | `403` — replacing the identity key is the one write that can make an account's own libraries unreadable. `GET` is permitted |
| A scoped credential | `403` — the key material is the account's, which is wider than one library |

### The pre-login parameters endpoint

`authKey` is a function of parameters only the server knows, so a client has to
ask before it can log in. That makes this the one endpoint that answers with
nothing authenticated, and it is sharper than it looks.

**It is an account-enumeration oracle by default.** An address nobody holds
must receive plausible parameters rather than a `404`, and the same ones every
time, or the difference between two requests answers the question. They are
derived from `HMAC(server secret, normalized address)`, exactly as the dummy
password hash closes the same gap on login, and at the default cost — because
the default is what a real account will almost always carry, and a fake at any
other cost would stand out.

**The secret is stored, not generated at boot.** A `ServerSecret` row, minted on
first need. A value regenerated per process would make one address answer
differently after a restart, which says "no account here" exactly as loudly as
a `404` would. An account that exists but has published nothing is answered the
same way, and is the case a naive handler gets wrong: the row is there, so it
is tempting to answer "no parameters".

**The answer is attacker-influenced input to the client's KDF.** A server
answering `m=4 GiB` does not weaken anything; it takes the device down. The
client validates against `store.KDFParams.Validate` before deriving — it
already does — and this is the request that makes that guard load-bearing.

**`POST`, for a request that reads.** The address is the one identifier this
endpoint takes and the request is unauthenticated, so a query string would put
every address anyone asked about into the access log, the proxy log, and any
`Referer` a browser sent onward.

Rate limited per address only, at sixty a minute, and every request spends a
token rather than only the failures — there is no failure here. A per-account
bucket would break the contract: an address that can be throttled is an address
that has an account. What makes a sweep useless is the indistinguishable
answer; the bucket bounds what the sweep costs this server.

**`client.OpenAccount` is the consumer.** It asks here before logging in,
derives both halves under what comes back, and sends the `authKey`. The
parameters an unknown address is answered with are a fake, so the credentials
derived from them mean nothing — which is why the identity is opened under the
parameters the blob itself carries whenever those differ from these.

## Operating it

```
silo user list                       every account
silo user add <email>                create one     (-role, -generate)
silo user passwd <email>             set a password (-generate); revokes everything
silo user disable <email>            stop every credential it holds
silo user enable <email>             undo a disable
silo token list <email>              id, kind, label, created, expires, last used, narrowing
silo token revoke <email> <id>       stop one credential
silo token revoke <email>            stop all of them
```

**Disabling does not delete.** Re-enabling restores the user's devices rather
than making everyone log in again, and the `is_active` join means the stop takes
effect on the next request either way.

**Passwords are never taken as a flag.** A flag is readable through `/proc` by
every other process on the host and kept in shell history afterwards. A terminal
is prompted twice with echo off, a pipe is read from stdin, and `-generate`
invents one and prints it once.

**`silo token list` prints the narrowing whenever there is one**, including a
read-only credential with no scope: a harmless row that looked identical to a
full one would undo the reason the listing exists.

**There is no cache anywhere in this path.** `Resolve` reads the row on every
request, so revocation takes effect on the next one — no TTL to wait out. If a
cache is ever added for load reasons it needs a per-account generation counter
that revocation, password change and deactivation all bump; until then, adding
one trades the best property this design has for a saving nobody has measured a
need for.

## The one JWT

Statelessness is load-bearing in exactly one place: the notification token, which
crosses to a process with no access to Silo's database.

`POST /api/silo/v1/libraries/{id}/notify-token` mints one for a caller who can
read the library, scoped to that library, HS256, `aud=silo:notif`, 72 hours. The
audience is required by the validator, so a token of one kind cannot be replayed
as another even though both are signed with the same key, and
`jwt.WithValidMethods` means a token cannot select its own algorithm.

`SILO_JWT_SECRET` is generated randomly at startup when unset. Sessions are rows
and survive a restart, so the only thing an ephemeral key invalidates is the
notification tokens in flight, and a client re-mints one per subscription — a
reconnect rather than a re-login. It should still become a keyfile; see
[what is left](#what-is-left-in-order).

The notification socket uses `middleware.OptionalCredential`: a credential that
is offered and bad is still refused, because a rejected credential must never be
quietly downgraded to anonymous. The caller believes it is authenticated and
would learn otherwise only from the permissions it silently stopped having.

## What is not authenticated

`GET /api/silo/v1/server-info`, `POST /api/silo/v1/auth/login`,
`POST /api/silo/v1/auth/kdf`, and `POST /api/silo/v1/auth/setup` — the last
because it is the request that creates the first account, so there is nothing
yet to authenticate it against; the setup token is what stands in its place.
Everything else under `/api/silo/v1` is mounted on a subrouter carrying
`middleware.RequireCredential`, and everything outside `/api/silo/v1/*` and
`/notification` is a 404.

---

# Part 2 — designed, not built

## Proof of possession

A bearer credential authenticates whoever holds it, and every way one leaks is a
variation on someone else coming to hold it: a database snapshot, a log line, a
laptop backup, a token pasted into a bug report. Hashing the stored side fixes
half of that. The other half is that the client still has to keep a usable copy.

It does not have to. Enrolment can register a **public key** instead — generated
on the device, non-exportable where the platform allows (Secure Enclave, TPM, a
0600 file for the TUI), sent up with the login. The row stores `public_key`, and
no secret exists to steal.

```
Authorization: Silo <credential-id>
Signature-Input: sig=("@method" "@target-uri" "date");created=1755950400
                     ;nonce="AAECAwQFBgcICQoLDA0ODw"
Signature: sig=:<base64>:
```

Nothing else in this document moves: kinds, ceilings, `is_active` and revocation
are all unchanged.

**It is not a JWT and not a token.** A JWT is a bearer assertion — a signed
statement you carry, which anyone else holding it can also present. Here nothing
is carried: the credential id is a public name, the private key never leaves the
device, and what is signed is *this request*, so a captured signature is a
receipt for a request that already happened. The close relatives are SSH
`publickey`, mTLS and AWS SigV4; SigV4 is the right mental model, and RFC 9421
HTTP Message Signatures is the standardised version of what it does by hand.

The server answers `501` to `Authorization: Silo` today, and `/auth/login`
answers `501` to a `public_key`. Both are deliberate: a half-verifier that checked
the id and not the signature would be a bearer scheme wearing a signature's name,
and minting a public-key row before the verifier exists would hand a client a
credential that can never resolve.

### The wire format, pinned

RFC 9421 is a framework, not a format: algorithm, signature encoding, covered
components and parameters are all left to the profile. Every one of those is a
place where a signer and a verifier are each correct and do not interoperate, so
each is pinned here rather than chosen twice.

- **Algorithm: `ecdsa-p256-sha256`.** One algorithm, not a negotiation. P-256
  beats Ed25519 here on key stores: the Secure Enclave and most TPMs hold P-256
  and will not hold Ed25519, and a non-exportable key is the point of this lane.
- **Signature encoding: raw `r ‖ s`, 64 bytes, not DER.** Go's `ecdsa.SignASN1`
  and CryptoKit's `derRepresentation` both default to DER, and a DER signature
  verified as raw fails while a raw one parsed as DER fails differently.
- **The stored SPKI is authoritative for the key *and* the curve.** Nothing the
  request says about which key or algorithm to use is honored. A signature the
  request gets to describe is a signature the request gets to weaken.
- **The wrong key type is rejected at enrolment, not at verification.** Checking
  later would leave rows that can never authenticate anything, failing with an
  error that looks like a signing bug on a device that is in fact holding a key
  the server should have refused — and the person debugging it is the one who
  cannot see the table.
- **The nonce is a signature parameter, not a covered component.** `;nonce="…"`
  per §2.3, not `"nonce"` in the component list, which would require inventing a
  `Nonce:` header. Parameters are covered anyway through `@signature-params`.
  Sixteen random bytes, base64.
- **`created` is required, and the skew window measures against it.** More than
  **60 seconds** out in either direction is rejected before the key is loaded, so
  the nonce cache spans exactly that window and stays bounded with no cleanup
  logic beyond expiry.
- **`created` and `nonce` are the only parameters honored.** `expires` would let
  a signer widen its own replay window; `alg` is the algorithm-confusion door.
- **`keyid` is not used**, and a request carrying one is rejected on mismatch
  rather than ignored: two fields naming the key is two fields that can disagree,
  and a verifier that silently prefers one is the confused-deputy bug somebody
  builds on later.

When the lane is built the signing contract gets a file under [`spec/`](spec/)
with cross-implementation vectors, because a Swift port has to produce
byte-identical signature bases and prose has never once been enough for that.
This section is the decision record; the spec will be the contract.

### The body is mostly not in the signature

Signing a digest of the body would mean buffering a five-gigabyte upload before
deciding whether the request is authentic. That is unacceptable, and it turns out
to be unnecessary: almost every write is content-addressed and carries the
content's own hash in the request line. A chunk id *is* the hash of the bytes and
the server verifies what arrives against it, so signing the request line binds
the payload already — and replaying such a request stores bytes that are already
stored under an id they already have.

What is left is the small set where replay means something, and both are already
protected. The head advance is a compare-and-swap, so a replayed advance fails
its precondition; the small mutations — mkdir, rename, move, delete, writes to
`entries` — accept `If-Match` / `If-None-Match`, and a conditional write is
replay-proof by construction.

So: **sign the request line, the date and a nonce always; add a content digest
only where the body is small and is not already named by its hash.**

Nonce and digest are two different jobs, and conflating them produces something
that does neither. Replay protection needs *uniqueness*, and a content hash is
not unique — uploading the same chunk twice is legitimate and common. Content
binding needs the digest, which need not be unique at all.

P-256 verification costs around a hundred microseconds, roughly twice Ed25519's
fifty. A directory walk issuing a few hundred requests pays single-digit
milliseconds in total, comfortably under the storage reads it is making anyway.
The only things that cannot play are an S3 frontend, whose SigV4 needs a shared
secret the server can recompute with, and a capability URL, because a URL cannot
sign anything.

## Argon2id, and the memory it costs the server

Every password can be reset from scratch, so the better primitive is available.
Argon2id is memory-hard; PBKDF2 is not, which is why a GPU eats it.
`validatePasswd` already dispatches on a stored prefix, so `argon2id$…` slots in
beside the existing formats, and `needsRehash` upgrades anything weaker on the
next successful login.

The trap is that memory-hardness is paid by *the server*, and unlike CPU it is
not self-limiting. PBKDF2 under load queues on the scheduler and the box stays
up; Argon2id at 64 MiB with twenty verifications in flight is 1.25 GiB resident,
and the login limiter bounds attempts per address and per account, not
concurrency — a hundred addresses arrive together quite happily.

So password verification runs behind a semaphore, four wide or thereabouts,
returning `429` with `Retry-After` when full rather than allocating. The width
and the memory parameter multiply, so they are chosen together and measured on
the target hardware rather than copied from a blog post; 64 MiB, t=3, p=4 is a
place to start measuring. This is affordable precisely because the password is an
enrolment credential — verification drops from every refresh on every device to
once per device, ever.

## Enrolling into an account that already exists

Sharing a library with an address nobody has enrolled under has to mint something
for `SharedLibrary.to_account_id` to point at: an inactive `Account` with the
address claimed and no `AccountPassword`, which cannot be signed in to. Enrolment
then finds the address taken and claims that account rather than colliding with
it, which reunites the shares with the person they were meant for.

**That is an email-reuse attack surface and it must be gated.** A corporate or
university address that lapses and is reassigned hands the new holder every share
the departed holder was given. So claiming an existing inactive account must
require verifying the address — a link to it, not merely typing it — and never
happen on the strength of a password chosen at a signup form.

Verification bounds the damage but does not remove it: the new holder does
control the mailbox. So decide, before this ships, whether shares older than some
threshold are surfaced to whoever granted them rather than silently reattached.
That is a product decision rather than a security control, and it is the one that
turns a quiet takeover into a visible event.

## OIDC

### Silo brokers login; it does not federate every request

The tempting design is to treat the IdP's access token as Silo's credential and
validate it per request. It is wrong for this server, whichever IdP is used.

A mounted filesystem issues requests at the rate of `ls`, not at the rate of page
loads: `stat` on a directory of two hundred entries is two hundred chances to
consult the IdP. Whatever the validation mechanism, the IdP is then in the hot
path of a filesystem, and when it is down the filesystem is down.

The access token is also the wrong shape. Its lifetime is the IdP's to choose, so
the client must hold a refresh token and re-mint constantly — a long-lived secret
on the device anyway, just one Silo cannot see, label, scope or revoke. And it
carries the IdP's audience and scopes, not Silo's: nothing in it says "read-only,
library X, this laptop".

So Silo brokers. The IdP authenticates the human once; Silo verifies that result,
mints its own `Credential` row, and every subsequent request is a local lookup.
The authentication decision is federated; the authorization state is Silo's.
That is what makes the rest of this document survive the change — OIDC adds one
enrolment path, nothing about `Resolve` moves, and an OIDC deployment differs
from a password deployment only in how a credential is obtained.

### Silo is the client, and the device in the device flow

A desktop client is compiled, signed and shipped to whoever installs it, and
every deployment it meets has a different identity provider — so there is no
`client_id` it can be built with. RFC 7591 dynamic registration exists for
exactly this and is commonly disabled, and a client's only login path cannot
depend on a feature that may not be present. **Silo holds the registration and
the client never speaks to the IdP.**

RFC 8628 is usually described as the flow for televisions. The requirement it
encodes is narrower: *the client cannot host the user agent*. Silo qualifies — it
serves JSON and bytes, has no HTML anywhere, and adding templates and cookie
sessions would introduce a class of vulnerability it currently cannot have.

```
1. Client → Silo   POST /api/silo/v1/device/code
                   { client_name: "SiloDrive 1.2 (macOS)", perm: "r" }

2. Silo   → IdP    POST /oauth/device_authorization  (client_secret_post + PKCE)
   IdP    → Silo   user_code, verification_uri, device_code, interval, expires_in

3. Silo   → Client { user_code, verification_uri, verification_uri_complete,
                     poll_token, interval, expires_in }

4. The client displays the code, or opens verification_uri_complete locally.
5. The user approves at the IdP's device page.
6. Silo polls the IdP's token endpoint, honouring interval and slow_down.
7. Silo verifies the id_token against JWKS, resolves the account, mints a
   Credential, and discards the IdP's tokens.
8. The client's poll returns the credential once, and it stores it.
```

**Silo's browser surface is nothing at all** — no form, no callback, no
`redirect_uri`, no `state`, no HTML. The browser never touches Silo. This is the
flow `gh auth login` and `az login --use-device-code` use, for the same reason.

Two codes exist and only one is human-visible: Silo mints an opaque `poll_token`
for the client and relays the IdP's `user_code` and URL for the person, so the
client never holds an IdP secret and never sees the `device_code`.

Three properties fall out of Silo being the client. The ID token's `aud` is
Silo's own `client_id`, so there is nothing to validate out-of-audience and no
reason to introspect. `sub` is Silo's pairwise subject, so `AccountIdentity`
stores an identifier that belongs to Silo. And the approval step is the IdP's
device page, which requires the code to be typed and approved **on every
enrolment** — unlike a consent screen, which is remembered and would wave the
second device through unseen.

Silo asks for `openid email profile` and **not** `offline_access`: after step 7
it never acts on the user's behalf again, so a refresh token would be one more
long-lived secret to store and lose.

The one dependency is an IdP implementing RFC 8628 — Clinch, Keycloak,
Authentik, Okta, Entra and Auth0 all do. For one that does not, the fallback is
an authorization-code flow with a Silo-hosted approval page, perhaps 150 lines,
and it should not be built until an IdP is met that needs it.

### Which token is verified, and how

Exactly one, exactly once: the **ID token**, at step 7. Verification is offline
against cached JWKS, about a millisecond, once per enrolment. Checked: signature,
`iss`, `aud` equals Silo's `client_id`, `exp`, `iat`, and `at_hash` when an
access token accompanies it.

Deliberately not used:

- **Introspection (RFC 7662).** It is the right mechanism when a resource server
  must validate opaque tokens it did not mint; Silo mints its own. Per request it
  would put a network call in the filesystem path, add a hard runtime dependency
  on the IdP, and bound revocation latency below by the cache TTL — *worse*
  revocation than deleting a row. Reaching for a cache to make a mechanism fast
  enough is a sign the mechanism is wrong.
- **Access-token JWTs.** Trades revocability for statelessness, the opposite of
  what a filesystem server wants, and not universal.
- **`userinfo` per request.** Same objection, and it is specified as an identity
  endpoint rather than an authorization one. It is useful on a slow schedule, out
  of the request path, to notice an address or group membership changed.

### Configuration

```
SILO_OIDC_ISSUER=https://id.example.com
SILO_OIDC_CLIENT_ID=silo
SILO_OIDC_CLIENT_SECRET=...
SILO_OIDC_TRUSTED=1     # this issuer's verified addresses may claim an account
```

Silo is a confidential client and authenticates with `client_secret_post`, with
PKCE (S256) on every flow: it costs a SHA-256 and closes code interception
independently of secret confidentiality. Every endpoint comes from
`/.well-known/openid-configuration` — hardcoding those URLs is how integrations
break on an IdP upgrade.

`server-info` should advertise whether OIDC is configured, so a client shows the
right first screen without being told; when it is not, the client collects an
address and password itself and posts them to `/auth/login`. A client otherwise
neither knows nor cares how the human was authenticated, which is the point of
brokering.

A deployment wanting the IdP to be the only path sets `SILO_PASSWORD_LOGIN=off`.
The failure mode to answer before that switch exists is the IdP being down with
nobody able to administer the server; resolve it with `silo user passwd` on the
host rather than a standing exception for admin accounts. Anyone who can run the
CLI already owns the data directory, so it grants nothing they did not have.

Dependencies: `github.com/coreos/go-oidc/v3` and `golang.org/x/oauth2`, neither
in `go.mod`. They cover discovery, JWKS fetching and rotation, and ID token
verification — three things that are subtly wrong when hand-rolled.

### Binding an external identity to a Silo account

`AccountIdentity(issuer, subject)` is the join. On a verified ID token:

1. Look up `(iss, sub)`. Found → that account. Done.
2. Not found, `email_verified` is true, and the address is in `AccountEmail` →
   link `(iss, sub)` to that account. **Only when the issuer is trusted**
   (`SILO_OIDC_TRUSTED`): against an IdP you do not control this is account
   takeover by whoever can set an email claim. Against your own, it is the
   difference between a working system and a support ticket.
3. Otherwise → provision `Account`, `AccountEmail`, `AccountIdentity`.

Never key on an address alone, and never treat an unverified address as
identifying.

**Why step 2 exists.** OIDC requires `sub` to be stable for the relying party,
but a good many IdPs derive it from a row deleted when the user revokes the
application's access — so re-authorizing mints a fresh `sub`, Silo sees a
stranger, provisions a second account, and orphans every library the first one
owned. A verified address recovers such a user instead of stranding them.

Test this against the actual IdP before deployment: the failure is silent and the
recovery — merging two accounts — is not something this schema supports. Clinch
has the behaviour today, where `sub` is `OidcUserConsent#sid` and "Revoke Access"
destroys the consent; "Logout" preserves it and is safe. The durable fix belongs
on the Clinch side, since a pairwise subject is specified to be stable and this
affects every relying party.

### Logout and revocation

Backchannel logout is a push channel: the IdP POSTs a signed logout token to
`backchannel_logout_uri` when a session ends or access is revoked. It is strictly
better than any poll, and it is the one endpoint here the IdP calls directly —
JSON in, 200 out, still no HTML.

Verify it like any other assertion: RS256 against JWKS, `iss` matches, `aud` is
Silo's `client_id`, `events` contains the backchannel-logout event, `jti` not
replayed, and **no** `nonce` — its presence means someone handed you an ID token.

What it *means* is left open by the spec. A logout token should revoke `session`
credentials; whether it revokes `device` credentials is a judgement, since
signing out of a web session on a phone should probably not unmount a laptop
while revoking the application's access probably should. Most IdPs send the same
token shape for both, so the distinction has to be drawn on the Silo side. The
defensible default is to revoke `session` on any logout token and leave `device`
to explicit revocation, where the labels make it clear what is being killed.

Independently of all of this, `is_active` is read on every `Resolve`. That is the
local kill switch, and it works when the IdP is unreachable — exactly when it is
most likely to be needed.

## S3 needs a master key, not a column

SigV4 is an HMAC the server must recompute, so an S3 frontend needs the secret in
usable form. There is no asymmetric variant of S3 request signing and presigned
URLs need the same secret, so hashing is not available. Derive rather than store:

```
access_key_id = "SILO" + base32(random(10))     // public; the Credential id
secret_key    = HKDF-SHA256(master_key, info = "silo-s3-v1|" + access_key_id)
```

The row holds no secret material at all: verification re-derives, and revocation
deletes the row so the derivation is never reached. The trade against encrypting
a stored secret is that rotating `master_key` invalidates every S3 credential at
once — correct for a single-node server where reissuing is cheap.

This forces a persistent master key (`SILO_MASTER_KEY`, or a 0600 keyfile created
on first run) and a decision about a missing one: refuse to start if S3
credentials exist, rather than silently breaking every one of them. The master
key then becomes the most sensitive value in the deployment, which is inherent to
serving S3 at all; the mitigation is keeping it out of the database and out of
the backup set.

## E2EE libraries have nowhere to put the second secret

Three different things authenticate something: the **account password**
(`AccountPassword`, grants the account), the **library content key** (client-side,
wrapped to member identity keys, never on the server in usable form — see
[`encryption.md`](encryption.md)), and the **device credential**.

A WebDAV or S3 mount of an E2EE library needs the second as well as the third,
and those protocols have nowhere to carry it. The server-side answer — hold a key
for the client — is exactly what the E2EE guardrails forbid. So frontends that
cannot run the client crypto must refuse E2EE libraries with an explicit error,
and the listing's `encrypted` flag is how they know. The failure mode if nobody
decides is a mount silently serving ciphertext. A paragraph of policy rather than
a project, but write it before the first frontend ships.

## Deliberately not built

**A no-authentication mode.** `SILO_AUTH=none`, resolving every request to a
synthetic credential, was argued for on ergonomics. Two things make it
unnecessary: the bootstrap credential puts a fresh server one warning line and
one `POST /auth/login` from usable, and a long-lived device credential in an
environment variable buys the same absence of a login round trip through the real
code path — revocable, nameable and narrowable, which the mode is not.

What is left is a mode that is safe in the case it was for and catastrophic in
every other, where the guards are the bulk of the work: environment or flag only,
never a value the database or a user-writable file can supply; refuse to start
off loopback without a second, differently-named acknowledgement; a startup
banner, a line on every request log, a field in `server-info`; never in a release
container's default configuration; and a rule for *which* account is everybody.
Deferred rather than rejected — reconsider when a concrete consumer asks.

## What is left, in order

1. **Proof of possession** — public keys registered at enrolment, RFC 9421
   signatures on the Silo lane. Independent of OIDC; whichever is wanted first.
2. **argon2id behind a concurrency semaphore.** Moot for a crossed-over
   account — `AccountPassword.hash` is a fast hash there, by the rule above —
   and still wanted for the accounts that have not crossed: enrolment, link
   redemption, and any account that has not yet been enrolled.
3. **A persistent JWT signing key**, so a restart does not disconnect every
   watching client. Nothing but notification tokens depends on it. The
   `ServerSecret` table holds exactly this shape of value — a secret that is
   the server's own and must outlive the process. A 0600 keyfile is the answer
   if the key has to be readable by an operator or shared across processes; if
   it does not, a row is one fewer file to get the permissions wrong on.
4. **Setup credential** — built; see
   [claiming a server](#claiming-a-server-that-has-no-accounts) in Part 1.
5. **OIDC** — `/device/code`, the device grant against the IdP, ID-token
   verification, `AccountIdentity` binding with verified-address recovery, and
   backchannel logout. Adds no browser surface, and produces exactly the
   `Credential` row Part 1 describes.
6. **Master key and S3 derivation.** Only gates S3; defer until S3 is wanted.

Outside that list: `role` can be set when an account is created but not
afterwards, so an install that wants a second administrator creates one with
`silo user -role admin add`.

*Capabilities* can be changed after the fact — `silo user grant` and `silo user
revoke`, over the closed set in [`plans/admin.md`](plans/admin.md). Two things
are refused there, and both are about the install rather than about tidiness:
nobody hands on or takes away an authority they do not hold themselves, and the
last holder of `grant` may not drop it. `setup.Claim` writes the role and all
six capabilities in one transaction, because a first boot that produced a
server nobody can administer is not a recoverable state.

Being an admin grants nothing on its own, and neither do the rows: `admin.Can`
is a conjunction and is the only place that rule is written. That is what makes
a demotion reversible without anybody having to remember a set — the rows
survive it and mean nothing while it stands.
