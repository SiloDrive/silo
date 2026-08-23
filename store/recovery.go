package store

import (
	"crypto/rand"
	"encoding/base32"
	"errors"
	"fmt"
	"strings"
)

// A recovery code is 160 bits, rendered in Crockford's base32 as four groups
// of eight characters:
//
//	7ZQD-8M4X-0RKB-VN3W-… (32 characters, 160 bits, no check symbol)
//
// 160 bits is the number that lets the wrap skip a password KDF entirely. A
// code this size is not guessable, so stretching it would cost the user
// seconds of argon2id at the worst possible moment — the one where they have
// already lost something — and buy nothing. HKDF is the right primitive for a
// secret that is already strong, and it is the primitive the rest of this
// format already uses.
const (
	RecoveryCodeBits  = 160
	RecoveryCodeSize  = RecoveryCodeBits / 8 // 20 bytes
	RecoveryCodeChars = RecoveryCodeBits / 5 // 32 characters, exactly, no padding
	RecoveryCodeGroup = 8
)

// RecoveryCodeSetSize is how many codes one set holds.
//
// Ten. Enough that a user who burns a few over the years is not immediately
// back where they started, few enough to print on a card. The set is generated
// together and shown once; the server stores one wrapped blob per code and
// never sees a code itself.
const RecoveryCodeSetSize = 10

// crockfordAlphabet excludes I, L, O and U: the first three because they are
// misread as 1, 1 and 0, and U because excluding it keeps a random code from
// spelling something the user has to read aloud to support.
const crockfordAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// ErrRecoveryCode reports a string that is not a recovery code.
var ErrRecoveryCode = errors.New("invalid recovery code")

// GenerateRecoveryCode mints one code in display form.
func GenerateRecoveryCode() (string, error) {
	var b [RecoveryCodeSize]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("store: generating recovery code: %w", err)
	}
	return FormatRecoveryCode(crockfordEncode(b[:])), nil
}

// GenerateRecoveryCodeSet mints a full set.
func GenerateRecoveryCodeSet() ([]string, error) {
	codes := make([]string, 0, RecoveryCodeSetSize)
	for range RecoveryCodeSetSize {
		code, err := GenerateRecoveryCode()
		if err != nil {
			return nil, err
		}
		codes = append(codes, code)
	}
	return codes, nil
}

// FormatRecoveryCode inserts the group separators. Display only — the groups
// are never part of what is decoded.
func FormatRecoveryCode(normalized string) string {
	var b strings.Builder
	for i := 0; i < len(normalized); i += RecoveryCodeGroup {
		if i > 0 {
			b.WriteByte('-')
		}
		b.WriteString(normalized[i:min(i+RecoveryCodeGroup, len(normalized))])
	}
	return b.String()
}

// NormalizeRecoveryCode turns what a person typed into the 32 characters that
// get decoded, and is the whole reason this format uses Crockford's alphabet
// rather than a hex string or standard base32.
//
// The rule, pinned so that two clients apply it identically:
//
//  1. ASCII spaces, tabs and hyphens are separators and are removed. They may
//     appear anywhere, not only where FormatRecoveryCode put them.
//  2. Letters are upper-cased.
//  3. I and L become 1; O becomes 0. This is Crockford's transcription rule
//     and it runs after upper-casing, so a lower-case l is covered too.
//  4. Everything remaining must be in the alphabet, and there must be exactly
//     32 characters of it.
//
// Nothing else is forgiven. U is not remapped to V — it is not in the
// alphabet, so a code containing one was mistyped, and a decoder that guesses
// at that turns a typo into a wrong key and an authentication failure the user
// cannot distinguish from a wrong code.
func NormalizeRecoveryCode(s string) (string, error) {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r == ' ' || r == '\t' || r == '-':
			continue
		case r >= 'a' && r <= 'z':
			r -= 'a' - 'A'
		}
		switch r {
		case 'I', 'L':
			r = '1'
		case 'O':
			r = '0'
		}
		if !strings.ContainsRune(crockfordAlphabet, r) {
			return "", fmt.Errorf("%w: %q is not a code character", ErrRecoveryCode, r)
		}
		b.WriteRune(r)
	}
	if b.Len() != RecoveryCodeChars {
		return "", fmt.Errorf("%w: %d characters, want %d", ErrRecoveryCode, b.Len(), RecoveryCodeChars)
	}
	return b.String(), nil
}

// crockfordCoding is base32 over the alphabet above: five bits a character,
// most-significant first, which is what RFC 4648 base32 already is once the
// alphabet is swapped. Twenty bytes is 160 bits is exactly 32 characters, so
// there is no padding case and none is defined — hence NoPadding rather than a
// padding character that would never appear.
//
// A port on a platform without base32 writes the accumulator loop by hand; the
// rule it has to implement is stated here, and the vectors pin the result. That
// is not a reason for this side to hand-roll it too.
var crockfordCoding = base32.NewEncoding(crockfordAlphabet).WithPadding(base32.NoPadding)

// crockfordEncode renders the twenty bytes of a code.
func crockfordEncode(b []byte) string { return crockfordCoding.EncodeToString(b) }

// decodeRecoveryCode reverses crockfordEncode over an already-normalized code.
// NormalizeRecoveryCode has already guaranteed the length and the alphabet, so
// the decode below cannot fail on well-formed input; the error is returned
// because this is also the function a port checks its own decoder against.
func decodeRecoveryCode(normalized string) ([]byte, error) {
	if len(normalized) != RecoveryCodeChars {
		return nil, fmt.Errorf("%w: %d characters, want %d", ErrRecoveryCode, len(normalized), RecoveryCodeChars)
	}
	out, err := crockfordCoding.DecodeString(normalized)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrRecoveryCode, err)
	}
	return out, nil
}

// RecoveryCodeSecret is the key material one code carries: the decoded 160
// bits, after normalization. Exported so a client can hold the secret without
// holding the string a user typed.
func RecoveryCodeSecret(code string) ([]byte, error) {
	normalized, err := NormalizeRecoveryCode(code)
	if err != nil {
		return nil, err
	}
	return decodeRecoveryCode(normalized)
}

// WrapForRecovery seals an identity private key under one recovery code.
//
// One blob per code, each independent of the others. That independence is what
// makes the redemption rule cheap: using a code deletes its blob and nothing
// else, so the remaining codes keep working. Regenerating the whole set on
// every use would be the tidier-looking rule and a worse one — it invalidates
// the codes a person is still holding at the exact moment they have proved
// they lost their device.
func WrapForRecovery(code, holder string, priv [X25519KeySize]byte) ([]byte, error) {
	secret, err := RecoveryCodeSecret(code)
	if err != nil {
		return nil, err
	}
	var salt [WrapSaltSize]byte
	if _, err := rand.Read(salt[:]); err != nil {
		return nil, fmt.Errorf("store: wrap salt: %w", err)
	}
	return wrapSecret(secret, domainWrapRecovery, wrapKindRecovery, salt, "", holder, priv[:])
}

// UnwrapWithRecovery opens what WrapForRecovery sealed.
//
// The caller redeems the code afterwards: delete this blob, leave the rest of
// the set standing, and — since the password is what the user has lost — take
// them through setting a new one, which re-wraps the identity key under a new
// wrapKey and touches no content key at all.
func UnwrapWithRecovery(code, holder string, blob []byte) ([X25519KeySize]byte, error) {
	secret, err := RecoveryCodeSecret(code)
	if err != nil {
		return [X25519KeySize]byte{}, err
	}
	return unwrapIdentity(secret, domainWrapRecovery, wrapKindRecovery, holder, blob)
}
