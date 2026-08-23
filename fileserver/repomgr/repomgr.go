// Package repomgr manages repo objects and file operations in repos.
package repomgr

import (
	"context"
	"crypto/sha1"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	// Change to non-blank imports when use
	"github.com/dkam/silo/fileserver/account"
	_ "github.com/dkam/silo/fileserver/blockmgr"
	"github.com/dkam/silo/fileserver/commitmgr"
	"github.com/dkam/silo/fileserver/dbutil"
	"github.com/dkam/silo/fileserver/objmgr"
	"github.com/dkam/silo/fileserver/option"
	storefmt "github.com/dkam/silo/store"
	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"
)

// Repo status
const (
	RepoStatusNormal = iota
	RepoStatusReadOnly
	NRepoStatus
)

// Repo contains information about a repo.
type Repo struct {
	ID string
	// Name, LastModifier and LastModificationTime come from the catalog, not
	// from the head commit. See the RepoInfo comment in dbutil/schema.go for
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

	// Set when repo is virtual
	VirtualInfo *VRepoInfo

	// ID for fs and block store
	StoreID string

	// store is this library's objects, opened at most once. See Store.
	store *objmgr.Store

	// Format is how this library's bytes are made — the chunker parameters
	// every client has to agree with, and whether the content is end-to-end
	// encrypted. Frozen at creation; see format.go.
	Format Format

	// Vestigial: the Seafile key ceremony, read out of the head commit of a
	// library in the old format and populated for no other kind. It is the one
	// thing still read from a commit, and it goes with the frozen lanes.
	IsEncrypted   bool
	EncVersion    int
	Magic         string
	RandomKey     string
	Salt          string
	PwdHash       string
	PwdHashAlgo   string
	PwdHashParams string
	Version       int
}

// VRepoInfo contains virtual repo information.
type VRepoInfo struct {
	RepoID       string
	OriginRepoID string
	Path         string
	BaseCommitID string
}

var seafileDB *sql.DB      // read handle
var seafileWriteDB *sql.DB // write handle
var dataDir string         // where object stores live

