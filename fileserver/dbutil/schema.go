package dbutil

// The Silo database schema: the shape a fresh database is created in.
//
// A change here is half of a change. The other half is the migration in
// migrate.go that takes an existing database to the same place, and the
// baseline test is what checks the two halves agree.
//
// Users, groups and repositories used to live in two separate SQLite files
// (one for accounts, one for libraries), inherited from two server processes.
// Silo is one process, so they are one database: no table name collides
// between the two halves, and a share permission check that has to read a
// group and a repository can now do it in a single statement.
const siloSchema = `
-- Accounts. An account is an opaque id, and an address is something it has.
--
-- Five tables and three jobs: what you are called (AccountEmail,
-- AccountIdentity), what proves it per request (Credential), and what
-- bootstraps the rest (AccountPassword). They stay apart because those jobs
-- have different rules. An address and an OIDC subject carry no proof material
-- at all -- the mailbox and the IdP do the proving -- so a row in the
-- credential table that can verify nothing would be a row in the wrong table.
-- A password is one per account, enforced by a primary key rather than by a
-- partial index somebody has to remember, and it is verified under a slow
-- memory-hard KDF while a 256-bit credential is verified under one SHA-256:
-- see docs/auth.md, "Why tokens want a fast hash, and passwords do not". One
-- secret_hash column invites one verification path, and it would be the wrong
-- one for whichever of the two lost the argument.
--
-- id is a UUIDv7 stored as its 16 raw bytes. v7 leads with a millisecond
-- timestamp, so accounts created together land together in the index instead
-- of scattering across it the way v4 would.
--
-- display is what a human is called, and is deliberately not an identifier.
CREATE TABLE IF NOT EXISTS Account (
  id         BLOB    PRIMARY KEY,
  display    TEXT,
  is_active  INTEGER NOT NULL DEFAULT 1,
  role       TEXT    NOT NULL DEFAULT 'user',
  ctime      INTEGER NOT NULL
);

-- An address belongs to exactly one account, which is what the primary key
-- says. Addresses are stored lowercased -- account.Normalize is the only
-- spelling rule, applied at the one door an address comes through.
--
-- The partial unique index is the one that matters. Without it an account
-- accumulates two primary addresses and every query that asks "what is this
-- account called" starts depending on row order.
CREATE TABLE IF NOT EXISTS AccountEmail (
  email       TEXT    PRIMARY KEY,
  account_id  BLOB    NOT NULL REFERENCES Account(id),
  is_primary  INTEGER NOT NULL DEFAULT 0,
  verified_at INTEGER
);
CREATE INDEX IF NOT EXISTS account_email_account_idx ON AccountEmail (account_id);
CREATE UNIQUE INDEX IF NOT EXISTS account_email_primary_idx ON AccountEmail (account_id) WHERE is_primary = 1;

-- An identity asserted by something outside Silo: an LDAP directory, an OIDC
-- provider. The pair is the key because a subject is only meaningful next to
-- the issuer that minted it.
CREATE TABLE IF NOT EXISTS AccountIdentity (
  issuer     TEXT    NOT NULL,
  subject    TEXT    NOT NULL,
  account_id BLOB    NOT NULL REFERENCES Account(id),
  ctime      INTEGER NOT NULL,
  PRIMARY KEY (issuer, subject)
);
CREATE INDEX IF NOT EXISTS account_identity_account_idx ON AccountIdentity (account_id);

-- Separate from Account because not every account has a password. An
-- OIDC-only account has none, and a nullable column is how "no password"
-- turns into "any password will do".
--
-- These are not the argon2 parameters in store/kdf.go, and the two get
-- confused because both stretch "the password" with argon2id. The client
-- stretches the password into authKey under the parameters store.KDFParams
-- carries; the server stretches the authKey it receives into this column under
-- its own. Two KDFs, two parameter sets, one living in each schema -- neither
-- is vestigial and raising one does not raise the other.
--
-- There is no algorithm column, and that is load-bearing rather than an
-- omission: every hash written here is self-describing, either the
-- PBKDF2SHA256$iterations$salt$hash form authmgr writes today or a successor
-- that names itself the same way. A bare hash with no prefix would be
-- unreadable to authmgr.validatePasswd, which dispatches on the prefix and
-- falls back to length. Anything writing this column has to keep that true.
--
-- client_kdf_params is the *client's* parameter set, not this column's: the
-- PHC string store.KDFParams.String() writes, carrying the per-user salt, so
-- there is no separate salt column. It sits here because it governs how a
-- password becomes the authKey that the hash column stores, and an account
-- with no password has none -- which is why it is nullable, not defaulted.
--
-- It is one fact stored twice. The same parameters ride inside the wrapped
-- identity key in AccountIdentityKey, because store.WrapIdentity makes every
-- blob self-describing; this copy exists so the pre-login endpoint can answer
-- "what were you stretched under" without handing out the blob itself, which
-- is an offline attack target. account.SetKeys checks the two agree at the
-- write, because a pair that can drift is a pair that will.
CREATE TABLE IF NOT EXISTS AccountPassword (
  account_id        BLOB    PRIMARY KEY REFERENCES Account(id),
  hash              TEXT    NOT NULL,
  changed_at        INTEGER NOT NULL,
  client_kdf_params TEXT
);

-- The account's published X25519 identity key, and its private half wrapped
-- under a key derived from the password. Exactly one row per account.
--
-- public_key is a column rather than something derived, because another
-- member's client wraps a library content key to it: it is read by people who
-- are not its owner and must be servable without unwrapping anything.
--
-- wrapped_key is opaque here. It is store wrap kind 1 -- sealed under wrapKey,
-- with the account id bound in as associated data, so this server can neither
-- read it nor hand one account's blob to another and watch what happens.
CREATE TABLE IF NOT EXISTS AccountIdentityKey (
  account_id  BLOB    PRIMARY KEY REFERENCES Account(id),
  public_key  BLOB    NOT NULL,
  wrapped_key BLOB    NOT NULL,
  updated_at  INTEGER NOT NULL
);

-- The same identity private key, wrapped once per recovery code. Ten rows per
-- account; store wrap kind 2.
--
-- Individually deletable rows, and the granularity is forced rather than
-- chosen: redeeming a code deletes its blob and the rest of the set stands.
-- Regenerating the whole set on redemption is the tidier-looking rule and the
-- worse one, because it invalidates the codes a person is still holding at the
-- moment they have proved they lost their device.
--
-- ordinal says which of the set a blob is, and is never the code. The server
-- never learns a code: redemption is the client fetching the set and trying
-- each blob.
CREATE TABLE IF NOT EXISTS AccountRecoveryWrap (
  account_id  BLOB    NOT NULL REFERENCES Account(id),
  ordinal     INTEGER NOT NULL,
  wrapped_key BLOB    NOT NULL,
  ctime       INTEGER NOT NULL,
  PRIMARY KEY (account_id, ordinal)
);

-- A secret this server holds for its own use, minted on first need and read
-- back on every start after that.
--
-- One table rather than one file per secret, because the property every
-- consumer needs is that the value outlives the process: the pre-login KDF
-- endpoint derives an unknown address's fake parameters from one of these, and
-- a secret regenerated at boot would make that answer differ across a restart
-- -- which is the enumeration oracle the fake exists to close.
--
-- name is the purpose, not a key id. Two purposes must never share a secret,
-- so the name is part of the primary key and reaching for a new purpose mints
-- a new row rather than reusing one.
-- Administrative authority, one row per capability an account holds.
--
-- Rows rather than columns, so that adding an administrative operation is data
-- and not a migration: a column per operation means a schema change, a CLI
-- flag, a JSON field and a checkbox for every new verb, forever, and "what can
-- this person do" becomes a read of N columns no query can ask generically.
--
-- A row is half of an answer, never the whole one. The rule is a conjunction
-- and lives in fileserver/admin: role = admin AND a row exists. So these rows
-- survive an account being demoted out of admin and mean nothing while it is,
-- which is what makes a demotion reversible without remembering a set.
--
-- See docs/plans/admin.md.
CREATE TABLE IF NOT EXISTS AccountCapability (
  account_id BLOB NOT NULL REFERENCES Account(id),
  capability TEXT NOT NULL,
  PRIMARY KEY (account_id, capability)
);

CREATE TABLE IF NOT EXISTS ServerSecret (
  name   TEXT    PRIMARY KEY,
  secret BLOB    NOT NULL,
  ctime  INTEGER NOT NULL
);

-- The one-time token that creates this server's first account.
--
-- At most one row, ever: the CHECK is what makes "single use" a property of the
-- schema rather than of everyone remembering to delete the old one. Claiming
-- deletes the row in the same transaction that inserts the account, so the two
-- cannot come apart -- a server can never end up with an account and a live
-- token, or with the token spent and no account to show for it.
--
-- The token is stored as it is read, not hashed, which is the opposite of every
-- Credential row and is deliberate. It has to be: the server reprints it at
-- every boot until it is claimed and "silo setup-token" prints it on demand,
-- and a hash can do neither. The trade costs nothing, because the row exists
-- only while the server has no accounts, and anyone who can read this table can
-- already INSERT INTO Account by hand.
CREATE TABLE IF NOT EXISTS SetupToken (
  id    INTEGER PRIMARY KEY CHECK (id = 1),
  token TEXT    NOT NULL,
  ctime INTEGER NOT NULL
);

-- Libraries, grants, quotas.
-- The head of a library's history: which commit it is on, and the root that
-- commit names.
--
-- root_id sits here rather than being read back out of the commit because the
-- two are one fact -- where the library is now -- and they move together in
-- one UPDATE. Keeping them in one row is what makes that atomic for free, and
-- it takes an object-store read off the path every request goes down.
--
-- It is also the only way it can work at all on an end-to-end encrypted
-- library, where the server can open a commit's public section but is doing so
-- on every load to learn something it wrote itself.
CREATE TABLE IF NOT EXISTS Branch (name VARCHAR(10), library_id CHAR(40), commit_id CHAR(64), root_id CHAR(64), PRIMARY KEY (library_id, name));
-- A library, and how its bytes are made.
--
-- The chunker parameters are stored per library rather than compiled in,
-- because a client that chunks differently computes different ids: they are
-- part of the library's identity, not a server setting that may drift under
-- it. Frozen at creation; changing one is the convert operation, which
-- rewrites every object, not an UPDATE.
--
-- They are also why there is no DEFAULT here. A creation path that forgets to
-- write them should fail loudly at the INSERT, not quietly produce a library
-- whose parameters came from whatever the schema happened to say.
--
-- e2ee is the library's own answer to "can the server read this", and it is
-- not the old is_encrypted: that was a password over a server-side key, and
-- it is being deleted along with the columns that fed it.
CREATE TABLE IF NOT EXISTS Library (
  library_id      CHAR(37) PRIMARY KEY,
  chunker      TEXT     NOT NULL,
  chunk_min    INTEGER  NOT NULL,
  chunk_target INTEGER  NOT NULL,
  chunk_max    INTEGER  NOT NULL,
  chunk_norm   INTEGER  NOT NULL,
  e2ee         INTEGER  NOT NULL
);
-- The listing sidecar: what a manifest says its file is, by manifest id.
--
-- A directory entry carries a name, a type and a child id, and deliberately no
-- size — the size lives in the file's manifest, and reading one manifest per
-- entry to answer one listing is the cost this table exists to pay once
-- instead of every time.
--
-- Keyed by object id alone, with no store or library column, because
-- content-addressing makes that correct: the id is the hash of the encoded
-- manifest, so two libraries holding the same id hold the same bytes and the
-- same declared size. A manifest shared by dedup is measured once.
--
-- Advisory, in the plan's sense. The manifest is the authority and this is a
-- copy of one number out of it; nothing sizes an allocation or decides a sync
-- from here. The row cannot go stale — an id names fixed bytes forever — so
-- the only failure available is a missing row, which reads as "not looked up
-- yet" and is filled the next time the entry is listed.
CREATE TABLE IF NOT EXISTS ObjectSize (
  object_id CHAR(64) PRIMARY KEY,
  file_size BIGINT   NOT NULL
);
-- An end-to-end encrypted library's content key, wrapped to one member's
-- X25519 public key. One row per member; sharing a library is one more row.
--
-- The server cannot open any of these and does not try. What it can do is
-- refuse to create a library whose key would have no row at all, which is the
-- failure this table exists to prevent: a content key that lives only on the
-- device that generated it is data loss wearing a feature's clothes.
--
-- The wrap binds both the holder and the library as associated data, so a
-- server that moved a blob between rows would produce one that no longer
-- opens. That is why the library id has to exist before the wrap can be made,
-- and why an E2EE library's id arrives with the create request rather than
-- being minted here.
CREATE TABLE IF NOT EXISTS LibraryKeyWrap (
  library_id  CHAR(37) NOT NULL,
  account_id  BLOB     NOT NULL REFERENCES Account(id),
  wrapped_key BLOB     NOT NULL,
  ctime       INTEGER  NOT NULL,
  PRIMARY KEY (library_id, account_id)
);
CREATE INDEX IF NOT EXISTS library_key_wrap_account_idx ON LibraryKeyWrap (account_id);

CREATE TABLE IF NOT EXISTS LibraryOwner (library_id CHAR(37) PRIMARY KEY, account_id BLOB NOT NULL REFERENCES Account(id));
CREATE INDEX IF NOT EXISTS OwnerIndex ON LibraryOwner (account_id);

CREATE TABLE IF NOT EXISTS LibraryHead (library_id CHAR(37) PRIMARY KEY, branch_name VARCHAR(10));
-- What a library holds, as the number quota is charged on.
--
-- size is logical size at head: the sum of file_size over the files the head
-- commit reaches. Not stored bytes and not disk. Under content-defined
-- chunking, dedup and deferred compaction those three diverge by multiples in
-- both directions, and only this one is predictable -- delete a 2 GB file, get
-- 2 GB back -- and only this one holds still while the server works. A
-- compaction run must never change what somebody is charged.
--
-- root_id is the root these totals are true at, and it is what makes the row
-- self-correcting rather than a cache somebody has to remember to invalidate.
-- A reader whose library is on a different root brings the row forward with a
-- Merkle delta against this one and CASes on it, so a total that was never
-- written, or was lost to a crash, costs the next reader a walk and nothing
-- else. It is the root rather than the commit because the root is what the
-- delta is computed between, and because a commit that publishes an identical
-- root changes no total.
--
CREATE TABLE IF NOT EXISTS LibraryUsage (
  library_id    CHAR(37) PRIMARY KEY,
  size       BIGINT   NOT NULL,
  file_count BIGINT   NOT NULL,
  root_id    CHAR(64) NOT NULL
);
CREATE TABLE IF NOT EXISTS LibraryHistoryLimit (library_id CHAR(37) PRIMARY KEY, days INTEGER);
CREATE TABLE IF NOT EXISTS LibraryValidSince (library_id CHAR(37) PRIMARY KEY, timestamp BIGINT);

CREATE TABLE IF NOT EXISTS VirtualLibrary (library_id CHAR(36) PRIMARY KEY, origin_library CHAR(36), path TEXT, base_commit CHAR(40));
CREATE INDEX IF NOT EXISTS virtuallibrary_origin_library_idx ON VirtualLibrary (origin_library);
CREATE TABLE IF NOT EXISTS GarbageLibraries (library_id CHAR(36) PRIMARY KEY);


-- A library's display metadata, and the authority for it.
--
-- These used to be a cache of fields copied out of the head commit on every
-- head move, with the commit as the source of truth. That is inverted: the
-- commit carries none of them, and this table is where they live.
--
-- The inversion is forced by end-to-end encryption. A sealed commit's author
-- and message are inside the seal, and a library's display name has nowhere
-- to live in one at all, so a server reading its own library's metadata out of
-- a commit would be reading fields it cannot read.
--
-- **Every column here is a server-observed fact, not a claim copied from a
-- client.** last_modifier is the account that was authenticated when the head
-- moved; update_time is the server's clock at that moment. On an E2EE library
-- these can differ from the author and created_at sealed inside the commit --
-- a client may seal any attribution it likes -- and that is correct rather
-- than a discrepancy to reconcile later. They answer different questions: the
-- sealed pair is what the library's members say happened, and these are what
-- the server witnessed. Where they disagree, this table is the one that is
-- not a claim.
--
-- **name is therefore permanently server-plaintext**, and so is any
-- description beside it. E2EE covers a library's content and the names of the
-- files inside it; it does not cover the library's own display name, which the
-- server has to sort, search and show in a listing to a client that has not
-- unlocked anything. Recorded as a boundary rather than a gap: see the
-- guardrails in docs/storage.md.
--
-- last_modifier holds an address rather than an account id, and stays that
-- way. It is display data of the same kind as a commit author: a record of
-- what someone was called at the time, not a key anything is resolved from.
CREATE TABLE IF NOT EXISTS LibraryInfo (library_id CHAR(36) PRIMARY KEY, name VARCHAR(255) NOT NULL, update_time INTEGER, version INTEGER, is_encrypted INTEGER, last_modifier VARCHAR(255), status INTEGER DEFAULT 0, type VARCHAR(10));
CREATE INDEX IF NOT EXISTS LibraryInfoTypeIndex on LibraryInfo (type);

CREATE TABLE IF NOT EXISTS UserQuota (account_id BLOB PRIMARY KEY REFERENCES Account(id), quota BIGINT);

-- How long a library keeps history, in days.
--
-- A row is the library's own policy; no row means the server default in
-- [history] keep_days applies. Absent rather than a stored sentinel for the
-- same reason UserQuota uses an absent row: "never configured" and "configured
-- to the value the default happens to hold today" are different states, and
-- only one of them follows the default when an operator changes it.
--
-- 0 means keep everything. It is a real setting and not "unset" -- a library
-- that must retain every commit is a thing somebody chooses, and it has to
-- survive a server default that says otherwise.
CREATE TABLE IF NOT EXISTS LibraryRetention (library_id CHAR(37) PRIMARY KEY, keep_days INTEGER NOT NULL);

CREATE TABLE IF NOT EXISTS GCID (library_id CHAR(36) PRIMARY KEY, gc_id VARCHAR(10));
CREATE TABLE IF NOT EXISTS LastGCID (id INTEGER PRIMARY KEY AUTOINCREMENT, library_id CHAR(36) NOT NULL, client_id VARCHAR(128) NOT NULL, gc_id VARCHAR(10) NOT NULL);
CREATE UNIQUE INDEX IF NOT EXISTS lastgcid_libraryid_clientid_idx ON LastGCID (library_id, client_id);

-- ApiToken is gone too. It stored a cleartext bearer secret as its own primary
-- key and slid its expiry forward on every use, so a mount that polled could
-- never age out.

-- One credential row for every secret a client presents to Silo. It replaced
-- three separate stores -- ApiToken, LibraryUserToken and session JWTs -- that
-- could not be revoked together, which is what made "revoke everything this
-- person holds" unanswerable. Every authenticated route resolves through it.
--
-- scope is empty for "every library". credential.ParseScope owns the encoding
-- of the narrower forms. It is TEXT and not CHAR(37) because a scope can name a
-- folder inside a library, not just the library. Empty rather than NULL so
-- there is one spelling of "unscoped" rather than two.
--
-- An s3 row carries neither secret_hash nor public_key -- it derives its
-- secret from the master key -- which is why the CHECK permits both being NULL.
-- One table answers "may this principal do op at (library, path)".
--
-- principal is the kind and its identifier: 'user:<uuid>', 'group:<id>',
-- 'anon', and later 'link:<credential-id>'. Encoded as one string rather than
-- as a kind column beside a nullable id column per kind, because the question
-- CheckPerm asks is "does any of these principals hold a grant here", and a
-- list of strings is one IN clause where three nullable columns are three
-- joins that can disagree.
--
-- path is the subtree the grant reaches, '/' for the whole library. It is what
-- makes a subfolder share expressible without a second mechanism.
--
-- listed means something only for the anon principal: a public library that
-- appears in the public listing, as against one reachable only by its link.
-- Same grant, different discovery, which is the whole point of unifying them.
--
-- The unique index is the model's shape rather than a nicety: two grants to
-- one principal on one path are not two facts, and without it "share again
-- with a different permission" silently accumulates rows that later disagree.
--
-- Named LibraryGrant rather than Grant because GRANT is SQL. The plan's table
-- is spelled Grant; a bare keyword as a table name works only quoted, and one
-- unquoted mention anywhere is a syntax error nobody sees until that path runs.
--
-- See docs/plans/sharing.md § The grant model.
CREATE TABLE IF NOT EXISTS LibraryGrant (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  principal  TEXT    NOT NULL,
  library_id CHAR(37) NOT NULL,
  path       TEXT    NOT NULL DEFAULT '/',
  perm       TEXT    NOT NULL,
  listed     INTEGER NOT NULL DEFAULT 0,
  created_by BLOB    REFERENCES Account(id),
  ctime      INTEGER NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS library_grant_one_idx
  ON LibraryGrant (principal, library_id, path);
CREATE INDEX IF NOT EXISTS library_grant_library_idx ON LibraryGrant (library_id);

-- An invite, and the address it is for.
--
-- The credential row carries the secret, the expiry and the revocation; this
-- carries what an invite is that a credential is not. Two tables rather than
-- columns on Credential because an invite is the only kind with an intended
-- recipient, and four kinds that never use a column is how a table stops
-- describing anything.
--
-- email is the binding, and the redeemer does not choose it. Delivery of the
-- invite to that inbox *is* the address verification, which is the whole reason
-- registration goes through an invite rather than a signup form. label on the
-- credential is display and is not this.
--
-- redeemed_at is what makes it single-use. NULL until spent; a second
-- redemption is refused by the write that sets it rather than by a check some
-- caller might skip.
--
-- See docs/plans/sharing.md § Accounts.
CREATE TABLE IF NOT EXISTS Invite (
  credential_id TEXT    PRIMARY KEY REFERENCES Credential(id),
  email         TEXT    NOT NULL,
  role          TEXT    NOT NULL,
  created_by    BLOB    NOT NULL REFERENCES Account(id),
  ctime         INTEGER NOT NULL,
  redeemed_at   INTEGER
);
CREATE INDEX IF NOT EXISTS invite_email_idx ON Invite (email);

CREATE TABLE IF NOT EXISTS Credential (
  id          TEXT   PRIMARY KEY,
  kind        TEXT   NOT NULL,
  secret_hash BLOB,
  public_key  BLOB,
  account_id  BLOB   NOT NULL REFERENCES Account(id),
  label       TEXT   NOT NULL,
  scope       TEXT   NOT NULL DEFAULT '',
  perm        TEXT   NOT NULL,
  client_id   TEXT,
  ctime       BIGINT NOT NULL,
  expires_at  BIGINT,
  last_used   BIGINT,
  CHECK (secret_hash IS NULL OR public_key IS NULL)
);
CREATE INDEX IF NOT EXISTS credential_account_idx ON Credential (account_id);
CREATE INDEX IF NOT EXISTS credential_expires_idx ON Credential (expires_at);
`
