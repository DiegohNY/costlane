package fakeprovider_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/DiegohNY/costlane/internal/fakeprovider"
	"github.com/DiegohNY/costlane/internal/pricing"
)

func newFake(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(fakeprovider.Handler())
	t.Cleanup(srv.Close)
	return srv
}

// result carries a response whose body has already been read and closed, so
// no caller can leak a connection by forgetting.
type result struct {
	status int
	header http.Header
	body   []byte
}

func post(t *testing.T, srv *httptest.Server, path, body string, headers map[string]string) result {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+path,
		strings.NewReader(body))
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("requesting: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading body: %v", err)
	}
	return result{status: resp.StatusCode, header: resp.Header, body: raw}
}

// The usage each dialect reports must normalise to the same counts, or the
// adapters are being tested against a fiction.
func TestEachDialectNormalisesToTheSameCounts(t *testing.T) {
	srv := newFake(t)
	headers := map[string]string{
		fakeprovider.HeaderPromptTokens:     "1000",
		fakeprovider.HeaderCompletionTokens: "200",
		fakeprovider.HeaderCachedTokens:     "300",
	}

	t.Run("openai", func(t *testing.T) {
		resp := post(t, srv, "/v1/chat/completions", `{"model":"gpt-6-astra"}`, headers)
		got, err := pricing.NormaliseOpenAI(resp.body)
		if err != nil {
			t.Fatalf("normalising: %v", err)
		}
		// OpenAI reports cached inside the prompt total, so plain input is
		// 1000 - 300.
		assertCount(t, got.Counts, pricing.KindInput, 700)
		assertCount(t, got.Counts, pricing.KindCachedRead, 300)
		assertCount(t, got.Counts, pricing.KindOutput, 200)
	})

	t.Run("anthropic", func(t *testing.T) {
		resp := post(t, srv, "/v1/messages",
			`{"model":"claude-sonnet-5","max_tokens":100}`, headers)
		got, err := pricing.NormaliseAnthropic(resp.body)
		if err != nil {
			t.Fatalf("normalising: %v", err)
		}
		// Anthropic reports cached alongside input, so input stays 1000.
		assertCount(t, got.Counts, pricing.KindInput, 1000)
		assertCount(t, got.Counts, pricing.KindCachedRead, 300)
		assertCount(t, got.Counts, pricing.KindOutput, 200)
	})

	t.Run("google", func(t *testing.T) {
		resp := post(t, srv, "/v1beta/models/gemini-3.8-flash:generateContent",
			`{"contents":[]}`, headers)
		got, err := pricing.NormaliseGoogle(resp.body)
		if err != nil {
			t.Fatalf("normalising: %v", err)
		}
		// Gemini reports cached inside the prompt total, like OpenAI.
		assertCount(t, got.Counts, pricing.KindInput, 700)
		assertCount(t, got.Counts, pricing.KindCachedRead, 300)
		assertCount(t, got.Counts, pricing.KindOutput, 200)
	})
}

// Anthropic rejects a request without max_tokens. Reproducing that is what
// makes the gateway's injection testable rather than assumed.
func TestAnthropicRequiresMaxTokens(t *testing.T) {
	srv := newFake(t)
	resp := post(t, srv, "/v1/messages", `{"model":"claude-sonnet-5"}`, nil)
	if resp.status != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for a request without max_tokens", resp.status)
	}
	if !strings.Contains(string(resp.body), "max_tokens") {
		t.Error("the error should name the missing field")
	}
}

func TestServedModelCanDifferFromRequested(t *testing.T) {
	srv := newFake(t)
	resp := post(t, srv, "/v1/chat/completions", `{"model":"gpt-4o"}`,
		map[string]string{fakeprovider.HeaderServedModel: "gpt-4o-2024-08-06"})

	var body struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(resp.body, &body); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if body.Model != "gpt-4o-2024-08-06" {
		t.Errorf("model = %q, want the served snapshot", body.Model)
	}
}

