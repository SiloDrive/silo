package silod

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dkam/silo/fileserver/account"
	"github.com/dkam/silo/fileserver/blockmgr"
	"github.com/dkam/silo/fileserver/commitmgr"
	"github.com/dkam/silo/fileserver/diff"
	"github.com/dkam/silo/fileserver/fsmgr"
	"github.com/dkam/silo/fileserver/option"
	"github.com/dkam/silo/fileserver/repomgr"
	"github.com/dkam/silo/fileserver/share"
	"github.com/dkam/silo/fileserver/utils"
	"github.com/dkam/silo/fileserver/workerpool"
	"github.com/gorilla/mux"
	log "github.com/sirupsen/logrus"
)

type checkExistType int32

const (
	checkFSExist    checkExistType = 0
	checkBlockExist checkExistType = 1
)

const (
	emptySHA1 = "0000000000000000000000000000000000000000"
	// The token and permission caches are bounded by option.AuthCacheTTL,
	// which is a security window and configurable. This one is not: a repo's
	// store id never changes while the repo exists, and repo deletion evicts
	// the entry outright.
	virtualRepoExpireTime      = 7200
	syncAPICleaningIntervalSec = 300
	maxObjectPackSize          = 1 << 20 // 1MB
	fsIdWorkers                = 10
)

// Body limits for the sync endpoints. server.go caps only MaxHeaderBytes, and
// the 1MB MaxBytesReader it installs covers the /api/silo/v1 JSON routes
// only — /seafhttp had no limit at all, so one request from any client with
// write access to one library could make the server allocate until it was
// OOM-killed.
//
// The limits are set well above what the protocol produces rather than tight
// to it, because the cost of being wrong is a client that cannot sync.
const (
	// A commit is a small JSON object: ids, timestamps and a description.
	maxCommitBodySize = 1 << 20 // 1MB
	// Lists of 40-char object or repo ids. 16MB is roughly 380k ids.
	maxIDListBodySize = 16 << 20 // 16MB
	// A pack of fs objects. The server builds its own packs to
	// maxObjectPackSize (1MB) and clients do the same, except that a single
	// object larger than that is sent alone — so the real bound is one
	// maximum-size fs object, well inside this.
	maxFSPackBodySize = 16 << 20 // 16MB
)

// readLimitedBody reads an entire request body, refusing anything past limit.
func readLimitedBody(rsp http.ResponseWriter, r *http.Request, limit int64) ([]byte, *appError) {
	data, err := io.ReadAll(http.MaxBytesReader(rsp, r.Body, limit))
	if err != nil {
		return nil, bodyLimitError(err)
	}
	return data, nil
}

// decodeLimitedJSON decodes a JSON request body into v, refusing anything past
// limit. The decoder streams, so the limit bounds the allocation rather than
// only rejecting it after the fact.
func decodeLimitedJSON(rsp http.ResponseWriter, r *http.Request, limit int64, v any) *appError {
	if err := json.NewDecoder(http.MaxBytesReader(rsp, r.Body, limit)).Decode(v); err != nil {
		return bodyLimitError(err)
	}
	return nil
}

func bodyLimitError(err error) *appError {
	var maxErr *http.MaxBytesError
	if errors.As(err, &maxErr) {
		msg := fmt.Sprintf("Request body exceeds %d bytes", maxErr.Limit)
		return &appError{nil, msg, http.StatusRequestEntityTooLarge}
	}
	return &appError{nil, err.Error(), http.StatusBadRequest}
}

var (
	tokenCache           sync.Map
	permCache            sync.Map
	virtualRepoInfoCache sync.Map
	calFsIdPool          *workerpool.WorkPool
)

type tokenInfo struct {
	repoID     string
	acct       *account.Account
	expireTime int64
}

// permInfo is a cached "this check passed", nothing more. The permission
// string itself is not kept: the cache key already carries the operation it
// was checked for, so a hit is only ever asked whether it is still fresh.
type permInfo struct {
	expireTime int64
}

type virtualRepoInfo struct {
	storeID    string
	expireTime int64
}

type repoEventData struct {
	eType      string
	user       string
	ip         string
	repoID     string
	path       string
	clientName string
}

type statsEventData struct {
	eType  string
	user   string
	repoID string
	bytes  uint64
}

func syncAPIInit() {
	ticker := time.NewTicker(time.Second * syncAPICleaningIntervalSec)
	go RecoverWrapper(func() {
		for range ticker.C {
			removeSyncAPIExpireCache()
		}
	})

	calFsIdPool = workerpool.CreateWorkerPool(getFsId, fsIdWorkers)
}

// calResult is how the worker pool hands a request's outcome back to the
// handler waiting on it. Only the error crosses: the caller already has the
// request.
type calResult struct {
	err *appError
}

func getFsId(args ...interface{}) error {
	if len(args) < 3 {
		return nil
	}

	resChan := args[0].(chan *calResult)
	rsp := args[1].(http.ResponseWriter)
	r := args[2].(*http.Request)

	queries := r.URL.Query()

	serverHead := queries.Get("server-head")
	if !utils.IsObjectIDValid(serverHead) {
		msg := "Invalid server-head parameter."
		appErr := &appError{nil, msg, http.StatusBadRequest}
		resChan <- &calResult{appErr}
		return nil
	}

	clientHead := queries.Get("client-head")
	if clientHead != "" && !utils.IsObjectIDValid(clientHead) {
		msg := "Invalid client-head parameter."
		appErr := &appError{nil, msg, http.StatusBadRequest}
		resChan <- &calResult{appErr}
		return nil
	}

	dirOnlyArg := queries.Get("dir-only")
	var dirOnly bool
	if dirOnlyArg != "" {
		dirOnly = true
	}

	vars := mux.Vars(r)
	repoID := vars["repoid"]
	user, appErr := validateToken(r, repoID, false)
	if appErr != nil {
		resChan <- &calResult{appErr}
		return nil
	}
	appErr = checkPermission(repoID, user.ID, "download", false)
	if appErr != nil {
		resChan <- &calResult{appErr}
		return nil
	}
	repo := repomgr.Get(repoID)
	if repo == nil {
		err := fmt.Errorf("failed to find repo %.8s", repoID)
		appErr := &appError{err, "", http.StatusInternalServerError}
		resChan <- &calResult{appErr}
		return nil
	}
	ret, err := calculateSendObjectList(r.Context(), repo, serverHead, clientHead, dirOnly)
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			err := fmt.Errorf("failed to get fs id list: %w", err)
			appErr := &appError{err, "", http.StatusInternalServerError}
			resChan <- &calResult{appErr}
			return nil
		}
		appErr := &appError{nil, "", http.StatusInternalServerError}
		resChan <- &calResult{appErr}
		return nil
	}

	var objList []byte
	if ret != nil {
		objList, err = json.Marshal(ret)
		if err != nil {
			appErr := &appError{err, "", http.StatusInternalServerError}
			resChan <- &calResult{appErr}
			return nil
		}
	} else {
		// when get obj list is nil, return []
		objList = []byte{'[', ']'}
	}

	rsp.Header().Set("Content-Length", strconv.Itoa(len(objList)))
	rsp.WriteHeader(http.StatusOK)
	_, _ = rsp.Write(objList)

	resChan <- &calResult{nil}

	return nil
}

