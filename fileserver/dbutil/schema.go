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

CREATE TABLE IF NOT EXISTS LibraryGroup (library_id CHAR(37), group_id INTEGER, account_id BLOB NOT NULL REFERENCES Account(id), permission CHAR(15));
CREATE UNIQUE INDEX IF NOT EXISTS groupid_libraryid_indx on LibraryGroup (group_id, library_id);
CREATE INDEX IF NOT EXISTS librarygroup_libraryid_index on LibraryGroup (library_id);
CREATE INDEX IF NOT EXISTS librarygroup_account_indx on LibraryGroup (account_id);
CREATE TABLE IF NOT EXISTS InnerPubLibrary (library_id CHAR(37) PRIMARY KEY, permission CHAR(15));

-- LibraryUserToken is gone. It held per-user, per-library sync tokens with no
-- expiry, for a lane that was deleted; the Credential table below replaced it.
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

CREATE TABLE IF NOT EXISTS SharedLibrary (library_id CHAR(37), from_account_id BLOB NOT NULL REFERENCES Account(id), to_account_id BLOB NOT NULL REFERENCES Account(id), permission CHAR(15));
CREATE INDEX IF NOT EXISTS LibraryIdIndex on SharedLibrary (library_id);
CREATE INDEX IF NOT EXISTS FromAccountIndex on SharedLibrary (from_account_id);
CREATE INDEX IF NOT EXISTS ToAccountIndex on SharedLibrary (to_account_id);

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

// SchemaVersion identifies the shape of the tables in siloSchema, stored in
// the database's own PRAGMA user_version. CreateSiloTables refuses to run
// against a database already stamped with a different version, rather than
// letting a rename or a dropped column fail wherever the mismatch happens to
// be hit first — which today is a raw SQLite error deep inside whichever
// CREATE statement is the first to mention a column the old shape lacks.
//
// Bump it whenever a change to siloSchema is not purely additive: a rename, a
// drop, a type change — anything CREATE TABLE/INDEX IF NOT EXISTS cannot
// apply to a table that already exists in the old shape. A change that only
// adds a new table or a new index needs no bump; CREATE TABLE IF NOT EXISTS
// already applies that safely to an older database.
// Version 2 added client_kdf_params to AccountPassword, which is a change to
// an existing table rather than a new one -- CREATE TABLE IF NOT EXISTS cannot
// apply it to a database already holding the old shape, which is exactly what
// this constant is for.
const SchemaVersion = 2

// CreateSiloTables creates all tables if they don't exist, after checking
// the database's schema version against SchemaVersion.
//
// A database already stamped with a different version is refused outright:
// this package has no migration path, and running siloSchema against a
// database in an incompatible shape produces a confusing failure on whichever
// statement first names the mismatch instead of a clear one up front.
//
// A database with no stamp is the interesting case, because it is two
// populations wearing one value. PRAGMA user_version is 0 both for a database
// this call is about to create and for every database written before the stamp
// existed — and the second group is not hypothetical. The library rename had
// already shipped when the stamp landed, so the unstamped population is
// exactly the one the stamp was introduced to protect, and letting it through
// meant the guard covered the next rename while doing nothing about the one
// that had happened.
//
// One query separates them: a database this call is about to create has an
// empty sqlite_master. Unstamped and empty is a fresh start. Unstamped and
// already holding tables is from before versioning, and is refused with the
// same message as a version mismatch, because for the operator it is the same
// situation.
//
// Only once the schema has been applied without error is user_version set, so
// a database is only ever stamped as a version it has actually been verified
// against.
func CreateSiloTables(db *sql.DB) error {
	stored, err := schemaVersion(db)
	if err != nil {
		return fmt.Errorf("failed to read schema version: %w", err)
	}
	if stored == 0 {
		populated, err := hasTables(db)
		if err != nil {
			return fmt.Errorf("failed to inspect the database: %w", err)
		}
		if populated {
			return fmt.Errorf("this database has tables but no schema version stamp, so it was written "+
				"before this build's schema (schema version %d); "+
				"refusing to start rather than run a mismatched schema against it. "+
				"If this database is disposable, delete it and let Silo recreate it; "+
				"otherwise migrate it by hand — this package has no migration path",
				SchemaVersion)
		}
	}
	if stored != 0 && stored != SchemaVersion {
		return fmt.Errorf("database schema version %d does not match what this build expects (schema version %d); "+
			"refusing to start rather than run a mismatched schema against it. "+
			"If this database is disposable, delete it and let Silo recreate it; "+
			"otherwise run the binary that wrote version %d, or migrate the database by hand",
			stored, SchemaVersion, stored)
	}

	if err := execSchema(db, siloSchema); err != nil {
		return err
	}

	if stored != SchemaVersion {
		if _, err := db.Exec(fmt.Sprintf("PRAGMA user_version = %d", SchemaVersion)); err != nil {
			return fmt.Errorf("failed to record schema version: %w", err)
		}
	}
	return nil
}

// hasTables reports whether the database holds any table of its own.
//
// sqlite_% is excluded because those are SQLite's: sqlite_sequence appears on
// its own the first time an AUTOINCREMENT column is written, and counting it
// would make a database SQLite populated look like one Silo populated.
func hasTables(db *sql.DB) (bool, error) {
	var n int
	err := db.QueryRow(
		`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%'`).Scan(&n)
	return n > 0, err
}

func schemaVersion(db *sql.DB) (int, error) {
	var v int
	if err := db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		return 0, err
	}
	return v, nil
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