// Init initialize status of repomgr package.
//
// dataDir is here because creating a library now writes objects as well as
// rows: a store-v2 library's first commit and its empty root are real objects
// that have to exist before the branch can point at them.
func Init(readDB, writeDB *sql.DB, dir string) {
	seafileDB = readDB
	seafileWriteDB = writeDB
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

// Store is this library's objects, opened once per loaded Repo.
//
// Every caller wanted the same two fields — StoreID and Format — and opening
// from those by hand at each site made the "storeID rather than the library
// id" rule above something six call sites had to remember, where writing
// repo.ID instead would have looked entirely reasonable. Now it is remembered
// in one place.
//
// Opened once because a Store is not free: it builds an object store per
// object type and each of those mkdirs its directories, so a request that
// resolved a path, read the file and wrote a commit paid for four or five
// identical handles. A Repo is a per-request value — nothing caches one, and
// GetWithReason mints a fresh one per call — so the handle lives exactly as
// long as the request that asked for it, and is used by the one goroutine
// serving it.
func (repo *Repo) Store() (*objmgr.Store, error) {
	if repo.store != nil {
		return repo.store, nil
	}
	st, err := OpenStore(repo.StoreID, repo.Format)
	if err != nil {
		return nil, err
	}
	repo.store = st
	return st, nil
}

// A repo can fail to load for four unrelated reasons, and only one of them is
// a statement about the request. Collapsing them — which returning a bare nil
// does — is how a server that has lost an object comes to answer 404, and a
// 404 tells a sync client the library was deleted and its local copy should
// go with it. The copy it would delete is the one that could have restored
// the object.
var (
	// ErrRepoNotFound means there is no such library. This is the only one of
	// the four that a client may act on by forgetting the library.
	ErrRepoNotFound = errors.New("no such library")

	// ErrRepoCorrupted means the library exists but the server cannot read its
	// head: an empty commit id in Branch, or a commit object the store has
	// lost. Nothing has been deleted, and the client's copy may be the only
	// intact one left.
	ErrRepoCorrupted = errors.New("library storage is damaged")

	// ErrRepoUnavailable means the database could not be read, so nothing is
	// known about the library either way. Expected to clear on its own.
	ErrRepoUnavailable = errors.New("library metadata is unavailable")
)

// Get returns Repo object by repo ID, or nil if it could not be read for any
// of the four reasons above.
//
// Anything answering a client should call GetWithReason instead: which failure
// happened decides what the client is told, and this signature throws that
// away.
func Get(id string) *Repo {
	repo, _ := GetWithReason(id)
	return repo
}

// repoSelect is the one row shape both loaders read.
//
// It lives in a constant because the column list and the Scan that consumes it
// have to agree, and two copies of a pair that has to agree is one copy too
// many — the format columns were added to one of them first, and the second
// loader silently returned libraries with a zeroed chunker until it wasn't.
const repoSelect = `SELECT r.repo_id, b.commit_id, b.root_id, v.origin_repo, v.path, v.base_commit, ` +
	`r.chunker, r.chunk_min, r.chunk_target, r.chunk_max, r.chunk_norm, r.e2ee, ` +
	`i.name, i.update_time, i.last_modifier FROM ` +
	`Repo r LEFT JOIN Branch b ON r.repo_id = b.repo_id ` +
	`LEFT JOIN VirtualRepo v ON r.repo_id = v.repo_id ` +
	`LEFT JOIN RepoInfo i ON r.repo_id = i.repo_id ` +
	`WHERE r.repo_id = ? AND b.name = 'master'`

// scanRepoRow reads one repoSelect row, including the virtual-repo columns and
// the store id they decide.
func scanRepoRow(rows *sql.Rows, id string, repo *Repo) error {
	var originRepoID, path, baseCommitID sql.NullString
	// Nullable because the joins are outer ones and because these columns
	// arrived after the tables did. A library with no RepoInfo row is not an
	// error to this loader: it has no display name yet, which is a different
	// thing from having no head.
	var rootID, name, lastModifier sql.NullString
	var updateTime sql.NullInt64
	if err := rows.Scan(&repo.ID, &repo.HeadCommitID, &rootID, &originRepoID, &path, &baseCommitID,
		&repo.Format.Chunker, &repo.Format.MinSize, &repo.Format.TargetSize,
		&repo.Format.MaxSize, &repo.Format.Normalization, &repo.Format.E2EE,
		&name, &updateTime, &lastModifier); err != nil {
		return err
	}
	repo.RootID = rootID.String
	repo.Name = name.String
	repo.LastModifier = lastModifier.String
	repo.LastModificationTime = updateTime.Int64

	if !originRepoID.Valid {
		repo.StoreID = repo.ID
		return nil
	}
	repo.VirtualInfo = &VRepoInfo{RepoID: id, OriginRepoID: originRepoID.String}
	repo.StoreID = originRepoID.String
	if path.Valid {
		repo.VirtualInfo.Path = path.String
	}
	if baseCommitID.Valid {
		repo.VirtualInfo.BaseCommitID = baseCommitID.String
	}
	return nil
}

// GetWithReason returns the repo, or the reason it could not be returned —
// one of ErrRepoNotFound, ErrRepoCorrupted or ErrRepoUnavailable, wrapped with
// the detail. Faults are logged here, once per repo per repoFaultInterval, so
// callers should not log again.
func GetWithReason(id string) (*Repo, error) {
	ctx, cancel := option.WithDBTimeout(context.Background())
	defer cancel()
	stmt, err := seafileDB.PrepareContext(ctx, repoSelect)
	if err != nil {
		return nil, fault(id, ErrRepoUnavailable, "failed to prepare sql %s: %v", repoSelect, err)
	}
	defer func() { _ = stmt.Close() }()

	rows, err := stmt.QueryContext(ctx, id)
	if err != nil {
		return nil, fault(id, ErrRepoUnavailable, "failed to query sql: %v", err)
	}
	defer func() { _ = rows.Close() }()

	repo := new(Repo)

	if rows.Next() {
		if err := scanRepoRow(rows, id, repo); err != nil {
			return nil, fault(id, ErrRepoUnavailable, "failed to scan sql rows: %v", err)
		}
	} else if err := rows.Err(); err != nil {
		// No row, but the iteration itself failed — that is the database
		// speaking, not an answer about whether the library exists.
		return nil, fault(id, ErrRepoUnavailable, "failed to read sql rows: %v", err)
	} else {
		clearFaults(id)
		return nil, ErrRepoNotFound
	}

	if repo.HeadCommitID == "" {
		return nil, fault(id, ErrRepoCorrupted, "Branch holds no head commit")
	}

	// A library whose stored parameters do not describe a chunker cannot be
	// read by anybody, so it is corrupted rather than merely unusual.
	if err := repo.Format.Validate(); err != nil {
		return nil, fault(id, ErrRepoCorrupted, "%v", err)
	}

	if err := loadSeafileCrypto(repo); err != nil {
		return nil, fault(id, ErrRepoCorrupted, "failed to load head commit %s: %v", repo.HeadCommitID, err)
	}
	if err := checkHeadPresent(repo); err != nil {
		return nil, fault(id, ErrRepoCorrupted, "%v", err)
	}
	clearFaults(id)

	return repo, nil
}

// IsStoreV2 reports whether this library's objects are the store-v2 format.
//
// The head id's width says which, exactly as it does for loadSeafileCrypto and
// for the object store's choice of digest: forty hex characters is a SHA-1
// Seafile commit, sixty-four is SHA-256. It is a real discriminator rather
// than a heuristic — the two formats cannot mint an id of the other's width —
// and it disappears with the Seafile lanes, when there is only one answer.
func (repo *Repo) IsStoreV2() bool {
	return len(repo.HeadCommitID) == 2*storefmt.IDSize
}

// checkHeadPresent verifies a store-v2 library's head commit object is
// actually there.
//
// Nothing else does any more, and that is the point. A Seafile library's head
// was read on every load by loadSeafileCrypto, so a missing object surfaced as
// corruption for free; a store-v2 head is never read here, because there is
// nothing in it this function needs. Without this check a library whose head
// object had gone would load clean and fail later, somewhere further from the
// cause — and the reason that matters is the one on GetWithReason's errors: a
// library that answers "not found" tells a sync client to delete its local
// copy, which is the copy that could have restored the object.
//
// A stat, not a read — and the Store it stats through is repo.Store, so the
// handle this check needs is the one the rest of the request was going to open
// anyway. What the check itself adds to a load is one lookup.
func checkHeadPresent(repo *Repo) error {
	if !repo.IsStoreV2() {
		return nil
	}
	id, err := storefmt.ParseID(repo.HeadCommitID)
	if err != nil {
		return fmt.Errorf("head commit id %q is unreadable: %w", repo.HeadCommitID, err)
	}
	st, err := repo.Store()
	if err != nil {
		return fmt.Errorf("failed to open store: %w", err)
	}
	ok, err := st.HasObject(id)
	if err != nil {
		return fmt.Errorf("failed to look for head commit %s: %w", repo.HeadCommitID, err)
	}
	if !ok {
		return fmt.Errorf("head commit %s is missing from the object store", repo.HeadCommitID)
	}
	return nil
}

// loadSeafileCrypto fills the vestigial Seafile fields from the head commit,
// and is the only thing left that reads one.
//
// It runs on libraries in the old format and on nothing else. The head id's
// width is what says which: forty characters is SHA-1 and a Seafile commit,
// sixty-four is SHA-256 and a store-v2 one, exactly as the object store
// chooses a digest by the width of an id. A store-v2 library has no key
// ceremony to read and no commit this decoder could parse, so asking would be
// a guaranteed failure rather than a lookup.
//
// This is a bridge with a scheduled end: it goes when the frozen lanes do,
// taking the fields it fills with it.
func loadSeafileCrypto(repo *Repo) error {
	const seafileCommitIDLen = 40
	if len(repo.HeadCommitID) != seafileCommitIDLen {
		return nil
	}
	commit, err := commitmgr.Load(repo.ID, repo.HeadCommitID)
	if err != nil {
		return err
	}
	repo.Version = commit.Version
	if commit.Encrypted == "true" {
		repo.IsEncrypted = true
		repo.EncVersion = commit.EncVersion
		if repo.EncVersion == 1 && commit.PwdHash == "" {
			repo.Magic = commit.Magic
		} else if repo.EncVersion == 2 {
			repo.RandomKey = commit.RandomKey
		} else if repo.EncVersion == 3 {
			repo.RandomKey = commit.RandomKey
			repo.Salt = commit.Salt
		} else if repo.EncVersion == 4 {
			repo.RandomKey = commit.RandomKey
			repo.Salt = commit.Salt
		}
		if repo.EncVersion >= 2 && commit.PwdHash == "" {
			repo.Magic = commit.Magic
		}
		if commit.PwdHash != "" {
			repo.PwdHash = commit.PwdHash
			repo.PwdHashAlgo = commit.PwdHashAlgo
			repo.PwdHashParams = commit.PwdHashParams
		}
	}
	return nil
}

// StatusFor maps a GetWithReason failure onto the status and body a client
// should see. It lives beside the errors it maps because both packages that
// serve repos over HTTP need it, and the one decision that matters — that only
// a missing row is a 404 — must not exist in two copies that can drift.
//
// The 500 body says the library still exists on purpose: it is the only thing
// standing between a damaged server and a client that decides to tidy up.
func StatusFor(err error) (int, string) {
	switch {
	case err == nil:
		return http.StatusOK, ""
	case errors.Is(err, ErrRepoNotFound):
		return http.StatusNotFound, "Repo not found"
	case errors.Is(err, ErrRepoUnavailable):
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
// then held for repoFaultInterval.
const repoFaultInterval = 5 * time.Minute

type faultKey struct {
	repoID string
	kind   error
}

var repoFaults = struct {
	sync.Mutex
	lastLogged map[faultKey]time.Time
}{lastLogged: make(map[faultKey]time.Time)}

// faultKinds is every kind clearFaults has to forget. Keep it in step with the
// sentinels above.
var faultKinds = []error{ErrRepoNotFound, ErrRepoCorrupted, ErrRepoUnavailable}

// fault wraps the detail as kind, logs it if it has not been logged recently,
// and returns the wrapped error.
func fault(repoID string, kind error, format string, args ...interface{}) error {
	err := fmt.Errorf("%w: repo %s: %s", kind, repoID, fmt.Sprintf(format, args...))
	if firstReport(repoID, kind) {
		log.Error(err)
	}
	return err
}

func firstReport(repoID string, kind error) bool {
	now := time.Now()

	repoFaults.Lock()
	defer repoFaults.Unlock()

	key := faultKey{repoID, kind}
	if last, ok := repoFaults.lastLogged[key]; ok && now.Sub(last) < repoFaultInterval {
		return false
	}
	// Entries are only ever added by a fault and dropped by a repair, so the
	// map tracks broken libraries. Sweeping the stale ones here keeps a repo
	// that was deleted rather than repaired from being remembered forever.
	for k, last := range repoFaults.lastLogged {
		if now.Sub(last) >= repoFaultInterval {
			delete(repoFaults.lastLogged, k)
		}
	}
	repoFaults.lastLogged[key] = now
	return true
}

// clearFaults forgets a library's faults, so that a recurrence after a repair
// is reported again instead of being suppressed as a repeat.
func clearFaults(repoID string) {
	repoFaults.Lock()
	defer repoFaults.Unlock()
	for _, kind := range faultKinds {
		delete(repoFaults.lastLogged, faultKey{repoID, kind})
	}
}

// RepoToCommit converts Repo to Commit.
func RepoToCommit(repo *Repo, commit *commitmgr.Commit) {
	commit.RepoID = repo.ID
	commit.RepoName = repo.Name
	if repo.IsEncrypted {
		commit.Encrypted = "true"
		commit.EncVersion = repo.EncVersion
		if repo.EncVersion == 1 && repo.PwdHash == "" {
			commit.Magic = repo.Magic
		} else if repo.EncVersion == 2 {
			commit.RandomKey = repo.RandomKey
		} else if repo.EncVersion == 3 {
			commit.RandomKey = repo.RandomKey
			commit.Salt = repo.Salt
		} else if repo.EncVersion == 4 {
			commit.RandomKey = repo.RandomKey
			commit.Salt = repo.Salt
		}
		if repo.EncVersion >= 2 && repo.PwdHash == "" {
			commit.Magic = repo.Magic
		}
		if repo.PwdHash != "" {
			commit.PwdHash = repo.PwdHash
			commit.PwdHashAlgo = repo.PwdHashAlgo
			commit.PwdHashParams = repo.PwdHashParams
		}
	} else {
		commit.Encrypted = "false"
	}
	commit.Version = repo.Version
}

// GetEx return repo object even if it's corrupted.
func GetEx(id string) *Repo {
	repo := new(Repo)
	ctx, cancel := option.WithDBTimeout(context.Background())
	defer cancel()
	stmt, err := seafileDB.PrepareContext(ctx, repoSelect)
	if err != nil {
		repo.IsCorrupted = true
		return repo
	}
	defer func() { _ = stmt.Close() }()

	rows, err := stmt.QueryContext(ctx, id)
	if err != nil {
		repo.IsCorrupted = true
		return repo
	}
	defer func() { _ = rows.Close() }()

	if rows.Next() {
		if err := scanRepoRow(rows, id, repo); err != nil {
			repo.IsCorrupted = true
			return repo
		}
	} else if rows.Err() != nil {
		repo.IsCorrupted = true
		return repo
	} else {
		return nil
	}

	if repo.HeadCommitID == "" {
		repo.IsCorrupted = true
		return repo
	}

	if err := repo.Format.Validate(); err != nil {
		_ = fault(id, ErrRepoCorrupted, "%v", err)
		repo.IsCorrupted = true
		return repo
	}

	if err := loadSeafileCrypto(repo); err != nil {
		// Same fault as in GetWithReason, and reached by the sync path on
		// every request, so it shares the same suppression.
		_ = fault(id, ErrRepoCorrupted, "failed to load head commit %s: %v", repo.HeadCommitID, err)
		repo.IsCorrupted = true
		return repo
	}
	clearFaults(id)

	return repo
}

// GetVirtualRepoInfo return virtual repo info by repo id.
func GetVirtualRepoInfo(repoID string) (*VRepoInfo, error) {
	sqlStr := "SELECT repo_id, origin_repo, path, base_commit FROM VirtualRepo WHERE repo_id = ?"
	vRepoInfo := new(VRepoInfo)

	ctx, cancel := option.WithDBTimeout(context.Background())
	defer cancel()
	row := seafileDB.QueryRowContext(ctx, sqlStr, repoID)
	if err := row.Scan(&vRepoInfo.RepoID, &vRepoInfo.OriginRepoID, &vRepoInfo.Path, &vRepoInfo.BaseCommitID); err != nil {
		if err != sql.ErrNoRows {
			return nil, err
		}
		return nil, nil
	}
	return vRepoInfo, nil
}

// GetVirtualRepoInfoByOrigin return virtual repo info by origin repo id.
func GetVirtualRepoInfoByOrigin(originRepo string) ([]*VRepoInfo, error) {
	sqlStr := "SELECT repo_id, origin_repo, path, base_commit " +
		"FROM VirtualRepo WHERE origin_repo=?"
	var vRepos []*VRepoInfo
	ctx, cancel := option.WithDBTimeout(context.Background())
	defer cancel()
	row, err := seafileDB.QueryContext(ctx, sqlStr, originRepo)
	if err != nil {
		return nil, err
	}
	defer func() { _ = row.Close() }()
	for row.Next() {
		vRepoInfo := new(VRepoInfo)
		if err := row.Scan(&vRepoInfo.RepoID, &vRepoInfo.OriginRepoID, &vRepoInfo.Path, &vRepoInfo.BaseCommitID); err != nil {
			if err != sql.ErrNoRows {
				return nil, err
			}
		}
		vRepos = append(vRepos, vRepoInfo)
	}

	return vRepos, nil
}

// GetAccountByToken returns the account a sync token belongs to, or the zero
// id if the token is not one of that repo's.
func GetAccountByToken(repoID string, token string) (account.ID, error) {
	var id account.ID
	sqlStr := "SELECT account_id FROM RepoUserToken WHERE repo_id = ? AND token = ?"

	ctx, cancel := option.WithDBTimeout(context.Background())
	defer cancel()
	row := seafileDB.QueryRowContext(ctx, sqlStr, repoID, token)
	if err := row.Scan(&id); err != nil {
		if err != sql.ErrNoRows {
			return account.Zero, err
		}
	}
	return id, nil
}

// GetAccountForToken resolves a sync token to its owner without naming a repo.
//
// GetAccountByToken is the one to use wherever the repo is known — it is the
// stronger check, since it also proves the token was issued for that repo.
// This exists for the batched endpoints, which are handed a list of repos and
// one token and have to establish who is asking before they can decide which
// of those repos to answer for.
func GetAccountForToken(token string) (account.ID, error) {
	var id account.ID
	ctx, cancel := option.WithDBTimeout(context.Background())
	defer cancel()

	row := seafileDB.QueryRowContext(ctx,
		"SELECT account_id FROM RepoUserToken WHERE token = ?", token)
	if err := row.Scan(&id); err != nil {
		if err == sql.ErrNoRows {
			return account.Zero, nil
		}
		return account.Zero, err
	}
	return id, nil
}

// GetRepoStatus return repo status by repo id.
func GetRepoStatus(repoID string) (int, error) {
	var status = -1

	// First, check origin repo's status.
	sqlStr := "SELECT i.status FROM VirtualRepo v LEFT JOIN RepoInfo i " +
		"ON i.repo_id=v.origin_repo WHERE v.repo_id=? " +
		"AND i.repo_id IS NOT NULL"

	ctx, cancel := option.WithDBTimeout(context.Background())
	defer cancel()
	row := seafileDB.QueryRowContext(ctx, sqlStr, repoID)
	if err := row.Scan(&status); err != nil {
		if err != sql.ErrNoRows {
			return status, err
		} else {
			status = -1
		}
	}
	if status >= 0 {
		return status, nil
	}

	// Then, check repo's own status.
	sqlStr = "SELECT status FROM RepoInfo WHERE repo_id=?"
	row = seafileDB.QueryRowContext(ctx, sqlStr, repoID)
	if err := row.Scan(&status); err != nil {
		if err != sql.ErrNoRows {
			return status, err
		}
	}
	return status, nil
}

// TokenPeerInfoExists check if the token exists.
func TokenPeerInfoExists(token string) (bool, error) {
	var exists string
	sqlStr := "SELECT token FROM RepoTokenPeerInfo WHERE token=?"

	ctx, cancel := option.WithDBTimeout(context.Background())
	defer cancel()
	row := seafileDB.QueryRowContext(ctx, sqlStr, token)
	if err := row.Scan(&exists); err != nil {
		if err != sql.ErrNoRows {
			return false, err
		}
		return false, nil
	}
	return true, nil
}

// AddTokenPeerInfo add token peer info to RepoTokenPeerInfo table.
func AddTokenPeerInfo(token, peerID, peerIP, peerName, clientVer string, syncTime int64) error {
	sqlStr := "INSERT INTO RepoTokenPeerInfo (token, peer_id, peer_ip, peer_name, sync_time, client_ver)" +
		"VALUES (?, ?, ?, ?, ?, ?)"

	ctx, cancel := option.WithDBTimeout(context.Background())
	defer cancel()
	if _, err := seafileWriteDB.ExecContext(ctx, sqlStr, token, peerID, peerIP, peerName, syncTime, clientVer); err != nil {
		return err
	}
	return nil
}

// UpdateTokenPeerInfo update token peer info to RepoTokenPeerInfo table.
func UpdateTokenPeerInfo(token, peerID, clientVer string, syncTime int64) error {
	sqlStr := "UPDATE RepoTokenPeerInfo SET " +
		"peer_ip=?, sync_time=?, client_ver=? WHERE token=?"
	ctx, cancel := option.WithDBTimeout(context.Background())
	defer cancel()
	if _, err := seafileWriteDB.ExecContext(ctx, sqlStr, peerID, syncTime, clientVer, token); err != nil {
		return err
	}
	return nil
}

// GetUploadTmpFile gets the timp file path of upload file.
func GetUploadTmpFile(repoID, filePath string) (string, error) {
	var filePathNoSlash string
	if filePath[0] == '/' {
		filePathNoSlash = filePath[1:]
	} else {
		filePathNoSlash = filePath
		filePath = "/" + filePath
	}

	var tmpFile string
	sqlStr := "SELECT tmp_file_path FROM WebUploadTempFiles WHERE repo_id = ? AND file_path = ?"

	ctx, cancel := option.WithDBTimeout(context.Background())
	defer cancel()
	row := seafileDB.QueryRowContext(ctx, sqlStr, repoID, filePath)
	if err := row.Scan(&tmpFile); err != nil {
		if err != sql.ErrNoRows {
			return "", err
		}
	}
	if tmpFile == "" {
		row := seafileDB.QueryRowContext(ctx, sqlStr, repoID, filePathNoSlash)
		if err := row.Scan(&tmpFile); err != nil {
			if err != sql.ErrNoRows {
				return "", err
			}
		}
	}

	return tmpFile, nil
}

// AddUploadTmpFile adds the tmp file path of upload file.
func AddUploadTmpFile(repoID, filePath, tmpFile string) error {
	if filePath[0] != '/' {
		filePath = "/" + filePath
	}

	sqlStr := "INSERT INTO WebUploadTempFiles (repo_id, file_path, tmp_file_path) VALUES (?, ?, ?)"

	ctx, cancel := option.WithDBTimeout(context.Background())
	defer cancel()
	_, err := seafileWriteDB.ExecContext(ctx, sqlStr, repoID, filePath, tmpFile)
	if err != nil {
		return err
	}

	return nil
}

// DelUploadTmpFile deletes the tmp file path of upload file.
func DelUploadTmpFile(repoID, filePath string) error {
	var filePathNoSlash string
	if filePath[0] == '/' {
		filePathNoSlash = filePath[1:]
	} else {
		filePathNoSlash = filePath
		filePath = "/" + filePath
	}

	sqlStr := "DELETE FROM WebUploadTempFiles WHERE repo_id = ? AND file_path IN (?, ?)"

	ctx, cancel := option.WithDBTimeout(context.Background())
	defer cancel()
	_, err := seafileWriteDB.ExecContext(ctx, sqlStr, repoID, filePath, filePathNoSlash)
	if err != nil {
		return err
	}

	return nil
}

// RecordHeadMove records what the server witnessed when a library's head
// moved: who was authenticated, and when.
//
// It replaces a function that copied a name, a timestamp, a format version and
// an encryption flag out of the new head commit into RepoInfo, keeping the
// commit as the source of truth and this table as a mirror. That is inverted
// now — see the RepoInfo comment in dbutil/schema.go — and the two fields left
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
func RecordHeadMove(ctx context.Context, tx *sql.Tx, repoID, modifier string, when int64) error {
	if when == 0 {
		when = time.Now().Unix()
	}
	_, err := tx.ExecContext(ctx,
		"INSERT INTO RepoInfo (repo_id, name, update_time, last_modifier) VALUES (?, '', ?, ?) "+
			"ON CONFLICT(repo_id) DO UPDATE SET update_time=excluded.update_time, "+
			"last_modifier=excluded.last_modifier",
		repoID, when, modifier)
	return err
}

// SetRepoName renames a library.
//
// It is a single UPDATE and mints no commit. The old spelling wrote a commit
// whose only change was the name it carried, which moved the head for a change
// that was not in the tree — every client saw a new head and diffed two
// identical roots. Under E2EE the server could not write that commit at all.
func SetRepoName(repoID, name string) error {
	ctx, cancel := option.WithDBTimeout(context.Background())
	defer cancel()
	res, err := seafileWriteDB.ExecContext(ctx,
		"UPDATE RepoInfo SET name=? WHERE repo_id=?", name, repoID)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return fmt.Errorf("library %s has no RepoInfo row", repoID)
	}
	return nil
}

// SetVirtualRepoBaseCommitPath updates the table of VirtualRepo.
func SetVirtualRepoBaseCommitPath(repoID, baseCommitID, newPath string) error {
	sqlStr := "UPDATE VirtualRepo SET base_commit=?, path=? WHERE repo_id=?"
	ctx, cancel := option.WithDBTimeout(context.Background())
	defer cancel()
	if _, err := seafileWriteDB.ExecContext(ctx, sqlStr, baseCommitID, newPath, repoID); err != nil {
		return err
	}
	return nil
}

// GetVirtualRepoIDsByOrigin return the virtual repo ids by origin repo id.
func GetVirtualRepoIDsByOrigin(repoID string) ([]string, error) {
	sqlStr := "SELECT repo_id FROM VirtualRepo WHERE origin_repo=?"

	var id string
	var ids []string
	ctx, cancel := option.WithDBTimeout(context.Background())
	defer cancel()
	row, err := seafileDB.QueryContext(ctx, sqlStr, repoID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = row.Close() }()
	for row.Next() {
		if err := row.Scan(&id); err != nil {
			if err != sql.ErrNoRows {
				return nil, err
			}
		}
		ids = append(ids, id)
	}

	return ids, nil
}

// DelVirtualRepo deletes virtual repo from database.
func DelVirtualRepo(repoID string, cloudMode bool) error {
	err := removeVirtualRepoOndisk(repoID, cloudMode)
	if err != nil {
		err := fmt.Errorf("failed to remove virtual repo on disk: %v", err)
		return err
	}
	sqlStr := "DELETE FROM VirtualRepo WHERE repo_id = ?"
	ctx, cancel := option.WithDBTimeout(context.Background())
	defer cancel()
	_, err = seafileWriteDB.ExecContext(ctx, sqlStr, repoID)
	if err != nil {
		return err
	}

	return nil
}

func removeVirtualRepoOndisk(repoID string, cloudMode bool) error {
	sqlStr := "DELETE FROM Repo WHERE repo_id = ?"
	ctx, cancel := option.WithDBTimeout(context.Background())
	defer cancel()
	_, err := seafileWriteDB.ExecContext(ctx, sqlStr, repoID)
	if err != nil {
		return err
	}
	sqlStr = "SELECT name, repo_id, commit_id FROM Branch WHERE repo_id=?"
	rows, err := seafileDB.QueryContext(ctx, sqlStr, repoID)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var name, id, commitID string
		if err := rows.Scan(&name, &id, &commitID); err != nil {
			if err != sql.ErrNoRows {
				return err
			}
		}
		sqlStr := "DELETE FROM RepoHead WHERE branch_name = ? AND repo_id = ?"
		_, err := seafileWriteDB.ExecContext(ctx, sqlStr, name, id)
		if err != nil {
			return err
		}
		sqlStr = "DELETE FROM Branch WHERE name=? AND repo_id=?"
		_, err = seafileWriteDB.ExecContext(ctx, sqlStr, name, id)
		if err != nil {
			return err
		}
	}

	sqlStr = "DELETE FROM RepoOwner WHERE repo_id = ?"
	_, err = seafileWriteDB.ExecContext(ctx, sqlStr, repoID)
	if err != nil {
		return err
	}

	sqlStr = "DELETE FROM SharedRepo WHERE repo_id = ?"
	_, err = seafileWriteDB.ExecContext(ctx, sqlStr, repoID)
	if err != nil {
		return err
	}

	sqlStr = "DELETE FROM RepoGroup WHERE repo_id = ?"
	_, err = seafileWriteDB.ExecContext(ctx, sqlStr, repoID)
	if err != nil {
		return err
	}
	if !cloudMode {
		sqlStr = "DELETE FROM InnerPubRepo WHERE repo_id = ?"
		_, err := seafileWriteDB.ExecContext(ctx, sqlStr, repoID)
		if err != nil {
			return err
		}
	}

	sqlStr = "DELETE FROM RepoUserToken WHERE repo_id = ?"
	_, err = seafileWriteDB.ExecContext(ctx, sqlStr, repoID)
	if err != nil {
		return err
	}

	sqlStr = "DELETE FROM RepoValidSince WHERE repo_id = ?"
	_, err = seafileWriteDB.ExecContext(ctx, sqlStr, repoID)
	if err != nil {
		return err
	}

	sqlStr = "DELETE FROM RepoSize WHERE repo_id = ?"
	_, err = seafileWriteDB.ExecContext(ctx, sqlStr, repoID)
	if err != nil {
		return err
	}

	sqlStr = "DELETE FROM RepoUsage WHERE repo_id = ?"
	_, err = seafileWriteDB.ExecContext(ctx, sqlStr, repoID)
	if err != nil {
		return err
	}

	_, err = seafileWriteDB.ExecContext(ctx, dbutil.InsertOrIgnore("GarbageRepos", "repo_id"), repoID)
	if err != nil {
		return err
	}

	return nil
}

// IsVirtualRepo check if the repo is a virtual reop.
func IsVirtualRepo(repoID string) (bool, error) {
	var exists int
	sqlStr := "SELECT 1 FROM VirtualRepo WHERE repo_id = ?"

	ctx, cancel := option.WithDBTimeout(context.Background())
	defer cancel()
	row := seafileDB.QueryRowContext(ctx, sqlStr, repoID)
	if err := row.Scan(&exists); err != nil {
		if err != sql.ErrNoRows {
			return false, err
		}
		return false, nil
	}
	return true, nil

}

// GetRepoOwner get the owner of repo.
func GetRepoOwner(repoID string) (account.ID, error) {
	var owner account.ID
	sqlStr := "SELECT account_id FROM RepoOwner WHERE repo_id=?"

	ctx, cancel := option.WithDBTimeout(context.Background())
	defer cancel()
	row := seafileDB.QueryRowContext(ctx, sqlStr, repoID)
	if err := row.Scan(&owner); err != nil {
		if err != sql.ErrNoRows {
			return account.Zero, err
		}
	}

	return owner, nil
}

func HasLastGCID(repoID, clientID string) (bool, error) {
	sqlStr := "SELECT 1 FROM LastGCID WHERE repo_id = ? AND client_id = ?"

	var exist int
	ctx, cancel := option.WithDBTimeout(context.Background())
	defer cancel()
	row := seafileDB.QueryRowContext(ctx, sqlStr, repoID, clientID)
	if err := row.Scan(&exist); err != nil {
		if err != sql.ErrNoRows {
			return false, err
		}
	}
	if exist == 0 {
		return false, nil
	}
	return true, nil
}

func GetLastGCID(repoID, clientID string) (string, error) {
	sqlStr := "SELECT gc_id FROM LastGCID WHERE repo_id = ? AND client_id = ?"

	var gcID sql.NullString
	ctx, cancel := option.WithDBTimeout(context.Background())
	defer cancel()
	row := seafileDB.QueryRowContext(ctx, sqlStr, repoID, clientID)
	if err := row.Scan(&gcID); err != nil {
		if err != sql.ErrNoRows {
			return "", err
		}
	}

	return gcID.String, nil
}

func GetCurrentGCID(repoID string) (string, error) {
	sqlStr := "SELECT gc_id FROM GCID WHERE repo_id = ?"

	var gcID sql.NullString
	ctx, cancel := option.WithDBTimeout(context.Background())
	defer cancel()
	row := seafileDB.QueryRowContext(ctx, sqlStr, repoID)
	if err := row.Scan(&gcID); err != nil {
		if err != sql.ErrNoRows {
			return "", err
		}
	}

	return gcID.String, nil
}

func RemoveLastGCID(repoID, clientID string) error {
	sqlStr := "DELETE FROM LastGCID WHERE repo_id = ? AND client_id = ?"
	ctx, cancel := option.WithDBTimeout(context.Background())
	defer cancel()
	if _, err := seafileWriteDB.ExecContext(ctx, sqlStr, repoID, clientID); err != nil {
		return err
	}
	return nil
}

func SetLastGCID(repoID, clientID, gcID string) error {
	exist, err := HasLastGCID(repoID, clientID)
	if err != nil {
		return err
	}
	ctx, cancel := option.WithDBTimeout(context.Background())
	defer cancel()
	if exist {
		sqlStr := "UPDATE LastGCID SET gc_id = ? WHERE repo_id = ? AND client_id = ?"
		if _, err = seafileWriteDB.ExecContext(ctx, sqlStr, gcID, repoID, clientID); err != nil {
			return err
		}
	} else {
		sqlStr := "INSERT INTO LastGCID (repo_id, client_id, gc_id) VALUES (?, ?, ?)"
		if _, err = seafileWriteDB.ExecContext(ctx, sqlStr, repoID, clientID, gcID); err != nil {
			return err
		}
	}
	return nil
}

// GenerateRepoToken creates a new per-repo sync token for the given user.
// Token format matches the C implementation: SHA1(UUID) → 40-char hex string.
//
// A new token is minted on every call rather than reusing an existing one for
// the same (repo, user). That is intentional: each client install gets its own
// token, so revoking one device does not stop the others syncing. Upstream
// Seahub's get_repo_token_nonnull collapses these to one token per user per
// repo; Silo does not, because the device identity that makes per-device
// revocation useful (client_id, bound to the token at first permission-check)
// is not available here at mint time.
//
// Sync tokens deliberately have no expiry. Seafile and SeaDrive persist them
// in local config and treat them as durable, so ageing them out would stop
// sync silently at the TTL. Revocation is the intended way to invalidate one.
func GenerateRepoToken(repoID string, id account.ID) (string, error) {
	u := uuid.New().String()
	h := sha1.New()
	h.Write([]byte(u))
	token := hex.EncodeToString(h.Sum(nil))

	sqlStr := "INSERT INTO RepoUserToken (repo_id, account_id, token, ctime) VALUES (?, ?, ?, ?)"
	ctx, cancel := option.WithDBTimeout(context.Background())
	defer cancel()
	if _, err := seafileWriteDB.ExecContext(ctx, sqlStr, repoID, id, token, time.Now().Unix()); err != nil {
		return "", fmt.Errorf("failed to insert repo token: %v", err)
	}

	return token, nil
}

// DeleteRepoTokensByAccount revokes every sync token an account holds, across
// all repos, stopping all of their devices from syncing. Returns the count.
func DeleteRepoTokensByAccount(id account.ID) (int64, error) {
	ctx, cancel := option.WithDBTimeout(context.Background())
	defer cancel()

	res, err := seafileWriteDB.ExecContext(ctx,
		"DELETE FROM RepoUserToken WHERE account_id = ?", id)
	if err != nil {
		return 0, fmt.Errorf("failed to delete repo tokens: %v", err)
	}
	notify(OnTokensRevoked, id)

	return dbutil.RowsAffected(res), nil
}

// DeleteRepoToken removes a specific sync token.
func DeleteRepoToken(repoID, token string, id account.ID) error {
	sqlStr := "DELETE FROM RepoUserToken WHERE repo_id = ? AND token = ? AND account_id = ?"
	ctx, cancel := option.WithDBTimeout(context.Background())
	defer cancel()
	if _, err := seafileWriteDB.ExecContext(ctx, sqlStr, repoID, token, id); err != nil {
		return fmt.Errorf("failed to delete repo token: %v", err)
	}
	notify(OnTokensRevoked, id)
	return nil
}

type RepoToken struct {
	RepoID string
	Token  string
	Ctime  sql.NullInt64
}

// ListRepoTokensByAccount returns all sync tokens for an account.
func ListRepoTokensByAccount(id account.ID) ([]RepoToken, error) {
	sqlStr := "SELECT repo_id, token, ctime FROM RepoUserToken WHERE account_id = ? ORDER BY ctime"
	ctx, cancel := option.WithDBTimeout(context.Background())
	defer cancel()
	rows, err := seafileDB.QueryContext(ctx, sqlStr, id)
	if err != nil {
		return nil, fmt.Errorf("failed to list repo tokens: %v", err)
	}
	defer func() { _ = rows.Close() }()

	var tokens []RepoToken
	for rows.Next() {
		var t RepoToken
		// A scan failure is returned rather than skipped: this list is what an
		// operator revokes from, and silently omitting a row would show a
		// token as already gone while it still authenticates.
		if err := rows.Scan(&t.RepoID, &t.Token, &t.Ctime); err != nil {
			return nil, fmt.Errorf("failed to read repo token row: %v", err)
		}
		tokens = append(tokens, t)
	}
	return tokens, rows.Err()
}

// CreateRepo makes a library owned by owner, in the given format. It mints the
// library's id, its initial objects and every row the library needs.
//
// It takes the whole account rather than an id because the owner is used for
// two different things here. RepoOwner records who may administer the
// library, and that is a key, so it stores the id. The commit's author and
// RepoInfo.last_modifier record what the creator was called at the time, and
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
func CreateRepo(name string, owner *account.Account, format Format) (string, error) {
	if err := format.Validate(); err != nil {
		return "", err
	}
	if format.E2EE {
		return "", fmt.Errorf("cannot yet create an end-to-end encrypted library: its content key has nowhere durable to live: %w", ErrNoContentKey)
	}
	repoID := uuid.New().String()

	store, err := OpenStore(repoID, format)
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

	tx, err := seafileWriteDB.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("failed to begin transaction: %v", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx,
		"INSERT INTO Repo (repo_id, chunker, chunk_min, chunk_target, chunk_max, chunk_norm, e2ee) VALUES (?, ?, ?, ?, ?, ?, ?)",
		repoID, format.Chunker, format.MinSize, format.TargetSize, format.MaxSize,
		format.Normalization, format.E2EE); err != nil {
		return "", fmt.Errorf("failed to insert repo: %v", err)
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO Branch (name, repo_id, commit_id, root_id) VALUES ('master', ?, ?, ?)",
		repoID, commitID.String(), root.String()); err != nil {
		return "", fmt.Errorf("failed to insert branch: %v", err)
	}
	if _, err := tx.ExecContext(ctx, dbutil.InsertOrReplace("RepoHead", "repo_id, branch_name"), repoID, "master"); err != nil {
		return "", fmt.Errorf("failed to insert repo head: %v", err)
	}
	if _, err := tx.ExecContext(ctx, dbutil.InsertOrReplace("RepoOwner", "repo_id, account_id"), repoID, owner.ID); err != nil {
		return "", fmt.Errorf("failed to insert repo owner: %v", err)
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO RepoInfo (repo_id, name, update_time, version, is_encrypted, last_modifier) VALUES (?, ?, ?, 1, 0, ?)",
		repoID, name, now, owner.Email); err != nil {
		return "", fmt.Errorf("failed to insert repo info: %v", err)
	}

	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("failed to commit transaction: %v", err)
	}

	return repoID, nil
}