func permissionCheckCB(rsp http.ResponseWriter, r *http.Request) *appError {
	queries := r.URL.Query()

	op := queries.Get("op")
	if op != "download" && op != "upload" {
		msg := "op is invalid"
		return &appError{nil, msg, http.StatusBadRequest}
	}

	clientID := queries.Get("client_id")
	if clientID != "" && len(clientID) != 40 {
		msg := "client_id is invalid"
		return &appError{nil, msg, http.StatusBadRequest}
	}

	clientVer := queries.Get("client_ver")
	if clientVer != "" {
		status := validateClientVer(clientVer)
		if status != http.StatusOK {
			msg := "client_ver is invalid"
			return &appError{nil, msg, status}
		}
	}

	clientName := queries.Get("client_name")
	if clientName != "" {
		clientName = html.UnescapeString(clientName)
	}

	vars := mux.Vars(r)
	repoID := vars["repoid"]
	repo := repomgr.GetEx(repoID)
	if repo == nil {
		msg := "repo was deleted"
		return &appError{nil, msg, seafHTTPResRepoDeleted}
	}

	if repo.IsCorrupted {
		msg := "repo was corrupted"
		return &appError{nil, msg, seafHTTPResRepoCorrupted}
	}

	user, err := validateToken(r, repoID, true)
	if err != nil {
		return err
	}
	err = checkPermission(repoID, user.ID, op, true)
	if err != nil {
		return err
	}
	// Same resolver, and so the same proxy-trust policy, as the login limiter:
	// this address is stored as the token's peer and reported in the sync
	// event, so an unconditionally trusted X-Forwarded-For would let any
	// client with a sync token forge both.
	ip := utils.ClientIP(r, option.TrustProxyHeaders)

	if op == "download" {
		onRepoOper("repo-download-sync", repoID, user.Email, ip, clientName)
	}
	if clientID != "" && clientName != "" {
		token := r.Header.Get("Seafile-Repo-Token")
		exists, err := repomgr.TokenPeerInfoExists(token)
		if err != nil {
			err := fmt.Errorf("failed to check token peer info for repo %s: %v", repoID, err)
			return &appError{err, "", http.StatusInternalServerError}
		}
		if !exists {
			if err := repomgr.AddTokenPeerInfo(token, clientID, ip, clientName, clientVer, int64(time.Now().Unix())); err != nil {
				err := fmt.Errorf("failed to add token peer info: %v", err)
				return &appError{err, "", http.StatusInternalServerError}
			}
		} else {
			if err := repomgr.UpdateTokenPeerInfo(token, clientID, clientVer, int64(time.Now().Unix())); err != nil {
				err := fmt.Errorf("failed to update token peer info: %v", err)
				return &appError{err, "", http.StatusInternalServerError}
			}
		}
	}
	return nil
}
func getBlockMapCB(rsp http.ResponseWriter, r *http.Request) *appError {
	vars := mux.Vars(r)
	repoID := vars["repoid"]
	fileID := vars["id"]

	user, appErr := validateToken(r, repoID, false)
	if appErr != nil {
		return appErr
	}
	appErr = checkPermission(repoID, user.ID, "download", false)
	if appErr != nil {
		return appErr
	}

	storeID, err := getRepoStoreID(repoID)
	if err != nil {
		err := fmt.Errorf("failed to get repo store id by repo id %s: %v", repoID, err)
		return &appError{err, "", http.StatusInternalServerError}
	}

	seafile, err := fsmgr.GetSeafile(storeID, fileID)
	if err != nil {
		msg := fmt.Sprintf("Failed to get seafile object by file id %s: %v", fileID, err)
		return &appError{nil, msg, http.StatusNotFound}
	}

	blockSizes := []int64{}
	for _, blockID := range seafile.BlkIDs {
		blockSize, err := blockmgr.Stat(storeID, blockID)
		if err != nil {
			err := fmt.Errorf("failed to find block %s/%s", storeID, blockID)
			return &appError{err, "", http.StatusInternalServerError}
		}
		blockSizes = append(blockSizes, blockSize)
	}

	return writeJSON(rsp, blockSizes)
}

func getAccessibleRepoListCB(rsp http.ResponseWriter, r *http.Request) *appError {
	queries := r.URL.Query()
	repoID := queries.Get("repo_id")

	if repoID == "" || !utils.IsValidUUID(repoID) {
		msg := "Invalid repo id."
		return &appError{nil, msg, http.StatusBadRequest}
	}

	user, appErr := validateToken(r, repoID, false)
	if appErr != nil {
		return appErr
	}

	obtainedRepos := make(map[string]string)

	repos, err := share.GetReposByOwner(user.ID)
	if err != nil {
		err := fmt.Errorf("failed to get repos by owner %s: %v", user.Email, err)
		return &appError{err, "", http.StatusInternalServerError}
	}

	repoObjects := []*share.SharedRepo{}
	for _, repo := range repos {
		if repo.RepoType != "" {
			continue
		}
		if _, ok := obtainedRepos[repo.ID]; !ok {
			obtainedRepos[repo.ID] = repo.ID
		}
		repo.Permission = "rw"
		repo.Type = "repo"
		repo.Owner = user.Email
		repoObjects = append(repoObjects, repo)
	}

	repos, err = share.ListShareRepos(user.ID, share.SharedWithMe)
	if err != nil {
		err := fmt.Errorf("failed to get share repos by user %s: %v", user.Email, err)
		return &appError{err, "", http.StatusInternalServerError}
	}
	for _, sRepo := range repos {
		if _, ok := obtainedRepos[sRepo.ID]; ok {
			continue
		}
		if sRepo.RepoType != "" {
			continue
		}
		sRepo.Type = "srepo"
		sRepo.Owner = strings.ToLower(sRepo.Owner)
		repoObjects = append(repoObjects, sRepo)
	}

	repos, err = share.GetGroupReposByUser(user.ID)
	if err != nil {
		err := fmt.Errorf("failed to get group repos by user %s: %v", user.Email, err)
		return &appError{err, "", http.StatusInternalServerError}
	}
	reposTable := filterGroupRepos(repos)

	for _, gRepo := range reposTable {
		if _, ok := obtainedRepos[gRepo.ID]; ok {
			continue
		}

		gRepo.Type = "grepo"
		gRepo.Owner = strings.ToLower(gRepo.Owner)
		repoObjects = append(repoObjects, gRepo)
	}

	repos, err = share.ListInnerPubRepos()
	if err != nil {
		err := fmt.Errorf("failed to get inner public repos: %v", err)
		return &appError{err, "", http.StatusInternalServerError}
	}

	for _, sRepo := range repos {
		if _, ok := obtainedRepos[sRepo.ID]; ok {
			continue
		}
		if sRepo.RepoType != "" {
			continue
		}

		sRepo.Type = "grepo"
		sRepo.Owner = "Organization"
		repoObjects = append(repoObjects, sRepo)
	}

	return writeJSON(rsp, repoObjects)
}

