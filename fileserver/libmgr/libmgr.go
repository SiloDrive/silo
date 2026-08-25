// Package libmgr manages library objects and file operations in libraries.
package libmgr

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	// Change to non-blank imports when use
	"github.com/dkam/silo/fileserver/account"
	"github.com/dkam/silo/fileserver/dbutil"
	"github.com/dkam/silo/fileserver/objmgr"
	"github.com/dkam/silo/fileserver/option"
	storefmt "github.com/dkam/silo/store"
	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"
)

// Library status
const (
	LibraryStatusNormal = iota
	LibraryStatusReadOnly
	NLibraryStatus
)

// Library contains information about a library.
type Library struct {
	ID string
	// Name, LastModifier and LastModificationTime come from the catalog, not
	// from the head commit. See the LibraryInfo comment in dbutil/schema.go for
	// why that inversion is forced rather than tidier: they are facts the
	// server observed, and on an E2EE library it could not read them out of a
	// commit even if it wanted to.
	Name                 string
	LastModifier         string
	LastModificationTime int64
	// HeadCommitID and RootID are one fact in two columns of one row, so they
	// move together and are read together.
	HeadCommitID string
	RootID       string
	IsCorrupted  bool

	// Set when library is virtual
	VirtualInfo *VLibraryInfo

	// ID for fs and block store
	StoreID string

	// store is this library's objects, opened at most once. See Store.
	store *objmgr.Store

	// Format is how this library's bytes are made — the chunker parameters
	// every client has to agree with, and whether the content is end-to-end
	// encrypted. Frozen at creation; see format.go.
	Format Format
}

// VLibraryInfo contains virtual library information.
type VLibraryInfo struct {
	LibraryID       string
	OriginLibraryID string
	Path            string
	BaseCommitID    string
}

var readDB *sql.DB  // read handle
var writeDB *sql.DB // write handle
var dataDir string  // where object stores live

// Init initialize status of libmgr package.
//
// dataDir is here because creating a library now writes objects as well as
// rows: a store-v2 library's first commit and its empty root are real objects
// that have to exist before the branch can point at them.
func Init(read, write *sql.DB, dir string) {
	readDB = read
	writeDB = write
	dataDir = dir
}

// OpenStore opens a library's store-v2 objects from the server's side.
//
// Server's side means without a content key, always — see Format.ServerParams.
// For a plain library that is the whole story and the Store can do everything.
// For an E2EE one the Store is permanently in objmgr's third mode: it moves
// bytes, verifies ids and reads public sections, and refuses anything needing
// the key rather than half-doing it.
//
// storeID rather than the library id because a virtual library's objects live
// in its origin's store.
func OpenStore(storeID string, f Format) (*objmgr.Store, error) {
	params, err := f.ServerParams()
	if err != nil {
		return nil, err
	}
	return objmgr.New(objmgr.Config{
		DataDir: dataDir,
		StoreID: storeID,
		E2EE:    f.E2EE,
		Params:  params,
	})
}

// Store is this library's objects, opened once per loaded Library.
//
// Every caller wanted the same two fields — StoreID and Format — and opening
// from those by hand at each site made the "storeID rather than the library
// id" rule above something six call sites had to remember, where writing
// library.ID instead would have looked entirely reasonable. Now it is remembered
// in one place.
//
// Opened once because a Store is not free: it builds an object store per
// object type and each of those mkdirs its directories, so a request that
// resolved a path, read the file and wrote a commit paid for four or five
// identical handles. A Library is a per-request value — nothing caches one, and
// GetWithReason mints a fresh one per call — so the handle lives exactly as
// long as the request that asked for it, and is used by the one goroutine
// serving it.
func (library *Library) Store() (*objmgr.Store, error) {
	if library.store != nil {
		return library.store, nil
	}
	st, err := OpenStore(library.StoreID, library.Format)
	if err != nil {
		return nil, err
	}
	library.store = st
	return st, nil
}

