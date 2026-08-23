package option

import (
	"testing"
	"time"
)

// loadWithTTL runs the option loader with SILO_API_TOKEN_TTL set to raw (or
// unset, when raw is empty) and returns the resulting APITokenTTL.
func loadWithTTL(t *testing.T, raw string) time.Duration {
	t.Helper()
	if raw != "" {
		t.Setenv("SILO_API_TOKEN_TTL", raw)
	}
	LoadFileServerOptions("")
	return APITokenTTL
}

func TestAPITokenTTLDefault(t *testing.T) {
	if got := loadWithTTL(t, ""); got != 30*24*time.Hour {
		t.Errorf("expected a 30 day default, got %s", got)
	}
}

func TestAPITokenTTLAcceptsValidOverride(t *testing.T) {
	if got := loadWithTTL(t, "168h"); got != 168*time.Hour {
		t.Errorf("expected 168h, got %s", got)
	}
}

// A value at the floor exactly is allowed; the check is "below", not "at or
// below".
func TestAPITokenTTLAcceptsExactMinimum(t *testing.T) {
	if got := loadWithTTL(t, MinAPITokenTTL.String()); got != MinAPITokenTTL {
		t.Errorf("expected the minimum %s to be accepted, got %s", MinAPITokenTTL, got)
	}
}

// The point of the floor. A too-short TTL makes the migration stamp every
// pre-existing token with an expiry that a later correct run will not repair,
// because the backfill only touches rows where expires_at IS NULL.
func TestAPITokenTTLRejectsValuesBelowMinimum(t *testing.T) {
	tests := []struct {
		name string
		raw  string
	}{
		// The realistic typo: "30m" meant as "30 days", which is 720h.
		{"minutes mistaken for days", "30m"},
		{"one second", "1s"},
		{"just under the floor", "59m"},
		{"zero", "0s"},
		{"negative", "-720h"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := loadWithTTL(t, tt.raw)
			if got < MinAPITokenTTL {
				t.Errorf("accepted %q as %s, below the %s floor", tt.raw, got, MinAPITokenTTL)
			}
			if got != 30*24*time.Hour {
				t.Errorf("expected the default to be kept, got %s", got)
			}
		})
	}
}

func TestAPITokenTTLRejectsUnparseable(t *testing.T) {
	for _, raw := range []string{"30", "banana", "30 days", ""} {
		if raw == "" {
			continue // covered by the default test; empty means unset
		}
		t.Run(raw, func(t *testing.T) {
			if got := loadWithTTL(t, raw); got != 30*24*time.Hour {
				t.Errorf("expected the default for %q, got %s", raw, got)
			}
		})
	}
}

// Whatever the loader produces must always be a TTL the migration will accept,
// since one feeds the other. This is the invariant the two guards jointly hold.
func TestLoadedTTLIsAlwaysPositive(t *testing.T) {
	for _, raw := range []string{"", "0s", "-1h", "banana", "1s", "720h"} {
		got := loadWithTTL(t, raw)
		if got <= 0 {
			t.Errorf("SILO_API_TOKEN_TTL=%q produced a non-positive TTL (%s), "+
				"which the schema would refuse", raw, got)
		}
	}
}
