package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/DiegohNY/costlane/internal/pricing"
)

// Anthropic translates the OpenAI dialect into the Messages API.
type Anthropic struct{ opts Options }

// NewAnthropic builds the adapter.
func NewAnthropic(opts Options) *Anthropic {
	if opts.BaseURL == "" {
		opts.BaseURL = "https://api.anthropic.com"
	}
	if opts.DefaultMaxTokens == 0 {
		opts.DefaultMaxTokens = 4096
	}
	return &Anthropic{opts: opts}
}

// Name identifies this provider in the price table.
func (p *Anthropic) Name() string { return "anthropic" }

// anthropicUnsupported lists OpenAI parameters the Messages API has no
// equivalent for.
//
// They are refused rather than dropped. A client that asks for logprobs and
// receives a response without them cannot tell whether the model declined or
// the gateway discarded the request, and silently changing the meaning of a
// request is the worst thing a proxy can do.
var anthropicUnsupported = []string{
	"logprobs", "top_logprobs", "n", "presence_penalty", "frequency_penalty",
	"logit_bias", "seed", "response_format", "modalities", "audio",
}

// Complete translates, forwards, and translates back.
func (p *Anthropic) Complete(ctx context.Context, req Request) (*Response, error) {
	translated, injected, err := TranslateAnthropicRequest(req.Body, p.opts.DefaultMaxTokens)
	if err != nil {
		return nil, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		p.opts.BaseURL+"/v1/messages", bytes.NewReader(translated))
	if err != nil {
		return nil, fmt.Errorf("provider: building request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-api-key", p.opts.APIKey.Expose())
	httpReq.Header.Set("anthropic-version", "2023-06-01")
	for name, value := range req.PassthroughHeaders {
		httpReq.Header.Set(name, value)
	}

	resp, err := p.opts.Client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("provider: calling anthropic: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("provider: reading anthropic response: %w", err)
	}

	if resp.StatusCode >= 400 {
		return nil, &ErrUpstream{
			StatusCode: resp.StatusCode,
			Body:       raw,
			RetryAfter: resp.Header.Get("Retry-After"),
			// Not the shape our clients expect, so it is re-described
			// rather than passed through.
			Native: false,
		}
	}

	out := &Response{
		StatusCode:        resp.StatusCode,
		ProviderRequestID: resp.Header.Get("Request-Id"),
		InjectedMaxTokens: injected,
	}

	normalised, err := pricing.NormaliseAnthropic(raw)
	if err != nil {
		out.Degraded = true
		out.ParseErrors++
		out.Counts = pricing.Counts{}
	} else {
		out.Counts = normalised.Counts
		out.Degraded = normalised.Degraded
		out.ParseErrors = normalised.ParseErrors
	}

	respBody, servedModel, finish, err := TranslateAnthropicResponse(raw)
	if err != nil {
		return nil, err
	}
	out.Body = respBody
	out.ServedModel = servedModel
	out.FinishReason = finish
	return out, nil
}

// TranslateAnthropicRequest converts an OpenAI chat completion into a
// Messages call.
//
// It returns the number of output tokens injected, if any, so the caller can
// tell the client that a ceiling it did not ask for was applied. It is
// exported so the golden fixtures can pin the translation directly.
func TranslateAnthropicRequest(body []byte, defaultMaxTokens int) ([]byte, int, error) {
	names, err := FieldNames(body)
	if err != nil {
		return nil, 0, err
	}
	for _, name := range names {
		for _, unsupported := range anthropicUnsupported {
			if name == unsupported {
				return nil, 0, &ErrUnsupportedParameter{Parameter: name, Provider: "anthropic"}
			}
		}
	}

	var incoming struct {
		Model       string            `json:"model"`
		Messages    []json.RawMessage `json:"messages"`
		MaxTokens   *int              `json:"max_tokens"`
		Temperature *float64          `json:"temperature"`
		TopP        *float64          `json:"top_p"`
		Stop        json.RawMessage   `json:"stop"`
		Stream      bool              `json:"stream"`
		Tools       json.RawMessage   `json:"tools"`
		ToolChoice  json.RawMessage   `json:"tool_choice"`
	}
	if err := json.Unmarshal(body, &incoming); err != nil {
		return nil, 0, fmt.Errorf("provider: parsing request: %w", err)
	}

	system, messages, err := splitSystemMessages(incoming.Messages)
	if err != nil {
		return nil, 0, err
	}

	out := map[string]any{
		"model":    incoming.Model,
		"messages": messages,
	}
	if system != "" {
		// Anthropic carries the system prompt as a top-level field rather
		// than as a message with a role.
		out["system"] = system
	}

	// The Messages API rejects a request without max_tokens, so one has to
	// be supplied. This is the single deliberate exception to not injecting
	// a ceiling the client did not ask for, and it is reported back in a
	// response header rather than applied quietly. Using the same default
	// as the reservation estimate keeps the amount reserved and the ceiling
	// actually applied in agreement.
	injected := 0
	switch {
	case incoming.MaxTokens != nil:
		out["max_tokens"] = *incoming.MaxTokens
	default:
		out["max_tokens"] = defaultMaxTokens
		injected = defaultMaxTokens
	}

	if incoming.Temperature != nil {
		// OpenAI accepts 0-2 while Anthropic accepts 0-1. Rescaling would
		// change the meaning of the number; refusing says so.
		if *incoming.Temperature > 1 {
			return nil, 0, &ErrUnsupportedParameter{
				Parameter: "temperature above 1.0", Provider: "anthropic",
			}
		}
		out["temperature"] = *incoming.Temperature
	}
	if incoming.TopP != nil {
		out["top_p"] = *incoming.TopP
	}
	if len(incoming.Stop) > 0 {
		out["stop_sequences"] = stopSequences(incoming.Stop)
	}
	if incoming.Stream {
		out["stream"] = true
	}
	if len(incoming.Tools) > 0 {
		tools, err := translateTools(incoming.Tools)
		if err != nil {
			return nil, 0, err
		}
		out["tools"] = tools
	}

	encoded, err := json.Marshal(out)
	if err != nil {
		return nil, 0, fmt.Errorf("provider: encoding request: %w", err)
	}
	return encoded, injected, nil
}

// splitSystemMessages lifts system turns out of the message list.
func splitSystemMessages(messages []json.RawMessage) (string, []json.RawMessage, error) {
	var (
		system string
		rest   []json.RawMessage
	)
	for _, raw := range messages {
		var m struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		}
		if err := json.Unmarshal(raw, &m); err != nil {
			return "", nil, fmt.Errorf("provider: parsing message: %w", err)
		}
		if m.Role != "system" {
			rest = append(rest, raw)
			continue
		}
		var text string
		if err := json.Unmarshal(m.Content, &text); err != nil {
			return "", nil, &ErrUnsupportedParameter{
				Parameter: "non-text system message", Provider: "anthropic",
			}
		}
		if system != "" {
			system += "\n\n"
		}
		system += text
	}
	if rest == nil {
		rest = []json.RawMessage{}
	}
	return system, rest, nil
}