func filterGroupRepos(repos []*share.SharedRepo) map[string]*share.SharedRepo {
	table := make(map[string]*share.SharedRepo)

	for _, repo := range repos {
		if repo.RepoType != "" {
			continue
		}
		if repoPrev, ok := table[repo.ID]; ok {
			if repo.Permission == "rw" && repoPrev.Permission == "r" {
				table[repo.ID] = repo
			}
		} else {
			table[repo.ID] = repo
		}
	}

	return table
}

func recvFSCB(rsp http.ResponseWriter, r *http.Request) *appError {
	vars := mux.Vars(r)
	repoID := vars["repoid"]

	user, appErr := validateToken(r, repoID, false)
	if appErr != nil {
		return appErr
	}

	appErr = checkPermission(repoID, user.ID, "upload", false)
	if appErr != nil {
		return appErr
	}

	storeID, err := getRepoStoreID(repoID)
	if err != nil {
		err := fmt.Errorf("failed to get repo store id by repo id %s: %v", repoID, err)
		return &appError{err, "", http.StatusInternalServerError}
	}
	fsBuf, appErr := readLimitedBody(rsp, r, maxFSPackBodySize)
	if appErr != nil {
		return appErr
	}

	// One inflate window for the whole pack. A pack holds up to
	// maxFSPackBodySize of small objects, so taking a reader per object would
	// allocate one window each — thousands per request on a path that
	// verifies by default.
	zlibReader := fsmgr.GetOneZlibReader()
	defer fsmgr.ReturnOneZlibReader(zlibReader)

	for len(fsBuf) > 44 {
		objID := string(fsBuf[:40])
		if !utils.IsObjectIDValid(objID) {
			msg := fmt.Sprintf("Fs obj id %s is invalid", objID)
			return &appError{nil, msg, http.StatusBadRequest}
		}

		var objSize uint32
		sizeBuffer := bytes.NewBuffer(fsBuf[40:44])
		if err := binary.Read(sizeBuffer, binary.BigEndian, &objSize); err != nil {
			msg := fmt.Sprintf("Failed to read fs obj size: %v", err)
			return &appError{nil, msg, http.StatusBadRequest}
		}

		if len(fsBuf) < int(44+objSize) {
			msg := "Request body size invalid"
			return &appError{nil, msg, http.StatusBadRequest}
		}

		objData := fsBuf[44 : 44+objSize]
		if err := fsmgr.WriteRawIngested(storeID, objID, objData, zlibReader); err != nil {
			if errors.Is(err, fsmgr.ErrVerification) {
				log.Warnf("rejecting fs object for repo %s: %v", repoID, err)
				return &appError{nil, "Fs object does not match its id", http.StatusBadRequest}
			}
			err := fmt.Errorf("failed to write fs obj %s:%s : %v", storeID, objID, err)
			return &appError{err, "", http.StatusInternalServerError}
		}
		fsBuf = fsBuf[44+objSize:]
	}
	if len(fsBuf) == 0 {
		rsp.WriteHeader(http.StatusOK)
		return nil
	}

	msg := "Request body size invalid"
	return &appError{nil, msg, http.StatusBadRequest}
}
func checkFSCB(rsp http.ResponseWriter, r *http.Request) *appError {
	return postCheckExistCB(rsp, r, checkFSExist)
}

func checkBlockCB(rsp http.ResponseWriter, r *http.Request) *appError {
	return postCheckExistCB(rsp, r, checkBlockExist)
}

func postCheckExistCB(rsp http.ResponseWriter, r *http.Request, existType checkExistType) *appError {
	vars := mux.Vars(r)
	repoID := vars["repoid"]

	user, appErr := validateToken(r, repoID, false)
	if appErr != nil {
		return appErr
	}
	appErr = checkPermission(repoID, user.ID, "download", false)
	if appErr != nil {
		return appErr
	}

	storeID, err := getRepoStoreID(repoID)
	if err != nil {
		err := fmt.Errorf("failed to get repo store id by repo id %s: %v", repoID, err)
		return &appError{err, "", http.StatusInternalServerError}
	}

	var objIDList []string
	if appErr := decodeLimitedJSON(rsp, r, maxIDListBodySize, &objIDList); appErr != nil {
		return appErr
	}

	neededObjs := []string{}
	var ret bool
	for i := 0; i < len(objIDList); i++ {
		if !utils.IsObjectIDValid(objIDList[i]) {
			continue
		}
		switch existType {
		case checkFSExist:
			ret, _ = fsmgr.Exists(storeID, objIDList[i])
		case checkBlockExist:
			ret = blockmgr.Exists(storeID, objIDList[i])
		}
		if !ret {
			neededObjs = append(neededObjs, objIDList[i])
		}
	}

	return writeJSON(rsp, neededObjs)
}

func packFSCB(rsp http.ResponseWriter, r *http.Request) *appError {
	vars := mux.Vars(r)
	repoID := vars["repoid"]

	user, appErr := validateToken(r, repoID, false)
	if appErr != nil {
		return appErr
	}
	appErr = checkPermission(repoID, user.ID, "download", false)
	if appErr != nil {
		return appErr
	}

	storeID, err := getRepoStoreID(repoID)
	if err != nil {
		err := fmt.Errorf("failed to get repo store id by repo id %s: %v", repoID, err)
		return &appError{err, "", http.StatusInternalServerError}
	}

	var fsIDList []string
	if appErr := decodeLimitedJSON(rsp, r, maxIDListBodySize, &fsIDList); appErr != nil {
		return appErr
	}

	var totalSize int
	var data bytes.Buffer
	for i := 0; i < len(fsIDList); i++ {
		if !utils.IsObjectIDValid(fsIDList[i]) {
			msg := fmt.Sprintf("Invalid fs id %s", fsIDList[i])
			return &appError{nil, msg, http.StatusBadRequest}
		}
		data.WriteString(fsIDList[i])
		var tmp bytes.Buffer
		if err := fsmgr.ReadRaw(storeID, fsIDList[i], &tmp); err != nil {
			err := fmt.Errorf("failed to read fs %s:%s: %v", storeID, fsIDList[i], err)
			return &appError{err, "", http.StatusInternalServerError}
		}
		tmpLen := make([]byte, 4)
		binary.BigEndian.PutUint32(tmpLen, uint32(tmp.Len()))
		data.Write(tmpLen)
		data.Write(tmp.Bytes())

		totalSize += tmp.Len()
		if totalSize >= maxObjectPackSize {
			break
		}
	}

	rsp.Header().Set("Content-Length", strconv.Itoa(data.Len()))
	rsp.WriteHeader(http.StatusOK)
	_, _ = rsp.Write(data.Bytes())
	return nil
}

