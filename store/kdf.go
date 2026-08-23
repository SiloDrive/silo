package store

import (
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"golang.org/x/crypto/argon2"
)

// KDFAlgorithm names the one password KDF this format defines. It is written
// into every parameter string so a second algorithm can be added later without
// a format change, the way ChunkerAlgorithm does for the chunker.
const KDFAlgorithm = "argon2id"

// KDFVersion is argon2's own version number, 0x13, as it appears in an argon2
// encoded string. It is not this format's version.
const KDFVersion = 19

// KDFSaltSize is the per-user salt the parameters carry. Sixteen bytes is
// argon2's recommendation and RFC 9106's floor.
const KDFSaltSize = 16

// The default cost. Bitwarden's client-side parameters, which is the closest
// published prior art to what this does: derivation on the user's own device,
// once per enrolment, on hardware ranging from a phone to a workstation.
//
// Memory is in KiB, the unit argon2 and its encoded string both use — 65536
// KiB is 64 MiB. Getting this wrong by a factor of 1024 is the classic
// argon2 parameter bug, and it fails silently: derivation still works, it is
// simply cheap.
const (
	DefaultKDFMemory = 64 << 10 // KiB
	DefaultKDFTime   = 3
	DefaultKDFLanes  = 4
)

// The floor. Parameters ride inside the wrapped blob and are therefore
// attacker-influenced: whoever writes the blob chooses the cost at which the
// key that opens it was derived.
//
// These are OWASP's published minimum for argon2id, chosen because a floor
// wants a citation behind it rather than an opinion. A port that disagrees is
// disagreeing with a number it can look up.
const (
	MinKDFMemory = 19456 // KiB — 19 MiB
	MinKDFTime   = 2
	MinKDFLanes  = 1
)

// The ceiling, which is the same attack from the other end. A server that
// answers a pre-login parameter request with m=4 GiB does not weaken anything
// — it stops the client dead, or takes the device down trying. The floor is
// the one everybody thinks of; a bound needs both sides.
const (
	MaxKDFMemory = 1 << 20 // KiB — 1 GiB
	MaxKDFTime   = 16
	MaxKDFLanes  = 16
)

// ErrKDFParams reports parameters this format will not derive under: outside
// the bounds, or a string that is not one of these parameters.
var ErrKDFParams = errors.New("invalid password KDF parameters")

// KDFParams is the cost and salt one account's password is stretched under.
//
// These are data, not constants, and that is the deliberate choice: a constant
// can never be raised, and this system will outlive the hardware the default
// was chosen on. What it costs is that the parameters become an input an
// attacker can influence — hence Validate, and hence the rule that it runs on
// every derivation rather than once at enrolment.
type KDFParams struct {
	Memory uint32 // KiB
	Time   uint32
	Lanes  uint8
	Salt   [KDFSaltSize]byte
}

// DefaultKDFParams is what a client uses when it is the one choosing.
func DefaultKDFParams(salt [KDFSaltSize]byte) KDFParams {
	return KDFParams{
		Memory: DefaultKDFMemory,
		Time:   DefaultKDFTime,
		Lanes:  DefaultKDFLanes,
		Salt:   salt,
	}
}

// Validate enforces the bounds.
//
// This lives here, in the shared package, on the one code path every client
// calls — not as a rule each client remembers to apply. A guard that has to be
// re-implemented per port is a guard that one port ships without.
func (p KDFParams) Validate() error {
	switch {
	case p.Memory < MinKDFMemory:
		return fmt.Errorf("%w: m=%d is below the %d KiB floor", ErrKDFParams, p.Memory, MinKDFMemory)
	case p.Memory > MaxKDFMemory:
		return fmt.Errorf("%w: m=%d is above the %d KiB ceiling", ErrKDFParams, p.Memory, MaxKDFMemory)
	case p.Time < MinKDFTime:
		return fmt.Errorf("%w: t=%d is below the floor of %d", ErrKDFParams, p.Time, MinKDFTime)
	case p.Time > MaxKDFTime:
		return fmt.Errorf("%w: t=%d is above the ceiling of %d", ErrKDFParams, p.Time, MaxKDFTime)
	case p.Lanes < MinKDFLanes:
		return fmt.Errorf("%w: p=%d is below the floor of %d", ErrKDFParams, p.Lanes, MinKDFLanes)
	case p.Lanes > MaxKDFLanes:
		return fmt.Errorf("%w: p=%d is above the ceiling of %d", ErrKDFParams, p.Lanes, MaxKDFLanes)
	}
	return nil
}

// String renders the parameters as an argon2 encoded string, minus the hash:
//
//	$argon2id$v=19$m=65536,t=3,p=4$c2FsdHNhbHRzYWx0c2FsdA
//
// Self-describing, like every other stretched secret in this system. The
// alternative — three integers in a binary header — encodes the same values
// and tells a person looking at a blob nothing, and this string is the form
// every argon2 library on every platform already parses.
//
// Salt is unpadded standard base64, which is what the PHC format specifies —
// standard, not URL: the one base64url in this format is NameToURL.
func (p KDFParams) String() string {
	return fmt.Sprintf("$%s$v=%d$m=%d,t=%d,p=%d$%s",
		KDFAlgorithm, KDFVersion, p.Memory, p.Time, p.Lanes,
		base64.RawStdEncoding.EncodeToString(p.Salt[:]))
}