// A library can fail to load for four unrelated reasons, and only one of them is
// a statement about the request. Collapsing them — which returning a bare nil
// does — is how a server that has lost an object comes to answer 404, and a
// 404 tells a sync client the library was deleted and its local copy should
// go with it. The copy it would delete is the one that could have restored
// the object.
var (
	// ErrLibraryNotFound means there is no such library. This is the only one of
	// the four that a client may act on by forgetting the library.
	ErrLibraryNotFound = errors.New("no such library")

	// ErrLibraryCorrupted means the library exists but the server cannot read its
	// head: an empty commit id in Branch, or a commit object the store has
	// lost. Nothing has been deleted, and the client's copy may be the only
	// intact one left.
	ErrLibraryCorrupted = errors.New("library storage is damaged")

	// ErrLibraryUnavailable means the database could not be read, so nothing is
	// known about the library either way. Expected to clear on its own.
	ErrLibraryUnavailable = errors.New("library metadata is unavailable")
)

// Get returns Library object by library ID, or nil if it could not be read for any
// of the four reasons above.
//
// Anything answering a client should call GetWithReason instead: which failure
// happened decides what the client is told, and this signature throws that
// away.
func Get(id string) *Library {
	library, _ := GetWithReason(id)
	return library
}

// librarySelect is the one row shape both loaders read.
//
// It lives in a constant because the column list and the Scan that consumes it
// have to agree, and two copies of a pair that has to agree is one copy too
// many — the format columns were added to one of them first, and the second
// loader silently returned libraries with a zeroed chunker until it wasn't.
const librarySelect = `SELECT r.library_id, b.commit_id, b.root_id, v.origin_library, v.path, v.base_commit, ` +
	`r.chunker, r.chunk_min, r.chunk_target, r.chunk_max, r.chunk_norm, r.e2ee, ` +
	`i.name, i.update_time, i.last_modifier FROM ` +
	`Library r LEFT JOIN Branch b ON r.library_id = b.library_id ` +
	`LEFT JOIN VirtualLibrary v ON r.library_id = v.library_id ` +
	`LEFT JOIN LibraryInfo i ON r.library_id = i.library_id ` +
	`WHERE r.library_id = ? AND b.name = 'master'`

// scanLibraryRow reads one librarySelect row, including the virtual-library columns and
// the store id they decide.
func scanLibraryRow(rows *sql.Rows, id string, library *Library) error {
	var originLibraryID, path, baseCommitID sql.NullString
	// Nullable because the joins are outer ones and because these columns
	// arrived after the tables did. A library with no LibraryInfo row is not an
	// error to this loader: it has no display name yet, which is a different
	// thing from having no head.
	var rootID, name, lastModifier sql.NullString
	var updateTime sql.NullInt64
	if err := rows.Scan(&library.ID, &library.HeadCommitID, &rootID, &originLibraryID, &path, &baseCommitID,
		&library.Format.Chunker, &library.Format.MinSize, &library.Format.TargetSize,
		&library.Format.MaxSize, &library.Format.Normalization, &library.Format.E2EE,
		&name, &updateTime, &lastModifier); err != nil {
		return err
	}
	library.RootID = rootID.String
	library.Name = name.String
	library.LastModifier = lastModifier.String
	library.LastModificationTime = updateTime.Int64

	if !originLibraryID.Valid {
		library.StoreID = library.ID
		return nil
	}
	library.VirtualInfo = &VLibraryInfo{LibraryID: id, OriginLibraryID: originLibraryID.String}
	library.StoreID = originLibraryID.String
	if path.Valid {
		library.VirtualInfo.Path = path.String
	}
	if baseCommitID.Valid {
		library.VirtualInfo.BaseCommitID = baseCommitID.String
	}
	return nil
}

