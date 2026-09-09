// Package provider defines the upstream LLM provider interface.
package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/DiegohNY/costlane/internal/obs"
	"github.com/DiegohNY/costlane/internal/pricing"
)

// ErrUnsupportedParameter reports a request field the destination provider
// has no equivalent for.
//
// Dropping it silently would change the meaning of the request without
// telling anyone: a client asking for logprobs and receiving a response
// without them has no way to know the gateway discarded the request.
type ErrUnsupportedParameter struct {
	Parameter string
	Provider  string
}

func (e *ErrUnsupportedParameter) Error() string {
	return fmt.Sprintf("provider: %q is not supported by %s", e.Parameter, e.Provider)
}

// ErrUpstream carries a provider failure with everything needed to answer
// the client.
type ErrUpstream struct {
	StatusCode int
	Body       []byte
	RetryAfter string
	// Native reports whether the body is already in the OpenAI shape and
	// can be passed through unchanged.
	Native bool
}

func (e *ErrUpstream) Error() string {
	return fmt.Sprintf("provider: upstream returned %d", e.StatusCode)
}

// Request is a chat completion in the OpenAI dialect, as it arrived.
//
// Body is kept as raw bytes rather than a parsed struct: a request that goes
// to an OpenAI-compatible endpoint should reach it byte for byte, including
// fields this gateway has never heard of. Parsing and re-serialising would
// silently drop them and reorder the rest.
type Request struct {
	Body   []byte
	Model  string
	Stream bool

	// MaxTokens is the client's own ceiling, zero when unset.
	MaxTokens int

	// PassthroughHeaders are added to the upstream call verbatim.
	//
	// Client headers are deliberately NOT forwarded wholesale: doing so
	// would send the caller's own Authorization header to the provider.
	// This carries the few that are safe and useful — today the fake
	// provider's scenario controls, which is what lets a test drive an
	// upstream failure end to end.
	PassthroughHeaders map[string]string
}

// Response is a completed non-streaming call.
type Response struct {
	StatusCode  int
	Body        []byte
	ServedModel string
	// ProviderRequestID is the upstream's own identifier: the only key for
	// disputing an invoice or opening a support ticket, so it is captured
	// even when the call fails.
	ProviderRequestID string
	Counts            pricing.Counts
	FinishReason      string
	Degraded          bool
	ParseErrors       int

	// InjectedMaxTokens records a ceiling the gateway had to supply
	// because the provider requires one. It is surfaced to the client
	// rather than applied quietly.
	InjectedMaxTokens int
}

// Provider talks to one upstream in its own dialect.
//
// The interface is the extension point for the multi-provider failover that
// v1 excludes: a router that today maps a model to one provider can later try
// several without the proxy knowing.
type Provider interface {
	// Name is the identifier used in the price table.
	Name() string
	// Complete performs a non-streaming call.
	Complete(ctx context.Context, req Request) (*Response, error)
}

// Options configure a provider client.
type Options struct {
	BaseURL string
	APIKey  obs.Secret
	Client  *http.Client

	// DefaultMaxTokens is injected where a provider requires the field and
	// the client did not set one. It matches the reservation estimate, so
	// the amount reserved and the ceiling actually applied agree by
	// construction.
	DefaultMaxTokens int
}

// NewHTTPClient builds the client used for upstream calls.
//
// http.DefaultClient would be wrong here in a way that only shows up under
// load: it caps idle connections per host at two, so a busy gateway
// renegotiates TLS constantly. That handshake would be a large share of the
// overhead this product measures and advertises.
func NewHTTPClient(totalTimeout, connectTimeout time.Duration) *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConns = 200
	transport.MaxIdleConnsPerHost = 100
	transport.MaxConnsPerHost = 0
	transport.IdleConnTimeout = 90 * time.Second
	transport.ForceAttemptHTTP2 = true
	transport.TLSHandshakeTimeout = connectTimeout
	transport.ExpectContinueTimeout = time.Second

	return &http.Client{
		Transport: transport,
		Timeout:   totalTimeout,
		// Redirects are not followed: an upstream redirecting our
		// credential to another host is not something to do quietly.
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// AsUnsupported reports whether err is an unsupported-parameter error.
func AsUnsupported(err error) (*ErrUnsupportedParameter, bool) {
	var target *ErrUnsupportedParameter
	ok := errors.As(err, &target)
	return target, ok
}

// AsUpstream reports whether err carries an upstream failure.
func AsUpstream(err error) (*ErrUpstream, bool) {
	var target *ErrUpstream
	ok := errors.As(err, &target)
	return target, ok
}
