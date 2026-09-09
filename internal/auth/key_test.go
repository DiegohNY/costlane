package auth

import (
	"strings"
	"testing"

	"github.com/DiegohNY/costlane/internal/obs"
)

// Generating many keys is the practical way to check the generator: a test
// cannot inspect which random source was used, but it can insist on the
// properties that source is meant to give.
func TestGeneratedKeysAreUniqueAndWellFormed(t *testing.T) {
	const n = 10_000
	seen := make(map[string]bool, n)

	for i := range n {
		key, err := NewKey()
		if err != nil {
			t.Fatalf("generating key %d: %v", i, err)
		}
		secret := key.Secret.Expose()

		if !strings.HasPrefix(secret, Prefix) {
			t.Fatalf("key %d has no %q prefix: %q", i, Prefix, key.Prefix)
		}
		if len(secret) != KeyLength {
			t.Fatalf("key %d is %d characters, want %d", i, len(secret), KeyLength)
		}
		if seen[secret] {
			t.Fatalf("key %d is a duplicate after %d draws", i, len(seen))
		}
		seen[secret] = true

		// The display prefix must identify a key without being enough to
		// reconstruct it.
		if !strings.HasPrefix(secret, key.Prefix) {
			t.Fatalf("display prefix %q does not match the key", key.Prefix)
		}
		if len(key.Prefix) >= len(secret) {
			t.Fatalf("display prefix is the whole key")
		}
	}

	if len(seen) != n {
		t.Errorf("generated %d distinct keys out of %d", len(seen), n)
	}
}

// Every character must come from the declared alphabet, or a key could
// break URL encoding or shell quoting for whoever pastes it.
func TestGeneratedKeysUseTheDeclaredAlphabet(t *testing.T) {
	for range 1000 {
		key, err := NewKey()
		if err != nil {
			t.Fatalf("generating: %v", err)
		}
		body := strings.TrimPrefix(key.Secret.Expose(), Prefix)
		for _, r := range body {
			if !strings.ContainsRune(alphabet, r) {
				t.Fatalf("key contains %q, which is outside the alphabet", r)
			}
		}
	}
}

// The hash is what the database stores and what a lookup matches on.
func TestHashIsStableAndDistinct(t *testing.T) {
	a := obs.Secret("cl_" + strings.Repeat("a", 32))
	b := obs.Secret("cl_" + strings.Repeat("b", 32))

	if want := Hash(a); string(Hash(a)) != string(want) {
		t.Error("hashing must be deterministic")
	}
	if string(Hash(a)) == string(Hash(b)) {
		t.Error("different keys must hash differently")
	}
	if n := len(Hash(a)); n != 32 {
		t.Errorf("hash is %d bytes, want 32 (SHA-256)", n)
	}
}

// A generated key never appears in a log or an error: it is a Secret from
// the moment it exists.
func TestGeneratedKeyIsRedactedByDefault(t *testing.T) {
	key, err := NewKey()
	if err != nil {
		t.Fatalf("generating: %v", err)
	}
	secret := key.Secret.Expose()

	for _, rendering := range []string{
		key.Secret.String(),
		strings.TrimSpace(strings.Join([]string{key.Prefix}, "")),
	} {
		if strings.Contains(rendering, secret) {
			t.Errorf("a rendering leaked the key: %s", rendering)
		}
	}
}