// GetWithReason returns the library, or the reason it could not be returned —
// one of ErrLibraryNotFound, ErrLibraryCorrupted or ErrLibraryUnavailable, wrapped with
// the detail. Faults are logged here, once per library per libraryFaultInterval, so
// callers should not log again.
func GetWithReason(id string) (*Library, error) {
	ctx, cancel := option.WithDBTimeout(context.Background())
	defer cancel()
	stmt, err := readDB.PrepareContext(ctx, librarySelect)
	if err != nil {
		return nil, fault(id, ErrLibraryUnavailable, "failed to prepare sql %s: %v", librarySelect, err)
	}
	defer func() { _ = stmt.Close() }()

	rows, err := stmt.QueryContext(ctx, id)
	if err != nil {
		return nil, fault(id, ErrLibraryUnavailable, "failed to query sql: %v", err)
	}
	defer func() { _ = rows.Close() }()

	library := new(Library)

	if rows.Next() {
		if err := scanLibraryRow(rows, id, library); err != nil {
			return nil, fault(id, ErrLibraryUnavailable, "failed to scan sql rows: %v", err)
		}
	} else if err := rows.Err(); err != nil {
		// No row, but the iteration itself failed — that is the database
		// speaking, not an answer about whether the library exists.
		return nil, fault(id, ErrLibraryUnavailable, "failed to read sql rows: %v", err)
	} else {
		clearFaults(id)
		return nil, ErrLibraryNotFound
	}

	if library.HeadCommitID == "" {
		return nil, fault(id, ErrLibraryCorrupted, "Branch holds no head commit")
	}

	// A library whose stored parameters do not describe a chunker cannot be
	// read by anybody, so it is corrupted rather than merely unusual.
	if err := library.Format.Validate(); err != nil {
		return nil, fault(id, ErrLibraryCorrupted, "%v", err)
	}

	if err := checkHeadPresent(library); err != nil {
		return nil, fault(id, ErrLibraryCorrupted, "%v", err)
	}
	clearFaults(id)

	return library, nil
}

// checkHeadPresent verifies a store-v2 library's head commit object is
// actually there.
//
// Nothing else does any more, and that is the point. The head used to be
// decoded on every load to fill in fields that no longer exist, so a missing
// object surfaced as corruption for free; the head is never read here now,
// because there is
// nothing in it this function needs. Without this check a library whose head
// object had gone would load clean and fail later, somewhere further from the
// cause — and the reason that matters is the one on GetWithReason's errors: a
// library that answers "not found" tells a sync client to delete its local
// copy, which is the copy that could have restored the object.
//
// A stat, not a read — and the Store it stats through is library.Store, so the
// handle this check needs is the one the rest of the request was going to open
// anyway. What the check itself adds to a load is one lookup.
func checkHeadPresent(library *Library) error {
	id, err := storefmt.ParseID(library.HeadCommitID)
	if err != nil {
		return fmt.Errorf("head commit id %q is unreadable: %w", library.HeadCommitID, err)
	}
	st, err := library.Store()
	if err != nil {
		return fmt.Errorf("failed to open store: %w", err)
	}
	ok, err := st.HasObject(id)
	if err != nil {
		return fmt.Errorf("failed to look for head commit %s: %w", library.HeadCommitID, err)
	}
	if !ok {
		return fmt.Errorf("head commit %s is missing from the object store", library.HeadCommitID)
	}
	return nil
}

// StatusFor maps a GetWithReason failure onto the status and body a client
// should see. It lives beside the errors it maps because both packages that
// serve libraries over HTTP need it, and the one decision that matters — that only
// a missing row is a 404 — must not exist in two copies that can drift.
//
// The 500 body says the library still exists on purpose: it is the only thing
// standing between a damaged server and a client that decides to tidy up.
func StatusFor(err error) (int, string) {
	switch {
	case err == nil:
		return http.StatusOK, ""
	case errors.Is(err, ErrLibraryNotFound):
		return http.StatusNotFound, "Library not found"
	case errors.Is(err, ErrLibraryUnavailable):
		return http.StatusServiceUnavailable, "Library metadata is temporarily unavailable; retry"
	default:
		return http.StatusInternalServerError,
			"Library exists but its storage is damaged on the server; do not delete your copy"
	}
}

// A fault on a library is persistent — a lost object stays lost — and every
// retrying client rediscovers it. One client retrying produced twelve
// identical lines in thirty-four seconds, which is enough to bury the first
// occurrence in the log and to turn one server fault into twelve reports in
// whatever the error hook forwards to. Each (library, kind) is reported once,
// then held for libraryFaultInterval.
const libraryFaultInterval = 5 * time.Minute

type faultKey struct {
	libraryID string
	kind      error
}

var libraryFaults = struct {
	sync.Mutex
	lastLogged map[faultKey]time.Time
}{lastLogged: make(map[faultKey]time.Time)}

