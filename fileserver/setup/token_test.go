package setup

import (
	"strings"
	"testing"
)

// The codec is lossless: what the operator reads is what the server stored.
func TestSetupTokenRoundTrips(t *testing.T) {
	tok, err := Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	back, err := Parse(tok.String())
	if err != nil {
		t.Fatalf("Parse(%q): %v", tok, err)
	}
	if !back.Equal(tok) {
		t.Errorf("round trip changed the token: %q became %q", tok, back)
	}
}

// Sixteen symbols, which is where the eighty bits are. A shorter token is a
// weaker one, so the width is asserted rather than assumed.
func TestSetupTokenIsSixteenSymbols(t *testing.T) {
	tok, err := Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	rendered := tok.String()
	if !strings.HasPrefix(rendered, "SILO-") {
		t.Errorf("token %q does not start with SILO-", rendered)
	}

	body := strings.ReplaceAll(strings.TrimPrefix(rendered, "SILO"), "-", "")
	if len(body) != 16 {
		t.Errorf("token %q carries %d symbols, want 16", rendered, len(body))
	}
	for _, r := range body {
		if !strings.ContainsRune("0123456789ABCDEFGHJKMNPQRSTVWXYZ", r) {
			t.Errorf("token %q contains %q, which is not in the alphabet", rendered, r)
		}
	}
}

// Two tokens are never the same one. A thousand draws is not proof of a good
// random source, but it does catch a seeded or truncated one.
func TestSetupTokensDoNotRepeat(t *testing.T) {
	seen := make(map[Token]bool, 1000)
	for i := 0; i < 1000; i++ {
		tok, err := Generate()
		if err != nil {
			t.Fatalf("Generate: %v", err)
		}
		if seen[tok] {
			t.Fatalf("Generate returned %q twice in 1000 draws", tok)
		}
		seen[tok] = true
	}
}

// Everything about how it is written down is noise: case, the separators, and
// whether the prefix was copied along with it.
func TestSetupTokenIgnoresCaseDashesAndPrefix(t *testing.T) {
	tok, err := Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	canonical := tok.String()
	body := strings.ReplaceAll(strings.TrimPrefix(canonical, "SILO"), "-", "")

	for _, spelling := range []string{
		canonical,
		strings.ToLower(canonical),
		body,
		strings.ToLower(body),
		"SILO" + body,
		strings.ReplaceAll(canonical, "-", " "),
		strings.ReplaceAll(canonical, "-", "_"),
		strings.ReplaceAll(canonical, "-", ""),
		"  " + canonical + "  ",
	} {
		got, err := Parse(spelling)
		if err != nil {
			t.Errorf("Parse(%q): %v", spelling, err)
			continue
		}
		if !got.Equal(tok) {
			t.Errorf("Parse(%q) gave %q, want %q", spelling, got, tok)
		}
	}
}

// Crockford's fold: the four glyphs the alphabet leaves out are accepted as
// the ones they are mistaken for, so a token copied by eye still works.
//
// The SIL0 and S1LO cases are the ones that matter. SILO carries an I and an O
// of its own, so a fold that runs before the prefix is stripped turns the
// prefix itself into four valid symbols and the whole token becomes malformed.
func TestSetupTokenFoldsAmbiguousGlyphs(t *testing.T) {
	// A token whose body is all zeros and ones renders as digits, so
	// substituting the letters back in is a faithful mistyping.
	var tok Token
	canonical := tok.String()

	misread := strings.NewReplacer("1", "I", "0", "O")
	for _, spelling := range []string{
		misread.Replace(canonical),                     // every digit misread
		"SIL0" + strings.TrimPrefix(canonical, "SILO"), // the prefix's O misread
		"S1LO" + strings.TrimPrefix(canonical, "SILO"), // the prefix's I misread
		"S1L0" + strings.TrimPrefix(canonical, "SILO"), // both
		strings.ToLower(misread.Replace(canonical)),    // and in lower case
	} {
		got, err := Parse(spelling)
		if err != nil {
			t.Errorf("Parse(%q): %v", spelling, err)
			continue
		}
		if !got.Equal(tok) {
			t.Errorf("Parse(%q) gave %q, want %q", spelling, got, tok)
		}
	}

	// U folds to V, which the all-zeros token cannot show.
	withV, err := Parse("SILO-VVVV-VVVV-VVVV-VVVV")
	if err != nil {
		t.Fatalf("Parse of a V token: %v", err)
	}
	withU, err := Parse("SILO-UUUU-UUUU-UUUU-UUUU")
	if err != nil {
		t.Fatalf("Parse of a U token: %v", err)
	}
	if !withU.Equal(withV) {
		t.Error("U did not fold to V")
	}
}

// Anything that cannot be a token is refused before a database is touched, and
// refused the same way whatever is wrong with it.
func TestSetupTokenRejectsAnythingElse(t *testing.T) {
	for _, s := range []string{
		"",
		"SILO-",
		"SILO-K7M4",                     // too short
		"SILO-K7M4-9XQ2-8FTH-3WNP-XXXX", // too long
		"SILO-K7M4-9XQ2-8FTH-3WN",       // one symbol short
		"silo_session_aaaaaaaaaaaaaaaa", // a credential token, not this
		"................",              // right length, wrong alphabet
		"S110",                          // the folded prefix on its own
	} {
		if _, err := Parse(s); err == nil {
			t.Errorf("Parse(%q) succeeded, want ErrMalformed", s)
		}
	}
}

// A token wrong in one place is a different token, not a near miss that
// compares equal.
func TestOneWrongSymbolIsADifferentToken(t *testing.T) {
	tok, err := Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	body := []byte(strings.ReplaceAll(strings.TrimPrefix(tok.String(), "SILO"), "-", ""))
	if body[0] == 'Z' {
		body[0] = 'Y'
	} else {
		body[0] = 'Z'
	}

	other, err := Parse(string(body))
	if err != nil {
		t.Fatalf("Parse of the altered token: %v", err)
	}
	if other.Equal(tok) {
		t.Error("a token altered in one symbol compared equal to the original")
	}
}
