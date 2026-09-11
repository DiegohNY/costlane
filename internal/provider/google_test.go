package provider_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/DiegohNY/costlane/internal/obs"
	"github.com/DiegohNY/costlane/internal/provider"
)

// The golden files are the translation contract, as they are for Anthropic:
// one file per case, so what a caller can ask Gemini for is enumerable
// rather than folded into prose.
func TestGoogleRequestTranslationGolden(t *testing.T) {
	dir := filepath.Join("testdata", "google")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading fixtures: %v", err)
	}

	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".in.json") {
			continue
		}
		base := strings.TrimSuffix(name, ".in.json")

		t.Run(base, func(t *testing.T) {
			in := readFixture(t, filepath.Join(dir, base+".in.json"))
			want := readFixture(t, filepath.Join(dir, base+".out.json"))

			got, err := provider.TranslateGoogleRequest(in, fixtureModel(t, in))
			if err != nil {
				t.Fatalf("translating: %v", err)
			}
			assertJSONEqual(t, got, want)
		})
	}
}

// A request that says nothing about thinking must leave thinkingConfig
// alone, so the model applies its own documented default rather than one
// costlane picked for it.
func TestGoogleAbsentReasoningEffortLeavesThinkingUntouched(t *testing.T) {
	body := []byte(`{"model":"gemini-3.8-flash","messages":[{"role":"user","content":"hi"}],"temperature":0.2}`)

	got, err := provider.TranslateGoogleRequest(body, "gemini-3.8-flash")
	if err != nil {
		t.Fatalf("translating: %v", err)
	}
	if strings.Contains(string(got), "thinking") {
		t.Errorf("a request with no reasoning_effort grew a thinking field:\n%s", got)
	}
}

// Fail closed. Every case here is a request costlane cannot honour, and the
// alternative to refusing is to forward it without the ceiling the caller
// asked for and bill them for the thinking they tried to avoid.
func TestGoogleRefusesUnmappableReasoningEffort(t *testing.T) {
	cases := []struct {
		name, model, effort, why string
	}{
		{
			name: "none_on_flash", model: "gemini-3.8-flash", effort: "none",
			why: "Google: reasoning cannot be turned off for Gemini 2.5 Pro or 3 models",
		},
		{
			name: "none_on_pro", model: "gemini-3.1-pro-preview", effort: "none",
			why: "the same, on the other seeded model",
		},
		{
			name: "minimal_on_flash", model: "gemini-3.8-flash", effort: "minimal",
			why: "Google: minimal is not supported for Gemini 3.8 Flash and will return an error",
		},
		{
			name: "unknown_model", model: "gemini-9-unreleased", effort: "low",
			why: "a model with no seeded table has no mapping to apply",
		},
		{
			name: "unknown_level", model: "gemini-3.8-flash", effort: "exhaustive",
			why: "a level outside the vocabulary is a typo, not an instruction",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			body := []byte(`{"model":"` + c.model + `","messages":[],"reasoning_effort":"` +
				c.effort + `"}`)

			_, err := provider.TranslateGoogleRequest(body, c.model)
			unsupported, ok := provider.AsUnsupported(err)
			if !ok {
				t.Fatalf("err = %v, want an unsupported-parameter error (%s)", err, c.why)
			}
			if unsupported.Parameter != "reasoning_effort" {
				t.Errorf("Parameter = %q, want reasoning_effort", unsupported.Parameter)
			}
			// The refusal has to say why, or a caller reading it learns
			// only that something was wrong with a parameter they spelled
			// correctly.
			if unsupported.Detail == "" {
				t.Error("the refusal carries no reason")
			}
			if !strings.Contains(unsupported.Error(), c.effort) {
				t.Errorf("the error does not name the level asked for: %v", unsupported)
			}
		})
	}
}

