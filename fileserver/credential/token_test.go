package credential

import (
	"crypto/sha256"
	"strings"
	"testing"
)

func TestNewTokenRoundTrip(t *testing.T) {
	for _, kind := range []Kind{KindDevice, KindSession, KindAccess, KindS3} {
		tok, s, err := NewToken(kind)
		if err != nil {
			t.Fatalf("NewToken(%q): %v", kind, err)
		}

		if !strings.HasPrefix(s, "silo_"+string(kind)+"_") {
			t.Errorf("NewToken(%q) = %q, want the kind visible in the prefix", kind, s)
		}
		if len(tok.Secret) != secretBytes {
			t.Errorf("NewToken(%q): secret is %d bytes, want %d", kind, len(tok.Secret), secretBytes)
		}

		got, err := ParseToken(s)
		if err != nil {
			t.Fatalf("ParseToken(NewToken(%q)): %v", kind, err)
		}
		if got.Kind != tok.Kind || got.ID != tok.ID || string(got.Secret) != string(tok.Secret) {
			t.Errorf("round trip changed the token: %+v then %+v", tok, got)
		}

		sum := sha256.Sum256(tok.Secret)
		if string(got.SecretHash()) != string(sum[:]) {
			t.Errorf("SecretHash is not SHA-256 of the secret")
		}
	}
}

func TestNewTokenIsUnique(t *testing.T) {
	// A collision here is a broken generator, not bad luck: ids are 80 bits
	// and the primary key depends on them.
	seen := make(map[string]bool)
	for i := 0; i < 1000; i++ {
		tok, _, err := NewToken(KindDevice)
		if err != nil {
			t.Fatalf("NewToken: %v", err)
		}
		if seen[tok.ID] {
			t.Fatalf("duplicate credential id %q after %d draws", tok.ID, i)
		}
		seen[tok.ID] = true
	}
}

func TestNewTokenRejectsUnknownKind(t *testing.T) {
	if _, _, err := NewToken(Kind("wat")); err == nil {
		t.Error("NewToken accepted an unknown kind")
	}
}

// A mistyped or truncated credential must be rejected as malformed — before
// any lookup — rather than as invalid after one.
func TestParseTokenRejectsMalformed(t *testing.T) {
	_, valid, err := NewToken(KindDevice)
	if err != nil {
		t.Fatalf("NewToken: %v", err)
	}

	tests := []struct {
		name string
		in   string
	}{
		{"empty", ""},
		{"no prefix", strings.TrimPrefix(valid, "silo_")},
		{"wrong prefix", "xyzz_" + strings.TrimPrefix(valid, "silo_")},
		{"too few fields", "silo_device_" + strings.Repeat("a", 68)},
		{"unknown kind", strings.Replace(valid, "_device_", "_wat_", 1)},
		{"truncated", valid[:len(valid)-1]},
		{"one character over", valid + "a"},
		{"uppercased", strings.ToUpper(valid)},
		{"transposed", transpose(valid)},
	}

	for _, tt := range tests {
		if got, err := ParseToken(tt.in); err == nil {
			t.Errorf("ParseToken(%s) = %+v, want error", tt.name, got)
		}
	}
}

// Every single-character substitution in the secret must be caught by the
// checksum. Thirty bits means the odds of missing one are about 1 in a
// billion, so a sweep over the whole string should find none.
func TestParseTokenCatchesSingleCharacterErrors(t *testing.T) {
	_, valid, err := NewToken(KindSession)
	if err != nil {
		t.Fatalf("NewToken: %v", err)
	}

	const alphabet = "abcdefghijklmnopqrstuvwxyz234567"
	missed := 0
	for i := len("silo_session_"); i < len(valid); i++ {
		for _, c := range alphabet {
			if byte(c) == valid[i] {
				continue
			}
			mutated := valid[:i] + string(c) + valid[i+1:]
			if _, err := ParseToken(mutated); err == nil {
				missed++
			}
		}
	}
	if missed != 0 {
		t.Errorf("%d single-character mutations were accepted", missed)
	}
}

// The checksum must cover the kind and the id, not only the secret — or a
// credential could be re-labelled into another lane and still look well
// formed.
func TestChecksumCoversTheWholeToken(t *testing.T) {
	tok, valid, err := NewToken(KindAccess)
	if err != nil {
		t.Fatalf("NewToken: %v", err)
	}

	relabelled := strings.Replace(valid, "_access_", "_device_", 1)
	if _, err := ParseToken(relabelled); err == nil {
		t.Error("a token relabelled into another kind was accepted")
	}

	// Swapping in another credential's id must fail too.
	other, _, err := NewToken(KindAccess)
	if err != nil {
		t.Fatalf("NewToken: %v", err)
	}
	swapped := strings.Replace(valid, tok.ID, other.ID, 1)
	if swapped == valid {
		t.Fatal("test did not substitute the id")
	}
	if _, err := ParseToken(swapped); err == nil {
		t.Error("a token carrying another credential's id was accepted")
	}
}

func TestParseErrorsDoNotLeakTheSecret(t *testing.T) {
	tok, valid, err := NewToken(KindDevice)
	if err != nil {
		t.Fatalf("NewToken: %v", err)
	}
	secret := b32.EncodeToString(tok.Secret)

	for _, in := range []string{valid[:len(valid)-1], valid + "a", strings.ToUpper(valid)} {
		_, err := ParseToken(in)
		if err == nil {
			continue
		}
		if strings.Contains(err.Error(), secret) || strings.Contains(strings.ToLower(err.Error()), strings.ToLower(secret)) {
			t.Errorf("error message contains the secret: %v", err)
		}
	}
}

func transpose(s string) string {
	b := []byte(s)
	i := len(b) - 8 // inside the secret, before the checksum
	b[i], b[i+1] = b[i+1], b[i]
	return string(b)
}
