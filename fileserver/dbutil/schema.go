package dbutil

import (
	"database/sql"
	"fmt"
	"strings"

	log "github.com/sirupsen/logrus"
)

// The Silo database schema.
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
  is_staff   INTEGER NOT NULL DEFAULT 0,
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
CREATE TABLE IF NOT EXISTS AccountPassword (
  account_id BLOB    PRIMARY KEY REFERENCES Account(id),
  hash       TEXT    NOT NULL,
  changed_at INTEGER NOT NULL
);

-- Groups.
CREATE TABLE IF NOT EXISTS "Group" (group_id INTEGER PRIMARY KEY AUTOINCREMENT, group_name VARCHAR(255), creator_account_id BLOB NOT NULL REFERENCES Account(id), timestamp BIGINT, type VARCHAR(32), parent_group_id INTEGER);
CREATE TABLE IF NOT EXISTS GroupUser (group_id INTEGER, account_id BLOB NOT NULL REFERENCES Account(id), is_staff tinyint);
CREATE UNIQUE INDEX IF NOT EXISTS groupid_account_indx on GroupUser (group_id, account_id);
CREATE INDEX IF NOT EXISTS groupuser_account_indx on GroupUser (account_id);
CREATE TABLE IF NOT EXISTS GroupStructure (group_id INTEGER PRIMARY KEY, path VARCHAR(1024));

-- Repositories, shares, tokens, permissions, quotas.
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
CREATE TABLE IF NOT EXISTS LibraryOwner (library_id CHAR(37) PRIMARY KEY, account_id BLOB NOT NULL REFERENCES Account(id));
CREATE INDEX IF NOT EXISTS OwnerIndex ON LibraryOwner (account_id);

CREATE TABLE IF NOT EXISTS LibraryGroup (library_id CHAR(37), group_id INTEGER, account_id BLOB NOT NULL REFERENCES Account(id), permission CHAR(15));
CREATE UNIQUE INDEX IF NOT EXISTS groupid_libraryid_indx on LibraryGroup (group_id, library_id);
CREATE INDEX IF NOT EXISTS librarygroup_libraryid_index on LibraryGroup (library_id);
CREATE INDEX IF NOT EXISTS librarygroup_account_indx on LibraryGroup (account_id);
CREATE TABLE IF NOT EXISTS InnerPubLibrary (library_id CHAR(37) PRIMARY KEY, permission CHAR(15));

CREATE TABLE IF NOT EXISTS LibraryUserToken (library_id CHAR(37), account_id BLOB NOT NULL REFERENCES Account(id), token CHAR(41), ctime BIGINT);
CREATE UNIQUE INDEX IF NOT EXISTS library_token_indx on LibraryUserToken (library_id, token);
CREATE INDEX IF NOT EXISTS library_token_account_indx on LibraryUserToken (account_id);
CREATE TABLE IF NOT EXISTS LibraryTokenPeerInfo (token CHAR(41) PRIMARY KEY, peer_id CHAR(41), peer_ip VARCHAR(50), peer_name VARCHAR(255), sync_time BIGINT, client_ver VARCHAR(20));

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
-- Separate from Librariesize, which the dying scheduler owns, because the two
-- cover disjoint sets of libraries -- 40-hex heads there, 64-hex heads here --
-- and sharing a row would have made the deletion a rewrite instead of a
-- subtraction.
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
-- guardrails in docs/plans/store-v2.md.
--
-- last_modifier holds an address rather than an account id, and stays that
-- way. It is display data of the same kind as a commit author: a record of
-- what someone was called at the time, not a key anything is resolved from.
CREATE TABLE IF NOT EXISTS LibraryInfo (library_id CHAR(36) PRIMARY KEY, name VARCHAR(255) NOT NULL, update_time INTEGER, version INTEGER, is_encrypted INTEGER, last_modifier VARCHAR(255), status INTEGER DEFAULT 0, type VARCHAR(10));
CREATE INDEX IF NOT EXISTS LibraryInfoTypeIndex on LibraryInfo (type);

CREATE TABLE IF NOT EXISTS UserQuota (account_id BLOB PRIMARY KEY REFERENCES Account(id), quota BIGINT);

CREATE TABLE IF NOT EXISTS SharedLibrary (library_id CHAR(37), from_account_id BLOB NOT NULL REFERENCES Account(id), to_account_id BLOB NOT NULL REFERENCES Account(id), permission CHAR(15));
CREATE INDEX IF NOT EXISTS LibraryIdIndex on SharedLibrary (library_id);
CREATE INDEX IF NOT EXISTS FromAccountIndex on SharedLibrary (from_account_id);
CREATE INDEX IF NOT EXISTS ToAccountIndex on SharedLibrary (to_account_id);

CREATE TABLE IF NOT EXISTS GCID (library_id CHAR(36) PRIMARY KEY, gc_id VARCHAR(10));
CREATE TABLE IF NOT EXISTS LastGCID (id INTEGER PRIMARY KEY AUTOINCREMENT, library_id CHAR(36) NOT NULL, client_id VARCHAR(128) NOT NULL, gc_id VARCHAR(10) NOT NULL);
CREATE UNIQUE INDEX IF NOT EXISTS lastgcid_libraryid_clientid_idx ON LastGCID (library_id, client_id);

CREATE TABLE IF NOT EXISTS ApiToken (token CHAR(40) PRIMARY KEY, account_id BLOB NOT NULL REFERENCES Account(id), ctime BIGINT, expires_at BIGINT NOT NULL);
CREATE INDEX IF NOT EXISTS apitoken_account_idx ON ApiToken (account_id);
CREATE INDEX IF NOT EXISTS apitoken_expires_idx ON ApiToken (expires_at);

-- One credential row for every secret a client presents to Silo, replacing the
-- three separate stores (ApiToken, LibraryUserToken, session JWTs) that could not
-- be revoked together. Both lanes still write their own tables. Nothing reads
-- this one yet.
--
-- scope is empty for "every library". credential.ParseScope owns the encoding
-- of the narrower forms. It is TEXT and not CHAR(37) because a scope can name a
-- folder inside a library, not just the library. Empty rather than NULL so
-- there is one spelling of "unscoped" rather than two.
--
-- An s3 row carries neither secret_hash nor public_key -- it derives its
-- secret from the master key -- which is why the CHECK permits both being NULL.
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

// CreateSiloTables creates all tables if they don't exist.
func CreateSiloTables(db *sql.DB) error {
	return execSchema(db, siloSchema)
}

// stripComments removes -- comments before the schema is split on semicolons.
//
// Splitting on ";" is how the statements are separated, and a comment
// containing one used to be cut in half and executed as SQL — a startup
// failure caused by a sentence. The schema is this package's own constant, so
// there are no string literals to worry about escaping around: every -- in it
// starts a comment.
func stripComments(schema string) string {
	var b strings.Builder
	for _, line := range strings.Split(schema, "\n") {
		if i := strings.Index(line, "--"); i >= 0 {
			line = line[:i]
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}

func execSchema(db *sql.DB, schema string) error {
	statements := strings.Split(stripComments(schema), ";")
	for _, stmt := range statements {
		stmt = strings.TrimSpace(stmt)
		if stmt == "" {
			continue
		}
		if _, err := db.Exec(stmt); err != nil {
			return fmt.Errorf("failed to execute schema statement: %v\nSQL: %s", err, stmt)
		}
	}
	log.Info("Database tables created successfully")
	return nil
}
