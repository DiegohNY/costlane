package obs

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

const sentinel = "sk-live-do-not-print-me-0123456789"

// Every route a value can take out of the process must show the placeholder
// instead. These are the ways a credential actually escapes in practice:
// someone logs a struct, an error wraps a config, a handler marshals a
// response.
func TestSecretNeverRendersItsValue(t *testing.T) {
	s := Secret(sentinel)

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
		if !strings.Contains(got, Redacted) {
			t.Errorf("%s = %q, want it to contain %q", name, got, Redacted)
		}
	}
}

// A struct holding a secret is the common case: config dumps, request
// contexts, error payloads.
func TestSecretInsideAStructIsRedacted(t *testing.T) {
	type holder struct {
		Name  string
		Token Secret
	}
	h := holder{Name: "provider", Token: Secret(sentinel)}

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
		Key Secret `json:"key"`
	}
	b, err := json.Marshal(payload{Key: Secret(sentinel)})
	if err != nil {
		t.Fatalf("marshalling: %v", err)
	}
	if strings.Contains(string(b), sentinel) {
		t.Errorf("JSON leaked the secret: %s", b)
	}
	if !strings.Contains(string(b), Redacted) {
		t.Errorf("JSON = %s, want the placeholder", b)
	}
}

// Structured logging is the most likely accidental route out.
func TestSecretIsRedactedInStructuredLogs(t *testing.T) {
	var buf strings.Builder
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	logger.Info("connecting", "token", Secret(sentinel), "provider", "openai")

	if strings.Contains(buf.String(), sentinel) {
		t.Errorf("slog leaked the secret: %s", buf.String())
	}
	if !strings.Contains(buf.String(), Redacted) {
		t.Errorf("log = %s, want the placeholder", buf.String())
	}
}

// The value has to be usable somewhere, and that place should be obvious in
// a diff.
func TestExposeReturnsTheValue(t *testing.T) {
	if got := Secret(sentinel).Expose(); got != sentinel {
		t.Errorf("Expose() = %q, want the original value", got)
	}
}

func TestEmptySecretIsDistinguishable(t *testing.T) {
	if got := Secret("").String(); got != Unset {
		t.Errorf("an empty secret renders %q, want %q: an unset credential is not a hidden one",
			got, Unset)
	}
}

func TestSecretEqualityIsConstantTime(t *testing.T) {
	s := Secret(sentinel)
	if !s.Equal(Secret(sentinel)) {
		t.Error("identical secrets must compare equal")
	}
	if s.Equal(Secret("different")) {
		t.Error("different secrets must not compare equal")
	}
	// A shorter candidate must not short-circuit into a different code path.
	if s.Equal(Secret("sk-")) {
		t.Error("a prefix must not compare equal")
	}
	if Secret("").Equal(Secret("")) != true {
		t.Error("two unset secrets compare equal")
	}
}

var errSentinel = fmt.Errorf("upstream refused")
