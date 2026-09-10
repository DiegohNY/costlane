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

	// ClosedCleanly reports whether the stream ended the way this
	// provider's protocol says a complete one does.
	//
	// There is no single answer, which is why it is per adapter. OpenAI
	// closes with [DONE]. Anthropic closes with message_stop. Gemini closes
	// by ending the connection after a chunk carrying a finishReason — for
	// it, EOF is not an interruption but the ordinary end, and treating a
	// missing [DONE] as truncation would flag every healthy Gemini stream.
	// A nil value means "the pump's [DONE] check is the right test", which
	// is true for a same-dialect stream.
	ClosedCleanly func() bool

	// UsageIsCumulative reports that every chunk restates the running
	// totals rather than contributing a piece of them.
	//
	// Gemini does this, and it has a consequence worth the field: when a
	// client disconnects mid-stream, the last chunk already read carries
	// the provider's exact count up to that point. The accounting is then a
	// measurement rather than an estimate, and the usage record can say
	// "provider" honestly. For a provider that reports usage only at the
	// end, the same disconnect leaves nothing but a count of what went past.
	UsageIsCumulative bool
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