// faultKinds is every kind clearFaults has to forget. Keep it in step with the
// sentinels above.
var faultKinds = []error{ErrLibraryNotFound, ErrLibraryCorrupted, ErrLibraryUnavailable}

// fault wraps the detail as kind, logs it if it has not been logged recently,
// and returns the wrapped error.
func fault(libraryID string, kind error, format string, args ...interface{}) error {
	err := fmt.Errorf("%w: library %s: %s", kind, libraryID, fmt.Sprintf(format, args...))
	if firstReport(libraryID, kind) {
		log.Error(err)
	}
	return err
}

func firstReport(libraryID string, kind error) bool {
	now := time.Now()

	libraryFaults.Lock()
	defer libraryFaults.Unlock()

	key := faultKey{libraryID, kind}
	if last, ok := libraryFaults.lastLogged[key]; ok && now.Sub(last) < libraryFaultInterval {
		return false
	}
	// Entries are only ever added by a fault and dropped by a repair, so the
	// map tracks broken libraries. Sweeping the stale ones here keeps a library
	// that was deleted rather than repaired from being remembered forever.
	for k, last := range libraryFaults.lastLogged {
		if now.Sub(last) >= libraryFaultInterval {
			delete(libraryFaults.lastLogged, k)
		}
	}
	libraryFaults.lastLogged[key] = now
	return true
}

// clearFaults forgets a library's faults, so that a recurrence after a repair
// is reported again instead of being suppressed as a repeat.
func clearFaults(libraryID string) {
	libraryFaults.Lock()
	defer libraryFaults.Unlock()
	for _, kind := range faultKinds {
		delete(libraryFaults.lastLogged, faultKey{libraryID, kind})
	}
}

// GetVirtualLibraryInfo return virtual library info by library id.
func GetVirtualLibraryInfo(libraryID string) (*VLibraryInfo, error) {
	sqlStr := "SELECT library_id, origin_library, path, base_commit FROM VirtualLibrary WHERE library_id = ?"
	vLibraryInfo := new(VLibraryInfo)

	ctx, cancel := option.WithDBTimeout(context.Background())
	defer cancel()
	row := readDB.QueryRowContext(ctx, sqlStr, libraryID)
	if err := row.Scan(&vLibraryInfo.LibraryID, &vLibraryInfo.OriginLibraryID, &vLibraryInfo.Path, &vLibraryInfo.BaseCommitID); err != nil {
		if err != sql.ErrNoRows {
			return nil, err
		}
		return nil, nil
	}
	return vLibraryInfo, nil
}

// RecordHeadMove records what the server witnessed when a library's head
// moved: who was authenticated, and when.
//
// It replaces a function that copied a name, a timestamp, a format version and
// an encryption flag out of the new head commit into LibraryInfo, keeping the
// commit as the source of truth and this table as a mirror. That is inverted
// now — see the LibraryInfo comment in dbutil/schema.go — and the two fields left
// are the two the server establishes itself rather than reads.
//
// modifier is the authenticated account, not a name a request supplied, and
// when is the server's clock. On an E2EE library both may differ from the
// author and created_at sealed inside the commit, and that is the intended
// relationship rather than a drift to reconcile: one pair is what the members
// say happened and the other is what the server saw.
//
// **It takes the caller's transaction**, and that is not a convenience. The
// head move and this record are one event, so they commit together or not at
// all. Recorded afterwards, a failure here arrives when the head has already
// moved, and the only honest thing left to do with it is retry a commit that
// already landed — which is precisely what a caller reading the error as lost
// contention would do.
//
// The name is not touched. A rename is its own operation, and rewriting the
// name here on every write is how a rename gets quietly undone by the next
// one. A library with no row yet gets one with an empty name rather than an
// error: this is display metadata, and refusing a write because of it would
// be the same misjudgement as reporting it late.
func RecordHeadMove(ctx context.Context, tx *sql.Tx, libraryID, modifier string, when int64) error {
	if when == 0 {
		when = time.Now().Unix()
	}
	_, err := tx.ExecContext(ctx,
		"INSERT INTO LibraryInfo (library_id, name, update_time, last_modifier) VALUES (?, '', ?, ?) "+
			"ON CONFLICT(library_id) DO UPDATE SET update_time=excluded.update_time, "+
			"last_modifier=excluded.last_modifier",
		libraryID, when, modifier)
	return err
}

