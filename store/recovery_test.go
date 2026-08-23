package store

import (
	"errors"
	"strings"
	"testing"
)

func TestARecoveryCodeIsTheShapeThePrintoutPromises(t *testing.T) {
	code, err := GenerateRecoveryCode()
	if err != nil {
		t.Fatal(err)
	}
	if want := RecoveryCodeChars + RecoveryCodeChars/RecoveryCodeGroup - 1; len(code) != want {
		t.Fatalf("%q is %d characters, want %d", code, len(code), want)
	}
	if n := strings.Count(code, "-"); n != RecoveryCodeChars/RecoveryCodeGroup-1 {
		t.Fatalf("%q has %d separators", code, n)
	}
	normalized, err := NormalizeRecoveryCode(code)
	if err != nil {
		t.Fatal(err)
	}
	if len(normalized) != RecoveryCodeChars {
		t.Fatalf("normalized to %d characters", len(normalized))
	}
	secret, err := RecoveryCodeSecret(code)
	if err != nil {
		t.Fatal(err)
	}
	if len(secret) != RecoveryCodeSize {
		t.Fatalf("decoded to %d bytes, want %d", len(secret), RecoveryCodeSize)
	}
	// 160 bits in, 160 bits out, and the round trip is exact — there is no
	// padding case in a 32-character code and none is defined.
	if FormatRecoveryCode(crockfordEncode(secret)) != code {
		t.Fatalf("re-encoded to %q, want %q", FormatRecoveryCode(crockfordEncode(secret)), code)
	}
}

func TestASetIsTenDistinctCodes(t *testing.T) {
	codes, err := GenerateRecoveryCodeSet()
	if err != nil {
		t.Fatal(err)
	}
	if len(codes) != RecoveryCodeSetSize {
		t.Fatalf("a set holds %d codes, want %d", len(codes), RecoveryCodeSetSize)
	}
	seen := map[string]bool{}
	for _, c := range codes {
		if seen[c] {
			t.Fatalf("%q appeared twice in one set", c)
		}
		seen[c] = true
	}
}

// Crockford's alphabet exists to forgive transcription, and forgiveness only
// works if two clients forgive identically. Every one of these must decode to
// the same twenty bytes.
func TestNormalizationForgivesExactlyWhatItPromises(t *testing.T) {
	canonical := "7ZQD8M4X0RKBVN3WPGHJ0123456789AB"
	for _, tc := range []struct {
		what string
		in   string
	}{
		{"as printed", "7ZQD-8M4X-0RKB-VN3W-PGHJ-0123-4567-89AB"},
		{"lower case", "7zqd-8m4x-0rkb-vn3w-pghj-0123-4567-89ab"},
		{"mixed case", "7zQd-8M4x-0rKb-Vn3W-pGhJ-0123-4567-89aB"},
		{"no separators", "7ZQD8M4X0RKBVN3WPGHJ0123456789AB"},
		{"spaces instead", "7ZQD 8M4X 0RKB VN3W PGHJ 0123 4567 89AB"},
		{"tabs and spaces", "7ZQD\t8M4X 0RKB-VN3W PGHJ\t0123 4567-89AB"},
		{"separators in the wrong places", "7-Z-Q-D8M4X0RKBVN3WPGHJ0123456789AB"},
		{"O typed for zero", "7ZQD-8M4X-ORKB-VN3W-PGHJ-O123-4567-89AB"},
		{"I typed for one", "7ZQD-8M4X-0RKB-VN3W-PGHJ-0I23-4567-89AB"},
		{"lower L typed for one", "7zqd-8m4x-0rkb-vn3w-pghj-0l23-4567-89ab"},
	} {
		got, err := NormalizeRecoveryCode(tc.in)
		if err != nil {
			t.Errorf("%s: %v", tc.what, err)
			continue
		}
		if got != canonical {
			t.Errorf("%s normalized to %q, want %q", tc.what, got, canonical)
		}
	}
}

// And forgives nothing else. A decoder that guesses turns a typo into a wrong
// key and an authentication failure the user cannot tell from a wrong code.
func TestNormalizationForgivesNothingElse(t *testing.T) {
	for _, tc := range []struct {
		what string
		in   string
	}{
		{"empty", ""},
		{"too short", "7ZQD8M4X0RKBVN3WPGHJ0123456789A"},
		{"too long", "7ZQD8M4X0RKBVN3WPGHJ0123456789ABC"},
		{"U, which Crockford excludes", "7ZQD8M4X0RKBVN3WPGHJ0123456789AU"},
		{"underscore as a separator", "7ZQD_8M4X_0RKB_VN3W_PGHJ_0123_4567_89AB"},
		{"a newline", "7ZQD8M4X0RKBVN3WPGHJ0123456789AB\n"},
		{"punctuation", "7ZQD8M4X0RKBVN3WPGHJ0123456789A!"},
		{"non-ASCII", "7ZQD8M4X0RKBVN3WPGHJ0123456789Aé"},
	} {
		if _, err := NormalizeRecoveryCode(tc.in); !errors.Is(err, ErrRecoveryCode) {
			t.Errorf("%s was accepted", tc.what)
		}
	}
}

