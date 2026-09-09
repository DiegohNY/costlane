package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// PumpOptions configure one streamed response.
type PumpOptions struct {
	// ClientCtx is cancelled when the client goes away. It is watched
	// between chunks rather than waited on: Go's server cancels it from a
	// background read, so it fires without our writing anything.
	ClientCtx context.Context
	// CancelUpstream stops the provider call. Under the cancel policy it
	// is invoked the moment the client is seen to be gone.
	CancelUpstream context.CancelFunc

	// Drain keeps reading after the client leaves, to learn the exact
	// usage. It costs tokens nobody will read, so it is off by default.
	Drain        bool
	DrainTimeout time.Duration
	// DrainSlot is acquired before draining and released after. A client
	// that opens and abandons a thousand requests must not turn into a
	// thousand upstream streams.
	DrainSlot Semaphore

	// WriteTimeout bounds a single write to the client. A reader that
	// stops reading would otherwise hold a stream and a goroutine open
	// indefinitely.
	WriteTimeout time.Duration

	// StripUsageChunk removes the usage chunk when the client did not ask
	// for it. The injection is ours, so removing it restores exactly what
	// the client would have received.
	StripUsageChunk bool

	// ObserveUsage receives the usage payload of a same-dialect stream.
	// The gateway injects include_usage for its own accounting, so the
	// chunk has to be read on the way past whether or not it is then
	// stripped: it is the only place the provider's own figures appear.
	ObserveUsage func(payload []byte)

	// Translate converts a provider frame into zero or more chunks. Nil
	// means the dialect matches and frames pass through untouched.
	Translate func(event, data []byte) ([][]byte, error)
	// TrailingChunks are appended before [DONE], for a provider whose
	// dialect has no usage chunk of its own.
	TrailingChunks func() [][]byte

	Metrics StreamMetrics
	Model   string
}

// Semaphore bounds concurrent drains.
type Semaphore interface {
	Acquire(ctx context.Context) error
	Release()
}

// StreamMetrics receives what a stream did.
type StreamMetrics interface {
	Chunk(model string)
	TimeToFirstByte(model string, d time.Duration)
	ClientDisconnected(model, policy string)
	DrainStarted()
	DrainFinished()
	DrainWait(d time.Duration)
}

// PumpResult reports how a stream ended.
type PumpResult struct {
	Chunks             int
	ClientDisconnected bool
	Drained            bool
	DrainTimedOut      bool
	ParseErrors        int
	SawDone            bool
	TTFT               time.Duration
}

// Pump relays an SSE stream from a provider to a client.
//
// One goroutine reads a frame, counts it, and writes it in the same
// iteration. There is no channel between reading and writing, because a
// channel is a buffer and the requirement is that a chunk reaches the client
// as soon as it arrives.
//
// When the client goes away, the default is to cancel the upstream call
// rather than drain it. Providers stop generating on disconnect and bill only
// for what they produced; draining to learn an exact figure means paying for
// tokens nobody will read, which is the wrong trade for a product whose
// purpose is not overspending.
func Pump(w http.ResponseWriter, body io.Reader, opts PumpOptions) (PumpResult, error) {
	var result PumpResult

	rc := http.NewResponseController(w)
	if err := rc.Flush(); err != nil {
		// Without a working Flusher every chunk would be buffered and the
		// stream would arrive in one piece. Failing loudly beats degrading
		// silently, because the degradation is invisible in tests.
		return result, fmt.Errorf("proxy: response writer cannot flush: %w", err)
	}

	started := time.Now()
	reader := NewFrameReader(body)
	// writer is cleared once the client is gone: the handler may have
	// returned by then, and touching its ResponseWriter afterwards is a
	// race.
	writer := w

	for {
		// Check for a departed client between chunks rather than waiting
		// for a write to fail. The server cancels this context from a
		// background read, so it fires even while we are blocked upstream.
		if writer != nil && opts.ClientCtx != nil && opts.ClientCtx.Err() != nil {
			result.ClientDisconnected = true
			writer = nil
			if !opts.Drain {
				if opts.Metrics != nil {
					opts.Metrics.ClientDisconnected(opts.Model, "cancel")
				}
				if opts.CancelUpstream != nil {
					opts.CancelUpstream()
				}
				return result, nil
			}
			if opts.Metrics != nil {
				opts.Metrics.ClientDisconnected(opts.Model, "drain")
			}
			if !acquireDrainSlot(opts) {
				// No capacity to drain: stop rather than queue behind
				// other abandoned streams.
				if opts.CancelUpstream != nil {
					opts.CancelUpstream()
				}
				return result, nil
			}
			defer opts.DrainSlot.Release()
			result.Drained = true
			if opts.Metrics != nil {
				opts.Metrics.DrainStarted()
				defer opts.Metrics.DrainFinished()
			}
		}

		frame, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return result, fmt.Errorf("proxy: reading upstream stream: %w", err)
		}
		if frame.Oversized {
			result.ParseErrors++
		}
		if frame.IsDone() {
			result.SawDone = true
		}

		chunks, parseErr := renderFrame(frame, opts)
		if parseErr {
			result.ParseErrors++
		}

		for _, chunk := range chunks {
			result.Chunks++
			if opts.Metrics != nil {
				opts.Metrics.Chunk(opts.Model)
			}
			if writer == nil {
				continue
			}
			if result.TTFT == 0 {
				// Measured to the first byte written to the client, not
				// the first received from the provider: the difference is
				// our own overhead.
				result.TTFT = time.Since(started)
				if opts.Metrics != nil {
					opts.Metrics.TimeToFirstByte(opts.Model, result.TTFT)
				}
			}
			if err := writeChunk(rc, writer, chunk, opts.WriteTimeout); err != nil {
				// A failed write confirms what the context check may have
				// already told us, and covers the case of a reader that
				// stopped reading without closing.
				result.ClientDisconnected = true
				writer = nil
				if !opts.Drain {
					if opts.CancelUpstream != nil {
						opts.CancelUpstream()
					}
					return result, nil
				}
			}
		}

		if frame.IsDone() {
			break
		}
	}

	if writer != nil {
		writeTrailers(rc, writer, opts, &result)
	}
	return result, nil
}

