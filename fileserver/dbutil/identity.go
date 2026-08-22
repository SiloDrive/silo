package dbutil

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"
)

// The identity split, first half: every table that keys a user by their email
// address gains an account id beside it, and every address in the database
// gets an Account to point at.
//
// Nothing reads the new columns yet. This is the additive step of docs/auth.md
// § "Identity: an account is not an email address" — the old columns still
// carry every query, so a database that has run this migration and a database
// that has not behave identically. The step that moves the reads is the one
// that cannot be half-applied.

// accountRef names one place a user is keyed by their address, and the column
// that will replace it.
//
// Only tables some Go code actually queries appear here. A table nothing reads
// gets no account id, because a column plus a backfill carried forever for a
// feature that does not exist is a cost with no matching benefit. What is left
// out, and why:
//
//	Binding, UserRole, LDAPUsers    no non-test Go code reads or writes them
//	RepoTrash, FileLocks            trash and file locking are not implemented
//	FolderUserPerm, FolderGroupPerm folder-level permissions are not implemented
//	UserShareQuota                  only UserQuota is read (quota.go:66)
//	OrgRepo, OrgSharedRepo,         reachable only when orgID >= 0, and both
//	OrgGroupRepo, OrgUserQuota,     callers of the org-aware share functions
//	OrgQuota, OrgInnerPubRepo       pass -1 (seadrive.go:227, sync_api.go:415)
//
// Whether those tables should exist at all is a question about which features
// Silo intends to have. It is a different question from this one, it is
// answered by dropping tables rather than by adding columns, and getting it
// wrong is irreversible — so it is not answered here.
//
// Group is spelled with quotes because it is a SQL keyword. option's
// SILO_GROUP_TABLE_NAME can point the share package at some other name, but
// the schema in this package only ever creates this one.
var accountRefs = []struct {
	table  string // as it must be spelled in SQL
	email  string // the column holding the address today
	column string // the account id column added beside it
}{
	{"EmailUser", "email", "account_id"},
	{"Credential", "email", "account_id"},
	{"ApiToken", "email", "account_id"},
	{"RepoUserToken", "email", "account_id"},
	{"RepoOwner", "owner_id", "account_id"},
	{"RepoGroup", "user_name", "account_id"},
	{"GroupUser", "user_name", "account_id"},
	{`"Group"`, "creator_name", "creator_account_id"},
	{"UserQuota", "user", "account_id"},
	{"SharedRepo", "from_email", "from_account_id"},
	{"SharedRepo", "to_email", "to_account_id"},
}

// migrateAccounts adds the account id columns and fills them in.
//
// It is safe to run on every start. The columns are added once, and the
// backfill is skipped unless there is something to backfill — which is also
// what keeps the additive window survivable: an account created between this
// migration and the one that moves the reads gets its Account on the next
// start rather than being silently invisible to the half-migrated half.
func migrateAccounts(db *sql.DB) error {
	added := false
	for _, ref := range accountRefs {
		a, err := AddColumnIfMissing(db, ref.table, ref.column, "BLOB")
		if err != nil {
			return err
		}
		added = added || a
	}

	if !added {
		var pending int
		if err := db.QueryRow(
			"SELECT COUNT(*) FROM EmailUser WHERE account_id IS NULL").Scan(&pending); err != nil {
			return fmt.Errorf("counting accounts to mint: %v", err)
		}
		if pending == 0 {
			return nil
		}
	}

	return backfillAccounts(db)
}

// normalizeEmail is the single spelling rule for an address.
//
// The whole string is lowercased, not just the domain. RFC 5321 says the local
// part is case-sensitive and no mail provider has behaved that way in decades,
// but the argument here is narrower than that: authmgr.ValidatePassword
// already retries a failed lookup against strings.ToLower(email), so a user
// whose row reads Dan@example.com can already sign in as dan@example.com. One
// normalised spelling is that behaviour with the second path removed rather
// than a new rule being imposed.
//
// It is done in Go and not in SQL on purpose. SQLite's lower() folds ASCII
// only, so a SQL backfill and a Go lookup would disagree on exactly the
// addresses that are hardest to notice.
func normalizeEmail(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

// backfillAccounts mints one Account per address and points every account id
// column at the right one.
func backfillAccounts(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("starting the account backfill: %v", err)
	}
	defer func() { _ = tx.Rollback() }()

	now := time.Now().Unix()

	// known maps a normalised address to the account that owns it, and is the
	// only place an address is turned into an id.
	known, err := loadAccountEmails(tx)
	if err != nil {
		return err
	}

	if err := mintFromEmailUser(tx, known, now); err != nil {
		return err
	}
	if err := mintTombstones(tx, known, now); err != nil {
		return err
	}
	if err := fillAccountColumns(tx, known); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing the account backfill: %v", err)
	}
	return nil
}

