package credential

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"fmt"
	"strings"
)

// Kind identifies which lane a credential belongs to. docs/auth.md's table of
// kinds is the authority on what each one is held by and how long it lives.
type Kind string

const (
	KindDevice  Kind = "device"  // Porter, the File Provider extension
	KindSession Kind = "session" // the TUI, the CLI
	KindAccess  Kind = "access"  // capability URLs
	KindS3      Kind = "s3"      // an S3 frontend, if it is ever built
)

func (k Kind) valid() bool {
	switch k {
	case KindDevice, KindSession, KindAccess, KindS3:
		return true
	}
	return false
}

// The token format, from docs/auth.md:
//
//	silo_<kind>_<id>_<base32(32 random bytes)><check>
//
// The silo_ prefix and the kind make a leaked credential identifiable on sight
// and let a handler reject one meant for another lane before touching the
// database. check is six base32 characters of SHA-256 over everything before
// it, so a truncated paste or a mistyped character is rejected as malformed
// rather than as invalid after a lookup.
const (
	tokenPrefix = "silo_"

	idBytes     = 10 // no padding, no leftover bits
	secretBytes = 32 // 256 bits, as auth.md specifies
	checkChars  = 6
)

// The encoded widths of the two halves. Derived rather than written down: a
// hand-computed length that disagrees with its bytes is a parser that rejects
// every token it mints, and nothing would fail to compile.
var (
	idChars     = b32.EncodedLen(idBytes)
	secretChars = b32.EncodedLen(secretBytes)
)

// Lowercase RFC 4648 base32, unpadded. Lowercase because the silo_ prefix and
// the kind are, and a token that changes case halfway is one more thing to get
// wrong when reading it out of a log. Unpadded because '=' is awkward in an
// environment variable and a URL.
//
// Decoding is strict: see decodeStrict. base32 is conventionally
// case-insensitive, but the check covers the literal string, so accepting an
// uppercase spelling would mean accepting a token whose own checksum does not
// match it. One encoding, one spelling.
var b32 = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

// A Token is a parsed credential string. It carries no proof that the
// credential exists or that the secret is right — that is Resolve's job. It
// carries only what can be known without touching the database.
type Token struct {
	Kind Kind

	// ID is the public half. It is the primary key of the Credential row, and
	// it is what a lookup uses, so the secret never appears in a query, a
	// query log, or a slow-query trace.
	ID string

	// Secret is the presented secret. Only SHA-256(Secret) is ever stored.
	Secret []byte
}

// SecretHash is what the Credential row stores.
func (t Token) SecretHash() []byte {
	sum := sha256.Sum256(t.Secret)
	return sum[:]
}

// NewToken mints a credential of the given kind, returning the parsed form and
// the string to hand to the client. The string is the only time the secret is
// representable — the caller stores SecretHash and forgets the rest.
func NewToken(kind Kind) (Token, string, error) {
	if !kind.valid() {
		return Token{}, "", fmt.Errorf("unknown credential kind %q", kind)
	}

	idRaw := make([]byte, idBytes)
	if _, err := rand.Read(idRaw); err != nil {
		return Token{}, "", fmt.Errorf("generating credential id: %v", err)
	}
	secret := make([]byte, secretBytes)
	if _, err := rand.Read(secret); err != nil {
		return Token{}, "", fmt.Errorf("generating credential secret: %v", err)
	}

	t := Token{Kind: kind, ID: b32.EncodeToString(idRaw), Secret: secret}
	return t, t.String(), nil
}

// String renders the credential as the client presents it. It contains the
// secret, so it belongs in a response body and nowhere else — not a log line,
// not an error, not a span attribute.
func (t Token) String() string {
	body := tokenPrefix + string(t.Kind) + "_" + t.ID + "_" + b32.EncodeToString(t.Secret)
	return body + checksum(body)
}

// checksum is the last six characters of the token: base32 of SHA-256 over
// everything before it. Thirty bits — enough that a mistyped character is
// caught, not enough to be mistaken for authentication.
func checksum(body string) string {
	sum := sha256.Sum256([]byte(body))
	return b32.EncodeToString(sum[:])[:checkChars]
}

// ParseToken reads a credential string presented by a client.
//
// Every error here means *malformed*, which is a different answer from
// invalid: nothing was looked up, so nothing about the credential's existence
// has been learned or leaked. Errors say what is structurally wrong, never
// what the value was.
func ParseToken(s string) (Token, error) {
	if !strings.HasPrefix(s, tokenPrefix) {
		return Token{}, fmt.Errorf("not a silo credential")
	}

	// Four fields: the prefix, the kind, the id, and the secret with the
	// checksum appended. No field but the last can contain an underscore, so
	// splitting is unambiguous.
	parts := strings.SplitN(s, "_", 4)
	if len(parts) != 4 {
		return Token{}, fmt.Errorf("malformed credential: expected 4 fields")
	}
	kind := Kind(parts[1])
	id, tail := parts[2], parts[3]

	if !kind.valid() {
		return Token{}, fmt.Errorf("unknown credential kind %q", kind)
	}
	if len(id) != idChars {
		return Token{}, fmt.Errorf("malformed credential: id is %d characters, want %d", len(id), idChars)
	}
	if len(tail) != secretChars+checkChars {
		return Token{}, fmt.Errorf("malformed credential: secret is %d characters, want %d",
			len(tail), secretChars+checkChars)
	}

	// Verify the checksum before anything else is trusted. This is what turns
	// a truncated paste into "malformed" at no cost rather than "invalid"
	// after a database round trip.
	body, check := s[:len(s)-checkChars], s[len(s)-checkChars:]
	if checksum(body) != check {
		return Token{}, fmt.Errorf("malformed credential: checksum mismatch")
	}

	if _, err := decodeStrict(id); err != nil {
		return Token{}, fmt.Errorf("malformed credential: id: %v", err)
	}
	secret, err := decodeStrict(tail[:secretChars])
	if err != nil {
		return Token{}, fmt.Errorf("malformed credential: secret: %v", err)
	}

	return Token{Kind: kind, ID: id, Secret: secret}, nil
}

// decodeStrict decodes base32 and rejects any spelling that is not the one the
// encoder produces. Go's decoder tolerates non-zero trailing bits, so two
// different strings can decode to identical bytes — which would mean two
// spellings of one credential id, and an id is a primary key. Re-encoding and
// comparing costs nothing and leaves exactly one valid form.
func decodeStrict(s string) ([]byte, error) {
	raw, err := b32.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("not base32")
	}
	if b32.EncodeToString(raw) != s {
		return nil, fmt.Errorf("not canonical base32")
	}
	return raw, nil
}
