package obs_test

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/DiegohNY/costlane/internal/obs"
)

const sentinel = "sk-live-do-not-print-me-0123456789"

// Every route a value can take out of the process must show the placeholder
// instead. These are the ways a credential actually escapes in practice:
// someone logs a struct, an error wraps a config, a handler marshals a
// response.
func TestSecretNeverRendersItsValue(t *testing.T) {
	s := obs.Secret(sentinel)

	renderings := map[string]string{
		"String()":     s.String(),
		"%v":           fmt.Sprintf("%v", s),
		"%s":           fmt.Sprintf("%s", s),
		"%q":           fmt.Sprintf("%q", s),
		"%#v":          fmt.Sprintf("%#v", s),
		"%+v":          fmt.Sprintf("%+v", s),
		"inside error": fmt.Errorf("connecting with %v: %w", s, errSentinel).Error(),
	}
	for name, got := range renderings {
		if strings.Contains(got, sentinel) {
			t.Errorf("%s leaked the secret: %s", name, got)
		}
		if !strings.Contains(got, obs.Redacted) {
			t.Errorf("%s = %q, want it to contain %q", name, got, obs.Redacted)
		}
	}
}

// A struct holding a secret is the common case: config dumps, request
// contexts, error payloads.
func TestSecretInsideAStructIsRedacted(t *testing.T) {
	type holder struct {
		Name  string
		Token obs.Secret
	}
	h := holder{Name: "provider", Token: obs.Secret(sentinel)}

	for _, got := range []string{
		fmt.Sprintf("%v", h),
		fmt.Sprintf("%+v", h),
		fmt.Sprintf("%#v", h),
	} {
		if strings.Contains(got, sentinel) {
			t.Errorf("a struct rendering leaked the secret: %s", got)
		}
	}
}

func TestSecretMarshalsAsRedacted(t *testing.T) {
	type payload struct {
		Key obs.Secret `json:"key"`
	}
	b, err := json.Marshal(payload{Key: obs.Secret(sentinel)})
	if err != nil {
		t.Fatalf("marshalling: %v", err)
	}
	if strings.Contains(string(b), sentinel) {
		t.Errorf("JSON leaked the secret: %s", b)
	}
	if !strings.Contains(string(b), obs.Redacted) {
		t.Errorf("JSON = %s, want the placeholder", b)
	}
}

// Structured logging is the most likely accidental route out.
func TestSecretIsRedactedInStructuredLogs(t *testing.T) {
	var buf strings.Builder
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	logger.Info("connecting", "token", obs.Secret(sentinel), "provider", "openai")

	if strings.Contains(buf.String(), sentinel) {
		t.Errorf("slog leaked the secret: %s", buf.String())
	}
	if !strings.Contains(buf.String(), obs.Redacted) {
		t.Errorf("log = %s, want the placeholder", buf.String())
	}
}

// The value has to be usable somewhere, and that place should be obvious in
// a diff.
func TestExposeReturnsTheValue(t *testing.T) {
	if got := obs.Secret(sentinel).Expose(); got != sentinel {
		t.Errorf("Expose() = %q, want the original value", got)
	}
}

func TestEmptySecretIsDistinguishable(t *testing.T) {
	if got := obs.Secret("").String(); got != obs.Unset {
		t.Errorf("an empty secret renders %q, want %q: an unset credential is not a hidden one",
			got, obs.Unset)
	}
}

func TestSecretEqualityIsConstantTime(t *testing.T) {
	s := obs.Secret(sentinel)
	if !s.Equal(obs.Secret(sentinel)) {
		t.Error("identical secrets must compare equal")
	}
	if s.Equal(obs.Secret("different")) {
		t.Error("different secrets must not compare equal")
	}
	// A shorter candidate must not short-circuit into a different code path.
	if s.Equal(obs.Secret("sk-")) {
		t.Error("a prefix must not compare equal")
	}
	if obs.Secret("").Equal(obs.Secret("")) != true {
		t.Error("two unset secrets compare equal")
	}
}

var errSentinel = fmt.Errorf("upstream refused")

// The pattern list covers the credential formats providers use today, which
// is precisely its limit: a key in a shape nobody anticipated passes through.
// A redactor that knows this process's own secrets does not have to guess.
func TestRedactorRemovesSecretsOfUnknownShape(t *testing.T) {
	// Nothing about this value looks like a credential.
	const opaque = "9f3b2c1d-not-a-recognisable-key-format"
	r := obs.NewRedactor(obs.Secret(opaque))

	body := []byte(`{"error":{"message":"rejected key ` + opaque + ` from tenant 42"}}`)
	got := string(r.Redact(body))

	if strings.Contains(got, opaque) {
		t.Errorf("a credential of unknown shape survived redaction: %s", got)
	}
	if !strings.Contains(got, obs.Redacted) {
		t.Errorf("nothing was redacted: %s", got)
	}
	// The rest of the message must survive, or an operator loses the
	// information they needed.
	if !strings.Contains(got, "tenant 42") {
		t.Errorf("redaction removed more than the credential: %s", got)
	}
}

// Known formats are still caught even when the redactor was never told about
// them, because a provider may echo a credential we do not hold.
func TestRedactorStillCatchesKnownPatterns(t *testing.T) {
	r := obs.NewRedactor()
	body := []byte(`{"message":"bad key sk-proj-abcdefghijklmnopqrstuvwxyz012345"}`)

	if got := string(r.Redact(body)); strings.Contains(got, "sk-proj-abcdefghij") {
		t.Errorf("a recognisable key survived: %s", got)
	}
}

// A very short secret is not redacted: doing so would mangle every message
// that happened to contain those characters.
func TestRedactorIgnoresValuesTooShortToBeCredentials(t *testing.T) {
	r := obs.NewRedactor(obs.Secret("ab"))
	body := []byte("a stable build")
	if got := string(r.Redact(body)); got != "a stable build" {
		t.Errorf("a two-character secret mangled the text: %s", got)
	}
}

func TestNilRedactorStillAppliesPatterns(t *testing.T) {
	var r *obs.Redactor
	body := []byte("key sk-proj-abcdefghijklmnopqrstuvwxyz012345")
	if got := string(r.Redact(body)); strings.Contains(got, "sk-proj-abcdefghij") {
		t.Errorf("a nil redactor let a known pattern through: %s", got)
	}
}