func loadAccountEmails(tx *sql.Tx) (map[string][]byte, error) {
	rows, err := tx.Query("SELECT email, account_id FROM AccountEmail")
	if err != nil {
		return nil, fmt.Errorf("reading existing addresses: %v", err)
	}
	defer func() { _ = rows.Close() }()

	known := make(map[string][]byte)
	for rows.Next() {
		var email string
		var id []byte
		if err := rows.Scan(&email, &id); err != nil {
			return nil, fmt.Errorf("reading existing addresses: %v", err)
		}
		known[email] = id
	}
	return known, rows.Err()
}

// mintFromEmailUser gives every user an Account, and every user with a real
// password an AccountPassword.
//
// Two EmailUser rows whose addresses differ only in case are refused rather
// than merged. Merging them would fuse two humans' libraries into one account
// — precisely the failure the split exists to prevent, created by the split
// itself — and no automatic choice between them is defensible. Refusing
// aborts the migration with both addresses named, which is a job for an
// operator and takes one UPDATE.
func mintFromEmailUser(tx *sql.Tx, known map[string][]byte, now int64) error {
	type user struct {
		email    string // as stored
		passwd   string
		isStaff  bool
		isActive bool
		ctime    sql.NullInt64
	}

	rows, err := tx.Query("SELECT email, passwd, is_staff, is_active, ctime FROM EmailUser")
	if err != nil {
		return fmt.Errorf("reading users: %v", err)
	}
	defer func() { _ = rows.Close() }()

	var users []user
	seen := make(map[string]string) // normalised -> the spelling that claimed it
	for rows.Next() {
		var u user
		var email sql.NullString
		var passwd sql.NullString
		if err := rows.Scan(&email, &passwd, &u.isStaff, &u.isActive, &u.ctime); err != nil {
			return fmt.Errorf("reading users: %v", err)
		}
		u.email = email.String
		u.passwd = passwd.String
		if u.email == "" {
			continue
		}

		norm := normalizeEmail(u.email)
		if first, dup := seen[norm]; dup {
			return fmt.Errorf("cannot split identity: EmailUser holds both %q and %q, "+
				"which are the same address once normalised. Merging them would join two "+
				"accounts' libraries. Delete or rename one, then start again", first, u.email)
		}
		seen[norm] = u.email
		users = append(users, u)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("reading users: %v", err)
	}

	for _, u := range users {
		norm := normalizeEmail(u.email)
		if _, ok := known[norm]; ok {
			continue
		}

		ctime := u.ctime.Int64
		if ctime == 0 {
			ctime = now
		}
		id, err := insertAccount(tx, u.isActive, u.isStaff, ctime)
		if err != nil {
			return err
		}
		if err := insertAccountEmail(tx, norm, id); err != nil {
			return err
		}
		known[norm] = id

		// "!" is Seafile's sentinel for an account that cannot sign in with a
		// password. It gets no AccountPassword row, which is the same thing
		// said in a way no comparison can get wrong.
		if u.passwd != "" && u.passwd != "!" {
			if _, err := tx.Exec(
				"INSERT INTO AccountPassword (account_id, hash, changed_at) VALUES (?, ?, ?)",
				id, u.passwd, now); err != nil {
				return fmt.Errorf("recording the password for %s: %v", norm, err)
			}
		}
	}
	return nil
}

