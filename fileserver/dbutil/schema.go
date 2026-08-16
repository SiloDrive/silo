package dbutil

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
)

// Ccnet database schema (users, groups)
const ccnetSchema = `
CREATE TABLE IF NOT EXISTS Binding (email TEXT, peer_id TEXT);
CREATE UNIQUE INDEX IF NOT EXISTS peer_index on Binding (peer_id);

CREATE TABLE IF NOT EXISTS EmailUser (id INTEGER NOT NULL PRIMARY KEY AUTOINCREMENT, email TEXT, passwd TEXT, is_staff bool NOT NULL, is_active bool NOT NULL, ctime INTEGER, reference_id TEXT);
CREATE UNIQUE INDEX IF NOT EXISTS email_index on EmailUser (email);
CREATE UNIQUE INDEX IF NOT EXISTS reference_id_index on EmailUser (reference_id);

CREATE TABLE IF NOT EXISTS LDAPConfig (cfg_group VARCHAR(255) NOT NULL, cfg_key VARCHAR(255) NOT NULL, value VARCHAR(255), property INTEGER);

CREATE TABLE IF NOT EXISTS LDAPUsers (id INTEGER PRIMARY KEY AUTOINCREMENT, email TEXT NOT NULL, password TEXT NOT NULL, is_staff BOOL NOT NULL, is_active BOOL NOT NULL, extra_attrs TEXT, reference_id TEXT);
CREATE UNIQUE INDEX IF NOT EXISTS ldapusers_email_index on LDAPUsers(email);
CREATE UNIQUE INDEX IF NOT EXISTS ldapusers_reference_id_index on LDAPUsers(reference_id);

CREATE TABLE IF NOT EXISTS UserRole (email TEXT, role TEXT, is_manual_set INTEGER DEFAULT 0);
CREATE INDEX IF NOT EXISTS userrole_email_index on UserRole (email);
CREATE UNIQUE INDEX IF NOT EXISTS userrole_userrole_index on UserRole (email, role);

CREATE TABLE IF NOT EXISTS "Group" (group_id INTEGER PRIMARY KEY AUTOINCREMENT, group_name VARCHAR(255), creator_name VARCHAR(255), timestamp BIGINT, type VARCHAR(32), parent_group_id INTEGER);
CREATE TABLE IF NOT EXISTS GroupUser (group_id INTEGER, user_name VARCHAR(255), is_staff tinyint);
CREATE UNIQUE INDEX IF NOT EXISTS groupid_username_indx on GroupUser (group_id, user_name);
CREATE INDEX IF NOT EXISTS username_indx on GroupUser (user_name);
CREATE TABLE IF NOT EXISTS GroupDNPair (group_id INTEGER, dn VARCHAR(255));
CREATE TABLE IF NOT EXISTS GroupStructure (group_id INTEGER PRIMARY KEY, path VARCHAR(1024));
`