// SetLibraryName renames a library.
//
// It is a single UPDATE and mints no commit. The old spelling wrote a commit
// whose only change was the name it carried, which moved the head for a change
// that was not in the tree — every client saw a new head and diffed two
// identical roots. Under E2EE the server could not write that commit at all.
func SetLibraryName(libraryID, name string) error {
	ctx, cancel := option.WithDBTimeout(context.Background())
	defer cancel()
	res, err := writeDB.ExecContext(ctx,
		"UPDATE LibraryInfo SET name=? WHERE library_id=?", name, libraryID)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return fmt.Errorf("library %s has no LibraryInfo row", libraryID)
	}
	return nil
}

// GetLibraryOwner get the owner of library.
func GetLibraryOwner(libraryID string) (account.ID, error) {
	var owner account.ID
	sqlStr := "SELECT account_id FROM LibraryOwner WHERE library_id=?"

	ctx, cancel := option.WithDBTimeout(context.Background())
	defer cancel()
	row := readDB.QueryRowContext(ctx, sqlStr, libraryID)
	if err := row.Scan(&owner); err != nil {
		if err != sql.ErrNoRows {
			return account.Zero, err
		}
	}

	return owner, nil
}

func GetCurrentGCID(libraryID string) (string, error) {
	sqlStr := "SELECT gc_id FROM GCID WHERE library_id = ?"

	var gcID sql.NullString
	ctx, cancel := option.WithDBTimeout(context.Background())
	defer cancel()
	row := readDB.QueryRowContext(ctx, sqlStr, libraryID)
	if err := row.Scan(&gcID); err != nil {
		if err != sql.ErrNoRows {
			return "", err
		}
	}

	return gcID.String, nil
}

// DeleteLibraryTokensByAccount revokes every sync token an account holds, across
// all libraries, stopping all of their devices from syncing. Returns the count.
func DeleteLibraryTokensByAccount(id account.ID) (int64, error) {
	ctx, cancel := option.WithDBTimeout(context.Background())
	defer cancel()

	res, err := writeDB.ExecContext(ctx,
		"DELETE FROM LibraryUserToken WHERE account_id = ?", id)
	if err != nil {
		return 0, fmt.Errorf("failed to delete library tokens: %v", err)
	}
	notify(OnTokensRevoked, id)

	return dbutil.RowsAffected(res), nil
}

// DeleteLibraryToken removes a specific sync token.
func DeleteLibraryToken(libraryID, token string, id account.ID) error {
	sqlStr := "DELETE FROM LibraryUserToken WHERE library_id = ? AND token = ? AND account_id = ?"
	ctx, cancel := option.WithDBTimeout(context.Background())
	defer cancel()
	if _, err := writeDB.ExecContext(ctx, sqlStr, libraryID, token, id); err != nil {
		return fmt.Errorf("failed to delete library token: %v", err)
	}
	notify(OnTokensRevoked, id)
	return nil
}

type LibraryToken struct {
	LibraryID string
	Token     string
	Ctime     sql.NullInt64
}

// ListLibraryTokensByAccount returns all sync tokens for an account.
func ListLibraryTokensByAccount(id account.ID) ([]LibraryToken, error) {
	sqlStr := "SELECT library_id, token, ctime FROM LibraryUserToken WHERE account_id = ? ORDER BY ctime"
	ctx, cancel := option.WithDBTimeout(context.Background())
	defer cancel()
	rows, err := readDB.QueryContext(ctx, sqlStr, id)
	if err != nil {
		return nil, fmt.Errorf("failed to list library tokens: %v", err)
	}
	defer func() { _ = rows.Close() }()

	var tokens []LibraryToken
	for rows.Next() {
		var t LibraryToken
		// A scan failure is returned rather than skipped: this list is what an
		// operator revokes from, and silently omitting a row would show a
		// token as already gone while it still authenticates.
		if err := rows.Scan(&t.LibraryID, &t.Token, &t.Ctime); err != nil {
			return nil, fmt.Errorf("failed to read library token row: %v", err)
		}
		tokens = append(tokens, t)
	}
	return tokens, rows.Err()
}

