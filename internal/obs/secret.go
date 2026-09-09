// Package obs provides structured logging with redaction, and metrics.
package obs

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"regexp"
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

// secretPatterns match credentials as providers format them. They exist for
// one purpose: a provider that echoes an API key back inside an error message
// must not have that message forwarded verbatim to a client, or written to
// our logs.
var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`sk-ant-[A-Za-z0-9_-]{16,}`),
	regexp.MustCompile(`sk-proj-[A-Za-z0-9_-]{16,}`),
	regexp.MustCompile(`sk-[A-Za-z0-9]{20,}`),
	regexp.MustCompile(`cl_[A-Za-z0-9]{20,}`),
	regexp.MustCompile(`AIza[A-Za-z0-9_-]{20,}`),
	regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{20,}`),
	regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9._\-]{20,}`),
}

// RedactSecrets replaces anything that looks like a credential.
//
// This is a backstop, not the primary defence: credentials are Secret values
// everywhere they are handled, so nothing we construct should contain one.
// What this catches is text we did not write — an upstream error body on its
// way to a client.
func RedactSecrets(b []byte) []byte {
	out := b
	for _, pattern := range secretPatterns {
		out = pattern.ReplaceAll(out, []byte(Redacted))
	}
	return out
}

// Redactor removes known credentials from text that we did not write.
//
// The pattern list above covers the formats providers use today, which is
// exactly its weakness: a credential in a shape nobody anticipated — a new
// provider, an internal token, a customer's own key echoed back — passes
// straight through. So a Redactor also carries the specific values this
// process holds, and removes those verbatim. Knowing our own secrets is a
// defence that does not depend on guessing their shape.
type Redactor struct {
	// literals are the exact credentials this process was configured with.
	literals [][]byte
}

// NewRedactor builds a redactor that knows the given secrets.
//
// Short values are ignored: redacting a two-character "key" would mangle
// every message it happened to appear in, and a credential that short is not
// one worth protecting.
func NewRedactor(secrets ...Secret) *Redactor {
	const minLiteralLength = 8

	r := &Redactor{}
	for _, s := range secrets {
		value := s.Expose()
		if len(value) >= minLiteralLength {
			r.literals = append(r.literals, []byte(value))
		}
	}
	return r
}

// Redact removes both the known patterns and this process's own secrets.
func (r *Redactor) Redact(b []byte) []byte {
	out := RedactSecrets(b)
	if r == nil {
		return out
	}
	for _, literal := range r.literals {
		out = bytes.ReplaceAll(out, literal, []byte(Redacted))
	}
	return out
}
