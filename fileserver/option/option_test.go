package option

import (
	"encoding/hex"
	"testing"
)

// The signing key comes from SILO_JWT_SECRET and from nowhere else.
//
// There used to be a fallback to JWT_PRIVATE_KEY, the unprefixed name this
// setting carried before the rename. A fallback like that is indefinite by
// construction -- nothing expires it, and nothing tells an operator which of
// the two names is live -- so a deployment can sit for years signing with a
// variable the documentation no longer mentions. This asserts the old name is
// inert rather than merely undocumented.
func TestLoadJWTConfigIgnoresTheOldVariableName(t *testing.T) {
	const legacy = "a-key-set-under-the-old-name"
	t.Setenv("JWT_PRIVATE_KEY", legacy)
	t.Setenv("SILO_JWT_SECRET", "")

	if err := LoadJWTConfig(); err != nil {
		t.Fatalf("LoadJWTConfig: %v", err)
	}
	if JWTPrivateKey == legacy {
		t.Error("JWT_PRIVATE_KEY is still being honoured")
	}
}

// The name that is documented is the name that works.
func TestLoadJWTConfigReadsSiloJWTSecret(t *testing.T) {
	const chosen = "a-key-the-operator-chose"
	t.Setenv("SILO_JWT_SECRET", chosen)

	if err := LoadJWTConfig(); err != nil {
		t.Fatalf("LoadJWTConfig: %v", err)
	}
	if JWTPrivateKey != chosen {
		t.Errorf("JWTPrivateKey is %q, want the value of SILO_JWT_SECRET", JWTPrivateKey)
	}
}

// With nothing set the server mints its own, and a fresh one each time. Two
// calls returning the same string would mean the key was derived from
// something stable, which is the property this deliberately does not have --
// and the reason a restart invalidates the notification tokens in flight.
func TestLoadJWTConfigMintsAnEphemeralKeyWhenUnset(t *testing.T) {
	t.Setenv("SILO_JWT_SECRET", "")
	t.Setenv("JWT_PRIVATE_KEY", "")

	if err := LoadJWTConfig(); err != nil {
		t.Fatalf("first LoadJWTConfig: %v", err)
	}
	first := JWTPrivateKey

	if _, err := hex.DecodeString(first); err != nil {
		t.Errorf("the generated key is not hex: %q", first)
	}
	if len(first) != 64 {
		t.Errorf("the generated key is %d characters, want 64 (32 bytes hex)", len(first))
	}

	if err := LoadJWTConfig(); err != nil {
		t.Fatalf("second LoadJWTConfig: %v", err)
	}
	if JWTPrivateKey == first {
		t.Error("two calls generated the same key; it is meant to be ephemeral")
	}
}
