package option

import (
	"os"
	"path/filepath"
	"testing"
)

// A quota that fails to parse becomes InfiniteQuota, which is the right answer
// for "nobody configured one" and the wrong one for "somebody configured one
// and this could not read it". The two were indistinguishable, so a config
// this could not read meant no limit at all -- which is the failure checkQuota
// exists to prevent, spelled in the config file instead of the code: a quota
// that comes to be unenforced without anybody noticing.
//
// Case was the way in. An operator writing the unit the way it is usually
// written got unlimited.
func TestQuotaUnitsAreReadWhicheverWayTheyAreWritten(t *testing.T) {
	const want = 900 * GB
	for _, s := range []string{"900gb", "900GB", "900Gb", "900gB", " 900gb", "900gb ", "900 gb", "  900  GB  "} {
		if got := parseQuota(s); got != want {
			t.Errorf("parseQuota(%q) = %d, want %d", s, got, want)
		}
	}
}

func TestQuotaUnitsScale(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want int64
	}{
		{"1kb", KB},
		{"100mb", 100 * MB},
		{"5tb", 5 * TB},
		// A bare number is gigabytes, which is what the multiplier defaults to.
		{"1024", 1024 * GB},
	} {
		if got := parseQuota(tc.in); got != tc.want {
			t.Errorf("parseQuota(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// Anything genuinely unreadable still means no ceiling, because that is the
// only answer a parser can give -- but the caller is told, which is what makes
// a typo visible instead of silent.
func TestAnUnreadableQuotaIsInfiniteAndNotZero(t *testing.T) {
	for _, s := range []string{"", "lots", "900gigabytes", "-"} {
		if got := parseQuota(s); got != InfiniteQuota {
			t.Errorf("parseQuota(%q) = %d, want InfiniteQuota", s, got)
		}
	}
}

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "silo.conf")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("writing the config: %v", err)
	}
	return path
}

// The three quota keys are three different questions, and the one that is
// easiest to get wrong is that [quota] default is per account and says nothing
// about the server.
func TestTheServerCeilingAndReserveAreReadFromTheConfig(t *testing.T) {
	LoadFileServerOptions(writeConfig(t, "[quota]\ndefault = 100gb\nserver = 900GB\nreserve = 2gb\n"))

	if DefaultQuota != 100*GB {
		t.Errorf("[quota] default = %d, want %d", DefaultQuota, 100*GB)
	}
	if ServerQuota != 900*GB {
		t.Errorf("[quota] server = %d, want %d", ServerQuota, 900*GB)
	}
	if DiskReserve != 2*GB {
		t.Errorf("[quota] reserve = %d, want %d", DiskReserve, 2*GB)
	}
}

// A server nobody has told about quotas refuses nothing, except for the
// reserve -- which is nonzero by default because a volume driven to its last
// block takes the database down with it.
func TestAnUnconfiguredServerHasNoCeilingAndKeepsItsReserve(t *testing.T) {
	LoadFileServerOptions(writeConfig(t, "[fileserver]\nport = 8082\n"))

	if ServerQuota != InfiniteQuota {
		t.Errorf("unconfigured [quota] server = %d, want InfiniteQuota", ServerQuota)
	}
	if DiskReserve != DefaultDiskReserve {
		t.Errorf("unconfigured [quota] reserve = %d, want the %d default", DiskReserve, DefaultDiskReserve)
	}
}

// A reserve that cannot be read falls back to the default rather than to none.
// parseQuota answers InfiniteQuota for anything unreadable, which is right for
// a ceiling and wrong here: it would mean "keep -2 bytes free" and disable the
// protection because of a typo.
func TestAnUnreadableReserveKeepsTheDefaultRatherThanDisablingItself(t *testing.T) {
	LoadFileServerOptions(writeConfig(t, "[quota]\nreserve = one gigabyte\n"))

	if DiskReserve != DefaultDiskReserve {
		t.Errorf("[quota] reserve = %d after an unreadable value, want the %d default", DiskReserve, DefaultDiskReserve)
	}
}

// A quota that overflows int64 is not a large quota, it is a negative one, and
// everything downstream reads anything <= 0 as no ceiling at all. So the
// arithmetic has to be checked before it is done: this is the same failure as
// the case bug -- a configured limit silently becoming no limit -- reached by
// typing a big number instead of a capital letter.
func TestAQuotaTooLargeToStoreDoesNotWrapIntoNoCeiling(t *testing.T) {
	for _, s := range []string{"99999999999tb", "9223372036854775807gb"} {
		got := parseQuota(s)
		if got > 0 {
			t.Errorf("parseQuota(%q) = %d, want a refusal rather than a number", s, got)
		}
		if got != InfiniteQuota {
			t.Errorf("parseQuota(%q) = %d, want InfiniteQuota so the caller warns", s, got)
		}
	}
}