// A caller who knows Gemini's own dialect might send its fields directly.
// They are refused by name, which is what every other unsupported parameter
// already gets — and what thinkingConfig did not get before this change: it
// was on no denylist, so it was dropped in silence.
func TestGoogleRefusesNativeThinkingFields(t *testing.T) {
	for _, param := range []string{
		"thinkingConfig", "thinking_level", "thinkingLevel",
		"thinkingBudget", "thinking_budget",
	} {
		t.Run(param, func(t *testing.T) {
			body := []byte(`{"model":"gemini-3.8-flash","messages":[],"` + param + `":1}`)

			_, err := provider.TranslateGoogleRequest(body, "gemini-3.8-flash")
			unsupported, ok := provider.AsUnsupported(err)
			if !ok {
				t.Fatalf("err = %v, want an unsupported-parameter error", err)
			}
			if unsupported.Parameter != param {
				t.Errorf("Parameter = %q, want %q", unsupported.Parameter, param)
			}
		})
	}
}

// The streaming path shares the translation, and this test says so rather
// than assuming it. v0.2.1 shipped a provider nothing constructed because no
// test crossed the seam production crosses; a translation nothing asserts on
// the streaming path is the same shape of hole.
func TestGoogleStreamSendsTheMappedThinkingLevel(t *testing.T) {
	var (
		gotBody []byte
		gotPath string
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	p := provider.NewGoogle(provider.Options{
		BaseURL: upstream.URL,
		APIKey:  obs.Secret("test-key-not-a-real-credential"),
		Client:  upstream.Client(),
	})

	body := []byte(`{"model":"gemini-3.8-flash","messages":[{"role":"user","content":"hi"}],` +
		`"stream":true,"reasoning_effort":"low"}`)
	stream, err := p.Stream(t.Context(), provider.Request{
		Body: body, Model: "gemini-3.8-flash", Stream: true,
	})
	if err != nil {
		t.Fatalf("streaming: %v", err)
	}
	defer func() { _ = stream.Body.Close() }()

	if !strings.Contains(gotPath, "streamGenerateContent") {
		t.Errorf("path = %q, want the streaming method", gotPath)
	}

	var sent struct {
		GenerationConfig struct {
			ThinkingConfig struct {
				ThinkingLevel string `json:"thinkingLevel"`
			} `json:"thinkingConfig"`
		} `json:"generationConfig"`
	}
	if err := json.Unmarshal(gotBody, &sent); err != nil {
		t.Fatalf("decoding what was sent upstream: %v\n%s", err, gotBody)
	}
	if sent.GenerationConfig.ThinkingConfig.ThinkingLevel != "low" {
		t.Errorf("the streamed request carried thinkingLevel %q, want low\n%s",
			sent.GenerationConfig.ThinkingConfig.ThinkingLevel, gotBody)
	}
}

// A streamed request costlane cannot map is refused before the connection is
// opened, not after the provider has started thinking.
func TestGoogleStreamRefusesUnmappableEffortBeforeCalling(t *testing.T) {
	called := false
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		called = true
	}))
	defer upstream.Close()

	p := provider.NewGoogle(provider.Options{
		BaseURL: upstream.URL,
		APIKey:  obs.Secret("test-key-not-a-real-credential"),
		Client:  upstream.Client(),
	})

	body := []byte(`{"model":"gemini-3.8-flash","messages":[],"stream":true,"reasoning_effort":"none"}`)
	_, err := p.Stream(t.Context(), provider.Request{
		Body: body, Model: "gemini-3.8-flash", Stream: true,
	})
	if _, ok := provider.AsUnsupported(err); !ok {
		t.Fatalf("err = %v, want an unsupported-parameter error", err)
	}
	if called {
		t.Error("the provider was called for a request costlane had already refused")
	}
}

// fixtureModel reads the model out of a fixture body. The gateway passes the
// routed model separately, because a "provider/model" prefix makes the two
// differ; for a fixture they are the same.
func fixtureModel(t *testing.T, body []byte) string {
	t.Helper()
	var in struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		t.Fatalf("reading the fixture model: %v", err)
	}
	return in.Model
}