// headCommitsMultiCB answers the batched head poll: given a list of repo ids,
// return each one's head commit.
//
// It answers only for the repos the caller can actually read. Unauthenticated
// — which is how upstream ships it, and how this did — it is an oracle:
// anyone who can reach the port and knows a repo's id learns whether that
// library exists and watches its head move, which is its activity, without
// ever holding a credential. It is also an unauthenticated way to make the
// server run a large IN query.
//
// The caller is identified from a sync token by value rather than against a
// named repo, because the whole point of the endpoint is that many repos come
// in one request. Each repo is then permission-checked individually, so
// holding a token for one library does not reveal anything about another.
func headCommitsMultiCB(rsp http.ResponseWriter, r *http.Request) *appError {
	token := r.Header.Get("Seafile-Repo-Token")
	if token == "" {
		token = utils.GetAuthorizationToken(r.Header)
	}
	if token == "" {
		return &appError{nil, "token is null", http.StatusBadRequest}
	}
	user, err := repomgr.GetAccountForToken(token)
	if err != nil {
		log.Errorf("Failed to resolve token for head-commits-multi: %v", err)
		return &appError{err, "", http.StatusInternalServerError}
	}
	if user.IsZero() {
		return &appError{nil, "Invalid token", http.StatusForbidden}
	}

	var repoIDList []string
	if appErr := decodeLimitedJSON(rsp, r, maxIDListBodySize, &repoIDList); appErr != nil {
		return appErr
	}
	if len(repoIDList) == 0 {
		return &appError{nil, "", http.StatusBadRequest}
	}

	var repoIDs strings.Builder
	var allowed int
	for i := 0; i < len(repoIDList); i++ {
		if !utils.IsValidUUID(repoIDList[i]) {
			return &appError{nil, "", http.StatusBadRequest}
		}
		// Filtered before the query rather than after, so an unreadable repo
		// is not even looked up.
		if checkPermission(repoIDList[i], user, "download", false) != nil {
			continue
		}
		if allowed > 0 {
			repoIDs.WriteString(",")
		}
		fmt.Fprintf(&repoIDs, "'%s'", repoIDList[i])
		allowed++
	}

	// Nothing the caller may read. An empty map rather than an error: which
	// of the ids were rejected is itself the thing not to disclose.
	if allowed == 0 {
		return writeJSON(rsp, map[string]string{})
	}

	// No shared-lock clause: SQLite has no row locks and no such syntax, and
	// needs no substitute. Under WAL a reader sees a consistent snapshot
	// without blocking, and writes are already serialised onto the single
	// write connection.
	sqlStr := fmt.Sprintf(
		"SELECT repo_id, commit_id FROM Branch WHERE name='master' AND "+
			"repo_id IN (%s)",
		repoIDs.String())

	ctx, cancel := context.WithTimeout(context.Background(), option.DBOpTimeout)
	defer cancel()
	rows, err := siloPair.Read.QueryContext(ctx, sqlStr)
	if err != nil {
		err := fmt.Errorf("failed to get commit id: %v", err)
		return &appError{err, "", http.StatusInternalServerError}
	}

	defer func() { _ = rows.Close() }()

	commitIDMap := make(map[string]string)
	var repoID string
	var commitID string
	for rows.Next() {
		if err := rows.Scan(&repoID, &commitID); err == nil {
			commitIDMap[repoID] = commitID
		}
	}

	if err := rows.Err(); err != nil {
		err := fmt.Errorf("failed to get commit id: %v", err)
		return &appError{err, "", http.StatusInternalServerError}
	}

	return writeJSON(rsp, commitIDMap)
}

// writeJSON sends v as the whole 200 response.
//
// Slices reaching here must be non-nil: a nil slice marshals to "null", and
// the sync clients expect "[]". Declaring them empty rather than nil is what
// keeps that true, and is why this takes any value rather than each handler
// special-casing the empty case on its way out.
func writeJSON(rsp http.ResponseWriter, v any) *appError {
	data, err := json.Marshal(v)
	if err != nil {
		err := fmt.Errorf("failed to marshal json: %v", err)
		return &appError{err, "", http.StatusInternalServerError}
	}

	rsp.Header().Set("Content-Length", strconv.Itoa(len(data)))
	rsp.WriteHeader(http.StatusOK)
	_, _ = rsp.Write(data)
	return nil
}

func getCheckQuotaCB(rsp http.ResponseWriter, r *http.Request) *appError {
	vars := mux.Vars(r)
	repoID := vars["repoid"]

	if _, err := validateToken(r, repoID, false); err != nil {
		return err
	}

	queries := r.URL.Query()
	delta := queries.Get("delta")
	if delta == "" {
		msg := "Invalid delta parameter"
		return &appError{nil, msg, http.StatusBadRequest}
	}

	deltaNum, err := strconv.ParseInt(delta, 10, 64)
	if err != nil {
		msg := "Invalid delta parameter"
		return &appError{nil, msg, http.StatusBadRequest}
	}

	ret, err := checkQuota(repoID, deltaNum)
	if err != nil {
		msg := "Internal error.\n"
		err := fmt.Errorf("failed to check quota: %v", err)
		return &appError{err, msg, http.StatusInternalServerError}
	}
	if ret == 1 {
		msg := "Out of quota.\n"
		return &appError{nil, msg, seafHTTPResNoQuota}
	}

	return nil
}

func getJWTTokenCB(rsp http.ResponseWriter, r *http.Request) *appError {
	vars := mux.Vars(r)
	repoID := vars["repoid"]

	if !option.EnableNotification {
		return &appError{nil, "", http.StatusNotFound}
	}

	user, appErr := validateToken(r, repoID, false)
	if appErr != nil {
		return appErr
	}

	exp := time.Now().Add(time.Hour * 72).Unix()
	tokenString, err := utils.GenNotifJWTToken(repoID, user.Email, exp)
	if err != nil {
		return &appError{err, "", http.StatusInternalServerError}
	}

	data := fmt.Sprintf("{\"jwt_token\":\"%s\"}", tokenString)

	_, _ = rsp.Write([]byte(data))

	return nil
}

func getFsObjIDCB(rsp http.ResponseWriter, r *http.Request) *appError {
	recvChan := make(chan *calResult)

	calFsIdPool.AddTask(recvChan, rsp, r)
	result := <-recvChan
	return result.err
}

func headCommitOperCB(rsp http.ResponseWriter, r *http.Request) *appError {
	switch r.Method {
	case http.MethodGet:
		return getHeadCommit(rsp, r)
	case http.MethodPut:
		return putUpdateBranchCB(rsp, r)
	}
	return &appError{nil, "", http.StatusBadRequest}
}

func commitOperCB(rsp http.ResponseWriter, r *http.Request) *appError {
	switch r.Method {
	case http.MethodGet:
		return getCommitInfo(rsp, r)
	case http.MethodPut:
		return putCommitCB(rsp, r)
	}
	return &appError{nil, "", http.StatusBadRequest}
}

