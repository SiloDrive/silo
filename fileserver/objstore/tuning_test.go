package objstore

import (
	"testing"
	"time"
)

func TestDefaultTuningValidates(t *testing.T) {
	if err := DefaultTuning().Validate(); err != nil {
		t.Fatalf("the compiled-in defaults do not validate: %v", err)
	}
}

func TestTuningValidateRejects(t *testing.T) {
	ok := DefaultTuning()
	for _, tc := range []struct {
		name string
		t    Tuning
	}{
		{"a pack size below the floor", Tuning{PackTarget: 4096, PackMaxAge: ok.PackMaxAge, PackSweep: ok.PackSweep}},
		{"a pack size above the ceiling", Tuning{PackTarget: maxPackTarget + 1, PackMaxAge: ok.PackMaxAge, PackSweep: ok.PackSweep}},
		{"no pack size at all", Tuning{PackMaxAge: ok.PackMaxAge, PackSweep: ok.PackSweep}},
		{"an age of zero", Tuning{PackTarget: ok.PackTarget, PackSweep: ok.PackSweep}},
		{"a sweep of zero", Tuning{PackTarget: ok.PackTarget, PackMaxAge: ok.PackMaxAge}},
		// The one that is not obviously wrong from its own value: an hourly
		// sweep is a perfectly reasonable interval, and against a five minute
		// age it means the age rule quietly does not hold.
		{"a sweep slower than the age", Tuning{PackTarget: ok.PackTarget, PackMaxAge: 5 * time.Minute, PackSweep: time.Hour}},
	} {
		if err := tc.t.Validate(); err == nil {
			t.Errorf("Validate accepted %s: %+v", tc.name, tc.t)
		}
	}
}

func TestConfigureApplies(t *testing.T) {
	wasTarget, wasAge, wasSweep := packTarget, packMaxAge, packSweep
	t.Cleanup(func() { packTarget, packMaxAge, packSweep = wasTarget, wasAge, wasSweep })

	want := Tuning{PackTarget: 2 << 30, PackMaxAge: 30 * time.Minute, PackSweep: time.Minute}
	if err := Configure(want); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if packTarget != want.PackTarget || packMaxAge != want.PackMaxAge || packSweep != want.PackSweep {
		t.Fatalf("Configure left %d/%s/%s, want %d/%s/%s",
			packTarget, packMaxAge, packSweep, want.PackTarget, want.PackMaxAge, want.PackSweep)
	}
}

// A rejected set must leave every knob where it was. Assigning as it goes
// would apply the fields ahead of the bad one, which is the state nobody
// configured and nobody can see.
func TestConfigureRejectsWholesale(t *testing.T) {
	wasTarget, wasAge, wasSweep := packTarget, packMaxAge, packSweep
	t.Cleanup(func() { packTarget, packMaxAge, packSweep = wasTarget, wasAge, wasSweep })

	bad := Tuning{PackTarget: 2 << 30, PackMaxAge: time.Minute, PackSweep: time.Hour}
	if err := Configure(bad); err == nil {
		t.Fatal("Configure accepted a sweep slower than the age")
	}
	if packTarget != wasTarget || packMaxAge != wasAge || packSweep != wasSweep {
		t.Fatalf("a rejected Configure moved the settings to %d/%s/%s", packTarget, packMaxAge, packSweep)
	}
}
