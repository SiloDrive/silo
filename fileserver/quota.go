package silod

import (
	"net/http"
	"strconv"
	"sync"

	"github.com/dkam/silo/fileserver/account"
	"github.com/dkam/silo/fileserver/diskfree"
	"github.com/dkam/silo/fileserver/libmgr"
	"github.com/dkam/silo/fileserver/option"
	log "github.com/sirupsen/logrus"
)

// errOverQuota is what a library over its owner's quota answers, alongside
// http.StatusInsufficientStorage.
//
// A real status rather than the sync lane's 443, which was invented by a
// protocol that owned both ends and dies with it. 507 says the server cannot
// store the representation, which is exactly the refusal — distinguishable
// from a 403, which would tell a client the request was not allowed and to
// stop rather than to free some space and try again.
const errOverQuota = "The owner of this library is out of quota"

// errServerFull is the refusal when the server as a whole has no room, which
// is a different fact from the owner being over their ceiling even though it
// carries the same 507.
//
// Two messages rather than one, because they send an operator to different
// places: the first is fixed by raising somebody's quota, and this one is not
// fixed by raising anybody's. A single message would have every disk-full
// incident reported as a quota bug.
const errServerFull = "This server is out of space"

// checkQuota refuses a write that would put a library's owner over quota.
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
func checkQuota(library *libmgr.Library, delta int64) *batchFailure {
	owner, err := libmgr.GetLibraryOwner(library.ID)
	if err != nil || owner.IsZero() {
		log.Errorf("Failed to find the owner of %s for a quota check: %v", library.ID, err)
		return &batchFailure{http.StatusInternalServerError, "Internal server error"}
	}
	unlock := lockOwner(owner)
	defer unlock()
	return checkQuotaLocked(library, owner, delta)
}

// ownerLocks serializes quota admission per account, so that two requests
// racing for the same owner's headroom read usage and decide one after the
// other rather than both against the same pre-write total.
//
// A bare read-then-decide, run twice concurrently, admits both: each reads
// the same usage, each computes usage+delta<=quota as true, and together they
// land the owner over quota by more than either alone would have. The lock is
// keyed by owner rather than by library because the same owner can hold many
// libraries and a head move on any of them changes the one total every other
// library's check is weighed against.
var ownerLocks sync.Map // account.ID -> *sync.Mutex

// lockOwner acquires the owner's admission lock and returns the func that
// releases it.
//
// A caller that only needs the read-then-decide to be atomic — the per-chunk
// estimate checks — can call checkQuota, which holds this only for the
// check. A caller whose write is what actually changes usage — the head-move
// gate — must hold it from before the check through the commit, via
// checkQuotaLocked, or the same race reopens one level up: the lock would
// protect the check but not the write the check was supposed to gate.
func lockOwner(id account.ID) func() {
	v, _ := ownerLocks.LoadOrStore(id, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// checkQuotaLocked is checkQuota's read-and-decide step, for a caller that
// already holds owner's admission lock (see lockOwner) across a write that
// changes usage — the head-move gate keeps the lock through its commit so
// the total this decides against cannot be admitted against twice.
func checkQuotaLocked(library *libmgr.Library, owner account.ID, delta int64) *batchFailure {
	quota, err := libmgr.AccountQuota(owner)
	if err != nil {
		log.Errorf("Failed to read the quota of the owner of %s: %v", library.ID, err)
		return &batchFailure{http.StatusInternalServerError, "Internal server error"}
	}
	if fail := checkServerLimits(delta); fail != nil {
		return fail
	}
	if quota <= 0 {
		return nil // no ceiling was ever set
	}
	usage, err := libmgr.AccountUsage(owner)
	if err != nil {
		log.Errorf("Failed to total the usage of the owner of %s: %v", library.ID, err)
		return &batchFailure{http.StatusInternalServerError, "Internal server error"}
	}
	if usage.Size+delta > quota {
		return &batchFailure{http.StatusInsufficientStorage, errOverQuota}
	}
	return nil
}

// checkServerLimits refuses a write the server as a whole has no room for.
// nil admits it.
//
// Two ceilings, and the write has to be under both: a configured total
// (option.ServerQuota, in the same logical-at-head currency as an account
// quota) and actual free space less a reserve (option.DiskReserve). Neither
// alone is sufficient, which is the reasoning docs/quota.md records -- the
// configured number does not know about the other tenant on the volume, and
// free space does not know the operator meant to keep 100 GB back for
// something else.
//
// The two are in different currencies on purpose and are not reconciled. A
// configured ceiling is a policy about what Silo may hold; free space is a
// fact about a disk. Each yields a headroom in bytes, the smaller wins, and
// pretending they measure the same thing would mean converting one into the
// other with a dedup ratio nobody can know in advance.
//
// Free space that cannot be read is not a refusal. Unlike an unreadable
// account quota -- where admitting the write is how a quota comes to be
// unenforced without anybody noticing -- an unreadable disk is a platform
// this build cannot ask, and refusing every write on a machine whose free
// space Silo merely cannot measure would take the server down rather than
// protect it. It is logged and the configured ceiling still applies.
//
// This deliberately does not serialize across owners the way lockOwner does
// for account quotas. A global admission lock would put every concurrent
// write on the server behind one mutex, and the overshoot it would prevent is
// bounded by the writes in flight -- which is a large part of what the
// reserve is for.
func checkServerLimits(delta int64) *batchFailure {
	if option.ServerQuota > 0 {
		used, err := libmgr.ServerUsage()
		if err != nil {
			log.Errorf("Failed to total this server's usage for the server ceiling: %v", err)
			return &batchFailure{http.StatusInternalServerError, "Internal server error"}
		}
		if used.Size+delta > option.ServerQuota {
			return &batchFailure{http.StatusInsufficientStorage, errServerFull}
		}
	}

	if option.DiskReserve > 0 {
		free, err := diskfree.Available(absDataDir)
		if err != nil {
			log.Warnf("Cannot read free space, so only the configured ceiling applies: %v", err)
			return nil
		}
		if free-delta < option.DiskReserve {
			return &batchFailure{http.StatusInsufficientStorage, errServerFull}
		}
	}
	return nil
}

// refuseOverQuota writes the refusal, and reports whether the caller should
// stop.
func refuseOverQuota(w http.ResponseWriter, library *libmgr.Library, delta int64) bool {
	fail := checkQuota(library, delta)
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