// DeleteRepo removes a repository and all associated DB records.
// Filesystem objects (commits, blocks, fs) are NOT deleted — GC handles that.
func DeleteRepo(repoID string) error {
	// Virtual repos derived from this one go first. Deleting only the origin
	// removed their VirtualRepo rows but left their Repo, Branch and
	// RepoUserToken rows in place, so each child survived as an apparently
	// ordinary library — while its StoreID still pointed at the origin's
	// object store, which GC had just reclaimed. A client kept syncing
	// against an empty store, and nothing ever cleaned the rows up.
	children, err := listVirtualRepoIDs(repoID)
	if err != nil {
		return err
	}
	for _, child := range children {
		// A repo listed as its own origin would otherwise recurse forever.
		if child == repoID {
			continue
		}
		if err := DeleteRepo(child); err != nil {
			return fmt.Errorf("failed to delete virtual repo %s of %s: %v", child, repoID, err)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), option.DBOpTimeout*2)
	defer cancel()

	tx, err := seafileWriteDB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %v", err)
	}
	defer func() { _ = tx.Rollback() }()

	deletes := []string{
		"DELETE FROM Repo WHERE repo_id = ?",
		"DELETE FROM Branch WHERE repo_id = ?",
		"DELETE FROM RepoHead WHERE repo_id = ?",
		"DELETE FROM RepoOwner WHERE repo_id = ?",
		"DELETE FROM RepoInfo WHERE repo_id = ?",
		"DELETE FROM SharedRepo WHERE repo_id = ?",
		"DELETE FROM RepoGroup WHERE repo_id = ?",
		"DELETE FROM InnerPubRepo WHERE repo_id = ?",
		"DELETE FROM RepoUserToken WHERE repo_id = ?",
		"DELETE FROM RepoSize WHERE repo_id = ?",
		"DELETE FROM RepoUsage WHERE repo_id = ?",
		"DELETE FROM RepoHistoryLimit WHERE repo_id = ?",
		"DELETE FROM RepoValidSince WHERE repo_id = ?",
	}

	for _, sqlStr := range deletes {
		if _, err := tx.ExecContext(ctx, sqlStr, repoID); err != nil {
			return fmt.Errorf("failed to delete repo records: %v", err)
		}
	}

	// Clean up virtual repos referencing this repo
	if _, err := tx.ExecContext(ctx, "DELETE FROM VirtualRepo WHERE repo_id = ? OR origin_repo = ?", repoID, repoID); err != nil {
		return fmt.Errorf("failed to delete virtual repo records: %v", err)
	}

	// Mark for garbage collection
	// Non-fatal — GC will still find orphaned objects
	_, _ = tx.ExecContext(ctx, dbutil.InsertOrIgnore("GarbageRepos", "repo_id"), repoID)

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("failed to commit transaction: %v", err)
	}

	notify(OnRepoDeleted, repoID)
	return nil
}

