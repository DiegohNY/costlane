// Package auth hashes, verifies and scopes API credentials.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"strings"

	"github.com/DiegohNY/costlane/internal/obs"
)

const (
	// Prefix marks a costlane virtual key, so a leaked credential is
	// recognisable in a log someone else owns.
	Prefix = "cl_"

	// bodyLength is the number of random characters after the prefix. At
	// 62 symbols each, 43 characters carry about 256 bits.
	bodyLength = 43

	// KeyLength is the full length of a generated key.
	KeyLength = len(Prefix) + bodyLength

	// displayLength is how much of a key is kept for display. It has to
	// identify a key in a list without being enough to guess it.
	displayLength = len(Prefix) + 5

	alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
)

// Key is a freshly generated credential. The secret exists only here and in
// the single response that returns it; the database stores nothing but its
// hash.
type Key struct {
	Secret obs.Secret
	// Prefix identifies the key in listings and logs. It is a prefix of the
	// secret, short enough to be useless on its own.
	Prefix string
	// Hash is what the database stores and what a lookup matches on.
	Hash []byte
}

// NewKey generates a virtual key using crypto/rand.
//
// The alphabet is unbiased by rejection sampling rather than by taking a
// modulus: 62 does not divide 256, so a modulus would make the first
// characters of the alphabet slightly more likely than the rest, and a
// predictable bias in a credential is a smaller keyspace than it appears.
func NewKey() (Key, error) {
	body, err := randomString(bodyLength)
	if err != nil {
		return Key{}, fmt.Errorf("auth: generating a key: %w", err)
	}

	secret := Prefix + body
	return Key{
		Secret: obs.Secret(secret),
		Prefix: secret[:displayLength],
		Hash:   hashString(secret),
	}, nil
}

// Hash returns the stored form of a presented key.
//
// SHA-256 rather than a slow KDF: verification sits on the critical path of
// every proxied request, and the key is 256 bits of entropy from
// crypto/rand. A deliberately slow hash defends against guessing a
// human-chosen password, which this is not, and would tax a gateway whose
// entire proposition is low overhead.
func Hash(key obs.Secret) []byte { return hashString(key.Expose()) }

func hashString(s string) []byte {
	sum := sha256.Sum256([]byte(s))
	return sum[:]
}

// DisplayPrefix returns the identifiable portion of a presented key, for
// logs and error messages. It never returns the whole key, however short.
func DisplayPrefix(key obs.Secret) string {
	s := key.Expose()
	if len(s) <= displayLength {
		return obs.Redacted
	}
	return s[:displayLength]
}

func randomString(n int) (string, error) {
	// Values at or above this bound would bias the modulus, so they are
	// drawn again.
	const limit = 256 - (256 % len(alphabet))

	var b strings.Builder
	b.Grow(n)

	buf := make([]byte, n)
	for b.Len() < n {
		if _, err := rand.Read(buf); err != nil {
			return "", err
		}
		for _, c := range buf {
			if int(c) >= limit {
				continue
			}
			b.WriteByte(alphabet[int(c)%len(alphabet)])
			if b.Len() == n {
				break
			}
		}
	}
	return b.String(), nil
}