// renderFrame turns a provider frame into the chunks to send, reporting
// whether it could not be parsed.
func renderFrame(frame Frame, opts PumpOptions) ([][]byte, bool) {
	if opts.Translate == nil {
		// Same dialect: the frame is relayed exactly as it arrived,
		// including one we cannot parse. Deciding on the client's behalf
		// that a chunk is unusable would make this gateway a breaking
		// point for every future format change.
		if isInjectedUsageChunk(frame.Data) {
			// Read it before deciding whether to forward it: this is the
			// only place the provider states its own token counts on a
			// same-dialect stream.
			if opts.ObserveUsage != nil {
				opts.ObserveUsage(frame.Data)
			}
			if opts.StripUsageChunk {
				return nil, false
			}
		}
		return [][]byte{frame.Raw}, false
	}

	if len(bytes.TrimSpace(frame.Data)) == 0 {
		return nil, false
	}
	chunks, err := opts.Translate(frame.Event, frame.Data)
	if err != nil {
		// A frame we cannot translate is forwarded raw rather than
		// dropped, and counted.
		return [][]byte{frame.Raw}, true
	}
	out := make([][]byte, 0, len(chunks))
	for _, c := range chunks {
		out = append(out, encodeSSE(c))
	}
	return out, false
}

func writeTrailers(rc *http.ResponseController, w http.ResponseWriter,
	opts PumpOptions, result *PumpResult) {
	if opts.TrailingChunks != nil {
		for _, chunk := range opts.TrailingChunks() {
			result.Chunks++
			_ = writeChunk(rc, w, encodeSSE(chunk), opts.WriteTimeout)
		}
	}
	if opts.Translate != nil && !result.SawDone {
		// A translated stream has no [DONE] of its own, and an OpenAI
		// client waits for one.
		_ = writeChunk(rc, w, []byte("data: [DONE]\n\n"), opts.WriteTimeout)
		result.SawDone = true
	}
}

func writeChunk(rc *http.ResponseController, w http.ResponseWriter,
	chunk []byte, timeout time.Duration) error {
	if timeout > 0 {
		// A deadline per write, never a WriteTimeout on the server: the
		// latter bounds the whole response and would cut every long stream
		// short.
		if err := rc.SetWriteDeadline(time.Now().Add(timeout)); err != nil &&
			!errors.Is(err, http.ErrNotSupported) {
			return err
		}
	}
	if _, err := w.Write(chunk); err != nil {
		return err
	}
	return rc.Flush()
}

func encodeSSE(payload []byte) []byte {
	out := make([]byte, 0, len(payload)+8)
	out = append(out, "data: "...)
	out = append(out, payload...)
	return append(out, '\n', '\n')
}

// isInjectedUsageChunk recognises the chunk the gateway asked for on the
// client's behalf.
//
// It matches on shape rather than position: a chunk with an empty choices
// array and a usage object is what include_usage adds. Stripping "the last
// chunk" would remove whatever happened to arrive last.
func isInjectedUsageChunk(data []byte) bool {
	var chunk struct {
		Choices []json.RawMessage `json:"choices"`
		Usage   json.RawMessage   `json:"usage"`
	}
	if err := json.Unmarshal(data, &chunk); err != nil {
		return false
	}
	return len(chunk.Choices) == 0 && len(chunk.Usage) > 0
}

func acquireDrainSlot(opts PumpOptions) bool {
	if opts.DrainSlot == nil {
		return true
	}
	timeout := opts.DrainTimeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	started := time.Now()
	err := opts.DrainSlot.Acquire(ctx)
	if opts.Metrics != nil {
		opts.Metrics.DrainWait(time.Since(started))
	}
	return err == nil
}