// CreateLibrary makes a library owned by owner, in the given format. It mints the
// library's id, its initial objects and every row the library needs.
//
// It takes the whole account rather than an id because the owner is used for
// two different things here. LibraryOwner records who may administer the
// library, and that is a key, so it stores the id. The commit's author and
// LibraryInfo.last_modifier record what the creator was called at the time, and
// those are display data — the commit's is baked into its content hash and
// could not be rewritten later even if it should be.
//
// format is a parameter rather than a default because it is frozen for the
// life of the library: every client that ever reads it chunks to these
// numbers, so the one moment it can be chosen is this one.
//
// The library is store-v2: an empty root directory object and an initial
// commit pointing at it, both minted here and both addressed by SHA-256. The
// commit carries no library name — the catalog is the authority for that, and
// a name sealed inside a commit would be unreadable in the library type this
// server exists to serve.
//
// E2EE is still refused, but the reason has moved. It is no longer that the
// server cannot mint a sealed commit — that is true and remains a client
// operation with a client-supplied id, and the wire shape for it is in
// porter-brief.md. It is that the content key would have nowhere to live: the
// CK is wrapped to each member's X25519 public key, that key is an account
// column that does not exist yet, and the wrap blob has no table. Creating an
// E2EE library before then would produce one whose key dies with the device
// that made it, which is data loss wearing a feature's clothes.
func CreateLibrary(name string, owner *account.Account, format Format) (string, error) {
	if err := format.Validate(); err != nil {
		return "", err
	}
	if format.E2EE {
		return "", fmt.Errorf("cannot yet create an end-to-end encrypted library: its content key has nowhere durable to live: %w", ErrNoContentKey)
	}
	libraryID := uuid.New().String()

	store, err := OpenStore(libraryID, format)
	if err != nil {
		return "", fmt.Errorf("failed to open store for new library: %w", err)
	}
	root, err := store.EmptyDir()
	if err != nil {
		return "", fmt.Errorf("failed to create empty root: %w", err)
	}
	now := time.Now().Unix()
	commitID, err := store.PutCommit(&storefmt.Commit{
		Root:      root,
		CreatedAt: now,
		Author:    owner.Email,
		Message:   "Created library",
	})
	if err != nil {
		return "", fmt.Errorf("failed to create initial commit: %w", err)
	}

	ctx, cancel := option.WithDBTimeout(context.Background())
	defer cancel()

	tx, err := writeDB.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("failed to begin transaction: %v", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx,
		"INSERT INTO Library (library_id, chunker, chunk_min, chunk_target, chunk_max, chunk_norm, e2ee) VALUES (?, ?, ?, ?, ?, ?, ?)",
		libraryID, format.Chunker, format.MinSize, format.TargetSize, format.MaxSize,
		format.Normalization, format.E2EE); err != nil {
		return "", fmt.Errorf("failed to insert library: %v", err)
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO Branch (name, library_id, commit_id, root_id) VALUES ('master', ?, ?, ?)",
		libraryID, commitID.String(), root.String()); err != nil {
		return "", fmt.Errorf("failed to insert branch: %v", err)
	}
	if _, err := tx.ExecContext(ctx, dbutil.InsertOrReplace("LibraryHead", "library_id, branch_name"), libraryID, "master"); err != nil {
		return "", fmt.Errorf("failed to insert library head: %v", err)
	}
	if _, err := tx.ExecContext(ctx, dbutil.InsertOrReplace("LibraryOwner", "library_id, account_id"), libraryID, owner.ID); err != nil {
		return "", fmt.Errorf("failed to insert library owner: %v", err)
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO LibraryInfo (library_id, name, update_time, version, is_encrypted, last_modifier) VALUES (?, ?, ?, 1, 0, ?)",
		libraryID, name, now, owner.Email); err != nil {
		return "", fmt.Errorf("failed to insert library info: %v", err)
	}

	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("failed to commit transaction: %v", err)
	}

	return libraryID, nil
}

