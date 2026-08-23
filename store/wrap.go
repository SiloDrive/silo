package store

import (
	"bytes"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/subtle"
	"errors"
	"fmt"
)

// The wrapping domains. One per thing that can hold a key, for the same reason
// the sealing domains exist: without them a wrapKey and a recovery code would
// derive into one key space.
const (
	domainWrapIdentity = "silo/wrap/idkey/v1"
	domainWrapRecovery = "silo/wrap/recovery/v1"
	domainWrapCK       = "silo/wrap/ck/v1"
)

// WrapVersion is the first byte of every wrapped blob.
const WrapVersion = 1

// The wrap kinds, which say what secret opens a wrapped identity key.
const (
	wrapKindPassword = 1
	wrapKindRecovery = 2
)

// X25519KeySize is the width of an identity key, public or private.
const X25519KeySize = 32

// CKSize is a library content key: 32 random bytes, generated client-side at
// library creation, never derived from a password. A key derived from a
// password could not be shared without sharing the password, and could not
// survive a password change.
const CKSize = 32

// WrapSaltSize is the per-wrap salt that makes the zero nonce correct for the
// password and recovery wraps — see wrapSecret.
const WrapSaltSize = 16

// The bounds on the two identifiers a wrap binds itself to. Both are text the
// caller supplies; see the spec's Key wrapping section for the exact spelling
// each one takes, because two clients that spell an account id differently
// produce blobs neither can open.
const (
	// UUIDTextBytes is the width of a UUID in canonical text, which is the only
	// thing either identifier may be — so it is a width, not a ceiling, and
	// checkIdentifier refuses anything else a moment after readBounded has
	// stopped a hostile length from allocating. One constant rather than two
	// because it states one fact; should the two identifiers ever stop being
	// the same shape, that is the day to spell them separately.
	UUIDTextBytes = 36

	// MaxKDFParamsBytes bounds the parameter string a wrapped identity
	// carries, so a hostile blob cannot make a parser allocate on a length it
	// chose. The longest string this format can write is 57 bytes — 15 for
	// "$argon2id$v=19$", 19 for "m=1048576,t=16,p=16" at the ceilings above,
	// 1 for the separator, and 22 for a 16-byte salt in unpadded base64 — so
	// this is that with room for a longer future parameter set, and the number
	// a reader enforces rather than the number a writer happens to produce.
	MaxKDFParamsBytes = 128
)

// ErrWrap reports a wrapped blob this format cannot have produced, or one that
// did not open. As with ErrDecrypt, a wrong key and a tampered blob are
// indistinguishable and the answer to both is the same.
var ErrWrap = errors.New("invalid wrapped key")

// Identity is a user's X25519 keypair: the key every library content key is
// wrapped to, and the only long-term secret a client keeps.
//
// One keypair per user, not per device. A device gets the unwrapped private
// key at enrolment and stores it in the platform key store; the wrapped blob
// on the server exists for the one moment a new device bootstraps and the
// password is typed again.
type Identity struct {
	priv *ecdh.PrivateKey
}

// GenerateIdentity mints a new keypair from the system CSPRNG.
func GenerateIdentity() (*Identity, error) {
	priv, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("store: generating identity: %w", err)
	}
	return &Identity{priv: priv}, nil
}

// IdentityFromPrivate rebuilds a keypair from stored private bytes.
//
// Any thirty-two bytes are a private key: X25519 clamping forces bit 254 set
// and the low three bits clear, and no scalar of that shape is a multiple of
// the group order, so even an all-zero input yields an ordinary public key
// rather than the identity point. There is nothing here to refuse. The
// degenerate case lives on the other side — a hostile *public* key — and is
// refused in sharedSecret.
func IdentityFromPrivate(priv [X25519KeySize]byte) (*Identity, error) {
	k, err := ecdh.X25519().NewPrivateKey(priv[:])
	if err != nil {
		return nil, fmt.Errorf("%w: not an X25519 private key: %v", ErrWrap, err)
	}
	return &Identity{priv: k}, nil
}

// Private returns the private key, for handing to the platform key store.
func (i *Identity) Private() [X25519KeySize]byte {
	var out [X25519KeySize]byte
	copy(out[:], i.priv.Bytes())
	return out
}

// Public returns the published half.
func (i *Identity) Public() [X25519KeySize]byte {
	var out [X25519KeySize]byte
	copy(out[:], i.priv.PublicKey().Bytes())
	return out
}