func TestRequestIDIsEchoed(t *testing.T) {
	srv := newFake(t)
	resp := post(t, srv, "/v1/chat/completions", `{"model":"m"}`,
		map[string]string{fakeprovider.HeaderRequestID: "req-abc123"})
	if got := resp.header.Get("X-Request-Id"); got != "req-abc123" {
		t.Errorf("X-Request-Id = %q, want req-abc123", got)
	}
}

func TestFailureScenariosAreReproducible(t *testing.T) {
	srv := newFake(t)

	t.Run("status and retry-after", func(t *testing.T) {
		resp := post(t, srv, "/v1/chat/completions", `{"model":"m"}`, map[string]string{
			fakeprovider.HeaderStatus:     "429",
			fakeprovider.HeaderRetryAfter: "30",
		})
		if resp.status != http.StatusTooManyRequests {
			t.Errorf("status = %d, want 429", resp.status)
		}
		if got := resp.header.Get("Retry-After"); got != "30" {
			t.Errorf("Retry-After = %q, want 30", got)
		}
	})

	t.Run("custom error body", func(t *testing.T) {
		const planted = `{"error":{"message":"bad key sk-planted-credential"}}`
		resp := post(t, srv, "/v1/chat/completions", `{"model":"m"}`, map[string]string{
			fakeprovider.HeaderStatus:    "401",
			fakeprovider.HeaderErrorBody: planted,
		})
		if got := string(resp.body); got != planted {
			t.Errorf("body = %s, want the planted body verbatim", got)
		}
	})
}

func TestStreamingEmitsUsageWhenAsked(t *testing.T) {
	srv := newFake(t)
	resp := post(t, srv, "/v1/chat/completions",
		`{"model":"m","stream":true,"stream_options":{"include_usage":true}}`,
		map[string]string{
			fakeprovider.HeaderPromptTokens:     "50",
			fakeprovider.HeaderCompletionTokens: "3",
		})

	body := string(resp.body)
	if !strings.Contains(body, `"usage"`) {
		t.Error("the stream should carry a usage chunk when include_usage is set")
	}
	if !strings.HasSuffix(strings.TrimSpace(body), "data: [DONE]") {
		t.Errorf("the stream must end with [DONE], got:\n%s", body)
	}
	if n := strings.Count(body, `"delta"`); n < 3 {
		t.Errorf("%d delta chunks, want at least the 3 requested", n)
	}
}

func TestStreamCanBeCutShort(t *testing.T) {
	srv := newFake(t)
	resp := post(t, srv, "/v1/chat/completions", `{"model":"m","stream":true}`,
		map[string]string{
			fakeprovider.HeaderCompletionTokens: "10",
			fakeprovider.HeaderFailAfterChunks:  "3",
		})

	body := string(resp.body)
	if strings.Contains(body, "[DONE]") {
		t.Error("a truncated stream must not reach [DONE]")
	}
	if n := strings.Count(body, "data:"); n != 3 {
		t.Errorf("%d chunks before the cut, want 3", n)
	}
}

func TestStreamCanEmitAMalformedChunk(t *testing.T) {
	srv := newFake(t)
	resp := post(t, srv, "/v1/chat/completions", `{"model":"m","stream":true}`,
		map[string]string{
			fakeprovider.HeaderCompletionTokens: "5",
			fakeprovider.HeaderMalformedChunkAt: "2",
		})

	body := string(resp.body)
	// The malformed frame is there for the gateway to forward rather than
	// judge, so the fake must actually produce one.
	if !strings.Contains(body, `data: {"choices":[{"delta":{"content":`) {
		t.Errorf("no malformed chunk in:\n%s", body)
	}
	if !strings.Contains(body, "[DONE]") {
		t.Error("the stream should still finish normally after a malformed chunk")
	}
}

func assertCount(t *testing.T, counts pricing.Counts, kind pricing.Kind, want int64) {
	t.Helper()
	if got := counts[kind]; got != want {
		t.Errorf("%s = %d, want %d", kind, got, want)
	}
}