func blockOperCB(rsp http.ResponseWriter, r *http.Request) *appError {
	switch r.Method {
	case http.MethodGet:
		return getBlockInfo(rsp, r)
	case http.MethodPut:
		return putSendBlockCB(rsp, r)
	}
	return &appError{nil, "", http.StatusBadRequest}
}

func putSendBlockCB(rsp http.ResponseWriter, r *http.Request) *appError {
	vars := mux.Vars(r)
	repoID := vars["repoid"]
	blockID := vars["id"]

	user, appErr := validateToken(r, repoID, false)
	if appErr != nil {
		return appErr
	}

	appErr = checkPermission(repoID, user.ID, "upload", false)
	if appErr != nil {
		return appErr
	}

	storeID, err := getRepoStoreID(repoID)
	if err != nil {
		err := fmt.Errorf("failed to get repo store id by repo id %s: %v", repoID, err)
		return &appError{err, "", http.StatusInternalServerError}
	}

	if err := blockmgr.Write(storeID, blockID, r.Body); err != nil {
		err := fmt.Errorf("failed to write block %.8s:%s: %v", storeID, blockID, err)
		return &appError{err, "", http.StatusInternalServerError}
	}

	sendStatisticMsg(storeID, user.Email, "sync-file-upload", uint64(r.ContentLength))

	return nil
}

func getBlockInfo(rsp http.ResponseWriter, r *http.Request) *appError {
	vars := mux.Vars(r)
	repoID := vars["repoid"]
	blockID := vars["id"]

	user, appErr := validateToken(r, repoID, false)
	if appErr != nil {
		return appErr
	}

	appErr = checkPermission(repoID, user.ID, "download", false)
	if appErr != nil {
		return appErr
	}

	storeID, err := getRepoStoreID(repoID)
	if err != nil {
		err := fmt.Errorf("failed to get repo store id by repo id %s: %v", repoID, err)
		return &appError{err, "", http.StatusInternalServerError}
	}

	blockSize, err := blockmgr.Stat(storeID, blockID)
	if err != nil {
		return &appError{err, "", http.StatusInternalServerError}
	}
	if blockSize <= 0 {
		err := fmt.Errorf("block %.8s:%s size invalid", storeID, blockID)
		return &appError{err, "", http.StatusInternalServerError}
	}

	blockLen := fmt.Sprintf("%d", blockSize)
	rsp.Header().Set("Content-Length", blockLen)
	if err := blockmgr.Read(storeID, blockID, rsp); err != nil {
		if !isNetworkErr(err) {
			log.Errorf("failed to read block %s: %v", blockID, err)
		}
		return nil
	}

	sendStatisticMsg(storeID, user.Email, "sync-file-download", uint64(blockSize))
	return nil
}

func getRepoStoreID(repoID string) (string, error) {
	var storeID string

	if value, ok := virtualRepoInfoCache.Load(repoID); ok {
		if info, ok := value.(*virtualRepoInfo); ok {
			if info.storeID != "" {
				storeID = info.storeID
			} else {
				storeID = repoID
			}
			info.expireTime = time.Now().Unix() + virtualRepoExpireTime
		}
	}
	if storeID != "" {
		return storeID, nil
	}

	var vInfo virtualRepoInfo
	var rID, originRepoID sql.NullString
	sqlStr := "SELECT repo_id, origin_repo FROM VirtualRepo where repo_id = ?"
	ctx, cancel := context.WithTimeout(context.Background(), option.DBOpTimeout)
	defer cancel()
	row := siloPair.Read.QueryRowContext(ctx, sqlStr, repoID)
	if err := row.Scan(&rID, &originRepoID); err != nil {
		if err == sql.ErrNoRows {
			vInfo.storeID = repoID
			vInfo.expireTime = time.Now().Unix() + virtualRepoExpireTime
			virtualRepoInfoCache.Store(repoID, &vInfo)
			return repoID, nil
		}
		return "", err
	}

	if !rID.Valid || !originRepoID.Valid {
		return "", nil
	}

	vInfo.storeID = originRepoID.String
	vInfo.expireTime = time.Now().Unix() + virtualRepoExpireTime
	virtualRepoInfoCache.Store(repoID, &vInfo)
	return originRepoID.String, nil
}

func sendStatisticMsg(repoID, user, operation string, bytes uint64) {
	rData := &statsEventData{operation, user, repoID, bytes}

	publishStatsEvent(rData)
}

func publishStatsEvent(rData *statsEventData) {
	log.Infof("stats event: type=%s user=%s repo=%s bytes=%d",
		rData.eType, rData.user, rData.repoID, rData.bytes)
}

func saveLastGCID(repoID, token string) error {
	repo := repomgr.Get(repoID)
	if repo == nil {
		return fmt.Errorf("failed to get repo: %s", repoID)
	}
	gcID, err := repomgr.GetCurrentGCID(repo.StoreID)
	if err != nil {
		return err
	}
	return repomgr.SetLastGCID(repoID, token, gcID)
}

func putCommitCB(rsp http.ResponseWriter, r *http.Request) *appError {
	vars := mux.Vars(r)
	repoID := vars["repoid"]
	commitID := vars["id"]
	user, appErr := validateToken(r, repoID, false)
	if appErr != nil {
		return appErr
	}
	appErr = checkPermission(repoID, user.ID, "upload", true)
	if appErr != nil {
		return appErr
	}

	data, appErr := readLimitedBody(rsp, r, maxCommitBodySize)
	if appErr != nil {
		return appErr
	}

	commit := new(commitmgr.Commit)
	if err := commit.FromData(data); err != nil {
		return &appError{nil, err.Error(), http.StatusBadRequest}
	}

	if commit.RepoID != repoID {
		msg := "The repo id in commit does not match current repo id"
		return &appError{nil, msg, http.StatusBadRequest}
	}

	// The commit is stored under the id in the URL while everything that
	// reads it goes by the id in the body. Letting the two disagree files a
	// commit under a name that does not describe it, permanently: the branch
	// head can then point at an id whose object says it is something else.
	if commit.CommitID != commitID {
		msg := "The commit id in the request does not match the commit"
		return &appError{nil, msg, http.StatusBadRequest}
	}

	if err := commitmgr.Save(commit); err != nil {
		err := fmt.Errorf("failed to add commit %s: %v", commitID, err)
		return &appError{err, "", http.StatusInternalServerError}
	} else {
		token := r.Header.Get("Seafile-Repo-Token")
		if token == "" {
			token = utils.GetAuthorizationToken(r.Header)
		}
		if err := saveLastGCID(repoID, token); err != nil {
			err := fmt.Errorf("failed to save gc id: %v", err)
			return &appError{err, "", http.StatusInternalServerError}
		}
	}

	return nil
}