// WrapIdentity seals a private identity key under a password-derived wrapKey.
//
// The blob is what the server stores per account, opaque to it. holder is the
// account it belongs to — its id in canonical lower-case hyphenated UUID text,
// and nothing else; see checkIdentifier — bound as associated data so a server
// cannot hand one account's blob to another and watch what happens.
//
// The parameters ride inside the blob rather than beside it. Self-describing
// is how every other stretched secret in this system is stored, and the
// alternative — parameters in one table, blob in another — is a pair that can
// drift, after which the blob is unopenable and nothing says why.
func WrapIdentity(wrapKey []byte, holder string, p KDFParams, priv [X25519KeySize]byte) ([]byte, error) {
	if len(wrapKey) == 0 {
		return nil, fmt.Errorf("%w: wrapping an identity needs a wrap key", ErrWrap)
	}
	if err := p.Validate(); err != nil {
		return nil, err
	}
	var salt [WrapSaltSize]byte
	if _, err := rand.Read(salt[:]); err != nil {
		return nil, fmt.Errorf("store: wrap salt: %w", err)
	}
	return wrapSecret(wrapKey, domainWrapIdentity, wrapKindPassword, salt, p.String(), holder, priv[:])
}

// UnwrapIdentity opens what WrapIdentity sealed, given the wrapKey already
// derived under the blob's own parameters.
//
// It re-validates those parameters. That is the second half of the floor rule
// and the half a port is likely to skip: a client that checks the bounds when
// it asks the server for parameters, and not when it opens a blob, still
// derives under whatever the blob says on a new-device bootstrap.
func UnwrapIdentity(wrapKey []byte, holder string, blob []byte) ([X25519KeySize]byte, error) {
	return unwrapIdentity(wrapKey, domainWrapIdentity, wrapKindPassword, holder, blob)
}

// KDFParamsFromBlob reads the parameters a wrapped identity was sealed under,
// so a client can derive the key that opens it. It does not validate the
// bounds — OpenIdentityWithPassword does, on the path that actually derives.
func KDFParamsFromBlob(blob []byte) (KDFParams, error) {
	f, err := parseWrap(blob)
	if err != nil {
		return KDFParams{}, err
	}
	if f.kind != wrapKindPassword {
		return KDFParams{}, fmt.Errorf("%w: this blob is opened by a recovery code, not a password", ErrWrap)
	}
	return ParseKDFParams(f.params)
}

// OpenIdentityWithPassword is the whole new-device bootstrap: read the
// parameters the blob carries, derive under them, open it.
//
// This exists so that the one flow where an attacker-chosen cost parameter
// meets a password has a single implementation with the floor already in it.
// A client assembling the three steps itself is a client that can assemble
// them in an order that skips Validate.
//
// It returns the credentials as well as the key, because the caller has just
// paid for the argon2id derivation and will want the authKey too.
func OpenIdentityWithPassword(password, holder string, blob []byte) (*Identity, Credentials, error) {
	p, err := KDFParamsFromBlob(blob)
	if err != nil {
		return nil, Credentials{}, err
	}
	// Validate runs inside DeriveCredentials, before argon2id is called: a
	// blob asking for a gigabyte must not get a gigabyte allocated to find
	// out it was refused.
	creds, err := DeriveCredentials(password, p)
	if err != nil {
		return nil, Credentials{}, err
	}
	priv, err := UnwrapIdentity(creds.WrapKey, holder, blob)
	if err != nil {
		return nil, Credentials{}, err
	}
	id, err := IdentityFromPrivate(priv)
	if err != nil {
		return nil, Credentials{}, err
	}
	return id, creds, nil
}

// WrapCK seals a library content key to one member's public identity key.
//
//	(esk, epk) = X25519 keygen                     -- fresh, per wrap
//	ss         = X25519(esk, recipient_pk)         -- refused if all-zero
//	K          = HKDF-SHA256(ss, salt="silo/wrap/ck/v1", info=SHA-256(AD), L=32)
//	blob       = AD ‖ AES-256-GCM(K, zero nonce, CK, AD)
//
// This is HPKE's shape — ephemeral DH, then a KDF over a context that includes
// both public keys — without claiming to be HPKE. RFC 9180 ships neither in
// Go's standard library nor in CryptoKit, so conformance would mean hand-porting
// its whole KEM/KDF/AEAD negotiation into Swift: a far larger correctness
// surface than these forty lines, for a wire nobody else will ever read. What
// the shape is borrowed for is the property: binding both public keys into the
// derivation is what stops a wrap being replayed at a different recipient.
//
// library is the library's id in canonical lower-case hyphenated UUID text and
// nothing else — the same rule a holder takes, see checkIdentifier — bound as
// associated data so a wrap cannot be replayed into another library.
//
// Sharing a library is one more call to this. A password change re-wraps only
// the member's own identity key and touches no CK at all.
func WrapCK(recipientPub [X25519KeySize]byte, library string, ck []byte) ([]byte, error) {
	pub, err := ecdh.X25519().NewPublicKey(recipientPub[:])
	if err != nil {
		return nil, fmt.Errorf("%w: not an X25519 public key: %v", ErrWrap, err)
	}
	eph, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("store: ephemeral key: %w", err)
	}
	return wrapCKWith(eph, pub, library, ck)
}

