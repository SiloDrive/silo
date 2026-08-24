package silod

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/dkam/silo/fileserver/account"
	"github.com/dkam/silo/fileserver/api"
	"github.com/dkam/silo/fileserver/libmgr"
	"github.com/dkam/silo/fileserver/option"
)

// checkQuotaV2 used to read owner, quota and usage as three independent
// queries with nothing serializing them: two requests racing the same
// owner's headroom could each read the same pre-write usage and both be
// admitted, together landing the owner over quota by more than either write
// alone. lockOwner closes that by making a second caller for the same owner
// wait for the first to finish — and must not block an unrelated owner's
// check while it does.
func TestLockOwnerSerializesTheSameOwnerButNotADifferentOne(t *testing.T) {
	owner := account.ID{9, 9, 9}
	other := account.ID{1, 2, 3}

	unlockFirst := lockOwner(owner)

	sameOwnerDone := make(chan struct{})
	go func() {
		unlock := lockOwner(owner)
		defer unlock()
		close(sameOwnerDone)
	}()

	select {
	case <-sameOwnerDone:
		t.Fatal("a second checkQuota for the same owner proceeded while the first still held the lock")
	case <-time.After(100 * time.Millisecond):
		// Still blocked, as it must be.
	}

	otherOwnerDone := make(chan struct{})
	go func() {
		unlock := lockOwner(other)
		defer unlock()
		close(otherOwnerDone)
	}()

	select {
	case <-otherOwnerDone:
		// A different owner is not held up by the first at all.
	case <-time.After(time.Second):
		t.Fatal("a different owner's quota check was blocked by an unrelated owner's lock")
	}

	unlockFirst()
	select {
	case <-sameOwnerDone:
		// The second caller for the same owner proceeds once the first releases.
	case <-time.After(time.Second):
		t.Fatal("the second caller for the same owner never proceeded after the first released")
	}
}

// setQuota gives an account a ceiling. Without one there is none: a server
// nobody has configured a quota on does not refuse writes.
func setQuota(t *testing.T, acct *account.Account, bytes int64) {
	t.Helper()
	ctx, cancel := option.WithDBTimeout(context.Background())
	defer cancel()
	if _, err := siloPair.Write.ExecContext(ctx,
		"INSERT INTO UserQuota (account_id, quota) VALUES (?, ?)", acct.ID, bytes); err != nil {
		t.Fatalf("set quota: %v", err)
	}
}

func TestAWriteThatWouldExceedQuotaIsRefused(t *testing.T) {
	libraryID, acct := storeV2Library(t)
	setQuota(t, acct, 1000)

	vars := map[string]string{"libraryid": libraryID, "path": "first.bin"}
	if w := do(t, entriesHandler, acct, "PUT", "/x", vars, bytes.Repeat([]byte("a"), 900)); w.Code != http.StatusCreated {
		t.Fatalf("first write = %d (%s), want 201", w.Code, w.Body.String())
	}

	// 900 of 1000 used, and this one asks for 200 more.
	vars = map[string]string{"libraryid": libraryID, "path": "second.bin"}
	w := do(t, entriesHandler, acct, "PUT", "/x", vars, bytes.Repeat([]byte("b"), 200))
	if w.Code != httpInsufficientStorage {
		t.Fatalf("over-quota write = %d (%s), want %d", w.Code, w.Body.String(), httpInsufficientStorage)
	}

	// And the refusal left nothing behind: the file must not exist.
	got := do(t, entriesHandler, acct, "GET", "/x", vars, nil)
	if got.Code != http.StatusNotFound {
		t.Errorf("GET of the refused file = %d, want 404 — the refusal committed something", got.Code)
	}
}

// Freeing space makes room. This is the property logical-at-head buys and
// stored bytes would not: the deleted file's chunks are still on disk until
// the collector runs, and the user is charged for neither.
func TestDeletingAFileMakesRoomImmediately(t *testing.T) {
	libraryID, acct := storeV2Library(t)
	setQuota(t, acct, 1000)

	vars := map[string]string{"libraryid": libraryID, "path": "big.bin"}
	if w := do(t, entriesHandler, acct, "PUT", "/x", vars, bytes.Repeat([]byte("a"), 900)); w.Code != http.StatusCreated {
		t.Fatalf("first write = %d, want 201", w.Code)
	}
	next := map[string]string{"libraryid": libraryID, "path": "next.bin"}
	if w := do(t, entriesHandler, acct, "PUT", "/x", next, bytes.Repeat([]byte("b"), 500)); w.Code != httpInsufficientStorage {
		t.Fatalf("write over the ceiling = %d, want %d", w.Code, httpInsufficientStorage)
	}

	if w := do(t, entriesHandler, acct, "DELETE", "/x", vars, nil); w.Code != http.StatusNoContent && w.Code != http.StatusOK {
		t.Fatalf("delete = %d (%s)", w.Code, w.Body.String())
	}
	if w := do(t, entriesHandler, acct, "PUT", "/x", next, bytes.Repeat([]byte("b"), 500)); w.Code != http.StatusCreated {
		t.Fatalf("write after freeing space = %d (%s), want 201", w.Code, w.Body.String())
	}
}

