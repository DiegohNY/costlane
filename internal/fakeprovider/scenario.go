// Package fakeprovider implements an LLM provider that speaks all three
// native dialects, for tests, benchmarks and local demonstration.
//
// It exists so the adapters are exercised against the shape a real provider
// returns rather than against our own idea of it, and so a reader can run the
// gateway without a credential or a bill.
package fakeprovider

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Scenario controls one response. Every field arrives as a request header, so
// a test states its intent in the request rather than by configuring a server
// instance.
type Scenario struct {
	// PromptTokens and CompletionTokens are reported verbatim in the usage
	// block, so a test asserting cost has exact numbers to expect rather
	// than a range.
	PromptTokens     int
	CompletionTokens int
	CachedTokens     int
	CacheWriteTokens int
	ReasoningTokens  int

	// ServedModel overrides the model echoed back, so the adapter's
	// handling of a provider serving something other than what was asked
	// can be exercised.
	ServedModel string

	// Status makes the provider fail with a given code.
	Status int
	// ErrorBody replaces the generated error body. A test uses it to plant
	// a credential and prove the gateway does not pass it on.
	ErrorBody string
	// RetryAfter is echoed in the Retry-After header, as a rate-limited
	// provider would.
	RetryAfter string

	// Latency delays the first byte.
	Latency time.Duration
	// ChunkDelay spaces streaming chunks apart.
	ChunkDelay time.Duration
	// FailAfterChunks cuts a stream short, without a terminating event.
	FailAfterChunks int
	// MalformedChunkAt emits an unparseable chunk at this index.
	MalformedChunkAt int
	// OmitUsage withholds the final usage chunk, as a provider that does
	// not support include_usage would.
	OmitUsage bool

	// RequestID is echoed as the provider's own request id.
	RequestID string
}

// Header names, all prefixed so they cannot collide with a real provider's.
//
//nolint:gosec // these are header names containing "Token", not credentials
const (
	HeaderPromptTokens     = "X-Fake-Prompt-Tokens"
	HeaderCompletionTokens = "X-Fake-Completion-Tokens"
	HeaderCachedTokens     = "X-Fake-Cached-Tokens"
	HeaderCacheWriteTokens = "X-Fake-Cache-Write-Tokens"
	HeaderReasoningTokens  = "X-Fake-Reasoning-Tokens"
	HeaderServedModel      = "X-Fake-Served-Model"
	HeaderStatus           = "X-Fake-Status"
	HeaderErrorBody        = "X-Fake-Error-Body"
	HeaderRetryAfter       = "X-Fake-Retry-After"
	HeaderLatency          = "X-Fake-Latency-Ms"
	HeaderChunkDelay       = "X-Fake-Chunk-Delay-Ms"
	HeaderFailAfterChunks  = "X-Fake-Fail-After-Chunks"
	HeaderMalformedChunkAt = "X-Fake-Malformed-Chunk-At"
	HeaderOmitUsage        = "X-Fake-Omit-Usage"
	HeaderRequestID        = "X-Fake-Request-Id"
)

// ScenarioFromRequest reads the scenario a caller asked for, falling back to
// a small successful response.
func ScenarioFromRequest(r *http.Request) Scenario {
	s := Scenario{
		PromptTokens:     intHeader(r, HeaderPromptTokens, 100),
		CompletionTokens: intHeader(r, HeaderCompletionTokens, 20),
		CachedTokens:     intHeader(r, HeaderCachedTokens, 0),
		CacheWriteTokens: intHeader(r, HeaderCacheWriteTokens, 0),
		ReasoningTokens:  intHeader(r, HeaderReasoningTokens, 0),
		ServedModel:      r.Header.Get(HeaderServedModel),
		Status:           intHeader(r, HeaderStatus, 0),
		ErrorBody:        r.Header.Get(HeaderErrorBody),
		RetryAfter:       r.Header.Get(HeaderRetryAfter),
		Latency:          msHeader(r, HeaderLatency),
		ChunkDelay:       msHeader(r, HeaderChunkDelay),
		FailAfterChunks:  intHeader(r, HeaderFailAfterChunks, 0),
		MalformedChunkAt: intHeader(r, HeaderMalformedChunkAt, -1),
		OmitUsage:        r.Header.Get(HeaderOmitUsage) != "",
		RequestID:        r.Header.Get(HeaderRequestID),
	}
	if s.RequestID == "" {
		s.RequestID = "fake-req-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return s
}

func intHeader(r *http.Request, name string, fallback int) int {
	raw := strings.TrimSpace(r.Header.Get(name))
	if raw == "" {
		return fallback
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	return n
}

func msHeader(r *http.Request, name string) time.Duration {
	return time.Duration(intHeader(r, name, 0)) * time.Millisecond
}
