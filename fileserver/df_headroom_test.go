package silod

import (
	"strings"
	"testing"

	"github.com/SiloDrive/silo/fileserver/libmgr"
	"github.com/SiloDrive/silo/fileserver/option"
)

// printServerHeadroom is what an operator reads after a sync stops, so what it
// says about a refusal has to be right. Four branches, and the one that would
// be most quietly wrong is the arithmetic: free space minus a reserve larger
// than the disk is not a negative amount of room, it is none.
func TestServerHeadroomReportsWhatAdmissionWillRefuse(t *testing.T) {
	// The store has to be up: printServerHeadroom asks libmgr for the server
	// total, and RunDF only reaches it after openStores. The ordering is a
	// property of the command rather than of this function, which is why the
	// function does not defend against it.
	sqliteTestDB(t)
	dir := t.TempDir()
	origData, origAbs := dataDir, absDataDir
	dataDir, absDataDir = dir, dir
	t.Cleanup(func() { dataDir, absDataDir = origData, origAbs })
	libmgr.Init(siloPair.Read, siloPair.Write, dir)

	origQuota, origReserve := option.ServerQuota, option.DiskReserve
	t.Cleanup(func() { option.ServerQuota, option.DiskReserve = origQuota, origReserve })

	t.Run("a reserve nothing can satisfy reports no room rather than negative room", func(t *testing.T) {
		option.ServerQuota, option.DiskReserve = option.InfiniteQuota, 1<<62
		out := captureStdout(t, printServerHeadroom)
		if !strings.Contains(out, "free on disk") {
			t.Fatalf("no free-disk line in %q", out)
		}
		if strings.Contains(out, "-") {
			t.Errorf("the footer reported a negative amount of room: %q", out)
		}
	})

	t.Run("no ceiling says so instead of printing a number", func(t *testing.T) {
		option.ServerQuota, option.DiskReserve = option.InfiniteQuota, 0
		out := captureStdout(t, printServerHeadroom)
		if !strings.Contains(out, "none") {
			t.Errorf("an unconfigured ceiling did not say so: %q", out)
		}
		if !strings.Contains(out, "no reserve set") {
			t.Errorf("a zero reserve did not say so: %q", out)
		}
	})

	t.Run("a configured ceiling reports what is left under it", func(t *testing.T) {
		option.ServerQuota, option.DiskReserve = 900*option.GB, option.GB
		out := captureStdout(t, printServerHeadroom)
		if !strings.Contains(out, "server ceiling") {
			t.Fatalf("no ceiling line in %q", out)
		}
		// The currency is named, because the census above it is in stored
		// bytes and these two numbers must not be read as the same thing.
		if !strings.Contains(out, "logical-at-head") {
			t.Errorf("the ceiling line does not say which currency it is in: %q", out)
		}
		if !strings.Contains(out, "keeping") {
			t.Errorf("the reserve is not reported: %q", out)
		}
	})
}