// translateTools converts OpenAI function definitions into Anthropic tools.
func translateTools(raw json.RawMessage) ([]map[string]any, error) {
	var tools []struct {
		Type     string `json:"type"`
		Function struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			Parameters  json.RawMessage `json:"parameters"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &tools); err != nil {
		return nil, fmt.Errorf("provider: parsing tools: %w", err)
	}

	out := make([]map[string]any, 0, len(tools))
	for _, t := range tools {
		if t.Type != "" && t.Type != "function" {
			return nil, &ErrUnsupportedParameter{
				Parameter: "tool type " + t.Type, Provider: "anthropic",
			}
		}
		tool := map[string]any{
			"name":        t.Function.Name,
			"description": t.Function.Description,
		}
		if len(t.Function.Parameters) > 0 {
			tool["input_schema"] = json.RawMessage(t.Function.Parameters)
		}
		out = append(out, tool)
	}
	return out, nil
}

func stopSequences(raw json.RawMessage) []string {
	var single string
	if err := json.Unmarshal(raw, &single); err == nil {
		return []string{single}
	}
	var many []string
	if err := json.Unmarshal(raw, &many); err == nil {
		return many
	}
	return nil
}

// TranslateAnthropicResponse converts a Messages response into the OpenAI
// shape.
func TranslateAnthropicResponse(raw []byte) (body []byte, servedModel, finishReason string, err error) {
	var incoming struct {
		ID      string `json:"id"`
		Model   string `json:"model"`
		Content []struct {
			Type  string          `json:"type"`
			Text  string          `json:"text"`
			ID    string          `json:"id"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
		Usage      struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &incoming); err != nil {
		return nil, "", "", fmt.Errorf("provider: parsing anthropic response: %w", err)
	}

	message := map[string]any{"role": "assistant"}
	var text string
	var toolCalls []map[string]any
	for _, block := range incoming.Content {
		switch block.Type {
		case "text":
			text += block.Text
		case "tool_use":
			arguments, _ := json.Marshal(block.Input)
			toolCalls = append(toolCalls, map[string]any{
				"id":   block.ID,
				"type": "function",
				"function": map[string]any{
					"name":      block.Name,
					"arguments": string(arguments),
				},
			})
		}
	}
	message["content"] = anyOrNil(text)
	if len(toolCalls) > 0 {
		message["tool_calls"] = toolCalls
	}

	finish := map[string]string{
		"end_turn":      "stop",
		"max_tokens":    "length",
		"stop_sequence": "stop",
		"tool_use":      "tool_calls",
	}[incoming.StopReason]
	if finish == "" {
		finish = "stop"
	}

	out := map[string]any{
		"id":      incoming.ID,
		"object":  "chat.completion",
		"model":   incoming.Model,
		"choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": finish}},
		"usage": map[string]any{
			"prompt_tokens":     incoming.Usage.InputTokens,
			"completion_tokens": incoming.Usage.OutputTokens,
			"total_tokens":      incoming.Usage.InputTokens + incoming.Usage.OutputTokens,
		},
	}
	encoded, err := json.Marshal(out)
	if err != nil {
		return nil, "", "", fmt.Errorf("provider: encoding response: %w", err)
	}
	return encoded, incoming.Model, finish, nil
}