// listVirtualRepoIDs returns the repos whose origin is repoID.
func listVirtualRepoIDs(repoID string) ([]string, error) {
	ctx, cancel := option.WithDBTimeout(context.Background())
	defer cancel()

	rows, err := seafileDB.QueryContext(ctx,
		"SELECT repo_id FROM VirtualRepo WHERE origin_repo = ?", repoID)
	if err != nil {
		return nil, fmt.Errorf("failed to list virtual repos of %s: %v", repoID, err)
	}
	defer func() { _ = rows.Close() }()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("failed to read virtual repo row: %v", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// OnRepoDeleted and OnTokensRevoked let the fileserver drop cached
// authorisations the moment the rows they were derived from go away. Without
// them a cached token or permission stays authoritative for its full TTL,
// which for a deletion means the server keeps accepting uploads to a library
// that no longer exists.
//
// They are package variables rather than a direct call because repomgr sits
// below the fileserver package and cannot import it. Nil until the server
// registers them, so the CLI paths — which have no caches — need no wiring.
var (
	OnRepoDeleted   func(repoID string)
	OnTokensRevoked func(id account.ID)
)

// notify fires a registered hook, if one is registered. It is generic because
// the hooks differ only in what they carry — a repo id, an account id — and a
// copy per argument type is a copy per future hook.
func notify[T any](hook func(T), arg T) {
	if hook != nil {
		hook(arg)
	}
}