// mintTombstones gives an Account to every address that appears in a table but
// has no user row — a share granted to someone since deleted, a library whose
// owner is gone.
//
// The alternative is a NULL account id, and a NULL is worse in two directions:
// the join that replaces the address silently drops the row, and the event log
// docs/plans/events.md describes has nothing to name as the actor. A tombstone
// is inactive and has no password, so it cannot be signed in to. If someone
// later registers that address they should inherit this account rather than
// collide with it — which is the enrolment work's problem, and is the right
// answer, because inheriting reunites the orphaned shares with the person.
func mintTombstones(tx *sql.Tx, known map[string][]byte, now int64) error {
	for _, ref := range accountRefs {
		if ref.table == "EmailUser" {
			continue
		}

		q := fmt.Sprintf("SELECT DISTINCT %s FROM %s WHERE %s IS NOT NULL AND %s <> ''",
			ref.email, ref.table, ref.email, ref.email)
		rows, err := tx.Query(q)
		if err != nil {
			return fmt.Errorf("reading %s.%s: %v", ref.table, ref.email, err)
		}

		var orphans []string
		for rows.Next() {
			var addr string
			if err := rows.Scan(&addr); err != nil {
				_ = rows.Close()
				return fmt.Errorf("reading %s.%s: %v", ref.table, ref.email, err)
			}
			if norm := normalizeEmail(addr); norm != "" {
				if _, ok := known[norm]; !ok {
					orphans = append(orphans, norm)
				}
			}
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return fmt.Errorf("reading %s.%s: %v", ref.table, ref.email, err)
		}

		for _, norm := range orphans {
			if _, ok := known[norm]; ok {
				continue // an earlier table in this same loop already minted it
			}
			id, err := insertAccount(tx, false, false, now)
			if err != nil {
				return err
			}
			if err := insertAccountEmail(tx, norm, id); err != nil {
				return err
			}
			known[norm] = id
			log.Warnf("Identity split: %s.%s names %s, which has no user row. "+
				"Minted an inactive account so the reference survives.",
				ref.table, ref.email, norm)
		}
	}
	return nil
}

// fillAccountColumns points every account id column at the account its address
// resolves to. Rows already carrying an id are left alone, so a re-run costs a
// scan and changes nothing.
func fillAccountColumns(tx *sql.Tx, known map[string][]byte) error {
	for _, ref := range accountRefs {
		q := fmt.Sprintf("SELECT DISTINCT %s FROM %s WHERE %s IS NOT NULL AND %s <> '' AND %s IS NULL",
			ref.email, ref.table, ref.email, ref.email, ref.column)
		rows, err := tx.Query(q)
		if err != nil {
			return fmt.Errorf("reading %s.%s: %v", ref.table, ref.email, err)
		}

		var addrs []string
		for rows.Next() {
			var addr string
			if err := rows.Scan(&addr); err != nil {
				_ = rows.Close()
				return fmt.Errorf("reading %s.%s: %v", ref.table, ref.email, err)
			}
			addrs = append(addrs, addr)
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return fmt.Errorf("reading %s.%s: %v", ref.table, ref.email, err)
		}

		update := fmt.Sprintf("UPDATE %s SET %s = ? WHERE %s = ? AND %s IS NULL",
			ref.table, ref.column, ref.email, ref.column)
		for _, addr := range addrs {
			id, ok := known[normalizeEmail(addr)]
			if !ok {
				// Every address reaching here was either minted above or
				// already present, so this cannot happen — and if it somehow
				// does, leaving the column NULL would hide it until the reads
				// move and a permission silently disappeared.
				return fmt.Errorf("no account for %s.%s = %q after minting",
					ref.table, ref.email, addr)
			}
			if _, err := tx.Exec(update, id, addr); err != nil {
				return fmt.Errorf("filling %s.%s: %v", ref.table, ref.column, err)
			}
		}
	}
	return nil
}

func insertAccount(tx *sql.Tx, isActive, isStaff bool, ctime int64) ([]byte, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return nil, fmt.Errorf("minting an account id: %v", err)
	}
	raw := id[:]
	if _, err := tx.Exec(
		"INSERT INTO Account (id, display, is_active, is_staff, ctime) VALUES (?, NULL, ?, ?, ?)",
		raw, isActive, isStaff, ctime); err != nil {
		return nil, fmt.Errorf("creating an account: %v", err)
	}
	return raw, nil
}

func insertAccountEmail(tx *sql.Tx, email string, accountID []byte) error {
	if _, err := tx.Exec(
		"INSERT INTO AccountEmail (email, account_id, is_primary, verified_at) VALUES (?, ?, 1, NULL)",
		email, accountID); err != nil {
		return fmt.Errorf("claiming the address %s: %v", email, err)
	}
	return nil
}
