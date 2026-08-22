package dbutil

import (
	"bytes"
	"database/sql"
	"testing"
	"time"
)

const testTTL = 30 * 24 * time.Hour

func addUser(t *testing.T, db *sql.DB, email, passwd string, active bool) {
	t.Helper()
	if _, err := db.Exec(
		"INSERT INTO EmailUser (email, passwd, is_staff, is_active, ctime) VALUES (?, ?, 0, ?, ?)",
		email, passwd, active, time.Now().Unix()); err != nil {
		t.Fatalf("failed to add user %s: %v", email, err)
	}
}

func accountFor(t *testing.T, db *sql.DB, email string) []byte {
	t.Helper()
	var id []byte
	if err := db.QueryRow(
		"SELECT account_id FROM AccountEmail WHERE email = ?", email).Scan(&id); err != nil {
		t.Fatalf("no account for %s: %v", email, err)
	}
	return id
}

func count(t *testing.T, db *sql.DB, query string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

// The backfill's whole job: every user gets an account, and every table that
// names them by address gains a column pointing at it.
func TestBackfillMintsAccounts(t *testing.T) {
	pair := schemaTestDB(t)
	db := pair.Write

	addUser(t, db, "dan@example.com", "pbkdf2$fake", true)
	addUser(t, db, "sam@example.com", "!", true)

	if _, err := db.Exec(
		"INSERT INTO SharedRepo (repo_id, from_email, to_email, permission) VALUES (?, ?, ?, ?)",
		"repo-1", "dan@example.com", "sam@example.com", "rw"); err != nil {
		t.Fatalf("failed to add share: %v", err)
	}
	if _, err := db.Exec(
		"INSERT INTO RepoOwner (repo_id, owner_id) VALUES (?, ?)",
		"repo-1", "dan@example.com"); err != nil {
		t.Fatalf("failed to add owner: %v", err)
	}

	if err := MigrateSiloTables(db, testTTL); err != nil {
		t.Fatalf("migration failed: %v", err)
	}

	dan := accountFor(t, db, "dan@example.com")
	sam := accountFor(t, db, "sam@example.com")
	if bytes.Equal(dan, sam) {
		t.Fatal("two users share one account")
	}
	if len(dan) != 16 {
		t.Errorf("account id is %d bytes, want a 16-byte UUID", len(dan))
	}

	// UUIDv7 leads with a millisecond timestamp, which is the whole reason for
	// choosing it: a v4 would put these two nowhere near each other.
	if dan[6]>>4 != 7 {
		t.Errorf("account id version nibble is %d, want 7", dan[6]>>4)
	}

	// A password sentinel is not a password.
	if n := count(t, db, "SELECT COUNT(*) FROM AccountPassword WHERE account_id = ?", dan); n != 1 {
		t.Errorf("dan has %d password rows, want 1", n)
	}
	if n := count(t, db, "SELECT COUNT(*) FROM AccountPassword WHERE account_id = ?", sam); n != 0 {
		t.Errorf("sam's \"!\" password became a real row")
	}

	var from, to, owner []byte
	if err := db.QueryRow(
		"SELECT from_account_id, to_account_id FROM SharedRepo WHERE repo_id = ?",
		"repo-1").Scan(&from, &to); err != nil {
		t.Fatalf("reading the share: %v", err)
	}
	if !bytes.Equal(from, dan) || !bytes.Equal(to, sam) {
		t.Error("the share's two account ids do not match its two addresses")
	}

	if err := db.QueryRow(
		"SELECT account_id FROM RepoOwner WHERE repo_id = ?", "repo-1").Scan(&owner); err != nil {
		t.Fatalf("reading the owner: %v", err)
	}
	if !bytes.Equal(owner, dan) {
		t.Error("RepoOwner.account_id does not match its owner_id")
	}

	// The old columns still carry every query. That is what makes this half
	// of the split safe to land on its own.
	if n := count(t, db,
		"SELECT COUNT(*) FROM SharedRepo WHERE from_email = ?", "dan@example.com"); n != 1 {
		t.Error("the migration disturbed the address columns")
	}
}

// An address in a share whose user was deleted years ago still has to resolve
// to something, or the join that replaces it drops the row.
func TestBackfillTombstonesOrphans(t *testing.T) {
	pair := schemaTestDB(t)
	db := pair.Write

	addUser(t, db, "dan@example.com", "pbkdf2$fake", true)
	if _, err := db.Exec(
		"INSERT INTO SharedRepo (repo_id, from_email, to_email, permission) VALUES (?, ?, ?, ?)",
		"repo-1", "dan@example.com", "ghost@example.com", "r"); err != nil {
		t.Fatalf("failed to add share: %v", err)
	}

	if err := MigrateSiloTables(db, testTTL); err != nil {
		t.Fatalf("migration failed: %v", err)
	}

	ghost := accountFor(t, db, "ghost@example.com")

	var active bool
	if err := db.QueryRow("SELECT is_active FROM Account WHERE id = ?", ghost).Scan(&active); err != nil {
		t.Fatalf("reading the tombstone: %v", err)
	}
	if active {
		t.Error("a tombstone account is active, so a deleted user could be signed in to")
	}
	if n := count(t, db, "SELECT COUNT(*) FROM AccountPassword WHERE account_id = ?", ghost); n != 0 {
		t.Error("a tombstone account has a password")
	}

	var to []byte
	if err := db.QueryRow(
		"SELECT to_account_id FROM SharedRepo WHERE repo_id = ?", "repo-1").Scan(&to); err != nil {
		t.Fatalf("reading the share: %v", err)
	}
	if !bytes.Equal(to, ghost) {
		t.Error("the orphaned share did not get the tombstone's id")
	}
}

// Two addresses that differ only in case are the failure the split exists to
// prevent, arriving through the split itself. Refusing is the only defensible
// answer: merging joins two people's libraries.
func TestBackfillRefusesCaseCollision(t *testing.T) {
	pair := schemaTestDB(t)
	db := pair.Write

	addUser(t, db, "dan@example.com", "pbkdf2$fake", true)
	addUser(t, db, "Dan@example.com", "pbkdf2$other", true)

	err := MigrateSiloTables(db, testTTL)
	if err == nil {
		t.Fatal("the migration merged two addresses that differ only in case")
	}
	if n := count(t, db, "SELECT COUNT(*) FROM Account"); n != 0 {
		t.Errorf("the failed migration left %d accounts behind", n)
	}
}

// A mixed-case address stored by an older version normalises to the spelling
// authmgr.ValidatePassword already falls back to.
func TestBackfillNormalisesCase(t *testing.T) {
	pair := schemaTestDB(t)
	db := pair.Write

	addUser(t, db, "Dan@Example.COM", "pbkdf2$fake", true)
	if err := MigrateSiloTables(db, testTTL); err != nil {
		t.Fatalf("migration failed: %v", err)
	}

	id := accountFor(t, db, "dan@example.com")

	// The user's own row still points at their account despite the spelling.
	var got []byte
	if err := db.QueryRow(
		"SELECT account_id FROM EmailUser WHERE email = ?", "Dan@Example.COM").Scan(&got); err != nil {
		t.Fatalf("reading the user: %v", err)
	}
	if !bytes.Equal(got, id) {
		t.Error("EmailUser.account_id does not point at the account minted for its address")
	}
}

// The migration runs on every start. Running it again must cost a scan and
// change nothing — and a user created between two starts must still get an
// account, which is what keeps the additive window survivable.
func TestBackfillIsIdempotentAndCatchesUp(t *testing.T) {
	pair := schemaTestDB(t)
	db := pair.Write

	addUser(t, db, "dan@example.com", "pbkdf2$fake", true)
	if err := MigrateSiloTables(db, testTTL); err != nil {
		t.Fatalf("first migration failed: %v", err)
	}
	dan := accountFor(t, db, "dan@example.com")

	if err := MigrateSiloTables(db, testTTL); err != nil {
		t.Fatalf("second migration failed: %v", err)
	}
	if n := count(t, db, "SELECT COUNT(*) FROM Account"); n != 1 {
		t.Errorf("re-running the migration left %d accounts, want 1", n)
	}
	if again := accountFor(t, db, "dan@example.com"); !bytes.Equal(again, dan) {
		t.Error("re-running the migration re-minted an existing account")
	}

	// A user added after the columns already exist.
	addUser(t, db, "sam@example.com", "pbkdf2$fake", true)
	if err := MigrateSiloTables(db, testTTL); err != nil {
		t.Fatalf("third migration failed: %v", err)
	}
	if n := count(t, db, "SELECT COUNT(*) FROM Account"); n != 2 {
		t.Errorf("a user created after the columns existed got no account: %d accounts", n)
	}
}

// A database from before the columns existed is the case the migration is for.
func TestMigrateAddsColumnsToAnOlderDatabase(t *testing.T) {
	pair := schemaTestDB(t)
	db := pair.Write

	if _, err := db.Exec("DROP TABLE RepoOwner"); err != nil {
		t.Fatalf("failed to drop RepoOwner: %v", err)
	}
	if _, err := db.Exec(
		"CREATE TABLE RepoOwner (repo_id CHAR(37) PRIMARY KEY, owner_id TEXT)"); err != nil {
		t.Fatalf("failed to recreate RepoOwner: %v", err)
	}
	if _, err := db.Exec(
		"INSERT INTO RepoOwner (repo_id, owner_id) VALUES (?, ?)", "repo-1", "dan@example.com"); err != nil {
		t.Fatalf("failed to add owner: %v", err)
	}
	addUser(t, db, "dan@example.com", "pbkdf2$fake", true)

	if err := MigrateSiloTables(db, testTTL); err != nil {
		t.Fatalf("migration failed: %v", err)
	}

	var owner []byte
	if err := db.QueryRow(
		"SELECT account_id FROM RepoOwner WHERE repo_id = ?", "repo-1").Scan(&owner); err != nil {
		t.Fatalf("account_id was not added to an older RepoOwner: %v", err)
	}
	if !bytes.Equal(owner, accountFor(t, db, "dan@example.com")) {
		t.Error("the added column was not backfilled")
	}
}

// Two primary addresses on one account is how "what is this account's email"
// starts depending on row order.
func TestAccountEmailAllowsOnePrimary(t *testing.T) {
	pair := schemaTestDB(t)
	db := pair.Write

	addUser(t, db, "dan@example.com", "pbkdf2$fake", true)
	if err := MigrateSiloTables(db, testTTL); err != nil {
		t.Fatalf("migration failed: %v", err)
	}
	dan := accountFor(t, db, "dan@example.com")

	// A second address is fine — that is the point of the table.
	if _, err := db.Exec(
		"INSERT INTO AccountEmail (email, account_id, is_primary) VALUES (?, ?, 0)",
		"dan@work.example.com", dan); err != nil {
		t.Fatalf("an account could not hold a second address: %v", err)
	}

	// A second *primary* is not.
	if _, err := db.Exec(
		"INSERT INTO AccountEmail (email, account_id, is_primary) VALUES (?, ?, 1)",
		"dan@other.example.com", dan); err == nil {
		t.Error("an account accepted a second primary address")
	}
}
