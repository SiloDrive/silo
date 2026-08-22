package account

import "testing"

// Two spellings of one address reaching two accounts is the failure the
// identity split exists to prevent, so the rule that stops it is worth
// pinning rather than leaving to whoever writes the next call site.
func TestNormalize(t *testing.T) {
	tests := []struct{ in, want string }{
		{"dan@example.com", "dan@example.com"},
		{"Dan@Example.COM", "dan@example.com"},
		{"  dan@example.com  ", "dan@example.com"},
		{"DAN@EXAMPLE.COM", "dan@example.com"},
		{"", ""},
		{"   ", ""},
	}
	for _, tt := range tests {
		if got := Normalize(tt.in); got != tt.want {
			t.Errorf("Normalize(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