// wrapCKWith is WrapCK with the ephemeral key supplied, so the test vectors
// can pin bytes that are otherwise fresh on every call. Nothing outside the
// vector generator may choose this key: reusing an ephemeral across two wraps
// reuses a (key, nonce) pair, which is the one thing the zero nonce cannot
// survive.
//
// The checks live here rather than in WrapCK because this is what emits the
// bytes, and the vector generator calls it directly. A rule enforced one layer
// above the writer is a rule the normative vectors are minted without.
func wrapCKWith(eph *ecdh.PrivateKey, pub *ecdh.PublicKey, library string, ck []byte) ([]byte, error) {
	if len(ck) != CKSize {
		return nil, fmt.Errorf("%w: content key is %d bytes, want %d", ErrWrap, len(ck), CKSize)
	}
	if err := checkIdentifier(library, "library"); err != nil {
		return nil, err
	}
	ss, err := sharedSecret(eph, pub)
	if err != nil {
		return nil, err
	}

	b := []byte{WrapVersion}
	b = append(b, eph.PublicKey().Bytes()...)
	b = append(b, pub.Bytes()...)
	b = appendUvarint(b, uint64(len(library)))
	b = append(b, library...)
	ad := bytes.Clone(b)

	sealed, err := sealSection(ss, domainWrapCK, ad, ck)
	if err != nil {
		return nil, err
	}
	return append(b, sealed...), nil
}

// UnwrapCK opens a content key wrapped to this identity.
func UnwrapCK(id *Identity, library string, blob []byte) ([]byte, error) {
	const head = 1 + 2*X25519KeySize
	if len(blob) < head {
		return nil, fmt.Errorf("%w: %d bytes is shorter than a wrapped content key", ErrWrap, len(blob))
	}
	if blob[0] != WrapVersion {
		return nil, fmt.Errorf("%w: version %d, this format writes %d", ErrWrap, blob[0], WrapVersion)
	}
	var ephBytes, recipient [X25519KeySize]byte
	copy(ephBytes[:], blob[1:])
	copy(recipient[:], blob[1+X25519KeySize:])

	// Checked explicitly rather than left to the tag. The AD carries the
	// recipient key, so a blob meant for somebody else fails to open anyway —
	// but "this key is not yours" and "this blob was tampered with" are
	// different things to tell a user, and only one of them is worth a
	// support conversation.
	if mine := id.Public(); subtle.ConstantTimeCompare(recipient[:], mine[:]) != 1 {
		return nil, fmt.Errorf("%w: wrapped to a different identity", ErrWrap)
	}

	p := head
	got, err := readBounded(blob, &p, UUIDTextBytes, ErrWrap, "blob", "library")
	if err != nil {
		return nil, err
	}
	if err := checkIdentifier(got, "library"); err != nil {
		return nil, err
	}
	if got != library {
		return nil, fmt.Errorf("%w: wrapped for library %q, opened as %q", ErrWrap, got, library)
	}
	ad := blob[:p]

	eph, err := ecdh.X25519().NewPublicKey(ephBytes[:])
	if err != nil {
		return nil, fmt.Errorf("%w: ephemeral key: %v", ErrWrap, err)
	}
	ss, err := sharedSecret(id.priv, eph)
	if err != nil {
		return nil, err
	}
	ck, err := openSection(ss, domainWrapCK, ad, blob[p:])
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrWrap, err)
	}
	if len(ck) != CKSize {
		return nil, fmt.Errorf("%w: unwrapped %d bytes, want a %d-byte content key", ErrWrap, len(ck), CKSize)
	}
	return ck, nil
}

