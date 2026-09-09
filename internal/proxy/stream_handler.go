package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/DiegohNY/costlane/internal/api"
	"github.com/DiegohNY/costlane/internal/pricing"
	"github.com/DiegohNY/costlane/internal/provider"
	"github.com/DiegohNY/costlane/internal/store"
	"github.com/DiegohNY/costlane/internal/usage"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// serveStream relays a streaming completion.
//
// The upstream call runs on a context detached from the request's. When a
// client disconnects, Go cancels the request context, and inheriting it would
// tear down the provider call before the pump could decide what to do — which
// is the decision the disconnect policy exists to make.
func (h *Handler) serveStream(w http.ResponseWriter, r *http.Request, in streamInput) {
	streamer, ok := in.upstream.(provider.Streamer)
	if !ok {
		api.WriteError(w, http.StatusNotImplemented, api.ErrorTypeInvalidRequest,
			"this provider does not support streaming through costlane yet")
		h.releaseReservation(context.WithoutCancel(r.Context()), in.reservationID)
		return
	}

	upstreamCtx, cancelUpstream := context.WithTimeout(
		context.WithoutCancel(r.Context()), h.opts.ProviderTimeout)
	defer cancelUpstream()

	stream, err := streamer.Stream(upstreamCtx, provider.Request{
		Body: in.body, Model: in.bareModel, Stream: true, MaxTokens: in.maxTokens,
		PassthroughHeaders: h.passthroughHeaders(r),
	})
	if err != nil {
		h.releaseReservation(context.WithoutCancel(r.Context()), in.reservationID)
		h.writeUpstreamError(w, err, in.upstream.Name())
		return
	}
	defer func() { _ = stream.Body.Close() }()

	// Headers must go out before the first chunk, and no proxy in front of
	// us may buffer the response: without these a correct stream still
	// arrives in one piece.
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	if stream.InjectedMaxTokens > 0 {
		w.Header().Set(HeaderInjectedMaxTokens, itoa(stream.InjectedMaxTokens))
	}
	w.WriteHeader(http.StatusOK)

	// Captured from the stream as it passes: on a same-dialect stream the
	// usage chunk is the only statement of the provider's own counts.
	var observed pricing.Counts
	observeUsage := func(payload []byte) {
		if counts, err := pricing.NormaliseOpenAI(wrapUsagePayload(payload)); err == nil {
			observed = counts.Counts
		}
	}

	// Once 200 has gone out the status cannot change, so a failure from
	// here on is reported inside the stream itself.
	result, pumpErr := Pump(w, stream.Body, PumpOptions{
		ClientCtx:      r.Context(),
		CancelUpstream: cancelUpstream,
		Drain:          in.drain,
		DrainTimeout:   in.drainTimeout,
		DrainSlot:      h.drains,
		WriteTimeout:   h.opts.StreamWriteTimeout,
		// The include_usage injection is ours whenever the client did not
		// ask for it, so the chunk it produces is removed again.
		StripUsageChunk: !stream.ClientWantsUsage,
		ObserveUsage:    observeUsage,
		Translate:       stream.Translate,
		TrailingChunks:  h.trailingChunks(stream),
		Metrics:         h.opts.StreamMetrics,
		Model:           in.canonicalModel,
	})

	if pumpErr != nil && !result.ClientDisconnected {
		writeStreamError(w, pumpErr)
	}

	in.observedCounts = observed
	h.settleStream(r, in, stream, result, pumpErr)
}

// trailingChunks appends a usage chunk for a dialect that has none, and only
// when the client asked for usage.
func (h *Handler) trailingChunks(stream *provider.Stream) func() [][]byte {
	if stream.TrailingChunks == nil || !stream.ClientWantsUsage {
		return nil
	}
	return stream.TrailingChunks
}

type streamInput struct {
	body           []byte
	bareModel      string
	canonicalModel string
	maxTokens      int
	upstream       provider.Provider
	reservationID  uuid.UUID
	keyID          uuid.UUID
	windowStart    time.Time
	requestID      string
	requestedModel string
	drain          bool
	drainTimeout   time.Duration
	started        time.Time
	observedCounts pricing.Counts
}

// wrapUsagePayload presents a bare usage chunk to the normaliser, which
// expects the shape of a full response.
func wrapUsagePayload(chunk []byte) []byte {
	out := make([]byte, 0, len(chunk)+2)
	out = append(out, chunk...)
	return out
}

