package provider_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/DiegohNY/costlane/internal/obs"
	"github.com/DiegohNY/costlane/internal/provider"
)

// A "provider/model" prefix is resolved by routing, and the name that reaches
// the provider must be the resolved one.
//
// The prefix exists so a model two providers both serve can be disambiguated
// at the call site. Routing strips it and hands the adapter the bare name in
// Request.Model — but two of the three adapters took the model from the
// request body instead, where the prefix is still attached. Anthropic sent
// "anthropic/claude-sonnet-5" as its model, and OpenAI forwarded a body still
// naming "openai/gpt-6-astra", so the one feature the prefix exists for
// produced an upstream 404 on the two providers that read it.
func TestRoutedModelReachesTheProvider(t *testing.T) {
	t.Run("openai", func(t *testing.T) {
		body, _ := captureUpstream(t, func(opts provider.Options) error {
			p := provider.NewOpenAI(opts)
			_, err := p.Complete(t.Context(), provider.Request{
				Body:  []byte(`{"model":"openai/gpt-6-astra","messages":[{"role":"user","content":"hi"}]}`),
				Model: "gpt-6-astra",
			})
			return err
		}, `{"id":"x","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},`+
			`"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)

		assertSentModel(t, body, "gpt-6-astra")
	})

	t.Run("anthropic", func(t *testing.T) {
		body, _ := captureUpstream(t, func(opts provider.Options) error {
			p := provider.NewAnthropic(opts)
			_, err := p.Complete(t.Context(), provider.Request{
				Body:  []byte(`{"model":"anthropic/claude-sonnet-5","messages":[{"role":"user","content":"hi"}]}`),
				Model: "claude-sonnet-5",
			})
			return err
		}, `{"id":"x","model":"claude-sonnet-5","content":[{"type":"text","text":"hi"}],`+
			`"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)

		assertSentModel(t, body, "claude-sonnet-5")
	})

	// Gemini carries the model in the path rather than in the body, so a
	// prefix would break the URL instead of the payload. It already used
	// the routed name; this pins it.
	t.Run("google", func(t *testing.T) {
		body, path := captureUpstream(t, func(opts provider.Options) error {
			p := provider.NewGoogle(opts)
			_, err := p.Complete(t.Context(), provider.Request{
				Body:  []byte(`{"model":"google/gemini-3.8-flash","messages":[{"role":"user","content":"hi"}]}`),
				Model: "gemini-3.8-flash",
			})
			return err
		}, `{"modelVersion":"gemini-3.8-flash","candidates":[{"content":{"parts":[{"text":"hi"}]},`+
			`"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1}}`)

		if !strings.Contains(path, "/models/gemini-3.8-flash:generateContent") {
			t.Errorf("path = %q, want the bare model in it", path)
		}
		if strings.Contains(path, "google/gemini") {
			t.Errorf("path = %q still carries the routing prefix", path)
		}
		if strings.Contains(string(body), "google/gemini") {
			t.Errorf("the translated body carries the routing prefix:\n%s", body)
		}
	})
}

// The prefix is the only reason to touch the body at all. Without one, an
// OpenAI-compatible request must arrive exactly as it was sent — which is a
// promise this repository makes in its README.
func TestUnprefixedOpenAIBodyIsForwardedByteForByte(t *testing.T) {
	sent := `{"model":"gpt-6-astra","messages":[{"role":"user","content":"hi"},` +
		`{"role":"assistant","content":"x"}],"seed":7,"unknown_future_field":{"a":[1,2,3]}}`

	body, _ := captureUpstream(t, func(opts provider.Options) error {
		p := provider.NewOpenAI(opts)
		_, err := p.Complete(t.Context(), provider.Request{
			Body: []byte(sent), Model: "gpt-6-astra",
		})
		return err
	}, `{"id":"x","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)

	if string(body) != sent {
		t.Errorf("the body was rewritten when nothing needed rewriting\n--- got ---\n%s\n--- want ---\n%s",
			body, sent)
	}
}

// captureUpstream runs call against a server that records the request it
// receives and answers with reply.
func captureUpstream(t *testing.T, call func(provider.Options) error, reply string) (body []byte, path string) {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		path = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, reply)
	}))
	defer srv.Close()

	if err := call(provider.Options{
		BaseURL:          srv.URL,
		APIKey:           obs.Secret("test-key-not-a-real-credential"),
		Client:           srv.Client(),
		DefaultMaxTokens: 4096,
	}); err != nil {
		t.Fatalf("calling the provider: %v", err)
	}
	return body, path
}

func assertSentModel(t *testing.T, body []byte, want string) {
	t.Helper()
	var sent struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &sent); err != nil {
		t.Fatalf("decoding what was sent upstream: %v\n%s", err, body)
	}
	if sent.Model != want {
		t.Errorf("the provider was asked for model %q, want %q\n%s", sent.Model, want, body)
	}
}
