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
// (ccnet.db and seafile.db), inherited from upstream's two server processes.
// Silo is one process, so they are one database: no table name collides
// between the two halves, and a share permission check that has to read a
// group and a repository can now do it in a single statement.
const siloSchema = `
-- Accounts. An account is an opaque id, and an address is something it has.
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
CREATE TABLE IF NOT EXISTS Branch (name VARCHAR(10), repo_id CHAR(40), commit_id CHAR(40), PRIMARY KEY (repo_id, name));
CREATE TABLE IF NOT EXISTS Repo (repo_id CHAR(37) PRIMARY KEY);
CREATE TABLE IF NOT EXISTS RepoOwner (repo_id CHAR(37) PRIMARY KEY, account_id BLOB NOT NULL REFERENCES Account(id));
CREATE INDEX IF NOT EXISTS OwnerIndex ON RepoOwner (account_id);

CREATE TABLE IF NOT EXISTS RepoGroup (repo_id CHAR(37), group_id INTEGER, account_id BLOB NOT NULL REFERENCES Account(id), permission CHAR(15));
CREATE UNIQUE INDEX IF NOT EXISTS groupid_repoid_indx on RepoGroup (group_id, repo_id);
CREATE INDEX IF NOT EXISTS repogroup_repoid_index on RepoGroup (repo_id);
CREATE INDEX IF NOT EXISTS repogroup_account_indx on RepoGroup (account_id);
CREATE TABLE IF NOT EXISTS InnerPubRepo (repo_id CHAR(37) PRIMARY KEY, permission CHAR(15));

CREATE TABLE IF NOT EXISTS RepoUserToken (repo_id CHAR(37), account_id BLOB NOT NULL REFERENCES Account(id), token CHAR(41), ctime BIGINT);
CREATE UNIQUE INDEX IF NOT EXISTS repo_token_indx on RepoUserToken (repo_id, token);
CREATE INDEX IF NOT EXISTS repo_token_account_indx on RepoUserToken (account_id);
CREATE TABLE IF NOT EXISTS RepoTokenPeerInfo (token CHAR(41) PRIMARY KEY, peer_id CHAR(41), peer_ip VARCHAR(50), peer_name VARCHAR(255), sync_time BIGINT, client_ver VARCHAR(20));

CREATE TABLE IF NOT EXISTS RepoHead (repo_id CHAR(37) PRIMARY KEY, branch_name VARCHAR(10));
CREATE TABLE IF NOT EXISTS RepoSize (repo_id CHAR(37) PRIMARY KEY, size BIGINT UNSIGNED, head_id CHAR(41));
CREATE TABLE IF NOT EXISTS RepoHistoryLimit (repo_id CHAR(37) PRIMARY KEY, days INTEGER);
CREATE TABLE IF NOT EXISTS RepoValidSince (repo_id CHAR(37) PRIMARY KEY, timestamp BIGINT);

CREATE TABLE IF NOT EXISTS VirtualRepo (repo_id CHAR(36) PRIMARY KEY, origin_repo CHAR(36), path TEXT, base_commit CHAR(40));
CREATE INDEX IF NOT EXISTS virtualrepo_origin_repo_idx ON VirtualRepo (origin_repo);
CREATE TABLE IF NOT EXISTS GarbageRepos (repo_id CHAR(36) PRIMARY KEY);

CREATE TABLE IF NOT EXISTS RepoFileCount (repo_id CHAR(36) PRIMARY KEY, file_count BIGINT UNSIGNED);

CREATE TABLE IF NOT EXISTS WebUploadTempFiles (repo_id CHAR(40) NOT NULL, file_path TEXT NOT NULL, tmp_file_path TEXT NOT NULL);

-- last_modifier holds an address rather than an account id, and stays that
-- way. It is display data of the same kind as a commit author: a record of
-- what someone was called at the time, not a key anything is resolved from.
CREATE TABLE IF NOT EXISTS RepoInfo (repo_id CHAR(36) PRIMARY KEY, name VARCHAR(255) NOT NULL, update_time INTEGER, version INTEGER, is_encrypted INTEGER, last_modifier VARCHAR(255), status INTEGER DEFAULT 0, type VARCHAR(10));
CREATE INDEX IF NOT EXISTS RepoInfoTypeIndex on RepoInfo (type);

CREATE TABLE IF NOT EXISTS UserQuota (account_id BLOB PRIMARY KEY REFERENCES Account(id), quota BIGINT);

CREATE TABLE IF NOT EXISTS SharedRepo (repo_id CHAR(37), from_account_id BLOB NOT NULL REFERENCES Account(id), to_account_id BLOB NOT NULL REFERENCES Account(id), permission CHAR(15));
CREATE INDEX IF NOT EXISTS RepoIdIndex on SharedRepo (repo_id);
CREATE INDEX IF NOT EXISTS FromAccountIndex on SharedRepo (from_account_id);
CREATE INDEX IF NOT EXISTS ToAccountIndex on SharedRepo (to_account_id);

CREATE TABLE IF NOT EXISTS GCID (repo_id CHAR(36) PRIMARY KEY, gc_id VARCHAR(10));
CREATE TABLE IF NOT EXISTS LastGCID (id INTEGER PRIMARY KEY AUTOINCREMENT, repo_id CHAR(36) NOT NULL, client_id VARCHAR(128) NOT NULL, gc_id VARCHAR(10) NOT NULL);
CREATE UNIQUE INDEX IF NOT EXISTS lastgcid_repoid_clientid_idx ON LastGCID (repo_id, client_id);

CREATE TABLE IF NOT EXISTS ApiToken (token CHAR(40) PRIMARY KEY, account_id BLOB NOT NULL REFERENCES Account(id), ctime BIGINT, expires_at BIGINT NOT NULL);
CREATE INDEX IF NOT EXISTS apitoken_account_idx ON ApiToken (account_id);
CREATE INDEX IF NOT EXISTS apitoken_expires_idx ON ApiToken (expires_at);

-- One credential row for every secret a client presents to Silo, replacing the
-- three separate stores (ApiToken, RepoUserToken, session JWTs) that could not
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
