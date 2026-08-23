package store

import (
	"bytes"
	"crypto/rand"
	"errors"
	"strings"
	"sync"
	"testing"
)

const (
	// The two account ids every wrap test binds to, in the one spelling this
	// format accepts: canonical lower-case hyphenated UUID text. They are
	// UUIDv7 because that is what account.NewID mints, though checkHolder does
	// not look at the version.
	testHolder      = "0192f0a1-3c5d-7e4b-8f26-9a7d5c3e1b04"
	testOtherHolder = "0193c48b-2e19-7a6f-b3d0-4e8c1f7a2960"

	// The two libraries every wrap test binds to. They are UUIDs because the
	// spec says a library id is one; the vectors bind these exact strings, so a
	// second spelling of either is a blob that opens against nothing.
	testLibrary      = "3f2a1c58-9b0d-4e77-8a61-5c2d0e4f9ab3"
	testOtherLibrary = "00000000-0000-4000-8000-000000000000"
	testPassword     = "correct horse battery staple"
)

func testIdentity(t *testing.T) *Identity {
	t.Helper()
	id, err := GenerateIdentity()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// testWrapKey is a wrapKey from a real derivation, computed once for the whole
// package. Every call site wants the same thing — thirty-two bytes that came
// out of the real flow — under the same password and the same floor
// parameters, and argon2id at the floor costs 15 ms a call.
var testWrapKeyOnce = sync.OnceValue(func() []byte {
	creds, err := DeriveCredentials(testPassword, cheapParams())
	if err != nil {
		panic(err) // floor parameters are legal by construction
	}
	return creds.WrapKey
})

func testWrapKey(t *testing.T) []byte {
	t.Helper()
	return testWrapKeyOnce()
}

func TestAnIdentityRoundTripsThroughItsWrap(t *testing.T) {
	id := testIdentity(t)
	wrapKey := testWrapKey(t)

	blob, err := WrapIdentity(wrapKey, testHolder, cheapParams(), id.Private())
	if err != nil {
		t.Fatal(err)
	}
	got, err := UnwrapIdentity(wrapKey, testHolder, blob)
	if err != nil {
		t.Fatal(err)
	}
	if got != id.Private() {
		t.Fatal("the key that came back is not the key that went in")
	}

	// The private key must not be findable in the blob it is sealed in.
	priv := id.Private()
	if bytes.Contains(blob, priv[:]) {
		t.Fatal("the private key appears in the clear in its own wrap")
	}
}

func TestAWrapDoesNotOpenUnderTheWrongPassword(t *testing.T) {
	id := testIdentity(t)
	blob, err := WrapIdentity(testWrapKey(t), testHolder, cheapParams(), id.Private())
	if err != nil {
		t.Fatal(err)
	}
	other, err := DeriveCredentials("hunter2", cheapParams())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := UnwrapIdentity(other.WrapKey, testHolder, blob); !errors.Is(err, ErrWrap) {
		t.Fatalf("opened under the wrong password: %v", err)
	}
}

// The attack, performed: a server holding two accounts' blobs hands A the one
// belonging to B. Without the holder in the associated data this is a way to
// learn whether two accounts share a password.
func TestABlobCannotBeMovedBetweenAccounts(t *testing.T) {
	shared := testWrapKey(t)
	mine, theirs := testIdentity(t), testIdentity(t)

	myBlob, err := WrapIdentity(shared, testHolder, cheapParams(), mine.Private())
	if err != nil {
		t.Fatal(err)
	}
	theirBlob, err := WrapIdentity(shared, testOtherHolder, cheapParams(), theirs.Private())
	if err != nil {
		t.Fatal(err)
	}

	// Same wrap key — the two accounts really do share a password — and the
	// blob still refuses to open as the wrong account.
	if _, err := UnwrapIdentity(shared, testHolder, theirBlob); !errors.Is(err, ErrWrap) {
		t.Fatalf("the second account's blob opened as the first: %v", err)
	}
	if _, err := UnwrapIdentity(shared, testOtherHolder, myBlob); !errors.Is(err, ErrWrap) {
		t.Fatalf("the first account's blob opened as the second: %v", err)
	}
}

func TestTamperingWithAnyByteOfAWrapIsRefused(t *testing.T) {
	id := testIdentity(t)
	wrapKey := testWrapKey(t)
	blob, err := WrapIdentity(wrapKey, testHolder, cheapParams(), id.Private())
	if err != nil {
		t.Fatal(err)
	}
	for i := range blob {
		bad := bytes.Clone(blob)
		bad[i] ^= 0x40
		if _, err := UnwrapIdentity(wrapKey, testHolder, bad); err == nil {
			t.Fatalf("byte %d could be flipped and the blob still opened", i)
		}
	}
}

// The floor's second half, and the half a port is likely to skip: enforced
// when a blob is opened, not only when parameters are asked for. The blob is
// built by hand because WrapIdentity refuses to write one.
func TestABlobCarryingWeakParametersIsRefusedAtOpen(t *testing.T) {
	weak := "$argon2id$v=19$m=8,t=1,p=1$AAAAAAAAAAAAAAAAAAAAAA"
	p, err := ParseKDFParams(weak)
	if err != nil {
		t.Fatal(err)
	}
	id := testIdentity(t)
	var salt [WrapSaltSize]byte
	blob, err := wrapSecret([]byte("whatever key that cost bought"), domainWrapIdentity,
		wrapKindPassword, salt, weak, testHolder, privBytes(id))
	if err != nil {
		t.Fatal(err)
	}

	// Reading the parameters is allowed; deriving under them is not.
	if got, err := KDFParamsFromBlob(blob); err != nil || got.Memory != p.Memory {
		t.Fatalf("could not read the weak parameters back: %+v %v", got, err)
	}
	if _, err := UnwrapIdentity([]byte("whatever key that cost bought"), testHolder, blob); !errors.Is(err, ErrKDFParams) {
		t.Fatalf("a blob with m=8 opened: %v", err)
	}
	if _, _, err := OpenIdentityWithPassword("hunter2", testHolder, blob); !errors.Is(err, ErrKDFParams) {
		t.Fatalf("the bootstrap path derived under m=8: %v", err)
	}
}

func TestTheBootstrapPathIsTheWholeFlow(t *testing.T) {
	id := testIdentity(t)
	p := cheapParams()
	creds, err := DeriveCredentials(testPassword, p)
	if err != nil {
		t.Fatal(err)
	}
	blob, err := WrapIdentity(creds.WrapKey, testHolder, p, id.Private())
	if err != nil {
		t.Fatal(err)
	}

	// A new device holds the password and whatever the server hands over.
	got, back, err := OpenIdentityWithPassword(testPassword, testHolder, blob)
	if err != nil {
		t.Fatal(err)
	}
	if got.Private() != id.Private() {
		t.Fatal("bootstrapped a different key")
	}
	if !bytes.Equal(back.AuthKey, creds.AuthKey) {
		t.Fatal("the auth key from the bootstrap differs from the one at enrolment")
	}
	if _, _, err := OpenIdentityWithPassword("hunter2", testHolder, blob); !errors.Is(err, ErrWrap) {
		t.Fatal("the wrong password bootstrapped an identity")
	}
}

func TestAContentKeyRoundTripsToItsMember(t *testing.T) {
	member := testIdentity(t)
	ck := make([]byte, CKSize)
	if _, err := rand.Read(ck); err != nil {
		t.Fatal(err)
	}

	blob, err := WrapCK(member.Public(), testLibrary, ck)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(blob, ck) {
		t.Fatal("the content key appears in the clear in its own wrap")
	}
	got, err := UnwrapCK(member, testLibrary, blob)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, ck) {
		t.Fatal("the content key that came back is not the one that went in")
	}
}