// Seafile database schema (repos, shares, tokens, permissions)
const seafileSchema = `
CREATE TABLE IF NOT EXISTS Branch (name VARCHAR(10), repo_id CHAR(40), commit_id CHAR(40), PRIMARY KEY (repo_id, name));
CREATE TABLE IF NOT EXISTS Repo (repo_id CHAR(37) PRIMARY KEY);
CREATE TABLE IF NOT EXISTS RepoOwner (repo_id CHAR(37) PRIMARY KEY, owner_id TEXT);
CREATE INDEX IF NOT EXISTS OwnerIndex ON RepoOwner (owner_id);

CREATE TABLE IF NOT EXISTS RepoGroup (repo_id CHAR(37), group_id INTEGER, user_name TEXT, permission CHAR(15));
CREATE UNIQUE INDEX IF NOT EXISTS groupid_repoid_indx on RepoGroup (group_id, repo_id);
CREATE INDEX IF NOT EXISTS repogroup_repoid_index on RepoGroup (repo_id);
CREATE INDEX IF NOT EXISTS repogroup_username_indx on RepoGroup (user_name);
CREATE TABLE IF NOT EXISTS InnerPubRepo (repo_id CHAR(37) PRIMARY KEY, permission CHAR(15));

CREATE TABLE IF NOT EXISTS RepoUserToken (repo_id CHAR(37), email VARCHAR(255), token CHAR(41), ctime BIGINT);
CREATE UNIQUE INDEX IF NOT EXISTS repo_token_indx on RepoUserToken (repo_id, token);
CREATE INDEX IF NOT EXISTS repo_token_email_indx on RepoUserToken (email);
CREATE TABLE IF NOT EXISTS RepoTokenPeerInfo (token CHAR(41) PRIMARY KEY, peer_id CHAR(41), peer_ip VARCHAR(50), peer_name VARCHAR(255), sync_time BIGINT, client_ver VARCHAR(20));
CREATE TABLE IF NOT EXISTS RepoSyncError (token CHAR(41) PRIMARY KEY, error_time BIGINT, error_con VARCHAR(1024));

CREATE TABLE IF NOT EXISTS RepoHead (repo_id CHAR(37) PRIMARY KEY, branch_name VARCHAR(10));
CREATE TABLE IF NOT EXISTS RepoSize (repo_id CHAR(37) PRIMARY KEY, size BIGINT UNSIGNED, head_id CHAR(41));
CREATE TABLE IF NOT EXISTS RepoHistoryLimit (repo_id CHAR(37) PRIMARY KEY, days INTEGER);
CREATE TABLE IF NOT EXISTS RepoValidSince (repo_id CHAR(37) PRIMARY KEY, timestamp BIGINT);
CREATE TABLE IF NOT EXISTS WebAP (repo_id CHAR(37) PRIMARY KEY, access_property CHAR(10));

CREATE TABLE IF NOT EXISTS VirtualRepo (repo_id CHAR(36) PRIMARY KEY, origin_repo CHAR(36), path TEXT, base_commit CHAR(40));
CREATE INDEX IF NOT EXISTS virtualrepo_origin_repo_idx ON VirtualRepo (origin_repo);
CREATE TABLE IF NOT EXISTS GarbageRepos (repo_id CHAR(36) PRIMARY KEY);

CREATE TABLE IF NOT EXISTS RepoTrash (repo_id CHAR(36) PRIMARY KEY, repo_name VARCHAR(255), head_id CHAR(40), owner_id VARCHAR(255), size BIGINT UNSIGNED, org_id INTEGER, del_time BIGINT);
CREATE INDEX IF NOT EXISTS repotrash_owner_id_idx ON RepoTrash(owner_id);
CREATE INDEX IF NOT EXISTS repotrash_org_id_idx ON RepoTrash(org_id);

CREATE TABLE IF NOT EXISTS RepoFileCount (repo_id CHAR(36) PRIMARY KEY, file_count BIGINT UNSIGNED);

CREATE TABLE IF NOT EXISTS FolderUserPerm (repo_id CHAR(36) NOT NULL, path TEXT NOT NULL, permission CHAR(15), user VARCHAR(255) NOT NULL);
CREATE INDEX IF NOT EXISTS folder_user_perm_idx ON FolderUserPerm(repo_id);
CREATE TABLE IF NOT EXISTS FolderGroupPerm (repo_id CHAR(36) NOT NULL, path TEXT NOT NULL, permission CHAR(15), group_id INTEGER NOT NULL);
CREATE INDEX IF NOT EXISTS folder_group_perm_idx ON FolderGroupPerm(repo_id);
CREATE TABLE IF NOT EXISTS FolderPermTimestamp (repo_id CHAR(36) PRIMARY KEY, timestamp INTEGER);

CREATE TABLE IF NOT EXISTS WebUploadTempFiles (repo_id CHAR(40) NOT NULL, file_path TEXT NOT NULL, tmp_file_path TEXT NOT NULL);

CREATE TABLE IF NOT EXISTS RepoInfo (repo_id CHAR(36) PRIMARY KEY, name VARCHAR(255) NOT NULL, update_time INTEGER, version INTEGER, is_encrypted INTEGER, last_modifier VARCHAR(255), status INTEGER DEFAULT 0, type VARCHAR(10));
CREATE INDEX IF NOT EXISTS RepoInfoTypeIndex on RepoInfo (type);

CREATE TABLE IF NOT EXISTS RepoStorageId (repo_id CHAR(40) NOT NULL, storage_id VARCHAR(255) NOT NULL);

CREATE TABLE IF NOT EXISTS UserQuota (user VARCHAR(255) PRIMARY KEY, quota BIGINT);
CREATE TABLE IF NOT EXISTS UserShareQuota (user VARCHAR(255) PRIMARY KEY, quota BIGINT);
CREATE TABLE IF NOT EXISTS OrgQuota (org_id INTEGER PRIMARY KEY, quota BIGINT);
CREATE TABLE IF NOT EXISTS OrgUserQuota (org_id INTEGER, user VARCHAR(255), quota BIGINT, PRIMARY KEY (org_id, user));
CREATE TABLE IF NOT EXISTS RoleQuota (role VARCHAR(255) PRIMARY KEY, quota BIGINT);

CREATE TABLE IF NOT EXISTS SeafileConf (cfg_group VARCHAR(255) NOT NULL, cfg_key VARCHAR(255) NOT NULL, value VARCHAR(255), property INTEGER);

CREATE TABLE IF NOT EXISTS FileLocks (repo_id CHAR(40) NOT NULL, path TEXT NOT NULL, user_name VARCHAR(255) NOT NULL, lock_time BIGINT, expire BIGINT);
CREATE INDEX IF NOT EXISTS FileLocksIndex ON FileLocks (repo_id);
CREATE TABLE IF NOT EXISTS FileLockTimestamp (repo_id CHAR(40) PRIMARY KEY, update_time BIGINT NOT NULL);

CREATE TABLE IF NOT EXISTS SharedRepo (repo_id CHAR(37), from_email VARCHAR(255), to_email VARCHAR(255), permission CHAR(15));
CREATE INDEX IF NOT EXISTS RepoIdIndex on SharedRepo (repo_id);
CREATE INDEX IF NOT EXISTS FromEmailIndex on SharedRepo (from_email);
CREATE INDEX IF NOT EXISTS ToEmailIndex on SharedRepo (to_email);

CREATE TABLE IF NOT EXISTS OrgRepo (org_id INTEGER, repo_id CHAR(37), user VARCHAR(255));
CREATE UNIQUE INDEX IF NOT EXISTS repoid_indx on OrgRepo (repo_id);
CREATE INDEX IF NOT EXISTS orgid_repoid_indx on OrgRepo (org_id, repo_id);
CREATE INDEX IF NOT EXISTS orgrepo_orgid_user_indx on OrgRepo (org_id, user);
CREATE INDEX IF NOT EXISTS orgrepo_user_indx on OrgRepo (user);
CREATE TABLE IF NOT EXISTS OrgGroupRepo (org_id INTEGER, repo_id CHAR(37), group_id INTEGER, owner VARCHAR(255), permission CHAR(15));
CREATE UNIQUE INDEX IF NOT EXISTS orgid_groupid_repoid_indx on OrgGroupRepo (org_id, group_id, repo_id);
CREATE INDEX IF NOT EXISTS org_repoid_index on OrgGroupRepo (repo_id);
CREATE INDEX IF NOT EXISTS org_owner_indx on OrgGroupRepo (owner);
CREATE TABLE IF NOT EXISTS OrgInnerPubRepo (org_id INTEGER, repo_id CHAR(37), permission CHAR(15), PRIMARY KEY (org_id, repo_id));

CREATE TABLE IF NOT EXISTS OrgSharedRepo (org_id INTEGER, repo_id CHAR(37), from_email VARCHAR(255), to_email VARCHAR(255), permission CHAR(15));
CREATE INDEX IF NOT EXISTS OrgRepoIdIndex on OrgSharedRepo (org_id, repo_id);
CREATE INDEX IF NOT EXISTS OrgFromEmailIndex on OrgSharedRepo (from_email);
CREATE INDEX IF NOT EXISTS OrgToEmailIndex on OrgSharedRepo (to_email);
CREATE INDEX IF NOT EXISTS OrgLibIdIndex on OrgSharedRepo (repo_id);

CREATE TABLE IF NOT EXISTS GCID (repo_id CHAR(36) PRIMARY KEY, gc_id VARCHAR(10));
CREATE TABLE IF NOT EXISTS LastGCID (id INTEGER PRIMARY KEY AUTOINCREMENT, repo_id CHAR(36) NOT NULL, client_id VARCHAR(128) NOT NULL, gc_id VARCHAR(10) NOT NULL);
CREATE UNIQUE INDEX IF NOT EXISTS lastgcid_repoid_clientid_idx ON LastGCID (repo_id, client_id);

CREATE TABLE IF NOT EXISTS SystemInfo (info_key VARCHAR(256), info_value VARCHAR(1024));

-- The index on expires_at is NOT declared here. This block runs before the
-- migration, and on a database created by an earlier version the CREATE TABLE
-- above is a no-op, leaving ApiToken without an expires_at column — so
-- indexing it here would fail and abort startup. MigrateSeafileTables adds the
-- index once the column is guaranteed to exist.
CREATE TABLE IF NOT EXISTS ApiToken (token CHAR(40) PRIMARY KEY, email VARCHAR(255) NOT NULL, ctime BIGINT, expires_at BIGINT);
CREATE INDEX IF NOT EXISTS apitoken_email_idx ON ApiToken (email);
`