func getCommitInfo(rsp http.ResponseWriter, r *http.Request) *appError {
	vars := mux.Vars(r)
	repoID := vars["repoid"]
	commitID := vars["id"]
	user, appErr := validateToken(r, repoID, false)
	if appErr != nil {
		return appErr
	}
	appErr = checkPermission(repoID, user.ID, "download", false)
	if appErr != nil {
		return appErr
	}
	if exists, _ := commitmgr.Exists(repoID, commitID); !exists {
		return &appError{nil, "", http.StatusNotFound}
	}

	var data bytes.Buffer
	err := commitmgr.ReadRaw(repoID, commitID, &data)
	if err != nil {
		err := fmt.Errorf("failed to read commit %s:%s: %v", repoID, commitID, err)
		return &appError{err, "", http.StatusInternalServerError}
	}

	dataLen := strconv.Itoa(data.Len())
	rsp.Header().Set("Content-Length", dataLen)
	rsp.WriteHeader(http.StatusOK)
	_, _ = rsp.Write(data.Bytes())

	return nil
}

func putUpdateBranchCB(rsp http.ResponseWriter, r *http.Request) *appError {
	queries := r.URL.Query()
	newCommitID := queries.Get("head")
	if newCommitID == "" || !utils.IsObjectIDValid(newCommitID) {
		msg := fmt.Sprintf("commit id %s is invalid", newCommitID)
		return &appError{nil, msg, http.StatusBadRequest}
	}

	vars := mux.Vars(r)
	repoID := vars["repoid"]
	user, appErr := validateToken(r, repoID, false)
	if appErr != nil {
		return appErr
	}

	appErr = checkPermission(repoID, user.ID, "upload", false)
	if appErr != nil && appErr.Code == http.StatusForbidden {
		return appErr
	}

	repo := repomgr.Get(repoID)
	if repo == nil {
		err := fmt.Errorf("repo %s is missing or corrupted", repoID)
		return &appError{err, "", http.StatusInternalServerError}
	}

	newCommit, err := commitmgr.Load(repoID, newCommitID)
	if err != nil {
		err := fmt.Errorf("failed to get commit %s for repo %s", newCommitID, repoID)
		return &appError{err, "", http.StatusInternalServerError}
	}

	base, err := commitmgr.Load(repoID, newCommit.ParentID.String)
	if err != nil {
		err := fmt.Errorf("failed to get commit %s for repo %s", newCommit.ParentID.String, repoID)
		return &appError{err, "", http.StatusInternalServerError}
	}

	if includeInvalidPath(base, newCommit) {
		msg := "Dir or file name is .."
		return &appError{nil, msg, http.StatusBadRequest}
	}

	ret, err := checkQuota(repoID, 0)
	if err != nil {
		err := fmt.Errorf("failed to check quota: %v", err)
		return &appError{err, "", http.StatusInternalServerError}
	}
	if ret == 1 {
		msg := "Out of quota.\n"
		return &appError{nil, msg, seafHTTPResNoQuota}
	}

	if option.VerifyClientBlocks {
		if body, err := checkBlocks(r.Context(), repo, base, newCommit); err != nil {
			return &appError{nil, body, seafHTTPResBlockMissing}
		}
	}

	token := r.Header.Get("Seafile-Repo-Token")
	if token == "" {
		token = utils.GetAuthorizationToken(r.Header)
	}
	if err := fastForwardOrMerge(user.Email, token, repo, base, newCommit); err != nil {
		if errors.Is(err, ErrGCConflict) {
			return &appError{nil, "GC Conflict.\n", http.StatusConflict}
		} else {
			err := fmt.Errorf("fast forward merge for repo %s is failed: %v", repoID, err)
			return &appError{err, "", http.StatusInternalServerError}
		}
	}

	go mergeVirtualRepoPool.AddTask(repoID, "")

	go updateSizePool.AddTask(repoID)

	rsp.WriteHeader(http.StatusOK)
	return nil
}

type checkBlockAux struct {
	storeID  string
	version  int
	fileList []string
}

func checkBlocks(ctx context.Context, repo *repomgr.Repo, base, remote *commitmgr.Commit) (string, error) {
	aux := new(checkBlockAux)
	aux.storeID = repo.StoreID
	aux.version = repo.Version
	opt := &diff.DiffOptions{
		FileCB: checkFileBlocks,
		DirCB:  checkDirCB,
		Ctx:    ctx,
		RepoID: repo.StoreID}
	opt.Data = aux

	trees := []string{base.RootID, remote.RootID}
	if err := diff.DiffTrees(trees, opt); err != nil {
		return "", err
	}

	if len(aux.fileList) == 0 {
		return "", nil
	}

	body, _ := json.Marshal(aux.fileList)

	return string(body), fmt.Errorf("block is missing")
}

func checkFileBlocks(ctx context.Context, baseDir string, files []*fsmgr.SeafDirent, data interface{}) error {
	select {
	case <-ctx.Done():
		return context.Canceled
	default:
	}

	file1 := files[0]
	file2 := files[1]

	aux, ok := data.(*checkBlockAux)
	if !ok {
		err := fmt.Errorf("failed to assert results")
		return err
	}

	if file2 == nil || file2.ID == emptySHA1 || (file1 != nil && file1.ID == file2.ID) {
		return nil
	}

	file, err := fsmgr.GetSeafile(aux.storeID, file2.ID)
	if err != nil {
		return err
	}
	for _, blkID := range file.BlkIDs {
		if !blockmgr.Exists(aux.storeID, blkID) {
			aux.fileList = append(aux.fileList, file2.Name)
			return nil
		}
	}

	return nil
}

func checkDirCB(ctx context.Context, baseDir string, dirs []*fsmgr.SeafDirent, data interface{}, recurse *bool) error {
	select {
	case <-ctx.Done():
		return context.Canceled
	default:
	}

	dir1 := dirs[0]
	dir2 := dirs[1]

	if dir1 == nil {
		// if dir2 is empty, stop diff.
		if dir2.ID == diff.EmptySha1 {
			*recurse = false
		} else {
			*recurse = true
		}
		return nil
	}

	// if dir2 is not exist, stop diff.
	if dir2 == nil {
		*recurse = false
		return nil
	}

	// if dir1 and dir2 are the same or dir2 is empty, stop diff.
	if dir1.ID == dir2.ID || dir2.ID == diff.EmptySha1 {
		*recurse = false
		return nil
	}

	return nil
}

func includeInvalidPath(baseCommit, newCommit *commitmgr.Commit) bool {
	var results []*diff.DiffEntry
	if err := diff.DiffCommits(baseCommit, newCommit, &results, true); err != nil {
		log.Infof("Failed to diff commits: %v", err)
		return false
	}

	for _, entry := range results {
		if entry.NewName != "" {
			if shouldIgnore(entry.NewName) {
				return true
			}
		} else {
			if shouldIgnore(entry.Name) {
				return true
			}
		}
	}

	return false
}

