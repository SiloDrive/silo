package option

import (
	"testing"
	"time"

	"github.com/dkam/silo/fileserver/objstore"
)

// The compiled-in answer is the one the store already had. A config file that
// says nothing about storage must not move any of these, or upgrading to a
// build that reads the section would silently retune every server that has no
// section in its file.
func TestStorageDefaultsMatchTheStore(t *testing.T) {
	LoadFileServerOptions("")

	want := objstore.DefaultTuning()
	if Packs.PackTarget != want.PackTarget {
		t.Errorf("Packs.PackTarget = %d, want %d", Packs.PackTarget, want.PackTarget)
	}
	if Packs.PackMaxAge != want.PackMaxAge {
		t.Errorf("PackMaxAge = %s, want %s", Packs.PackMaxAge, want.PackMaxAge)
	}
	if Packs.PackSweep != want.PackSweep {
		t.Errorf("Packs.PackSweep = %s, want %s", Packs.PackSweep, want.PackSweep)
	}
	if OrphanAge != DefaultOrphanAge {
		t.Errorf("OrphanAge = %s, want %s", OrphanAge, DefaultOrphanAge)
	}
	if CompactMinAge != DefaultOrphanAge {
		t.Errorf("CompactMinAge = %s, want %s", CompactMinAge, DefaultOrphanAge)
	}
	if CompactThreshold != DefaultCompactThreshold {
		t.Errorf("CompactThreshold = %g, want %g", CompactThreshold, DefaultCompactThreshold)
	}
	if CompactBudget != 0 {
		t.Errorf("CompactBudget = %d, want 0 (no cap)", CompactBudget)
	}
	if CommitAttempts != DefaultCommitAttempts {
		t.Errorf("CommitAttempts = %d, want %d", CommitAttempts, DefaultCommitAttempts)
	}
	if LibraryFaultInterval != DefaultLibraryFaultInterval {
		t.Errorf("LibraryFaultInterval = %s, want %s", LibraryFaultInterval, DefaultLibraryFaultInterval)
	}
}

func TestStorageSectionIsRead(t *testing.T) {
	LoadFileServerOptions(writeConfig(t, `[storage]
pack_size = 2gb
pack_max_age = 30m
pack_sweep = 1m
orphan_age = 48h
compact_threshold = 0.25
compact_min_age = 72h
compact_budget = 10gb
commit_attempts = 9
library_fault_interval = 15m
`))

	if Packs.PackTarget != 2*GB {
		t.Errorf("Packs.PackTarget = %d, want %d", Packs.PackTarget, 2*GB)
	}
	if Packs.PackMaxAge != 30*time.Minute {
		t.Errorf("PackMaxAge = %s, want 30m", Packs.PackMaxAge)
	}
	if Packs.PackSweep != time.Minute {
		t.Errorf("Packs.PackSweep = %s, want 1m", Packs.PackSweep)
	}
	if OrphanAge != 48*time.Hour {
		t.Errorf("OrphanAge = %s, want 48h", OrphanAge)
	}
	if CompactThreshold != 0.25 {
		t.Errorf("CompactThreshold = %g, want 0.25", CompactThreshold)
	}
	if CompactMinAge != 72*time.Hour {
		t.Errorf("CompactMinAge = %s, want 72h", CompactMinAge)
	}
	if CompactBudget != 10*GB {
		t.Errorf("CompactBudget = %d, want %d", CompactBudget, 10*GB)
	}
	if CommitAttempts != 9 {
		t.Errorf("CommitAttempts = %d, want 9", CommitAttempts)
	}
	if LibraryFaultInterval != 15*time.Minute {
		t.Errorf("LibraryFaultInterval = %s, want 15m", LibraryFaultInterval)
	}
}

// The environment beats the file, the same way SILO_HOST does.
func TestStorageEnvironmentOverridesTheFile(t *testing.T) {
	t.Setenv("SILO_PACK_MAX_AGE", "45m")
	t.Setenv("SILO_PACK_SIZE", "1gb")

	LoadFileServerOptions(writeConfig(t, "[storage]\npack_max_age = 30m\npack_size = 2gb\n"))

	if Packs.PackMaxAge != 45*time.Minute {
		t.Errorf("PackMaxAge = %s, want the environment's 45m", Packs.PackMaxAge)
	}
	if Packs.PackTarget != GB {
		t.Errorf("Packs.PackTarget = %d, want the environment's %d", Packs.PackTarget, GB)
	}
}

// A value that cannot be read keeps the default rather than becoming zero.
// Zero is a live setting for several of these -- a sweep interval of nothing,
// an age guard of nothing -- so falling to it on a typo is the one outcome
// that must not happen.
func TestStorageUnreadableValuesKeepTheDefault(t *testing.T) {
	LoadFileServerOptions(writeConfig(t, `[storage]
pack_size = 512
pack_max_age = five minutes
pack_sweep = -1s
compact_threshold = 40%
commit_attempts = 0
`))

	if Packs.PackTarget != objstore.DefaultTuning().PackTarget {
		t.Errorf("Packs.PackTarget = %d: a size with no unit should be refused, not read as bytes", Packs.PackTarget)
	}
	if Packs.PackMaxAge != objstore.DefaultTuning().PackMaxAge {
		t.Errorf("PackMaxAge = %s, want the default", Packs.PackMaxAge)
	}
	if Packs.PackSweep != objstore.DefaultTuning().PackSweep {
		t.Errorf("Packs.PackSweep = %s: a negative interval should be refused", Packs.PackSweep)
	}
	if CompactThreshold != DefaultCompactThreshold {
		t.Errorf("CompactThreshold = %g, want the default", CompactThreshold)
	}
	if CommitAttempts != DefaultCommitAttempts {
		t.Errorf("CommitAttempts = %d: zero attempts never commits", CommitAttempts)
	}
}

func TestParseBytes(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want int64
		bad  bool
	}{
		{in: "0", want: 0},
		{in: "512mb", want: 512 * MB},
		{in: "  10 GB ", want: 10 * GB},
		{in: "2tb", want: 2 * TB},
		{in: "512", bad: true},  // no unit: ambiguous against [quota], refused
		{in: "mb", bad: true},   // a unit with no number
		{in: "-1gb", bad: true}, // a negative size
		{in: "9999999999tb", bad: true},
		{in: "", bad: true},
	} {
		got, err := ParseBytes(tc.in)
		if tc.bad {
			if err == nil {
				t.Errorf("ParseBytes(%q) = %d, want an error", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseBytes(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseBytes(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// StorageTuning has to be applicable: the defaults this package ships must
// pass the validation the store applies to them, or a server with no config
// file at all refuses to start.
func TestStorageTuningIsValid(t *testing.T) {
	LoadFileServerOptions("")
	if err := Packs.Validate(); err != nil {
		t.Fatalf("the shipped defaults do not validate: %v", err)
	}
}
