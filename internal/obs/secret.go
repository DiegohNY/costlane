// Package obs provides structured logging with redaction, and metrics.
package obs

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
)

// Redacted is what a secret renders as. Unset distinguishes a credential
// that was never configured from one that is being hidden — a distinction
// that matters when debugging why authentication fails.
const (
	Redacted = "[redacted]"
	Unset    = "[unset]"
)

// Secret wraps a credential so that it cannot reach a log, an error message
// or a JSON response by accident.
//
// The type implements every interface Go reaches for when rendering a value:
// Stringer for %v and %s, Formatter for %#v and %q which Stringer does not
// cover, json.Marshaler for responses, and slog.LogValuer for structured
// logs. Reading the value requires calling Expose, which is greppable and
// obvious in a diff.
//
// This is the same move as the scope parameter in the repository layer: the
// compiler enforcing what discipline otherwise has to.
type Secret string

// String renders the placeholder. It satisfies fmt.Stringer, which covers
// %v and %s.
func (s Secret) String() string {
	if s == "" {
		return Unset
	}
	return Redacted
}

// Format covers the verbs Stringer does not, notably %q and %#v. Without it
// a struct printed with %#v would show the underlying string.
func (s Secret) Format(f fmt.State, verb rune) {
	_, _ = io.WriteString(f, s.String())
}

// MarshalJSON keeps secrets out of API responses and of any error payload
// that happens to embed a config struct.
func (s Secret) MarshalJSON() ([]byte, error) {
	return json.Marshal(s.String())
}

// LogValue keeps secrets out of structured logs, which is the most likely
// accidental route out of the process.
func (s Secret) LogValue() slog.Value {
	return slog.StringValue(s.String())
}

// Expose returns the underlying value. Call it only where the credential is
// actually used — building an upstream request, comparing a presented key —
// and never to log, format or return it.
func (s Secret) Expose() string { return string(s) }

// IsSet reports whether a value was configured at all.
func (s Secret) IsSet() bool { return s != "" }

// Equal compares two secrets in constant time.
//
// Both sides are hashed first so the comparison runs over a fixed length:
// comparing the raw strings would leak the length of the configured
// credential through timing, since a length mismatch can return early.
func (s Secret) Equal(other Secret) bool {
	a := sha256.Sum256([]byte(s))
	b := sha256.Sum256([]byte(other))
	return subtle.ConstantTimeCompare(a[:], b[:]) == 1
}
