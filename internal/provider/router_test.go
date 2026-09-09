package provider_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/DiegohNY/costlane/internal/provider"
)

type stubProvider struct{ name string }

func (s stubProvider) Name() string { return s.name }
func (s stubProvider) Complete(context.Context, provider.Request) (*provider.Response, error) {
	return &provider.Response{}, nil
}

func TestRouteByModelName(t *testing.T) {
	r := provider.NewRouter(stubProvider{"openai"}, stubProvider{"anthropic"})
	r.SetModelProviders(map[string]string{
		"gpt-6-astra":     "openai",
		"claude-sonnet-5": "anthropic",
	})

	p, model, err := r.Route("claude-sonnet-5")
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if p.Name() != "anthropic" || model != "claude-sonnet-5" {
		t.Errorf("routed to %s/%s", p.Name(), model)
	}
}

// The prefix disambiguates a model two providers both serve, and says so at
// the call site rather than in configuration.
func TestExplicitPrefixOverridesTheTable(t *testing.T) {
	r := provider.NewRouter(stubProvider{"openai"}, stubProvider{"anthropic"})
	r.SetModelProviders(map[string]string{"shared-model": "openai"})

	p, model, err := r.Route("anthropic/shared-model")
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if p.Name() != "anthropic" {
		t.Errorf("routed to %s, want the prefixed provider", p.Name())
	}
	if model != "shared-model" {
		t.Errorf("model = %q, want the prefix stripped", model)
	}
}

func TestUnknownModelIsRefused(t *testing.T) {
	r := provider.NewRouter(stubProvider{"openai"})
	r.SetModelProviders(map[string]string{"gpt-6-astra": "openai"})

	if _, _, err := r.Route("a-model-nobody-serves"); err == nil {
		t.Error("an unroutable model must be refused")
	}
	// A prefix naming a provider that is not configured is equally unusable.
	if _, _, err := r.Route("mistral/some-model"); err == nil {
		t.Error("a prefix naming an unconfigured provider must be refused")
	}
}

// A model priced for a provider that is not configured must not be listed as
// available.
func TestModelsExcludesUnconfiguredProviders(t *testing.T) {
	r := provider.NewRouter(stubProvider{"openai"})
	r.SetModelProviders(map[string]string{
		"gpt-6-astra":     "openai",
		"claude-sonnet-5": "anthropic",
	})

	models := r.Models()
	if _, ok := models["gpt-6-astra"]; !ok {
		t.Error("a routable model is missing from the listing")
	}
	if _, ok := models["claude-sonnet-5"]; ok {
		t.Error("a model whose provider is not configured must not be listed")
	}
}

func TestGoogleRequestTranslation(t *testing.T) {
	body := []byte(`{"model":"gemini-3.8-flash","messages":[
		{"role":"system","content":"Be terse."},
		{"role":"user","content":"hi"},
		{"role":"assistant","content":"hello"}
	],"max_tokens":100,"temperature":0.5}`)

	out, err := provider.TranslateGoogleRequest(body)
	if err != nil {
		t.Fatalf("translating: %v", err)
	}

	var got struct {
		Contents []struct {
			Role  string `json:"role"`
			Parts []struct {
				Text string `json:"text"`
			} `json:"parts"`
		} `json:"contents"`
		SystemInstruction *struct {
			Parts []struct {
				Text string `json:"text"`
			} `json:"parts"`
		} `json:"systemInstruction"`
		GenerationConfig struct {
			MaxOutputTokens int     `json:"maxOutputTokens"`
			Temperature     float64 `json:"temperature"`
		} `json:"generationConfig"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("decoding: %v", err)
	}

	// System text moves to its own field, as it does for Anthropic.
	if got.SystemInstruction == nil || got.SystemInstruction.Parts[0].Text != "Be terse." {
		t.Errorf("systemInstruction = %+v", got.SystemInstruction)
	}
	if len(got.Contents) != 2 {
		t.Fatalf("%d contents, want 2 with the system turn lifted out", len(got.Contents))
	}
	// Gemini names the assistant role "model".
	if got.Contents[1].Role != "model" {
		t.Errorf("assistant role became %q, want model", got.Contents[1].Role)
	}
	if got.GenerationConfig.MaxOutputTokens != 100 {
		t.Errorf("maxOutputTokens = %d, want 100", got.GenerationConfig.MaxOutputTokens)
	}
}

// Thoughts are billed as output and reported separately, so the completion
// total a client sees must include them.
func TestGoogleResponseIncludesThoughtsInOutput(t *testing.T) {
	raw := []byte(`{"modelVersion":"gemini-3.8-flash","candidates":[{
		"content":{"parts":[{"text":"hello"}]},"finishReason":"STOP"}],
		"usageMetadata":{"promptTokenCount":100,"candidatesTokenCount":20,
		"thoughtsTokenCount":30,"totalTokenCount":150}}`)

	body, served, finish, err := provider.TranslateGoogleResponse(raw)
	if err != nil {
		t.Fatalf("translating: %v", err)
	}
	if served != "gemini-3.8-flash" || finish != "stop" {
		t.Errorf("served=%q finish=%q", served, finish)
	}

	var out struct {
		Usage struct {
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if out.Usage.CompletionTokens != 50 {
		t.Errorf("completion_tokens = %d, want 50 (20 candidates + 30 thoughts)",
			out.Usage.CompletionTokens)
	}
}

func TestGoogleRefusesUnsupportedParameters(t *testing.T) {
	for _, param := range []string{"logprobs", "seed", "n", "frequency_penalty"} {
		t.Run(param, func(t *testing.T) {
			body := []byte(`{"model":"gemini-3.8-flash","messages":[],"` + param + `":1}`)
			_, err := provider.TranslateGoogleRequest(body)
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
