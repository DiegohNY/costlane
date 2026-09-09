package provider

import (
	"context"
	"io"
	"net/http"
)

// Stream is a live response from a provider.
type Stream struct {
	Body       io.ReadCloser
	StatusCode int
	Header     http.Header

	ServedModel       string
	ProviderRequestID string

	// Translate converts one provider frame into chunks, or is nil when
	// the dialect already matches.
	Translate func(event, data []byte) ([][]byte, error)
	// TrailingChunks appends what the provider's dialect does not send,
	// such as a usage chunk for Anthropic.
	TrailingChunks func() [][]byte
	// Usage reports the counts gathered while the stream ran.
	Usage func() (Counts, bool)

	// InjectedMaxTokens records a ceiling supplied because the provider
	// requires one.
	InjectedMaxTokens int

	// ClientWantsUsage reports whether the client asked for the usage
	// chunk. When it did not, the gateway strips the chunk it injected on
	// the client's behalf.
	ClientWantsUsage bool
}

// Counts mirrors pricing.Counts without importing it, so the provider
// package stays free of a pricing dependency.
type Counts map[string]int64

// Streamer is a provider that can stream.
//
// It is separate from Provider so an adapter can exist before it streams,
// and so the router can report plainly that a model cannot be streamed
// rather than failing halfway through one.
type Streamer interface {
	Provider
	Stream(ctx context.Context, req Request) (*Stream, error)
}
