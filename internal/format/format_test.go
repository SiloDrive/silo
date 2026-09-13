package format

import "testing"

func TestCountGroupsThousands(t *testing.T) {
	for _, tt := range []struct {
		n    int
		want string
	}{
		{0, "0"},
		{7, "7"},
		{999, "999"},
		{1000, "1,000"},
		{1204, "1,204"},
		{12345, "12,345"},
		{1234567, "1,234,567"},
		{-4321, "-4,321"},
	} {
		if got := Count(tt.n); got != tt.want {
			t.Errorf("Count(%d) = %q, want %q", tt.n, got, tt.want)
		}
	}
}
