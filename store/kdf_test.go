package store

import (
	"bytes"
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"golang.org/x/crypto/argon2"
)

// The floor parameters, used everywhere a test needs a real derivation but not
// a slow one. They are legal by construction — that is what a floor means.
func cheapParams() KDFParams {
	var salt [KDFSaltSize]byte
	copy(salt[:], "sixteen byte salt")
	return KDFParams{Memory: MinKDFMemory, Time: MinKDFTime, Lanes: MinKDFLanes, Salt: salt}
}

func TestTheTwoHalvesOfAPasswordAreDifferentKeys(t *testing.T) {
	creds, err := DeriveCredentials("correct horse battery staple", cheapParams())
	if err != nil {
		t.Fatal(err)
	}
	if len(creds.AuthKey) != 32 || len(creds.WrapKey) != 32 {
		t.Fatalf("got %d and %d byte keys", len(creds.AuthKey), len(creds.WrapKey))
	}
	if bytes.Equal(creds.AuthKey, creds.WrapKey) {
		t.Fatal("the key sent to the server is the key that opens the identity")
	}
}

func TestDerivationIsDeterministicAndSaltedPerUser(t *testing.T) {
	p := cheapParams()
	first, err := DeriveCredentials("hunter2", p)
	if err != nil {
		t.Fatal(err)
	}
	again, err := DeriveCredentials("hunter2", p)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first.WrapKey, again.WrapKey) {
		t.Fatal("the same password under the same parameters gave two wrap keys")
	}

	other := p
	other.Salt[0] ^= 1
	salted, err := DeriveCredentials("hunter2", other)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(first.WrapKey, salted.WrapKey) {
		t.Fatal("two users sharing a password shared a wrap key")
	}

	wrong, err := DeriveCredentials("hunter3", p)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(first.AuthKey, wrong.AuthKey) {
		t.Fatal("two passwords gave one auth key")
	}
}

// The floor is the guard the whole params-as-data decision rests on: the
// parameters ride inside a blob the server hands over, so whoever writes the
// blob picks the cost at which the key that opens it was derived.
func TestDerivationRefusesParametersBelowTheFloor(t *testing.T) {
	for _, tc := range []struct {
		what string
		p    KDFParams
	}{
		{"memory", KDFParams{Memory: MinKDFMemory - 1, Time: DefaultKDFTime, Lanes: DefaultKDFLanes}},
		{"memory at argon2's own minimum", KDFParams{Memory: 8, Time: DefaultKDFTime, Lanes: DefaultKDFLanes}},
		{"time", KDFParams{Memory: DefaultKDFMemory, Time: MinKDFTime - 1, Lanes: DefaultKDFLanes}},
		{"lanes", KDFParams{Memory: DefaultKDFMemory, Time: DefaultKDFTime, Lanes: 0}},
	} {
		if _, err := DeriveCredentials("hunter2", tc.p); !errors.Is(err, ErrKDFParams) {
			t.Errorf("%s below the floor derived anyway: %v", tc.what, err)
		}
	}
}

// The same attack from the other end. A server answering a pre-login
// parameter request with a gigabyte does not weaken anything — it takes the
// device down.
func TestDerivationRefusesParametersAboveTheCeiling(t *testing.T) {
	for _, tc := range []struct {
		what string
		p    KDFParams
	}{
		{"memory", KDFParams{Memory: MaxKDFMemory + 1, Time: DefaultKDFTime, Lanes: DefaultKDFLanes}},
		{"time", KDFParams{Memory: DefaultKDFMemory, Time: MaxKDFTime + 1, Lanes: DefaultKDFLanes}},
		{"lanes", KDFParams{Memory: DefaultKDFMemory, Time: DefaultKDFTime, Lanes: MaxKDFLanes + 1}},
	} {
		if _, err := DeriveCredentials("hunter2", tc.p); !errors.Is(err, ErrKDFParams) {
			t.Errorf("%s above the ceiling derived anyway: %v", tc.what, err)
		}
	}
}

func TestDefaultParametersAreInsideTheBounds(t *testing.T) {
	var salt [KDFSaltSize]byte
	if err := DefaultKDFParams(salt).Validate(); err != nil {
		t.Fatalf("this format's own defaults are refused by its own floor: %v", err)
	}
}

func TestParametersRoundTripThroughTheirString(t *testing.T) {
	var salt [KDFSaltSize]byte
	for i := range salt {
		salt[i] = byte(i * 7)
	}
	p := DefaultKDFParams(salt)
	s := p.String()
	if want := "$argon2id$v=19$m=65536,t=3,p=4$"; !strings.HasPrefix(s, want) {
		t.Fatalf("rendered %q, want the %q prefix", s, want)
	}
	back, err := ParseKDFParams(s)
	if err != nil {
		t.Fatal(err)
	}
	if back != p {
		t.Fatalf("round trip gave %+v, want %+v", back, p)
	}
	if back.String() != s {
		t.Fatalf("re-rendered %q, want %q", back.String(), s)
	}
}

