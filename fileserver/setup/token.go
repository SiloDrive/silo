// Package setup owns the one-time token that creates a server's first account.
//
// A brand new Silo has no accounts, so it has nothing to authenticate anyone
// against and no way to tell its owner apart from anyone else who can reach the
// port. The setup token is the answer: the server mints one on a boot that
// finds no accounts, prints it where only someone with access to the host can
// read it, and trades it exactly once for an account whose address and password
// the operator chooses. It authenticates nobody, names no account, and grants
// no access to anything -- it authorises one irreversible transition, from a
// server with no accounts to a server with one.
//
// It is deliberately not a Credential. docs/auth.md's rule that every secret a
// client presents is a row in Credential resolved by credential.Resolve cannot
// reach here: Credential.account_id references Account(id), and the whole
// premise of this token is that no Account row exists yet. The rule's own table
// is unreachable at this point in the server's life.
package setup

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base32"
	"errors"
	"regexp"
	"strings"
)

// alphabet is Crockford's base32: no I, L, O or U, so nothing in a printed
// token can be confused for something else at a glance. It is a named constant
// because the encoder and the redaction pattern must be drawn from the same
// thirty-two characters, and a second copy is how they would stop being.
const alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// crockford encodes and decodes in that alphabet. base32.NewEncoding panics
// unless given 32 distinct bytes, and a package-level var is the one place
// where that panic is useful -- at init, not at first mint.
var crockford = base32.NewEncoding(alphabet).WithPadding(base32.NoPadding)

// tokenBytes is 80 bits, which is exactly sixteen base32 symbols with no
// padding and no leftover bits. Sixteen is short enough to read off a log line
// and retype, and 2^80 is far past anything a rate-limited endpoint could be
// walked through.
const tokenBytes = 10

// symbols is what String renders and Parse accepts, after the prefix.
const symbols = tokenBytes * 8 / 5

// prefix makes a leaked token identifiable on sight, the same reason
// credential tokens carry "silo_".
const prefix = "SILO"

// foldedPrefix is what prefix becomes after the ambiguous glyphs are folded:
// SILO contains an I and an O, so folding turns it into four *valid* symbols.
// It is the only spelling Parse has to strip, because folding maps SILO, SIL0,
// S1LO and S1L0 all onto it.
const foldedPrefix = "S110"

// ErrMalformed reports a string that cannot be a setup token, whatever is
// stored. Returned before any database is touched, so a typo costs a string
// comparison rather than a query -- the same shape as credential.ParseToken.
var ErrMalformed = errors.New("that is not a setup token")

// Token is a setup token in its decoded form. Fixed length, so comparing two
// leaks nothing through timing, not even their length.
type Token [tokenBytes]byte

// Generate returns a fresh token.
//
// There is no rejection sampling here, and its absence is deliberate rather
// than forgotten: authmgr.GeneratePassword redraws because its 56-character
// alphabet does not divide 256, so indexing into it with a random byte would
// favour the first few characters. Base32 is a bit packing rather than a
// modulus and 32 divides 256 exactly, so ten uniform bytes are already sixteen
// uniform symbols. Adding a loop here would be cargo.
func Generate() (Token, error) {
	var t Token
	if _, err := rand.Read(t[:]); err != nil {
		return Token{}, err
	}
	return t, nil
}

// String renders the token the way it is printed, stored and typed:
// SILO-K7M4-9XQ2-8FTH-3WNP.
func (t Token) String() string {
	body := crockford.EncodeToString(t[:])

	var b strings.Builder
	b.WriteString(prefix)
	for i := 0; i < len(body); i += 4 {
		b.WriteByte('-')
		b.WriteString(body[i : i+4])
	}
	return b.String()
}

// IsZero reports the zero token, which Generate never returns.
func (t Token) IsZero() bool { return t == Token{} }

// Equal compares in constant time. The comparison is not the only thing
// standing between an attacker and the server -- eighty bits and a rate limiter
// are -- but a secret compared with == is a habit that outlives the place it
// was safe in.
func (t Token) Equal(other Token) bool {
	return subtle.ConstantTimeCompare(t[:], other[:]) == 1
}

// Parse reads a token as a person might have typed it: any case, dashes,
// spaces or underscores anywhere, the prefix present or missing, and the four
// glyphs Crockford leaves out substituted for the ones they look like.
//
// The prefix is stripped after the glyphs are folded, and once. SILO folds to
// S110, and so does every spelling of it an operator might transcribe off a
// screen -- SIL0, S1LO, S1L0 -- so folding first is what makes one strip cover
// all four.
func Parse(s string) (Token, error) {
	s = strings.Map(func(r rune) rune {
		switch r {
		case '-', ' ', '_':
			return -1
		}
		return r
	}, strings.ToUpper(s))

	s = fold(s)

	// Guarded on the length that remains, so a token whose own first four
	// symbols happen to be S110 is not eaten by the strip.
	if len(s) == symbols+len(foldedPrefix) {
		s = strings.TrimPrefix(s, foldedPrefix)
	}

	if len(s) != symbols {
		return Token{}, ErrMalformed
	}

	raw, err := crockford.DecodeString(s)
	if err != nil {
		return Token{}, ErrMalformed
	}

	var t Token
	copy(t[:], raw)
	return t, nil
}

// fold maps the four glyphs Crockford omits onto the ones they are mistaken
// for. Crockford's own rule, so a token read aloud or off a screen survives.
func fold(s string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case 'I', 'L':
			return '1'
		case 'O':
			return '0'
		case 'U':
			return 'V'
		}
		return r
	}, s)
}

// Redacted is what Redact leaves behind. It keeps the prefix so a reader can
// tell what was removed.
const Redacted = prefix + "-[redacted]"

// redactPattern matches a token as String renders one.
//
// It is built from the same constants String is -- the prefix, the group
// count, and the alphabet -- rather than written out again, so that changing
// the alphabet or the length moves the pattern with them. A hand-copied
// pattern that no longer matches what Generate produces is worse than no
// pattern: it compiles, it runs, and it still reads as protection. The
// alphabet contains no character that is special inside a class, which is what
// makes interpolating it safe.
var redactPattern = regexp.MustCompile(
	`\b` + prefix + strings.Repeat(`-[`+alphabet+`]{4}`, symbols/4) + `\b`)

// Redact replaces every setup token in s with Redacted.
//
// It lives here, beside String, because this package owns the format. Its one
// caller is the error reporter's BeforeSend hook, which decides *which* fields
// to run it over -- that is a policy question and belongs there; what a token
// looks like is this package's answer to give.
func Redact(s string) string {
	return redactPattern.ReplaceAllString(s, Redacted)
}