// Sharing is one more wrap. Each member gets their own blob and no member's
// blob opens for anyone else.
func TestAContentKeyWrapIsNotPortableBetweenMembers(t *testing.T) {
	alice, bob := testIdentity(t), testIdentity(t)
	ck := bytes.Repeat([]byte{0xa5}, CKSize)

	forAlice, err := WrapCK(alice.Public(), testLibrary, ck)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := UnwrapCK(bob, testLibrary, forAlice); !errors.Is(err, ErrWrap) {
		t.Fatalf("Bob opened Alice's wrap: %v", err)
	}

	// And with Bob's public key substituted into the blob, so the explicit
	// recipient check is not the only thing standing in the way.
	forged := bytes.Clone(forAlice)
	bobPub := bob.Public()
	copy(forged[1+X25519KeySize:], bobPub[:])
	if _, err := UnwrapCK(bob, testLibrary, forged); !errors.Is(err, ErrWrap) {
		t.Fatalf("rewriting the recipient field opened the wrap: %v", err)
	}
}

// The wrap binds the library, so a server cannot take the blob that gave a
// member access to one library and present it as their key for another.
func TestAContentKeyWrapIsNotPortableBetweenLibraries(t *testing.T) {
	member := testIdentity(t)
	ck := bytes.Repeat([]byte{0x5a}, CKSize)
	blob, err := WrapCK(member.Public(), testLibrary, ck)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := UnwrapCK(member, testOtherLibrary, blob); !errors.Is(err, ErrWrap) {
		t.Fatalf("the wrap opened for a library it was not made for: %v", err)
	}
}