// DeleteLibrary removes a repository and all associated DB records.
// Filesystem objects (commits, blocks, fs) are NOT deleted — GC handles that.
func DeleteLibrary(libraryID string) error {
	// Virtual libraries derived from this one go first. Deleting only the origin
	// removed their VirtualLibrary rows but left their Library, Branch and
	// LibraryUserToken rows in place, so each child survived as an apparently
	// ordinary library — while its StoreID still pointed at the origin's
	// object store, which GC had just reclaimed. A client kept syncing
	// against an empty store, and nothing ever cleaned the rows up.
	children, err := listVirtualLibraryIDs(libraryID)
	if err != nil {
		return err
	}
	for _, child := range children {
		// A library listed as its own origin would otherwise recurse forever.
		if child == libraryID {
			continue
		}
		if err := DeleteLibrary(child); err != nil {
			return fmt.Errorf("failed to delete virtual library %s of %s: %v", child, libraryID, err)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), option.DBOpTimeout*2)
	defer cancel()

	tx, err := writeDB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %v", err)
	}
	defer func() { _ = tx.Rollback() }()

	deletes := []string{
		"DELETE FROM Library WHERE library_id = ?",
		"DELETE FROM Branch WHERE library_id = ?",
		"DELETE FROM LibraryHead WHERE library_id = ?",
		"DELETE FROM LibraryOwner WHERE library_id = ?",
		"DELETE FROM LibraryInfo WHERE library_id = ?",
		"DELETE FROM SharedLibrary WHERE library_id = ?",
		"DELETE FROM LibraryGroup WHERE library_id = ?",
		"DELETE FROM InnerPubLibrary WHERE library_id = ?",
		"DELETE FROM LibraryUserToken WHERE library_id = ?",
		"DELETE FROM LibraryUsage WHERE library_id = ?",
		"DELETE FROM LibraryHistoryLimit WHERE library_id = ?",
		"DELETE FROM LibraryValidSince WHERE library_id = ?",
	}

	for _, sqlStr := range deletes {
		if _, err := tx.ExecContext(ctx, sqlStr, libraryID); err != nil {
			return fmt.Errorf("failed to delete library records: %v", err)
		}
	}

	// Clean up virtual libraries referencing this library
	if _, err := tx.ExecContext(ctx, "DELETE FROM VirtualLibrary WHERE library_id = ? OR origin_library = ?", libraryID, libraryID); err != nil {
		return fmt.Errorf("failed to delete virtual library records: %v", err)
	}

	// Mark for garbage collection
	// Non-fatal — GC will still find orphaned objects
	_, _ = tx.ExecContext(ctx, dbutil.InsertOrIgnore("GarbageLibraries", "library_id"), libraryID)

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("failed to commit transaction: %v", err)
	}

	notify(OnLibraryDeleted, libraryID)
	return nil
}

// listVirtualLibraryIDs returns the libraries whose origin is libraryID.
func listVirtualLibraryIDs(libraryID string) ([]string, error) {
	ctx, cancel := option.WithDBTimeout(context.Background())
	defer cancel()

	rows, err := readDB.QueryContext(ctx,
		"SELECT library_id FROM VirtualLibrary WHERE origin_library = ?", libraryID)
	if err != nil {
		return nil, fmt.Errorf("failed to list virtual libraries of %s: %v", libraryID, err)
	}
	defer func() { _ = rows.Close() }()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("failed to read virtual library row: %v", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// OnLibraryDeleted and OnTokensRevoked let the fileserver drop cached
// authorisations the moment the rows they were derived from go away. Without
// them a cached token or permission stays authoritative for its full TTL,
// which for a deletion means the server keeps accepting uploads to a library
// that no longer exists.
//
// They are package variables rather than a direct call because libmgr sits
// below the fileserver package and cannot import it. Nil until the server
// registers them, so the CLI paths — which have no caches — need no wiring.
var (
	OnLibraryDeleted func(libraryID string)
	OnTokensRevoked  func(id account.ID)
)

// notify fires a registered hook, if one is registered. It is generic because
// the hooks differ only in what they carry — a library id, an account id — and a
// copy per argument type is a copy per future hook.
func notify[T any](hook func(T), arg T) {
	if hook != nil {
		hook(arg)
	}
}
