package fakeprovider

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Handler serves all three native dialects.
//
// Speaking the providers' own shapes, rather than a convenient common one,
// is the point: an adapter tested against our idea of a provider proves
// nothing about the provider.
func Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", handleOpenAI)
	mux.HandleFunc("POST /v1/messages", handleAnthropic)
	mux.HandleFunc("POST /v1beta/models/{model}", handleGemini)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	return mux
}

type openAIRequest struct {
	Model         string `json:"model"`
	Stream        bool   `json:"stream"`
	StreamOptions *struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options"`
}

func handleOpenAI(w http.ResponseWriter, r *http.Request) {
	s := ScenarioFromRequest(r)
	var req openAIRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": map[string]any{"message": "invalid JSON", "type": "invalid_request_error"},
		})
		return
	}

	model := s.ServedModel
	if model == "" {
		model = req.Model
	}
	w.Header().Set("X-Request-Id", s.RequestID)

	if s.Latency > 0 {
		time.Sleep(s.Latency)
	}
	if s.Status != 0 {
		writeProviderError(w, s, "openai")
		return
	}

	if req.Stream {
		streamOpenAI(w, s, model, req.StreamOptions != nil && req.StreamOptions.IncludeUsage)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"id":      "chatcmpl-" + s.RequestID,
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       map[string]any{"role": "assistant", "content": generatedText(s)},
			"finish_reason": "stop",
		}},
		"usage": openAIUsage(s),
	})
}

func openAIUsage(s Scenario) map[string]any {
	usage := map[string]any{
		"prompt_tokens":     s.PromptTokens,
		"completion_tokens": s.CompletionTokens,
		"total_tokens":      s.PromptTokens + s.CompletionTokens,
	}
	// Cached and cache-write tokens are subsets of the prompt total here,
	// exactly as OpenAI reports them.
	if s.CachedTokens > 0 || s.CacheWriteTokens > 0 {
		usage["prompt_tokens_details"] = map[string]any{
			"cached_tokens":      s.CachedTokens,
			"cache_write_tokens": s.CacheWriteTokens,
		}
	}
	if s.ReasoningTokens > 0 {
		usage["completion_tokens_details"] = map[string]any{
			"reasoning_tokens": s.ReasoningTokens,
		}
	}
	return usage
}

type anthropicRequest struct {
	Model     string `json:"model"`
	Stream    bool   `json:"stream"`
	MaxTokens *int   `json:"max_tokens"`
}

func handleAnthropic(w http.ResponseWriter, r *http.Request) {
	s := ScenarioFromRequest(r)
	var req anthropicRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, anthropicError("invalid JSON"))
		return
	}

	// The real API rejects a request without max_tokens, and the gateway
	// has to inject one. Enforcing it here is what makes that injection
	// testable rather than assumed.
	if req.MaxTokens == nil {
		writeJSON(w, http.StatusBadRequest,
			anthropicError("max_tokens: field required"))
		return
	}

	model := s.ServedModel
	if model == "" {
		model = req.Model
	}
	w.Header().Set("Request-Id", s.RequestID)

	if s.Latency > 0 {
		time.Sleep(s.Latency)
	}
	if s.Status != 0 {
		writeProviderError(w, s, "anthropic")
		return
	}

	if req.Stream {
		streamAnthropic(w, s, model)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"id":          "msg_" + s.RequestID,
		"type":        "message",
		"role":        "assistant",
		"model":       model,
		"content":     []any{map[string]any{"type": "text", "text": generatedText(s)}},
		"stop_reason": "end_turn",
		"usage":       anthropicUsage(s),
	})
}

func anthropicUsage(s Scenario) map[string]any {
	// Anthropic's cache fields sit alongside input_tokens rather than
	// inside it: the documented identity is
	// total = input + cache_read + cache_creation.
	usage := map[string]any{
		"input_tokens":  s.PromptTokens,
		"output_tokens": s.CompletionTokens,
	}
	if s.CachedTokens > 0 {
		usage["cache_read_input_tokens"] = s.CachedTokens
	}
	if s.CacheWriteTokens > 0 {
		usage["cache_creation_input_tokens"] = s.CacheWriteTokens
		usage["cache_creation"] = map[string]any{
			"ephemeral_5m_input_tokens": s.CacheWriteTokens,
			"ephemeral_1h_input_tokens": 0,
		}
	}
	return usage
}

func handleGemini(w http.ResponseWriter, r *http.Request) {
	s := ScenarioFromRequest(r)
	// Gemini encodes both the model and the method in the path, as
	// models/{model}:generateContent.
	pathModel := r.PathValue("model")
	model, method, _ := strings.Cut(pathModel, ":")
	model = strings.TrimPrefix(model, "models/")

	if s.ServedModel != "" {
		model = s.ServedModel
	}
	w.Header().Set("X-Request-Id", s.RequestID)

	if s.Latency > 0 {
		time.Sleep(s.Latency)
	}
	if s.Status != 0 {
		writeProviderError(w, s, "google")
		return
	}

	if strings.Contains(method, "streamGenerateContent") {
		streamGemini(w, s, model)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"modelVersion": model,
		"candidates": []any{map[string]any{
			"content":      map[string]any{"role": "model", "parts": []any{map[string]any{"text": generatedText(s)}}},
			"finishReason": "STOP",
		}},
		"usageMetadata": geminiUsage(s),
	})
}

func geminiUsage(s Scenario) map[string]any {
	// promptTokenCount includes cachedContentTokenCount, while
	// thoughtsTokenCount sits outside candidatesTokenCount: Gemini is the
	// one provider that goes both ways.
	usage := map[string]any{
		"promptTokenCount":     s.PromptTokens,
		"candidatesTokenCount": s.CompletionTokens,
		"totalTokenCount":      s.PromptTokens + s.CompletionTokens + s.ReasoningTokens,
	}
	if s.CachedTokens > 0 {
		usage["cachedContentTokenCount"] = s.CachedTokens
	}
	if s.ReasoningTokens > 0 {
		usage["thoughtsTokenCount"] = s.ReasoningTokens
	}
	return usage
}

func writeProviderError(w http.ResponseWriter, s Scenario, provider string) {
	if s.RetryAfter != "" {
		w.Header().Set("Retry-After", s.RetryAfter)
	}
	if s.ErrorBody != "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(s.Status)
		_, _ = w.Write([]byte(s.ErrorBody))
		return
	}
	message := fmt.Sprintf("the fake provider was asked to return %d", s.Status)
	switch provider {
	case "anthropic":
		writeJSON(w, s.Status, anthropicError(message))
	case "google":
		writeJSON(w, s.Status, map[string]any{
			"error": map[string]any{"code": s.Status, "message": message, "status": "FAILED_PRECONDITION"},
		})
	default:
		writeJSON(w, s.Status, map[string]any{
			"error": map[string]any{"message": message, "type": "server_error"},
		})
	}
}

func anthropicError(message string) map[string]any {
	return map[string]any{
		"type":  "error",
		"error": map[string]any{"type": "invalid_request_error", "message": message},
	}
}

// generatedText produces a body whose length tracks the token count, so a
// gateway estimating from bytes sees something plausible.
func generatedText(s Scenario) string {
	if s.CompletionTokens <= 0 {
		return ""
	}
	return strings.TrimSpace(strings.Repeat("token ", s.CompletionTokens))
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