// sharedSecret performs the X25519 agreement and refuses an all-zero result.
//
// Go's crypto/ecdh already rejects the low-order points that produce it, so
// this check never fires here. It exists because the spec requires it and the
// Swift port has to implement it: on a platform where the primitive hands back
// a zero shared secret instead of an error, every wrap made against a
// low-order "public key" would derive the same key, and the attacker who
// published that key can read them all.
func sharedSecret(priv *ecdh.PrivateKey, pub *ecdh.PublicKey) ([]byte, error) {
	ss, err := priv.ECDH(pub)
	if err != nil {
		return nil, fmt.Errorf("%w: X25519 agreement: %v", ErrWrap, err)
	}
	var zero [X25519KeySize]byte
	if subtle.ConstantTimeCompare(ss, zero[:]) == 1 {
		return nil, fmt.Errorf("%w: all-zero shared secret", ErrWrap)
	}
	return ss, nil
}

// wrapFields is a parsed password or recovery wrap, up to the ciphertext.
type wrapFields struct {
	kind   byte
	params string
	holder string
	ad     []byte
	sealed []byte
}

// wrapSecret is the common body of the two identity wraps.
//
//	AD   = version ‖ kind ‖ salt ‖ params ‖ holder
//	K    = HKDF-SHA256(secret, salt=<domain>, info=SHA-256(AD), L=32)
//	blob = AD ‖ AES-256-GCM(K, zero nonce, private key, AD)
//
// The salt is the reason the zero nonce is safe here, and it is worth being
// precise about how this differs from the sealed containers. There, the public
// section carries seal_hash and the key therefore commits to the plaintext, so
// no two plaintexts can meet one key. Here there is no seal_hash — publishing
// SHA-256 of a private key would hand out a confirmation oracle for it — and
// freshness comes from the salt instead: a fresh sixteen bytes per wrap means
// a fresh key per wrap, so re-wrapping a different identity key under an
// unchanged password never reuses a (key, nonce) pair.
//
// Without it, that case is a live nonce-reuse bug: same password, same KDF
// salt, same wrapKey, two different private keys, one nonce. Two ciphertexts
// XOR to the two plaintexts and both keys fall out.
func wrapSecret(secret []byte, domain string, kind byte, salt [WrapSaltSize]byte, params, holder string, plaintext []byte) ([]byte, error) {
	if err := checkIdentifier(holder, "holder"); err != nil {
		return nil, err
	}
	if len(plaintext) != X25519KeySize {
		return nil, fmt.Errorf("%w: wrapping %d bytes, want a %d-byte private key",
			ErrWrap, len(plaintext), X25519KeySize)
	}
	if len(params) > MaxKDFParamsBytes {
		return nil, fmt.Errorf("%w: parameters are %d bytes, above %d",
			ErrWrap, len(params), MaxKDFParamsBytes)
	}

	b := []byte{WrapVersion, kind}
	b = append(b, salt[:]...)
	b = appendUvarint(b, uint64(len(params)))
	b = append(b, params...)
	b = appendUvarint(b, uint64(len(holder)))
	b = append(b, holder...)
	ad := bytes.Clone(b)

	sealed, err := sealSection(secret, domain, ad, plaintext)
	if err != nil {
		return nil, err
	}
	return append(b, sealed...), nil
}

// parseWrap splits a password or recovery blob into its fields without
// opening it, so a caller can read the parameters it needs to derive the key.
func parseWrap(blob []byte) (wrapFields, error) {
	const head = 2 + WrapSaltSize
	if len(blob) < head {
		return wrapFields{}, fmt.Errorf("%w: %d bytes is shorter than a wrap header", ErrWrap, len(blob))
	}
	if blob[0] != WrapVersion {
		return wrapFields{}, fmt.Errorf("%w: version %d, this format writes %d", ErrWrap, blob[0], WrapVersion)
	}
	f := wrapFields{kind: blob[1]}
	if f.kind != wrapKindPassword && f.kind != wrapKindRecovery {
		return wrapFields{}, fmt.Errorf("%w: unknown wrap kind %d", ErrWrap, f.kind)
	}

	p := head
	var err error
	if f.params, err = readBounded(blob, &p, MaxKDFParamsBytes, ErrWrap, "blob", "parameters"); err != nil {
		return wrapFields{}, err
	}
	if f.holder, err = readBounded(blob, &p, UUIDTextBytes, ErrWrap, "blob", "holder"); err != nil {
		return wrapFields{}, err
	}
	// The spelling rule is applied where a blob is read as well as where one is
	// written — checkIdentifier is the same check WrapIdentity and
	// WrapForRecovery run before they seal. A reader that takes the holder as
	// it finds it will happily compare one spelling of an account id against
	// another and report only that the blob belongs to somebody else.
	if err := checkIdentifier(f.holder, "holder"); err != nil {
		return wrapFields{}, err
	}
	if len(blob)-p < X25519KeySize+TagSize {
		return wrapFields{}, fmt.Errorf("%w: blob ends inside its sealed key", ErrWrap)
	}
	f.ad, f.sealed = blob[:p], blob[p:]
	return f, nil
}