// CreateCcnetTables creates all ccnet tables if they don't exist.
func CreateCcnetTables(db *sql.DB) error {
	return execSchema(db, ccnetSchema)
}

// CreateSeafileTables creates all seafile tables if they don't exist.
func CreateSeafileTables(db *sql.DB) error {
	return execSchema(db, seafileSchema)
}

// MigrateSeafileTables brings a database created by an earlier version up to
// the current schema. CREATE TABLE IF NOT EXISTS silently leaves an existing
// table alone, so columns added after a release need an explicit migration.
//
// Both backfills date existing rows from *now* rather than from their real
// creation time. That is deliberate: dating them from ctime would expire every
// API token issued before the upgrade the moment the server restarts, logging
// every client out on a flag day. Giving them a full fresh interval applies
// the new policy going forward without that.
// The TTL is passed in rather than read from the option package so that
// dbutil stays free of dependencies on the rest of the server.
func MigrateSeafileTables(db *sql.DB, apiTokenTTL time.Duration) error {
	// A zero TTL reaches here whenever a caller forgets to load options first,
	// and the backfill below would then stamp every pre-existing token with
	// expires_at = now — signing out every client as a side effect of running
	// whatever command made the mistake. The callers get this right today; the
	// guard is here because the damage is silent, immediate and irreversible,
	// and the correct ordering is not visible from this function.
	if apiTokenTTL <= 0 {
		return fmt.Errorf("refusing to migrate with a non-positive API token TTL (%v): "+
			"load the file server options before the databases", apiTokenTTL)
	}

	now := time.Now().Unix()

	// The backfills run only in the start that adds the column. Every row
	// written afterwards populates it, so re-running them would be a
	// write-transaction table scan that can never match anything — on every
	// server start and every gc/token invocation, for the life of the install.
	added, err := AddColumnIfMissing(db, "ApiToken", "expires_at", "BIGINT")
	if err != nil {
		return err
	}
	if added {
		if _, err := db.Exec(
			"UPDATE ApiToken SET expires_at = ? WHERE expires_at IS NULL",
			now+int64(apiTokenTTL.Seconds())); err != nil {
			return fmt.Errorf("failed to backfill ApiToken.expires_at: %v", err)
		}
	}

	// Safe only now that expires_at is guaranteed to exist. See the note in
	// seafileSchema for why it isn't declared alongside the table.
	addIndexIfMissing(db, "apitoken_expires_idx", "ApiToken", "expires_at")

	added, err = AddColumnIfMissing(db, "RepoUserToken", "ctime", "BIGINT")
	if err != nil {
		return err
	}
	if added {
		if _, err := db.Exec(
			"UPDATE RepoUserToken SET ctime = ? WHERE ctime IS NULL", now); err != nil {
			return fmt.Errorf("failed to backfill RepoUserToken.ctime: %v", err)
		}
	}

	return nil
}