// ParseKDFParams reads what String wrote, and refuses anything else.
//
// Strict by construction: exact field order, no whitespace, no unknown fields,
// no second algorithm. The parameters decide a key, so a parser that shrugs at
// a field it does not recognise is a parser that derives the wrong key and
// reports success.
//
// It does NOT validate the bounds. Reading a blob to find out what is wrong
// with it has to be possible; deriving under what it says does not. Every
// derivation path calls Validate itself.
func ParseKDFParams(s string) (KDFParams, error) {
	fields := strings.Split(s, "$")
	// A leading "$" makes the first field empty: "", alg, v, params, salt.
	if len(fields) != 5 || fields[0] != "" {
		return KDFParams{}, fmt.Errorf("%w: %q is not an argon2 encoded string", ErrKDFParams, s)
	}
	if fields[1] != KDFAlgorithm {
		return KDFParams{}, fmt.Errorf("%w: algorithm %q, this format defines only %q",
			ErrKDFParams, fields[1], KDFAlgorithm)
	}
	if fields[2] != "v="+strconv.Itoa(KDFVersion) {
		return KDFParams{}, fmt.Errorf("%w: %q, want v=%d", ErrKDFParams, fields[2], KDFVersion)
	}

	var p KDFParams
	costs := strings.Split(fields[3], ",")
	if len(costs) != 3 {
		return KDFParams{}, fmt.Errorf("%w: %q is not m,t,p", ErrKDFParams, fields[3])
	}
	m, err := kdfCost(costs[0], "m")
	if err != nil {
		return KDFParams{}, err
	}
	t, err := kdfCost(costs[1], "t")
	if err != nil {
		return KDFParams{}, err
	}
	lanes, err := kdfCost(costs[2], "p")
	if err != nil {
		return KDFParams{}, err
	}
	if lanes > 255 {
		return KDFParams{}, fmt.Errorf("%w: p=%d does not fit a lane count", ErrKDFParams, lanes)
	}
	p.Memory, p.Time, p.Lanes = m, t, uint8(lanes)

	salt, err := base64.RawStdEncoding.DecodeString(fields[4])
	if err != nil {
		return KDFParams{}, fmt.Errorf("%w: salt is not unpadded base64: %v", ErrKDFParams, err)
	}
	if len(salt) != KDFSaltSize {
		return KDFParams{}, fmt.Errorf("%w: salt is %d bytes, want %d", ErrKDFParams, len(salt), KDFSaltSize)
	}
	copy(p.Salt[:], salt)
	return p, nil
}

// kdfCost reads one "m=65536" field.
//
// The leading-zero rule is the only one ParseUint does not already apply: it
// takes no sign, no underscores, no spaces and no non-ASCII digits, but it
// reads "03" as 3. Two spellings of one number is one spelling too many for a
// string that has to round-trip byte for byte, so that one is refused here.
func kdfCost(field, name string) (uint32, error) {
	digits, ok := strings.CutPrefix(field, name+"=")
	if !ok {
		return 0, fmt.Errorf("%w: %q is not %s=", ErrKDFParams, field, name)
	}
	if len(digits) > 1 && digits[0] == '0' {
		return 0, fmt.Errorf("%w: %q is not a canonical number", ErrKDFParams, field)
	}
	v, err := strconv.ParseUint(digits, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("%w: %q is not a number", ErrKDFParams, field)
	}
	return uint32(v), nil
}

// The two halves of the password, and the domains that separate them.
const (
	domainAuth = "silo/auth/v1"
	domainWrap = "silo/wrap/v1"
)

// Credentials is what a password becomes on the client, and the whole point of
// the split: one of these two values goes to the server and the other never
// does.
type Credentials struct {
	// AuthKey is sent to the server as the password. The server stores a hash
	// of it and can do nothing else with it.
	AuthKey []byte
	// WrapKey opens the identity key. It never leaves the device — not to the
	// server, not into a log, not into a crash report.
	WrapKey []byte
}

// DeriveCredentials performs the client-side split.
//
//	master  = argon2id(password, salt, params)
//	authKey = HKDF-SHA256(master, salt="silo/auth/v1", info="", L=32)
//	wrapKey = HKDF-SHA256(master, salt="silo/wrap/v1", info="", L=32)
//
// The trap this closes: silo's login sends the raw password to the server. If
// that same password also wrapped the identity key, the server would see the
// wrapping secret at every login and the end-to-end encryption would be
// decoration. After the split the server sees authKey, which unwraps nothing.
//
// Deriving both from one master rather than running argon2id twice matters
// more than it looks: two derivations would double the cost of every login for
// no gain, and HKDF's whole job is turning one strong secret into several
// independent ones.
//
// The two are independent in the sense that matters — recovering wrapKey from
// authKey means inverting HKDF — but they are not independent of the password.
// A password weak enough to guess yields both. That is what argon2id is for
// and why the floor above is enforced rather than suggested.
func DeriveCredentials(password string, p KDFParams) (Credentials, error) {
	if err := p.Validate(); err != nil {
		return Credentials{}, err
	}
	master := argon2.IDKey([]byte(password), p.Salt[:], p.Time, p.Memory, p.Lanes, 32)

	auth, err := hkdf.Key(sha256.New, master, []byte(domainAuth), "", 32)
	if err != nil {
		return Credentials{}, fmt.Errorf("store: deriving auth key: %w", err)
	}
	wrap, err := hkdf.Key(sha256.New, master, []byte(domainWrap), "", 32)
	if err != nil {
		return Credentials{}, fmt.Errorf("store: deriving wrap key: %w", err)
	}
	return Credentials{AuthKey: auth, WrapKey: wrap}, nil
}
