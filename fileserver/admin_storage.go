package silod

// The storage panel's numbers, and the ones it refuses to invent.
//
// It lives in this package rather than in api/ because it needs absDataDir --
// where the store actually is -- which is a server fact rather than an API one,
// the same reason patchLibraryHandler and batchHandler are here.
//
// docs/plans/admin.md § The page, and what it can honestly show is the owning
// document. Its rule is the whole design of this file: three of the five things
// an operator asks for are not measurable today, and a panel that rendered a
// plausible number for them would be worse than one that says so.

import (
	"encoding/json"
	"net/http"

	"github.com/SiloDrive/silo/fileserver/diskfree"
	"github.com/SiloDrive/silo/fileserver/libmgr"
	"github.com/SiloDrive/silo/fileserver/option"
	"github.com/SiloDrive/silo/fileserver/traffic"
	log "github.com/sirupsen/logrus"
)

// adminStorage is what this server can honestly say about its own size.
//
// Three currencies, labelled, and deliberately not reconciled -- the same three
// `silo df` prints, for the reason its printServerHeadroom gives. A write is
// refused at the lower of the configured ceiling and free space less the
// reserve, and neither of those is the stored-bytes figure a census produces.
type adminStorage struct {
	// LogicalSize and FileCount total every library at head, excluding virtual
	// ones. Logical-at-head is the currency the ceiling is measured in, which
	// is why it is the one reported beside it.
	//
	// It is not bytes on disk. Stored bytes divided into head, history and
	// unreferenced is objmgr.Census, behind `silo df`, and it walks a library's
	// whole store -- a command an operator runs, not a thing a page does on
	// every load. The page says which number this is rather than picking one
	// and hoping.
	LogicalSize int64 `json:"logical_size"`
	FileCount   int64 `json:"file_count"`

	// ServerQuota is the configured ceiling in bytes, or null when nobody set
	// one. Null rather than zero for the reason the per-account quota route
	// uses null: a stored zero would read as "no bytes allowed", and this
	// panel exists not to invent numbers.
	ServerQuota *int64 `json:"server_quota"`

	// DiskFree is physical free space where the store lives; DiskReserve is how
	// much of it the server refuses to spend.
	DiskFree    *int64 `json:"disk_free"`
	DiskReserve int64  `json:"disk_reserve"`
	// DiskError is why DiskFree is null, when it is. A disk that cannot be
	// measured is reported as one rather than as zero free -- zero is the one
	// wrong answer that looks like an emergency.
	DiskError string `json:"disk_error,omitempty"`

	// AtRestSealed is here to be said rather than assumed. Every object on this
	// server is sealed under storage.key regardless of whether its library is
	// end-to-end encrypted, so a page showing only the per-library E2EE flag
	// would imply the plain libraries were sitting in the clear. Two
	// independent facts, and this is the one that is not per library.
	AtRestSealed bool `json:"at_rest_sealed"`

	// Throughput is wire bytes at the HTTP boundary -- what crossed the
	// socket, not what reached the disk.
	//
	// A fourth currency, and the one furthest from the other three. Logical
	// size, free space and the ceiling all describe bytes at rest; this
	// describes bytes in motion, and the two do not reconcile even in
	// principle: dedup means a chunk that arrived is often never written, a
	// stored frame carries overhead the wire never saw, and a request re-sent
	// after a 401 crosses twice and lands once. Reading a discrepancy between
	// them as an error is the mistake this comment exists to prevent.
	Throughput traffic.Snapshot `json:"throughput"`

	// Unmeasured names what this server cannot answer yet, so a panel can say
	// so in the server's own words instead of rendering something plausible.
	// A panel that says "not measured yet" is a correct panel.
	Unmeasured []string `json:"unmeasured"`
}

func adminStorageHandler(w http.ResponseWriter, r *http.Request) {
	out := adminStorage{
		DiskReserve:  option.DiskReserve,
		AtRestSealed: true,
		Throughput:   traffic.Default.Read(),
		Unmeasured: []string{
			"Storage locations: there are no durable tiers to name, so there is nothing to show.",
			"Cache versus full copy: a device credential records that a device enrolled, not what it kept.",
			"Stored bytes by part: head, history and unreferenced come from a census that walks a library's whole store. Run silo df.",
		},
	}
	if option.ServerQuota > 0 {
		ceiling := option.ServerQuota
		out.ServerQuota = &ceiling
	}

	used, err := libmgr.ServerUsage()
	if err != nil {
		log.Errorf("Failed to total server usage: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
	out.LogicalSize, out.FileCount = used.Size, used.FileCount

	if free, err := diskfree.Available(absDataDir); err != nil {
		out.DiskError = err.Error()
	} else {
		out.DiskFree = &free
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(out); err != nil {
		log.Errorf("Failed to encode the storage panel: %v", err)
	}
}