func anyOrNil(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// Stream forwards a streaming request, translating events as they arrive.
//
// Anthropic's protocol differs from OpenAI's in shape as well as in names, so
// a translator carries the state needed to bridge them: the input token count
// arrives first and the output count last, and tool arguments stream as
// partial JSON under a content-block index that has to be remapped.
func (p *Anthropic) Stream(ctx context.Context, req Request) (*Stream, error) {
	translated, injected, err := TranslateAnthropicRequest(req.Body, p.opts.DefaultMaxTokens)
	if err != nil {
		return nil, err
	}
	translated, err = SetField(translated, "stream", true)
	if err != nil {
		return nil, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		p.opts.BaseURL+"/v1/messages", bytes.NewReader(translated))
	if err != nil {
		return nil, fmt.Errorf("provider: building request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-api-key", p.opts.APIKey.Expose())
	httpReq.Header.Set("anthropic-version", "2023-06-01")
	httpReq.Header.Set("Accept", "text/event-stream")
	for name, value := range req.PassthroughHeaders {
		httpReq.Header.Set(name, value)
	}

	resp, err := p.opts.Client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("provider: calling anthropic: %w", err)
	}

	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		return nil, &ErrUpstream{
			StatusCode: resp.StatusCode, Body: raw,
			RetryAfter: resp.Header.Get("Retry-After"), Native: false,
		}
	}

	translator := NewAnthropicStreamTranslator()
	return &Stream{
		Body:              resp.Body,
		StatusCode:        resp.StatusCode,
		Header:            resp.Header,
		ProviderRequestID: resp.Header.Get("Request-Id"),
		InjectedMaxTokens: injected,
		Translate:         translator.Translate,
		// Anthropic sends no usage chunk of its own, so one is assembled
		// from the counts gathered across the stream.
		TrailingChunks: func() [][]byte {
			return [][]byte{translator.UsageChunk()}
		},
		Usage: func() (Counts, bool) {
			input, cachedRead, cacheWrite, output, reported := translator.Usage()
			return Counts{
				"input":          int64(input),
				"cached_read":    int64(cachedRead),
				"cache_write_5m": int64(cacheWrite),
				"output":         int64(output),
			}, reported
		},
	}, nil
}