func TestARecoveryCodeOpensTheIdentityItWrapped(t *testing.T) {
	id := testIdentity(t)
	code, err := GenerateRecoveryCode()
	if err != nil {
		t.Fatal(err)
	}
	blob, err := WrapForRecovery(code, testHolder, id.Private())
	if err != nil {
		t.Fatal(err)
	}
	got, err := UnwrapWithRecovery(code, testHolder, blob)
	if err != nil {
		t.Fatal(err)
	}
	if got != id.Private() {
		t.Fatal("the recovery wrap returned a different key")
	}

	// The forgiveness has to reach all the way through the wrap, not just the
	// normalizer — this is how a person actually types it back in.
	messy := strings.ToLower(strings.ReplaceAll(code, "0", "O"))
	if got, err := UnwrapWithRecovery(messy, testHolder, blob); err != nil || got != id.Private() {
		t.Fatalf("the code as a person would type it did not open the wrap: %v", err)
	}
}

func TestAnotherCodeDoesNotOpenIt(t *testing.T) {
	id := testIdentity(t)
	mine, err := GenerateRecoveryCode()
	if err != nil {
		t.Fatal(err)
	}
	theirs, err := GenerateRecoveryCode()
	if err != nil {
		t.Fatal(err)
	}
	blob, err := WrapForRecovery(mine, testHolder, id.Private())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := UnwrapWithRecovery(theirs, testHolder, blob); !errors.Is(err, ErrWrap) {
		t.Fatalf("a different code opened the wrap: %v", err)
	}
	if _, err := UnwrapWithRecovery(mine, "account:9", blob); !errors.Is(err, ErrWrap) {
		t.Fatalf("the wrap opened for the wrong account: %v", err)
	}
}

// Redemption is single-use and the rest of the set stands. The format makes
// that possible by wrapping once per code: the blobs are independent, so
// deleting one invalidates exactly one code.
func TestRedeemingOneCodeLeavesTheRestOfTheSetStanding(t *testing.T) {
	id := testIdentity(t)
	codes, err := GenerateRecoveryCodeSet()
	if err != nil {
		t.Fatal(err)
	}
	blobs := map[string][]byte{}
	for _, c := range codes {
		blob, err := WrapForRecovery(c, testHolder, id.Private())
		if err != nil {
			t.Fatal(err)
		}
		blobs[c] = blob
	}

	// The server deletes the blob for the code that was just used.
	delete(blobs, codes[0])
	if _, ok := blobs[codes[0]]; ok {
		t.Fatal("redemption did not remove the blob")
	}
	for _, c := range codes[1:] {
		got, err := UnwrapWithRecovery(c, testHolder, blobs[c])
		if err != nil || got != id.Private() {
			t.Fatalf("a code that was not used stopped working: %v", err)
		}
	}
}

// Two wraps of one key under one code must not repeat a (key, nonce) pair,
// which is what the per-wrap salt is for on this side of the format.
func TestEveryRecoveryWrapCarriesItsOwnSalt(t *testing.T) {
	id := testIdentity(t)
	code, err := GenerateRecoveryCode()
	if err != nil {
		t.Fatal(err)
	}
	first, err := WrapForRecovery(code, testHolder, id.Private())
	if err != nil {
		t.Fatal(err)
	}
	second, err := WrapForRecovery(code, testHolder, id.Private())
	if err != nil {
		t.Fatal(err)
	}
	if string(first) == string(second) {
		t.Fatal("two wraps under one code produced identical blobs")
	}
	if string(first[2:2+WrapSaltSize]) == string(second[2:2+WrapSaltSize]) {
		t.Fatal("the per-wrap salt repeated")
	}
}

// The two identity wraps are different kinds and neither opens as the other,
// even though both seal the same thirty-two bytes.
func TestAPasswordWrapAndARecoveryWrapAreNotInterchangeable(t *testing.T) {
	id := testIdentity(t)
	code, err := GenerateRecoveryCode()
	if err != nil {
		t.Fatal(err)
	}
	secret, err := RecoveryCodeSecret(code)
	if err != nil {
		t.Fatal(err)
	}
	recoveryBlob, err := WrapForRecovery(code, testHolder, id.Private())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := UnwrapIdentity(secret, testHolder, recoveryBlob); !errors.Is(err, ErrWrap) {
		t.Fatalf("a recovery blob opened on the password path: %v", err)
	}

	passwordBlob, err := WrapIdentity(testWrapKey(t), testHolder, cheapParams(), id.Private())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := UnwrapWithRecovery(code, testHolder, passwordBlob); !errors.Is(err, ErrWrap) {
		t.Fatalf("a password blob opened on the recovery path: %v", err)
	}
}
