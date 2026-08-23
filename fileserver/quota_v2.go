package silod

import (
	"net/http"
	"strconv"

	"github.com/dkam/silo/fileserver/repomgr"
	log "github.com/sirupsen/logrus"
)

// httpInsufficientStorage is what a library over its owner's quota answers.
//
// A real status rather than the sync lane's 443, which was invented by a
// protocol that owned both ends and dies with it. 507 says the server cannot
// store the representation, which is exactly the refusal — distinguishable
// from a 403, which would tell a client the request was not allowed and to
// stop rather than to free some space and try again.
const httpInsufficientStorage = 507

const errOverQuota = "The owner of this library is out of quota"

// checkQuotaV2 refuses a write that would put a library's owner over quota.
// nil admits it.
//
// The charge is logical size at head, so what is being asked is whether the
// files the library would then reach add up to more than the owner may hold.
// Not stored bytes: dedup and compaction move those under the user's feet, and
// a number that changes because the server ran a background job is not one
// anybody can act on.
//
// An overwrite is charged as though it were an addition, because the size it
// replaces is not known here without resolving the path first. That refuses
// slightly early at the boundary and never late, and it does not accumulate:
// the moment the head moves, accounting measures the tree and the exact number
// replaces this estimate.
//
// A quota it cannot read is a refusal and not a shrug. Admitting writes
// because the lookup failed is how a quota comes to be unenforced without
// anybody noticing, which is the failure this lane already had and is the
// reason this function exists.
func checkQuotaV2(repo *repomgr.Repo, delta int64) *batchFailure {
	owner, err := repomgr.GetRepoOwner(repo.ID)
	if err != nil || owner.IsZero() {
		log.Errorf("Failed to find the owner of %s for a quota check: %v", repo.ID, err)
		return &batchFailure{http.StatusInternalServerError, "Internal server error"}
	}
	quota, err := repomgr.AccountQuota(owner)
	if err != nil {
		log.Errorf("Failed to read the quota of the owner of %s: %v", repo.ID, err)
		return &batchFailure{http.StatusInternalServerError, "Internal server error"}
	}
	if quota <= 0 {
		return nil // no ceiling was ever set
	}
	usage, err := repomgr.AccountUsage(owner)
	if err != nil {
		log.Errorf("Failed to total the usage of the owner of %s: %v", repo.ID, err)
		return &batchFailure{http.StatusInternalServerError, "Internal server error"}
	}
	if usage.Size+delta > quota {
		return &batchFailure{httpInsufficientStorage, errOverQuota}
	}
	return nil
}

// refuseOverQuota writes the refusal, and reports whether the caller should
// stop.
func refuseOverQuota(w http.ResponseWriter, repo *repomgr.Repo, delta int64) bool {
	fail := checkQuotaV2(repo, delta)
	if fail == nil {
		return false
	}
	http.Error(w, fail.message, fail.code)
	return true
}

// declaredLength is the size a request says it is bringing, or zero when it
// declines to say.
//
// Worth asking before the body is read, because refusing a 40 GB upload after
// receiving it wastes the bytes on both ends. Worth asking again afterwards,
// because a chunked request declares nothing and a lying one declares whatever
// it likes — which is why this is a first pass and never the only check.
func declaredLength(r *http.Request) int64 {
	if r.ContentLength > 0 {
		return r.ContentLength
	}
	n, err := strconv.ParseInt(r.Header.Get("Content-Length"), 10, 64)
	if err != nil || n < 0 {
		return 0
	}
	return n
}
