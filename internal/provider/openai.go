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

// OpenAI speaks the dialect the gateway itself exposes, so a request reaches
// it essentially unchanged.
type OpenAI struct{ opts Options }

// NewOpenAI builds the adapter.
func NewOpenAI(opts Options) *OpenAI {
	if opts.BaseURL == "" {
		opts.BaseURL = "https://api.openai.com"
	}
	return &OpenAI{opts: opts}
}

// Name identifies this provider in the price table.
func (p *OpenAI) Name() string { return "openai" }

// Complete forwards a request and reads back the usage.
func (p *OpenAI) Complete(ctx context.Context, req Request) (*Response, error) {
	body := req.Body

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		p.opts.BaseURL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("provider: building request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+p.opts.APIKey.Expose())
	for name, value := range req.PassthroughHeaders {
		httpReq.Header.Set(name, value)
	}

	resp, err := p.opts.Client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("provider: calling openai: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("provider: reading openai response: %w", err)
	}

	requestID := resp.Header.Get("X-Request-Id")
	if resp.StatusCode >= 400 {
		return nil, &ErrUpstream{
			StatusCode: resp.StatusCode,
			Body:       raw,
			RetryAfter: resp.Header.Get("Retry-After"),
			// The body is already in the shape our clients expect, so it
			// travels unchanged rather than being re-described.
			Native: true,
		}
	}

	out := &Response{
		StatusCode:        resp.StatusCode,
		Body:              raw,
		ProviderRequestID: requestID,
	}

	normalised, err := pricing.NormaliseOpenAI(raw)
	if err != nil {
		// The call succeeded and consumed tokens, so a usage block we
		// cannot read is a degraded record, not a failed request.
		out.Degraded = true
		out.ParseErrors++
		out.Counts = pricing.Counts{}
	} else {
		out.Counts = normalised.Counts
		out.Degraded = normalised.Degraded
		out.ParseErrors = normalised.ParseErrors
	}

	var meta struct {
		Model   string `json:"model"`
		Choices []struct {
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &meta); err == nil {
		out.ServedModel = meta.Model
		if len(meta.Choices) > 0 {
			out.FinishReason = meta.Choices[0].FinishReason
		}
	}
	return out, nil
}