// Every one of these decides a key. A parser that shrugs at a field it does
// not recognise is a parser that derives the wrong key and reports success.
func TestTheParameterParserRefusesEverythingElse(t *testing.T) {
	valid := DefaultKDFParams([KDFSaltSize]byte{}).String()
	for _, tc := range []struct {
		what string
		in   string
	}{
		{"empty", ""},
		{"no leading dollar", strings.TrimPrefix(valid, "$")},
		{"unknown algorithm", strings.Replace(valid, "argon2id", "argon2i", 1)},
		{"unknown version", strings.Replace(valid, "v=19", "v=16", 1)},
		{"missing version", strings.Replace(valid, "$v=19", "", 1)},
		{"reordered costs", strings.Replace(valid, "m=65536,t=3,p=4", "t=3,m=65536,p=4", 1)},
		{"missing cost", strings.Replace(valid, ",p=4", "", 1)},
		{"extra cost", strings.Replace(valid, "p=4", "p=4,k=1", 1)},
		{"leading zero", strings.Replace(valid, "t=3", "t=03", 1)},
		{"signed", strings.Replace(valid, "t=3", "t=+3", 1)},
		{"whitespace", strings.Replace(valid, "m=65536,", "m=65536, ", 1)},
		{"hex salt", strings.Replace(valid, "$"+valid[strings.LastIndexByte(valid, '$')+1:], "$0011223344556677", 1)},
		{"padded salt", valid + "=="},
		{"trailing field", valid + "$deadbeef"},
	} {
		if _, err := ParseKDFParams(tc.in); !errors.Is(err, ErrKDFParams) {
			t.Errorf("%s (%q) parsed: %v", tc.what, tc.in, err)
		}
	}
}

// Reading a blob to find out what is wrong with it has to be possible;
// deriving under what it says does not.
func TestTheParserReadsOutOfBoundsParametersWithoutBlessingThem(t *testing.T) {
	weak := "$argon2id$v=19$m=8,t=1,p=1$AAAAAAAAAAAAAAAAAAAAAA"
	p, err := ParseKDFParams(weak)
	if err != nil {
		t.Fatalf("the parser refused to read weak parameters: %v", err)
	}
	if p.Memory != 8 {
		t.Fatalf("read m=%d, want 8", p.Memory)
	}
	if err := p.Validate(); !errors.Is(err, ErrKDFParams) {
		t.Fatal("weak parameters passed Validate")
	}
	if _, err := DeriveCredentials("hunter2", p); !errors.Is(err, ErrKDFParams) {
		t.Fatal("weak parameters derived a key")
	}
}

// The argument order, checked against somebody else's numbers.
//
// argon2.IDKey takes time before memory, and swapping them is the classic
// argon2 bug: t=65536, m=3 is a derivation that runs, returns 32 bytes, and
// agrees with nothing. Every vector in keys.json would still be internally
// consistent — this is the check that is not.
//
// Generated by the reference implementation's CLI (the argon2 package, from
// the Argon2 authors), with a printable salt so the command is reproducible:
//
//	echo -n "correct horse battery staple" | \
//	  argon2 0123456789abcdef -id -t 3 -k 65536 -p 4 -l 32 -r
func TestArgon2idMatchesTheReferenceImplementation(t *testing.T) {
	var salt [KDFSaltSize]byte
	copy(salt[:], "0123456789abcdef")

	for _, tc := range []struct {
		name   string
		p      KDFParams
		master string
	}{
		{"default parameters",
			KDFParams{Memory: DefaultKDFMemory, Time: DefaultKDFTime, Lanes: DefaultKDFLanes, Salt: salt},
			"efb51f9a76584f6dd6a4f7942a1a2f6ae5a6e4ec5142ff674dfd5d27eb45e446"},
		{"floor parameters",
			KDFParams{Memory: MinKDFMemory, Time: MinKDFTime, Lanes: MinKDFLanes, Salt: salt},
			"832e52b959b967b570ee4781f6c7bda7ced019ca266ac781fd2d94d4e853b0cd"},
	} {
		got := argon2.IDKey([]byte("correct horse battery staple"), tc.p.Salt[:],
			tc.p.Time, tc.p.Memory, tc.p.Lanes, 32)
		if hex.EncodeToString(got) != tc.master {
			t.Errorf("%s: master %s, the reference implementation says %s",
				tc.name, hex.EncodeToString(got), tc.master)
		}
	}
}