// Every wrap mints a new ephemeral key, which is what licenses the zero nonce
// on this side of the format. Two wraps of one content key to one member must
// share no bytes past the version.
func TestEveryContentKeyWrapUsesAFreshEphemeral(t *testing.T) {
	member := testIdentity(t)
	ck := bytes.Repeat([]byte{7}, CKSize)

	first, err := WrapCK(member.Public(), testLibrary, ck)
	if err != nil {
		t.Fatal(err)
	}
	second, err := WrapCK(member.Public(), testLibrary, ck)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(first, second) {
		t.Fatal("two wraps of the same key produced the same blob: the ephemeral is not fresh")
	}
	if bytes.Equal(first[1:1+X25519KeySize], second[1:1+X25519KeySize]) {
		t.Fatal("the ephemeral public key repeated")
	}
	// Both still open to the same content key.
	for _, blob := range [][]byte{first, second} {
		got, err := UnwrapCK(member, testLibrary, blob)
		if err != nil || !bytes.Equal(got, ck) {
			t.Fatalf("a wrap did not open: %v", err)
		}
	}
}

// Go refuses the low-order points that make X25519 agree on zero, so this
// never gets as far as the explicit check. The test pins the behaviour anyway,
// because the spec requires the check and the Swift port has to implement it
// on a platform where it may not be free.
func TestALowOrderPublicKeyIsRefused(t *testing.T) {
	for _, name := range []string{"all zero", "order one", "order eight"} {
		var pub [X25519KeySize]byte
		switch name {
		case "order one":
			pub[0] = 1
		case "order eight":
			copy(pub[:], []byte{
				0xe0, 0xeb, 0x7a, 0x7c, 0x3b, 0x41, 0xb8, 0xae, 0x16, 0x56, 0xe3, 0xfa, 0xf1, 0x9f, 0xc4, 0x6a,
				0xda, 0x09, 0x8d, 0xeb, 0x9c, 0x32, 0xb1, 0xfd, 0x86, 0x62, 0x05, 0x16, 0x5f, 0x49, 0xb8, 0x00,
			})
		}
		if _, err := WrapCK(pub, testLibrary, bytes.Repeat([]byte{1}, CKSize)); !errors.Is(err, ErrWrap) {
			t.Errorf("%s public key was wrapped to: %v", name, err)
		}
	}
}

func TestAWrapRefusesAnIdentifierItCannotBind(t *testing.T) {
	id := testIdentity(t)
	wrapKey := testWrapKey(t)

	for _, tc := range []struct{ what, holder string }{
		{"an empty holder", ""},
		{"an over-long holder", string(bytes.Repeat([]byte{'a'}, HolderBytes+1))},
		{"an upper-case UUID", strings.ToUpper(testHolder)},
		{"the unhyphenated form", strings.ReplaceAll(testHolder, "-", "")},
		{"a braced UUID", "{" + testHolder + "}"},
		{"a urn-prefixed UUID", "urn:uuid:" + testHolder},
		{"a holder with a trailing space", testHolder + " "},
		{"a hyphen in the wrong place", "0192f0a13-c5d-7e4b-8f26-9a7d5c3e1b04"},
		{"a non-hex digit", strings.Replace(testHolder, "f", "g", 1)},
		{"the nil UUID", nilUUID},
		{"an account-scheme id", "account:7"},
		{"an email address", "person@example.com"},
	} {
		if _, err := WrapIdentity(wrapKey, tc.holder, cheapParams(), id.Private()); !errors.Is(err, ErrWrap) {
			t.Errorf("wrapped to %s (%q): %v", tc.what, tc.holder, err)
		}
	}

	if _, err := WrapCK(id.Public(), "", bytes.Repeat([]byte{1}, CKSize)); !errors.Is(err, ErrWrap) {
		t.Error("wrapped a content key to no library")
	}
	if _, err := WrapCK(id.Public(), testLibrary, []byte("short")); !errors.Is(err, ErrWrap) {
		t.Error("wrapped a content key of the wrong width")
	}
}

