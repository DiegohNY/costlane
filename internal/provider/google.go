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

// Google translates the OpenAI dialect into generateContent.
type Google struct{ opts Options }

// NewGoogle builds the adapter.
func NewGoogle(opts Options) *Google {
	if opts.BaseURL == "" {
		opts.BaseURL = "https://generativelanguage.googleapis.com"
	}
	return &Google{opts: opts}
}

// Name identifies this provider in the price table.
func (p *Google) Name() string { return "google" }

// googleUnsupported lists OpenAI parameters generateContent has no
// equivalent for. As with Anthropic they are refused rather than dropped.
var googleUnsupported = []string{
	"logprobs", "top_logprobs", "n", "presence_penalty", "frequency_penalty",
	"logit_bias", "seed", "modalities", "audio",
}

// Complete translates, forwards, and translates back.
func (p *Google) Complete(ctx context.Context, req Request) (*Response, error) {
	translated, err := TranslateGoogleRequest(req.Body)
	if err != nil {
		return nil, err
	}

	// Gemini puts the model and the method in the path rather than the body.
	url := fmt.Sprintf("%s/v1beta/models/%s:generateContent", p.opts.BaseURL, req.Model)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url,
		bytes.NewReader(translated))
	if err != nil {
		return nil, fmt.Errorf("provider: building request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-goog-api-key", p.opts.APIKey.Expose())
	for name, value := range req.PassthroughHeaders {
		httpReq.Header.Set(name, value)
	}

	resp, err := p.opts.Client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("provider: calling google: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("provider: reading google response: %w", err)
	}

	if resp.StatusCode >= 400 {
		return nil, &ErrUpstream{
			StatusCode: resp.StatusCode,
			Body:       raw,
			RetryAfter: resp.Header.Get("Retry-After"),
			Native:     false,
		}
	}

	out := &Response{
		StatusCode:        resp.StatusCode,
		ProviderRequestID: resp.Header.Get("X-Request-Id"),
	}

	normalised, err := pricing.NormaliseGoogle(raw)
	if err != nil {
		out.Degraded = true
		out.ParseErrors++
		out.Counts = pricing.Counts{}
	} else {
		out.Counts = normalised.Counts
		out.Degraded = normalised.Degraded
		out.ParseErrors = normalised.ParseErrors
	}

	body, served, finish, err := TranslateGoogleResponse(raw)
	if err != nil {
		return nil, err
	}
	out.Body = body
	out.ServedModel = served
	out.FinishReason = finish
	return out, nil
}

// TranslateGoogleRequest converts a chat completion into generateContent.
func TranslateGoogleRequest(body []byte) ([]byte, error) {
	names, err := FieldNames(body)
	if err != nil {
		return nil, err
	}
	for _, name := range names {
		for _, unsupported := range googleUnsupported {
			if name == unsupported {
				return nil, &ErrUnsupportedParameter{Parameter: name, Provider: "google"}
			}
		}
	}

	var incoming struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
		MaxTokens   *int            `json:"max_tokens"`
		Temperature *float64        `json:"temperature"`
		TopP        *float64        `json:"top_p"`
		Stop        json.RawMessage `json:"stop"`
	}
	if err := json.Unmarshal(body, &incoming); err != nil {
		return nil, fmt.Errorf("provider: parsing request: %w", err)
	}

	var (
		contents          []map[string]any
		systemInstruction map[string]any
	)
	for _, m := range incoming.Messages {
		part := map[string]any{"text": m.Content}
		if m.Role == "system" {
			// Gemini carries system text in its own field, like Anthropic.
			systemInstruction = map[string]any{"parts": []any{part}}
			continue
		}
		// Gemini names the assistant role "model".
		role := m.Role
		if role == "assistant" {
			role = "model"
		}
		contents = append(contents, map[string]any{"role": role, "parts": []any{part}})
	}
	if contents == nil {
		contents = []map[string]any{}
	}

	out := map[string]any{"contents": contents}
	if systemInstruction != nil {
		out["systemInstruction"] = systemInstruction
	}

	generation := map[string]any{}
	if incoming.MaxTokens != nil {
		generation["maxOutputTokens"] = *incoming.MaxTokens
	}
	if incoming.Temperature != nil {
		generation["temperature"] = *incoming.Temperature
	}
	if incoming.TopP != nil {
		generation["topP"] = *incoming.TopP
	}
	if len(incoming.Stop) > 0 {
		generation["stopSequences"] = stopSequences(incoming.Stop)
	}
	if len(generation) > 0 {
		out["generationConfig"] = generation
	}

	encoded, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("provider: encoding request: %w", err)
	}
	return encoded, nil
}

// TranslateGoogleResponse converts generateContent output into the OpenAI
// shape.
func TranslateGoogleResponse(raw []byte) (body []byte, servedModel, finishReason string, err error) {
	var incoming struct {
		ModelVersion string `json:"modelVersion"`
		Candidates   []struct {
			Content struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"content"`
			FinishReason string `json:"finishReason"`
		} `json:"candidates"`
		UsageMetadata struct {
			PromptTokenCount     int `json:"promptTokenCount"`
			CandidatesTokenCount int `json:"candidatesTokenCount"`
			ThoughtsTokenCount   int `json:"thoughtsTokenCount"`
		} `json:"usageMetadata"`
	}
	if err := json.Unmarshal(raw, &incoming); err != nil {
		return nil, "", "", fmt.Errorf("provider: parsing google response: %w", err)
	}

	var text string
	finish := "stop"
	if len(incoming.Candidates) > 0 {
		for _, part := range incoming.Candidates[0].Content.Parts {
			text += part.Text
		}
		finish = map[string]string{
			"STOP":       "stop",
			"MAX_TOKENS": "length",
			"SAFETY":     "content_filter",
			"RECITATION": "content_filter",
		}[incoming.Candidates[0].FinishReason]
		if finish == "" {
			finish = "stop"
		}
	}

	// Thoughts are billed as output and reported separately, so the
	// completion total the client sees includes them.
	completion := incoming.UsageMetadata.CandidatesTokenCount + incoming.UsageMetadata.ThoughtsTokenCount

	out := map[string]any{
		"object": "chat.completion",
		"model":  incoming.ModelVersion,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       map[string]any{"role": "assistant", "content": text},
			"finish_reason": finish,
		}},
		"usage": map[string]any{
			"prompt_tokens":     incoming.UsageMetadata.PromptTokenCount,
			"completion_tokens": completion,
			"total_tokens":      incoming.UsageMetadata.PromptTokenCount + completion,
		},
	}
	encoded, err := json.Marshal(out)
	if err != nil {
		return nil, "", "", fmt.Errorf("provider: encoding response: %w", err)
	}
	return encoded, incoming.ModelVersion, finish, nil
}