// getHeadCommit returns a repo's head commit, or the deleted status if the
// repo is gone.
//
// The existence check deliberately runs before the token check, which does
// mean anyone holding a repo id learns whether that library still exists.
// That cannot be closed by reordering: DeleteRepo removes the repo's
// RepoUserToken rows along with everything else, so after a deletion there is
// no credential left to authenticate with. Requiring one first would turn
// every deleted library into a 403, and a client that cannot tell "deleted"
// from "not yours" never removes it — the library would sit in the client
// forever, retrying.
//
// What is not disclosed is the head itself, which is behind validateToken
// below. Existence alone, to someone who already knows a 122-bit id, is the
// price of the client being able to clean up.
func getHeadCommit(rsp http.ResponseWriter, r *http.Request) *appError {
	vars := mux.Vars(r)
	repoID := vars["repoid"]
	sqlStr := "SELECT EXISTS(SELECT 1 FROM Repo WHERE repo_id=?)"
	var exists bool
	ctx, cancel := context.WithTimeout(context.Background(), option.DBOpTimeout)
	defer cancel()
	row := siloPair.Read.QueryRowContext(ctx, sqlStr, repoID)
	if err := row.Scan(&exists); err != nil {
		if err != sql.ErrNoRows {
			log.Errorf("DB error when check repo %s existence: %v", repoID, err)
			msg := `{"is_corrupted": 1}`
			rsp.WriteHeader(http.StatusOK)
			_, _ = rsp.Write([]byte(msg))
			return nil
		}
	}
	if !exists {
		return &appError{nil, "", seafHTTPResRepoDeleted}
	}

	if _, err := validateToken(r, repoID, false); err != nil {
		return err
	}

	var commitID string
	sqlStr = "SELECT commit_id FROM Branch WHERE name='master' AND repo_id=?"
	row = siloPair.Read.QueryRowContext(ctx, sqlStr, repoID)

	if err := row.Scan(&commitID); err != nil {
		if err != sql.ErrNoRows {
			log.Errorf("DB error when get branch master: %v", err)
			msg := `{"is_corrupted": 1}`
			rsp.WriteHeader(http.StatusOK)
			_, _ = rsp.Write([]byte(msg))
			return nil
		}
	}
	if commitID == "" {
		return &appError{nil, "", http.StatusBadRequest}
	}

	msg := fmt.Sprintf("{\"is_corrupted\": 0, \"head_commit_id\": \"%s\"}", commitID)
	rsp.WriteHeader(http.StatusOK)
	_, _ = rsp.Write([]byte(msg))
	return nil
}

func checkPermission(repoID string, user account.ID, op string, skipCache bool) *appError {
	key := fmt.Sprintf("%s:%s:%s", repoID, user, op)
	if !skipCache {
		if value, ok := permCache.Load(key); ok {
			// The expiry is checked here, not only by the sweeper. A hit used
			// to be served without looking at it, so an entry stayed
			// authoritative for its whole life plus however long until the
			// next sweep — a permission withdrawn, or a library deleted, kept
			// authorising uploads for that entire window.
			if info, ok := value.(*permInfo); ok && info.expireTime > time.Now().Unix() {
				return nil
			}
		}
	}

	permCache.Delete(key)

	if op == "upload" {
		status, err := repomgr.GetRepoStatus(repoID)
		if err != nil {
			msg := fmt.Sprintf("Failed to get repo status by repo id %s: %v", repoID, err)
			return &appError{nil, msg, http.StatusForbidden}
		}
		if status != repomgr.RepoStatusNormal && status != -1 {
			return &appError{nil, "", http.StatusForbidden}
		}
	}

	perm := share.CheckPerm(repoID, user)
	if perm != "" {
		if perm == "r" && op == "upload" {
			return &appError{nil, "", http.StatusForbidden}
		}
		if expireTime, caching := authCacheExpiry(); caching {
			permCache.Store(key, &permInfo{expireTime: expireTime})
		}
		return nil
	}

	return &appError{nil, "", http.StatusForbidden}
}

// validateToken resolves a sync token to the account holding it.
//
// It returns the whole account because both halves are needed downstream: the
// id is what a permission check keys on, and the address is what goes into a
// commit as its author and into the statistics stream. Neither is derivable
// from the other without another query, so the one lookup returns both.
func validateToken(r *http.Request, repoID string, skipCache bool) (*account.Account, *appError) {
	token := r.Header.Get("Seafile-Repo-Token")
	if token == "" {
		token = utils.GetAuthorizationToken(r.Header)
		if token == "" {
			msg := "token is null"
			return nil, &appError{nil, msg, http.StatusBadRequest}
		}
	}

	if !skipCache {
		if value, ok := tokenCache.Load(token); ok {
			if info, ok := value.(*tokenInfo); ok && info.expireTime > time.Now().Unix() {
				if info.repoID != repoID {
					msg := "Invalid token"
					return nil, &appError{nil, msg, http.StatusForbidden}
				}
				return info.acct, nil
			}
		}
	}

	id, err := repomgr.GetAccountByToken(repoID, token)
	if err != nil {
		// The token is a bearer credential — log the repo instead, which is
		// the useful correlation key and not a secret.
		log.Errorf("Failed to resolve token for repo %s: %v", repoID, err)
		tokenCache.Delete(token)
		return nil, &appError{err, "", http.StatusInternalServerError}
	}
	if id.IsZero() {
		tokenCache.Delete(token)
		msg := "Invalid token"
		return nil, &appError{nil, msg, http.StatusForbidden}
	}

	ctx, cancel := context.WithTimeout(r.Context(), option.DBOpTimeout)
	defer cancel()
	acct, err := account.ByID(ctx, id)
	if err != nil {
		tokenCache.Delete(token)
		msg := "Invalid token"
		return nil, &appError{nil, msg, http.StatusForbidden}
	}
	// A sync token outlives a password change by design, so deactivating the
	// account is the only thing that stops it. Asking here is what makes that
	// true for the sync lane as well as the API one.
	if !acct.IsActive {
		tokenCache.Delete(token)
		msg := "Invalid token"
		return nil, &appError{nil, msg, http.StatusForbidden}
	}

	if expireTime, caching := authCacheExpiry(); caching {
		tokenCache.Store(token, &tokenInfo{acct: acct, expireTime: expireTime, repoID: repoID})
	}

	return acct, nil
}

// authCacheExpiry returns the expiry stamp for a new auth cache entry, and
// whether caching is on at all. option.AuthCacheTTL of zero means every
// request re-checks the database, which is the only way to make a revocation
// made outside this process take effect instantly.
func authCacheExpiry() (int64, bool) {
	if option.AuthCacheTTL <= 0 {
		return 0, false
	}
	return time.Now().Add(option.AuthCacheTTL).Unix(), true
}