// addIndexIfMissing creates an index, tolerating one that is already there.
//
// MySQL has no portable "CREATE INDEX IF NOT EXISTS", so the duplicate case is
// absorbed rather than tested for. An index is only an optimisation, so a
// failure here is logged and the caller continues — the alternative, aborting
// startup because an index could not be created, trades a slow sweep for an
// outage.
func addIndexIfMissing(db *sql.DB, name, table, columns string) {
	stmt := fmt.Sprintf("CREATE INDEX IF NOT EXISTS %s ON %s (%s)", name, table, columns)
	if DBEngine == EngineMySQL {
		stmt = fmt.Sprintf("CREATE INDEX %s ON %s (%s)", name, table, columns)
	}
	if _, err := db.Exec(stmt); err != nil {
		log.Debugf("Index %s on %s(%s) not created: %v", name, table, columns, err)
	}
}

// AddColumnIfMissing adds a column to an existing table unless it is already
// there.
//
// Presence is probed with a zero-row SELECT rather than an engine-specific
// catalogue query — PRAGMA table_info on SQLite, information_schema on MySQL —
// so one implementation covers both engines.
//
// It reports whether the column was actually added, so a caller that needs to
// backfill the new column can do it once rather than on every start.
func AddColumnIfMissing(db *sql.DB, table, column, definition string) (bool, error) {
	probe := fmt.Sprintf("SELECT %s FROM %s LIMIT 0", column, table)
	if _, err := db.Exec(probe); err == nil {
		return false, nil
	}

	alter := fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, column, definition)
	if _, err := db.Exec(alter); err != nil {
		// Another process may have added the column between the probe and the
		// ALTER. Re-probe before reporting failure: losing that race is
		// success, not an error.
		if _, perr := db.Exec(probe); perr == nil {
			// The winner of the race owns the backfill.
			return false, nil
		}
		return false, fmt.Errorf("failed to add column %s.%s: %v", table, column, err)
	}

	log.Infof("Migrated: added column %s.%s", table, column)
	return true, nil
}

func execSchema(db *sql.DB, schema string) error {
	statements := strings.Split(schema, ";")
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