// The spelling rule has to hold on the way in as well as on the way out, and
// this is the case that proves it: a holder is fixed-width, so a blob carrying
// a mis-spelled one is structurally identical to a good blob and a reader that
// only compares strings would report nothing worse than "belongs to somebody
// else". Splicing rather than wrapping is the only way to build one, because
// wrapSecret now refuses to.
func TestABlobCarryingAMisspelledHolderIsRefusedOnRead(t *testing.T) {
	id := testIdentity(t)
	wrapKey := testWrapKey(t)
	blob, err := WrapIdentity(wrapKey, testHolder, cheapParams(), id.Private())
	if err != nil {
		t.Fatal(err)
	}
	at := bytes.Index(blob, []byte(testHolder))
	if at < 0 {
		t.Fatal("the holder is not in the blob it is meant to bind")
	}

	for _, spelling := range []string{strings.ToUpper(testHolder), nilUUID} {
		spliced := bytes.Clone(blob)
		copy(spliced[at:], spelling)
		if _, err := UnwrapIdentity(wrapKey, spelling, spliced); !errors.Is(err, ErrWrap) {
			t.Errorf("a blob holding %q was read: %v", spelling, err)
		}
	}
}

func TestAWrappedBlobIsRefusedBeforeItIsParsed(t *testing.T) {
	id := testIdentity(t)
	wrapKey := testWrapKey(t)
	blob, err := WrapIdentity(wrapKey, testHolder, cheapParams(), id.Private())
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		what string
		in   []byte
	}{
		{"empty", nil},
		{"header only", blob[:2]},
		{"truncated", blob[:len(blob)-1]},
		{"wrong version", append([]byte{99}, blob[1:]...)},
		{"unknown kind", append([]byte{blob[0], 9}, blob[2:]...)},
		{"recovery kind", append([]byte{blob[0], wrapKindRecovery}, blob[2:]...)},
	} {
		if _, err := UnwrapIdentity(wrapKey, testHolder, tc.in); err == nil {
			t.Errorf("%s opened", tc.what)
		}
	}
}

func TestAnIdentityRebuildsFromItsPrivateBytes(t *testing.T) {
	id := testIdentity(t)
	back, err := IdentityFromPrivate(id.Private())
	if err != nil {
		t.Fatal(err)
	}
	if back.Public() != id.Public() {
		t.Fatal("the same private key gave two public keys")
	}
}

// Clamping means any thirty-two bytes are a usable private key, including all
// zeros — the degenerate case is a hostile public key, not a private one, and
// the test above covers it. What must hold is that a rebuilt key is the same
// key: a client reads this back out of a platform key store on every start.
func TestEvenAnAllZeroPrivateKeyIsAnOrdinaryIdentity(t *testing.T) {
	id, err := IdentityFromPrivate([X25519KeySize]byte{})
	if err != nil {
		t.Fatal(err)
	}
	if id.Public() == ([X25519KeySize]byte{}) {
		t.Fatal("its public key is the identity point, which sharedSecret would agree to zero on")
	}
	ck := bytes.Repeat([]byte{9}, CKSize)
	blob, err := WrapCK(id.Public(), testLibrary, ck)
	if err != nil {
		t.Fatal(err)
	}
	got, err := UnwrapCK(id, testLibrary, blob)
	if err != nil || !bytes.Equal(got, ck) {
		t.Fatalf("it could not open its own wrap: %v", err)
	}
}

func TestEphemeralKeysAreNotReusedAcrossRecipients(t *testing.T) {
	alice, bob := testIdentity(t), testIdentity(t)
	ck := bytes.Repeat([]byte{3}, CKSize)
	forAlice, err := WrapCK(alice.Public(), testLibrary, ck)
	if err != nil {
		t.Fatal(err)
	}
	forBob, err := WrapCK(bob.Public(), testLibrary, ck)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(forAlice[1:1+X25519KeySize], forBob[1:1+X25519KeySize]) {
		t.Fatal("one ephemeral key served two recipients")
	}
}

// privBytes is the slice form of an identity's private key, for the hand-built
// blobs the attack tests need.
func privBytes(id *Identity) []byte {
	k := id.Private()
	return k[:]
}