// invalidateRepoAuth drops every cached authorisation for a repository. It
// runs when the repo is deleted: a cached permission outlives the rows it was
// derived from, and would keep authorising uploads to a library that no
// longer exists — recreating the storage directories `silo gc -delete` had
// just reclaimed.
func invalidateRepoAuth(repoID string) {
	deleteCachedTokens(func(info *tokenInfo) bool { return info.repoID == repoID })
	permCache.Range(func(key, value interface{}) bool {
		if k, ok := key.(string); ok && strings.HasPrefix(k, repoID+":") {
			permCache.Delete(key)
		}
		return true
	})
	virtualRepoInfoCache.Delete(repoID)
}

// invalidateUserAuth drops every cached token belonging to an account, so a
// revocation this process performs takes effect on the next request rather
// than at the next cache expiry.
func invalidateUserAuth(id account.ID) {
	deleteCachedTokens(func(info *tokenInfo) bool { return info.acct != nil && info.acct.ID == id })
}

// deleteCachedTokens drops every cached token entry that match selects.
func deleteCachedTokens(match func(*tokenInfo) bool) {
	tokenCache.Range(func(key, value interface{}) bool {
		if info, ok := value.(*tokenInfo); ok && match(info) {
			tokenCache.Delete(key)
		}
		return true
	})
}

func validateClientVer(clientVer string) int {
	versions := strings.Split(clientVer, ".")
	if len(versions) != 3 {
		return http.StatusBadRequest
	}
	if _, err := strconv.Atoi(versions[0]); err != nil {
		return http.StatusBadRequest
	}
	if _, err := strconv.Atoi(versions[1]); err != nil {
		return http.StatusBadRequest
	}
	if _, err := strconv.Atoi(versions[2]); err != nil {
		return http.StatusBadRequest
	}

	return http.StatusOK
}

func onRepoOper(eType, repoID, user, ip, clientName string) {
	rData := new(repoEventData)
	vInfo, err := repomgr.GetVirtualRepoInfo(repoID)

	if err != nil {
		log.Errorf("Failed to get virtual repo info by repo id %s: %v", repoID, err)
		return
	}
	if vInfo != nil {
		rData.repoID = vInfo.OriginRepoID
		rData.path = vInfo.Path
	} else {
		rData.repoID = repoID
	}
	rData.eType = eType
	rData.user = user
	rData.ip = ip
	rData.clientName = clientName

	publishRepoEvent(rData)
}

func publishRepoEvent(rData *repoEventData) {
	if rData.path == "" {
		rData.path = "/"
	}
	log.Infof("repo event: type=%s user=%s repo=%s path=%s",
		rData.eType, rData.user, rData.repoID, rData.path)
}

func publishUpdateEvent(repoID string, commitID string) {
	log.Infof("update event: repo=%s commit=%s", repoID, commitID)
}

func removeSyncAPIExpireCache() {
	deleteTokens := func(key interface{}, value interface{}) bool {
		if info, ok := value.(*tokenInfo); ok {
			if info.expireTime <= time.Now().Unix() {
				tokenCache.Delete(key)
			}
		}
		return true
	}

	deletePerms := func(key interface{}, value interface{}) bool {
		if info, ok := value.(*permInfo); ok {
			if info.expireTime <= time.Now().Unix() {
				permCache.Delete(key)
			}
		}
		return true
	}

	deleteVirtualRepoInfo := func(key interface{}, value interface{}) bool {
		if info, ok := value.(*virtualRepoInfo); ok {
			if info.expireTime <= time.Now().Unix() {
				virtualRepoInfoCache.Delete(key)
			}
		}
		return true
	}

	tokenCache.Range(deleteTokens)
	permCache.Range(deletePerms)
	virtualRepoInfoCache.Range(deleteVirtualRepoInfo)
}

type collectFsInfo struct {
	startTime int64
	isTimeout bool
	results   []interface{}
}

var ErrTimeout = fmt.Errorf("get fs id list timeout")

func calculateSendObjectList(ctx context.Context, repo *repomgr.Repo, serverHead string, clientHead string, dirOnly bool) ([]interface{}, error) {
	masterHead, err := commitmgr.Load(repo.ID, serverHead)
	if err != nil {
		err := fmt.Errorf("failed to load server head commit %s:%s: %v", repo.ID, serverHead, err)
		return nil, err
	}
	var remoteHead *commitmgr.Commit
	remoteHeadRoot := emptySHA1
	if clientHead != "" {
		remoteHead, err = commitmgr.Load(repo.ID, clientHead)
		if err != nil {
			err := fmt.Errorf("failed to load remote head commit %s:%s: %v", repo.ID, clientHead, err)
			return nil, err
		}
		remoteHeadRoot = remoteHead.RootID
	}

	info := new(collectFsInfo)
	info.startTime = time.Now().Unix()
	if remoteHeadRoot != masterHead.RootID && masterHead.RootID != emptySHA1 {
		info.results = append(info.results, masterHead.RootID)
	}

	var opt *diff.DiffOptions
	if !dirOnly {
		opt = &diff.DiffOptions{
			FileCB: collectFileIDs,
			DirCB:  collectDirIDs,
			Ctx:    ctx,
			RepoID: repo.StoreID}
		opt.Data = info
	} else {
		opt = &diff.DiffOptions{
			FileCB: collectFileIDsNOp,
			DirCB:  collectDirIDs,
			Ctx:    ctx,
			RepoID: repo.StoreID}
		opt.Data = info
	}
	trees := []string{masterHead.RootID, remoteHeadRoot}

	if err := diff.DiffTrees(trees, opt); err != nil {
		if info.isTimeout {
			return nil, ErrTimeout
		}
		return nil, err
	}
	return info.results, nil
}

func collectFileIDs(ctx context.Context, baseDir string, files []*fsmgr.SeafDirent, data interface{}) error {
	select {
	case <-ctx.Done():
		return context.Canceled
	default:
	}

	file1 := files[0]
	file2 := files[1]
	info, ok := data.(*collectFsInfo)
	if !ok {
		err := fmt.Errorf("failed to assert results")
		return err
	}

	if file1 != nil &&
		(file2 == nil || file1.ID != file2.ID) &&
		file1.ID != emptySHA1 {
		info.results = append(info.results, file1.ID)
	}

	return nil
}

func collectFileIDsNOp(ctx context.Context, baseDir string, files []*fsmgr.SeafDirent, data interface{}) error {
	return nil
}

func collectDirIDs(ctx context.Context, baseDir string, dirs []*fsmgr.SeafDirent, data interface{}, recurse *bool) error {
	select {
	case <-ctx.Done():
		return context.Canceled
	default:
	}

	info, ok := data.(*collectFsInfo)
	if !ok {
		err := fmt.Errorf("failed to assert fs info")
		return err
	}
	dir1 := dirs[0]
	dir2 := dirs[1]

	if dir1 != nil &&
		(dir2 == nil || dir1.ID != dir2.ID) &&
		dir1.ID != emptySHA1 {
		info.results = append(info.results, dir1.ID)
	}

	if option.FsIdListRequestTimeout > 0 {
		now := time.Now().Unix()
		if now-info.startTime > option.FsIdListRequestTimeout {
			info.isTimeout = true
			return ErrTimeout
		}
	}

	return nil
}