// A server with no quota configured refuses nothing. This is the case every
// fresh install is in, and getting it wrong means a server that was told
// nothing about quotas rejects every write on the grounds that zero bytes are
// allowed.
func TestWithNoQuotaConfiguredNothingIsRefused(t *testing.T) {
	libraryID, acct := storeV2Library(t)

	vars := map[string]string{"libraryid": libraryID, "path": "big.bin"}
	if w := do(t, entriesHandler, acct, "PUT", "/x", vars, bytes.Repeat([]byte("a"), 1<<20)); w.Code != http.StatusCreated {
		t.Fatalf("write with no quota set = %d (%s), want 201", w.Code, w.Body.String())
	}
}

// The account report says what is used, and labels the number. Absent quota is
// how unlimited is reported, never a sentinel: option.InfiniteQuota is -2, and
// a widget rendering "-2 bytes" is the predictable end of putting it on the
// wire.
func TestAccountUsageReportsWhatIsUsedAndLabelsIt(t *testing.T) {
	libraryID, acct := storeV2Library(t)

	vars := map[string]string{"libraryid": libraryID, "path": "a.bin"}
	if w := do(t, entriesHandler, acct, "PUT", "/x", vars, bytes.Repeat([]byte("a"), 4096)); w.Code != http.StatusCreated {
		t.Fatalf("write = %d", w.Code)
	}

	var report struct {
		Usage int64  `json:"usage"`
		Quota *int64 `json:"quota"`
		Kind  string `json:"kind"`
	}
	w := do(t, api.AccountUsageHandler, acct, "GET", "/account/usage", nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("account/usage = %d (%s)", w.Code, w.Body.String())
	}
	if err := json.Unmarshal(w.Body.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.Usage != 4096 {
		t.Errorf("usage = %d, want 4096", report.Usage)
	}
	if report.Quota != nil {
		t.Errorf("quota = %d with none configured, want the field absent", *report.Quota)
	}
	if report.Kind == "" {
		t.Error("the figure is unlabelled; a client cannot say which number it is showing")
	}

	setQuota(t, acct, 50000)
	w = do(t, api.AccountUsageHandler, acct, "GET", "/account/usage", nil, nil)
	if err := json.Unmarshal(w.Body.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.Quota == nil || *report.Quota != 50000 {
		t.Errorf("quota = %v, want 50000", report.Quota)
	}
}

// The listing carries each library's own size, because a library shared with
// you appears there but is charged to its owner.
func TestTheLibrariesListingCarriesEachLibrarysSize(t *testing.T) {
	libraryID, acct := storeV2Library(t)
	api.Init(siloPair.Read, siloPair.Write) // the listing reads the catalog directly
	vars := map[string]string{"libraryid": libraryID, "path": "a.bin"}
	if w := do(t, entriesHandler, acct, "PUT", "/x", vars, bytes.Repeat([]byte("a"), 2048)); w.Code != http.StatusCreated {
		t.Fatalf("write = %d", w.Code)
	}

	var listing []struct {
		ID        string `json:"id"`
		Size      *int64 `json:"size"`
		FileCount *int64 `json:"file_count"`
	}
	w := do(t, api.ListLibrariesHandler, acct, "GET", "/libraries", nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("libraries = %d (%s)", w.Code, w.Body.String())
	}
	if err := json.Unmarshal(w.Body.Bytes(), &listing); err != nil {
		t.Fatal(err)
	}
	if len(listing) != 1 {
		t.Fatalf("listed %d libraries, want 1", len(listing))
	}
	if listing[0].Size == nil || *listing[0].Size != 2048 {
		t.Errorf("size = %v, want 2048", listing[0].Size)
	}
	if listing[0].FileCount == nil || *listing[0].FileCount != 1 {
		t.Errorf("file_count = %v, want 1", listing[0].FileCount)
	}
}

// Usage is exact after any sequence of writes, because it is a difference
// between trees rather than a running estimate. An overwrite is the case a
// counter gets wrong: charge the new size without crediting the old and the
// number climbs forever.
func TestUsageIsExactAfterAnOverwrite(t *testing.T) {
	libraryID, acct := storeV2Library(t)
	vars := map[string]string{"libraryid": libraryID, "path": "a.bin"}

	for _, size := range []int{5000, 100, 3000} {
		if w := do(t, entriesHandler, acct, "PUT", "/x", vars, bytes.Repeat([]byte("a"), size)); w.Code != http.StatusCreated {
			t.Fatalf("write of %d = %d (%s)", size, w.Code, w.Body.String())
		}
	}

	library, err := libmgr.GetWithReason(libraryID)
	if err != nil {
		t.Fatal(err)
	}
	u, err := libmgr.Usage(library)
	if err != nil {
		t.Fatal(err)
	}
	if u.Size != 3000 || u.FileCount != 1 {
		t.Fatalf("usage = %+v after three writes to one path, want 3000 bytes in 1 file", u)
	}
}