func unwrapIdentity(secret []byte, domain string, kind byte, holder string, blob []byte) ([X25519KeySize]byte, error) {
	var out [X25519KeySize]byte
	if len(secret) == 0 {
		return out, fmt.Errorf("%w: unwrapping needs a key", ErrWrap)
	}
	f, err := parseWrap(blob)
	if err != nil {
		return out, err
	}
	if f.kind != kind {
		return out, fmt.Errorf("%w: this blob is kind %d, opened as kind %d", ErrWrap, f.kind, kind)
	}
	if f.holder != holder {
		return out, fmt.Errorf("%w: wrapped for holder %q, opened as %q", ErrWrap, f.holder, holder)
	}
	if kind == wrapKindPassword {
		p, err := ParseKDFParams(f.params)
		if err != nil {
			return out, err
		}
		if err := p.Validate(); err != nil {
			return out, err
		}
	} else if f.params != "" {
		return out, fmt.Errorf("%w: a recovery wrap carries no KDF parameters", ErrWrap)
	}

	priv, err := openSection(secret, domain, f.ad, f.sealed)
	if err != nil {
		return out, fmt.Errorf("%w: %w", ErrWrap, err)
	}
	if len(priv) != X25519KeySize {
		return out, fmt.Errorf("%w: unwrapped %d bytes, want %d", ErrWrap, len(priv), X25519KeySize)
	}
	copy(out[:], priv)
	return out, nil
}

// nilUUID names nothing. As an account id it is account.Zero on the server, the
// id a handler holds when it has lost the user somewhere upstream.
const nilUUID = "00000000-0000-0000-0000-000000000000"

// checkIdentifier applies this format's spelling rule to one of the two
// identifiers a wrap binds itself to: an account id, or a library id.
//
// Canonical lower-case hyphenated UUID text — 8-4-4-4-12 lower-case hex digits
// — and nothing else. Not the thirty-two character unhyphenated form, not
// braces, not a "urn:uuid:" prefix, not upper case: a permissive UUID parser
// accepts every one of those and they are all different byte strings. These
// identifiers are associated data, so two clients that disagree about the
// spelling seal blobs neither can open, and the error at that point says only
// that the key was wrong.
//
// One rule for both, because the spec states one rule for both. They are
// refused rather than truncated: a truncated id still binds, just to the wrong
// account or the wrong library.
//
// It is written out rather than handed to a UUID library on purpose. This is a
// spelling rule, not a parse — a library that reads the id back correctly has
// still not told you whether the text was canonical — and what a port has to
// implement is exactly the loop below, on a platform whose own UUID type is
// permissive in its own way.
//
// The version and variant nibbles are deliberately not checked. Silo mints
// UUIDv7 for accounts and takes a library id from its creator, but which UUID
// either is drawn from is the server's business and not something a blob should
// refuse to open over.
//
// The nil UUID is refused for the reason the empty string is: it is well formed
// and names nobody, so a wrap bound to it binds to nothing while looking like
// it binds to something.
func checkIdentifier(s, what string) error {
	if s == "" {
		return fmt.Errorf("%w: %s is empty, and a wrap that binds to nothing binds nothing", ErrWrap, what)
	}
	if len(s) != UUIDTextBytes {
		return fmt.Errorf("%w: %s %q is %d bytes, and a UUID is %d",
			ErrWrap, what, s, len(s), UUIDTextBytes)
	}
	for i := range len(s) {
		switch c := s[i]; i {
		case 8, 13, 18, 23:
			if c != '-' {
				return fmt.Errorf("%w: %s %q wants a hyphen at %d", ErrWrap, what, s, i)
			}
		default:
			if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
				return fmt.Errorf("%w: %s %q is not lower-case hexadecimal at %d", ErrWrap, what, s, i)
			}
		}
	}
	if s == nilUUID {
		return fmt.Errorf("%w: %s is the nil UUID, which names nothing", ErrWrap, what)
	}
	return nil
}