// settleStream closes the reservation and records what the stream consumed.
//
// A stream that ended early still consumed tokens, so it settles like any
// other. Where the provider's own usage never arrived, the count comes from
// what passed through, and the record says which — an estimate is never
// presented as a measurement.
func (h *Handler) settleStream(r *http.Request, in streamInput,
	stream *provider.Stream, result PumpResult, pumpErr error) {
	// The accounting outlives the request. By the time a stream ends the
	// client may be gone and its context cancelled, and settling on that
	// context would abort the transaction — leaving the reservation
	// pending and real spend missing from the budget. Settle already
	// detaches internally; the usage record has to do the same.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 30*time.Second)
	defer cancel()

	counts := pricing.Counts{}
	source := "provider"

	// A same-dialect stream states its usage in the chunk the pump saw.
	if len(in.observedCounts) > 0 {
		counts = in.observedCounts
	}

	if len(counts) == 0 && stream.Usage != nil {
		raw, reported := stream.Usage()
		for kind, n := range raw {
			counts[pricing.Kind(kind)] = n
		}
		if !reported {
			source = "estimate"
		}
	}
	if len(counts) == 0 {
		// Nothing usable came back: fall back to what crossed the wire.
		counts[pricing.KindOutput] = int64(result.Chunks)
		source = "estimate"
	}
	if result.ClientDisconnected && !result.Drained {
		// Cancelled mid-stream, so the provider's own total never arrived.
		source = "tokenizer"
	}

	table := h.opts.Pricing.Table()
	now := time.Now().UTC()
	served := stream.ServedModel
	if served == "" {
		served = in.canonicalModel
	}

	cost, costResult, _ := h.cost(table, served, in.upstream.Name(), now, counts)

	if _, err := h.opts.DB.Settle(ctx, store.SettleInput{
		ReservationID: in.reservationID, ActualUSD: cost,
	}); err != nil && h.opts.Logger != nil {
		h.opts.Logger.Error("settling a stream failed",
			"error", err, "request_id", in.requestID)
	}

	errorCode := ""
	if pumpErr != nil {
		errorCode = "stream_interrupted"
	} else if !result.SawDone {
		errorCode = "stream_truncated"
	}

	record := usage.Record{
		ID: uuid.New(), KeyID: in.keyID, ReservationID: &in.reservationID,
		RequestID: in.requestID, RequestedModel: in.requestedModel,
		ServedModel: served, Provider: in.upstream.Name(),
		ProviderRequestID: stream.ProviderRequestID, WindowStart: in.windowStart,
		TokenDetail: counts, UsageSource: source,
		Unpriced: costResult.Unpriced, PartiallyPriced: costResult.PartiallyPriced,
		Streamed:           true,
		ClientDisconnected: result.ClientDisconnected,
		DrainTimeout:       result.DrainTimedOut,
		ParseErrors:        result.ParseErrors,
		// The client saw a 200 before anything went wrong, so that is what
		// the record reports; error_code carries what actually happened.
		StatusCode: http.StatusOK,
		ErrorCode:  errorCode,
		LatencyMS:  int(time.Since(in.started).Milliseconds()),
		TTFTMicros: result.TTFT.Microseconds(),
	}
	if !costResult.Unpriced {
		c := cost
		record.CostUSD = &c
	}

	if err := h.recordTo(ctx, record); err != nil && h.opts.Logger != nil {
		h.opts.Logger.Error("writing a stream usage record failed",
			"error", err, "request_id", in.requestID)
	}
}

func (h *Handler) releaseReservation(ctx context.Context, id uuid.UUID) {
	if _, err := h.opts.DB.Settle(ctx, store.SettleInput{
		ReservationID: id, ActualUSD: decimal.Zero,
	}); err != nil && h.opts.Logger != nil {
		h.opts.Logger.Error("releasing a reservation failed", "error", err)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// writeStreamError reports a failure that happened after the headers went
// out.
//
// The status line is already 200 and cannot be revised, so the error travels
// as an SSE event in the same envelope a client's error handling already
// understands, followed by the [DONE] it is waiting for. The usage record
// keeps status 200 — what the client saw — with error_code carrying what
// actually happened.
func writeStreamError(w http.ResponseWriter, err error) {
	payload := map[string]any{
		"error": map[string]any{
			"message": "the upstream stream ended unexpectedly",
			"type":    api.ErrorTypeUpstreamTimeout,
			"code":    api.ErrorTypeUpstreamTimeout,
		},
	}
	encoded, marshalErr := json.Marshal(payload)
	if marshalErr != nil {
		return
	}

	rc := http.NewResponseController(w)
	_, _ = w.Write(encodeSSE(encoded))
	_, _ = w.Write([]byte("data: [DONE]\n\n"))
	_ = rc.Flush()
}
