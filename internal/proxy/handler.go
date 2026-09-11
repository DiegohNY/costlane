// Package proxy forwards chat completions and counts tokens in flight.
package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/DiegohNY/costlane/internal/api"
	"github.com/DiegohNY/costlane/internal/auth"
	"github.com/DiegohNY/costlane/internal/obs"
	"github.com/DiegohNY/costlane/internal/pricing"
	"github.com/DiegohNY/costlane/internal/provider"
	"github.com/DiegohNY/costlane/internal/store"
	"github.com/DiegohNY/costlane/internal/usage"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// Response headers the gateway adds. The two cost headers are the feature a
// customer notices first: the price of a call is visible in the reply
// itself, without opening a dashboard or querying an API.
//
//nolint:gosec // header names, not credentials
const (
	HeaderRequestID         = "X-Costlane-Request-Id"
	HeaderCostUSD           = "X-Costlane-Cost-Usd"
	HeaderBudgetRemaining   = "X-Costlane-Budget-Remaining-Usd"
	HeaderInjectedMaxTokens = "X-Costlane-Injected-Max-Tokens"
)

// Options configure the proxy.
type Options struct {
	DB      *store.DB
	Router  *provider.Router
	Pricing *pricing.Snapshot
	Logger  Logger
	Metrics Metrics

	MaxBodyBytes     int64
	DefaultMaxTokens int

	// Redactor removes this process's own credentials from text it did not
	// write. The pattern list catches known formats; this catches the rest.
	Redactor *obs.Redactor

	// Usage takes records off the request path. Nil writes them
	// synchronously, which is what the tests do.
	Usage UsageSink

	// LogPrompts stores request and response bodies alongside a usage
	// record. It is off by default: a prompt is a copy of a customer's
	// data, and it should exist only while someone is debugging.
	LogPrompts bool

	ProviderTimeout     time.Duration
	DrainTimeout        time.Duration
	StreamWriteTimeout  time.Duration
	MaxConcurrentDrains int
	StreamMetrics       StreamMetrics

	// PassthroughHeaderPrefixes names request headers that may be relayed
	// upstream. It is empty in production; tests set it so a fake provider
	// can be driven end to end.
	PassthroughHeaderPrefixes []string
	TierGuard                 float64
	ReservationTTL            time.Duration
}

// UsageSink accepts a finished record.
type UsageSink interface {
	Add(ctx context.Context, record usage.Record) error
}

// Logger is the subset of structured logging the proxy needs.
type Logger interface {
	Error(msg string, args ...any)
	Warn(msg string, args ...any)
}

// Metrics receives what a request did.
//
// Token counts arrive as a plain map rather than a pricing.Counts, so the
// metrics package does not have to know about pricing: an observer should
// depend on as little of the thing it observes as possible.
type Metrics interface {
	RequestCompleted(model, providerName, status string, d time.Duration)
	TokensCounted(model string, counts map[string]int64)
	Unpriced(model string)
	ModelMismatch(requested, served string)
}

// Handler serves chat completions.
type Handler struct {
	opts   Options
	drains ChannelSemaphore
}

// New builds the proxy handler.
func New(opts Options) *Handler {
	if opts.MaxBodyBytes == 0 {
		opts.MaxBodyBytes = 10 << 20
	}
	if opts.DefaultMaxTokens == 0 {
		opts.DefaultMaxTokens = 4096
	}
	if opts.ReservationTTL == 0 {
		opts.ReservationTTL = 6 * time.Minute
	}
	if opts.ProviderTimeout == 0 {
		opts.ProviderTimeout = 5 * time.Minute
	}
	if opts.StreamWriteTimeout == 0 {
		opts.StreamWriteTimeout = 30 * time.Second
	}
	if opts.MaxConcurrentDrains == 0 {
		opts.MaxConcurrentDrains = 64
	}
	return &Handler{opts: opts, drains: NewSemaphore(opts.MaxConcurrentDrains)}
}

// drainTimeout prefers a per-key setting over the global default.
func drainTimeout(perKeyMS *int, fallback time.Duration) time.Duration {
	if perKeyMS != nil && *perKeyMS > 0 {
		return time.Duration(*perKeyMS) * time.Millisecond
	}
	if fallback > 0 {
		return fallback
	}
	return 60 * time.Second
}

// request is the little the proxy needs to read from a body it otherwise
// forwards untouched.
type request struct {
	Model     string `json:"model"`
	Stream    bool   `json:"stream"`
	MaxTokens int    `json:"max_tokens"`
}

// ServeHTTP handles POST /v1/chat/completions.
//
// Everything that can fail without touching the database happens first: the
// body is read and parsed, the model is resolved and priced, and the context
// tier is chosen. A reservation taken and then released because the body was
// malformed is a wasted round trip on the critical path and a spurious row in
// the audit trail.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	requestID := uuid.NewString()
	w.Header().Set(HeaderRequestID, requestID)

	token, ok := api.BearerToken(r)
	if !ok {
		api.WriteError(w, http.StatusUnauthorized, api.ErrorTypeInvalidAPIKey,
			"a credential is required in the Authorization header")
		return
	}

	body, err := readBody(w, r, h.opts.MaxBodyBytes)
	if err != nil {
		api.WriteError(w, http.StatusBadRequest, api.ErrorTypeInvalidRequest,
			"the request body could not be read")
		return
	}

	var req request
	if err := json.Unmarshal(body, &req); err != nil {
		api.WriteError(w, http.StatusBadRequest, api.ErrorTypeInvalidRequest,
			"the request body is not valid JSON")
		return
	}
	if req.Model == "" {
		api.WriteError(w, http.StatusBadRequest, api.ErrorTypeInvalidRequest,
			"model is required")
		return
	}
	table := h.opts.Pricing.Table()
	now := time.Now().UTC()

	upstream, bareModel, err := h.opts.Router.Route(req.Model)
	if err != nil {
		api.WriteError(w, http.StatusNotFound, api.ErrorTypeModelNotFound,
			fmt.Sprintf("no provider serves model %q", req.Model))
		return
	}

	canonical := table.Resolve(bareModel, upstream.Name(), now)

	estimate, estimateResult, err := table.Estimate(pricing.EstimateRequest{
		Model: canonical, Provider: upstream.Name(), Tier: pricing.TierStandard,
		At: now, PromptBytes: len(body), MaxTokens: req.MaxTokens,
		DefaultMaxTokens: h.opts.DefaultMaxTokens, GuardFactor: h.opts.TierGuard,
	})
	if err != nil {
		api.WriteError(w, http.StatusInternalServerError, api.ErrorTypeInternal,
			"the request could not be priced")
		return
	}

	keyHash := auth.Hash(token)

	// Reserve-and-settle presupposes a price. An unpriced model estimates
	// at zero, so a reservation for it would succeed against any budget and
	// let the spend through untracked — which is the one outcome a
	// spend-control gateway must not produce. The check happens before the
	// reserve, because by the time a zero reservation succeeds there is
	// nothing left to refuse.
	if estimateResult.Unpriced {
		budgeted, err := h.opts.DB.KeyHasBudget(r.Context(), keyHash)
		if err != nil {
			api.WriteReserveError(w, err)
			return
		}
		if budgeted {
			api.WriteError(w, http.StatusBadRequest, api.ErrorTypeModelNotPriced,
				fmt.Sprintf("model %q has no active price, and this key has a budget: "+
					"costlane refuses rather than letting untracked spend through", req.Model))
			return
		}
	}

	res, err := h.opts.DB.Reserve(r.Context(), store.ReserveInput{
		KeyHash:      keyHash,
		Model:        req.Model,
		EstimatedUSD: estimate,
		TTL:          h.opts.ReservationTTL,
	})
	if err != nil {
		api.WriteReserveError(w, err)
		return
	}

	// The settle must run on every path, including a panic in the handler:
	// a reservation that is never closed holds budget until the reaper.
	var settled bool
	defer func() {
		if !settled {
			if _, err := h.opts.DB.Settle(r.Context(), store.SettleInput{
				ReservationID: res.ReservationID,
				ActualUSD:     decimal.Zero,
			}); err != nil && h.opts.Logger != nil {
				h.opts.Logger.Error("releasing an abandoned reservation failed",
					"error", err, "request_id", requestID)
			}
		}
	}()

	if req.Stream {
		h.serveStream(w, r, streamInput{
			body: body, bareModel: bareModel, canonicalModel: canonical,
			maxTokens: req.MaxTokens, upstream: upstream,
			reservationID: res.ReservationID, keyID: res.KeyID,
			windowStart: res.WindowStart, requestID: requestID,
			requestedModel: req.Model,
			drain:          res.DisconnectPolicy == "drain",
			drainTimeout:   drainTimeout(res.DrainTimeoutMS, h.opts.DrainTimeout),
			started:        started,
		})
		settled = true
		return
	}

	out, err := upstream.Complete(r.Context(), provider.Request{
		Body: body, Model: bareModel, Stream: false, MaxTokens: req.MaxTokens,
		PassthroughHeaders: h.passthroughHeaders(r),
	})
	if err != nil {
		settled = true
		h.settleFailure(r, res.ReservationID)
		h.writeUpstreamError(w, err, upstream.Name())
		return
	}

	// Cost is computed from what the provider says it served, not from what
	// was asked for.
	servedModel := out.ServedModel
	if servedModel == "" {
		servedModel = canonical
	}
	if servedModel != canonical && h.opts.Metrics != nil {
		h.opts.Metrics.ModelMismatch(canonical, servedModel)
	}

	cost, costResult, err := h.cost(table, servedModel, upstream.Name(), now, out.Counts)
	if err != nil && h.opts.Logger != nil {
		h.opts.Logger.Warn("pricing the response failed",
			"error", err, "request_id", requestID, "model", servedModel)
	}

	settled = true
	if _, err := h.opts.DB.Settle(r.Context(), store.SettleInput{
		ReservationID: res.ReservationID, ActualUSD: cost,
	}); err != nil && h.opts.Logger != nil {
		h.opts.Logger.Error("settling failed", "error", err, "request_id", requestID)
	}

	h.recordUsage(r.Context(), usageInput{
		keyID: res.KeyID, reservationID: res.ReservationID, requestID: requestID,
		requestedModel: req.Model, servedModel: servedModel,
		provider: upstream.Name(), windowStart: res.WindowStart,
		counts: out.Counts, cost: cost, result: costResult,
		providerRequestID: out.ProviderRequestID, finishReason: out.FinishReason,
		statusCode: out.StatusCode, parseErrors: out.ParseErrors,
		degraded: out.Degraded, latency: time.Since(started),
		requestBody: body, responseBody: out.Body,
	})

	h.writeResponse(r.Context(), w, out, cost, costResult, res.KeyID)

	if h.opts.Metrics != nil {
		h.opts.Metrics.RequestCompleted(servedModel, upstream.Name(), "200", time.Since(started))
		h.opts.Metrics.TokensCounted(servedModel, countsAsMap(out.Counts))
		if costResult.Unpriced {
			h.opts.Metrics.Unpriced(servedModel)
		}
	}
}

// passthroughHeaders selects the request headers that may reach an upstream.
//
// The allowlist is short by design. Forwarding the client's headers wholesale
// would send its Authorization header — the virtual key — to the provider,
// and would let a caller set headers on our account.
func (h *Handler) passthroughHeaders(r *http.Request) map[string]string {
	if len(h.opts.PassthroughHeaderPrefixes) == 0 {
		return nil
	}
	out := map[string]string{}
	for name, values := range r.Header {
		for _, prefix := range h.opts.PassthroughHeaderPrefixes {
			if len(values) > 0 && strings.HasPrefix(strings.ToLower(name), strings.ToLower(prefix)) {
				out[name] = values[0]
			}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

type usageInput struct {
	keyID             uuid.UUID
	reservationID     uuid.UUID
	requestID         string
	requestedModel    string
	servedModel       string
	provider          string
	windowStart       time.Time
	counts            pricing.Counts
	cost              decimal.Decimal
	result            pricing.Result
	providerRequestID string
	finishReason      string
	statusCode        int
	parseErrors       int
	degraded          bool
	latency           time.Duration
	requestBody       []byte
	responseBody      []byte
}

// recordUsage writes the accounting for a completed request.
//
// A failure here is logged rather than returned: the request succeeded and
// the budget has already moved, so failing the response now would report a
// problem the client cannot act on while hiding one it already paid for.
// Reconciliation counts what this loses.
func (h *Handler) recordUsage(ctx context.Context, in usageInput) {
	source := "provider"
	if in.degraded {
		source = "estimate"
	}

	record := usage.Record{
		ID: uuid.New(), KeyID: in.keyID, ReservationID: &in.reservationID,
		RequestID: in.requestID, RequestedModel: in.requestedModel,
		ServedModel: in.servedModel, Provider: in.provider,
		ProviderRequestID: in.providerRequestID, WindowStart: in.windowStart,
		TokenDetail: in.counts, UsageSource: source,
		Unpriced: in.result.Unpriced, PartiallyPriced: in.result.PartiallyPriced,
		FinishReason: in.finishReason, StatusCode: in.statusCode,
		ParseErrors: in.parseErrors,
		LatencyMS:   int(in.latency.Milliseconds()),
	}
	// An unpriced request has no cost: NULL means unknown, never free.
	if !in.result.Unpriced {
		cost := in.cost
		record.CostUSD = &cost
	}

	// Detached: a client that disconnected after the provider replied has
	// still consumed tokens, and its record must survive the cancellation.
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()

	if err := h.recordTo(writeCtx, record); err != nil && h.opts.Logger != nil {
		h.opts.Logger.Error("writing the usage record failed",
			"error", err, "request_id", in.requestID)
	}

	h.storePayload(writeCtx, record.ID, in.requestBody, in.responseBody)
}

// storePayload keeps the bodies when prompt logging is enabled.
//
// They go to a table of their own so that it can be dropped, or given a far
// shorter retention, without touching the accounting — and a failure here is
// logged rather than returned, because losing a debugging aid must not fail a
// request that already succeeded.
func (h *Handler) storePayload(ctx context.Context, recordID uuid.UUID,
	request, response []byte) {
	if !h.opts.LogPrompts || len(request) == 0 {
		return
	}
	if err := h.opts.DB.WritePayload(ctx, recordID, request, response); err != nil &&
		h.opts.Logger != nil {
		h.opts.Logger.Error("storing a request payload failed", "error", err)
	}
}

// recordTo hands a record to the buffer, or writes it directly when there is
// none.
func (h *Handler) recordTo(ctx context.Context, record usage.Record) error {
	if h.opts.Usage != nil {
		return h.opts.Usage.Add(ctx, record)
	}
	return h.opts.DB.WriteUsageRecord(ctx, record)
}

// countsAsMap converts billing classes to plain strings for an observer.
func countsAsMap(counts pricing.Counts) map[string]int64 {
	out := make(map[string]int64, len(counts))
	for kind, n := range counts {
		out[string(kind)] = n
	}
	return out
}

func (h *Handler) cost(table *pricing.Table, model, providerName string,
	now time.Time, counts pricing.Counts) (decimal.Decimal, pricing.Result, error) {
	result, err := table.Cost(pricing.Request{
		Model: model, Provider: providerName, Tier: pricing.TierStandard,
		At: now, Counts: counts,
	})
	if err != nil {
		return decimal.Zero, pricing.Result{}, err
	}
	return result.Cost, result, nil
}

// settleFailure closes a reservation for a request that never produced
// billable usage.
func (h *Handler) settleFailure(r *http.Request, reservationID uuid.UUID) {
	if _, err := h.opts.DB.Settle(r.Context(), store.SettleInput{
		ReservationID: reservationID, ActualUSD: decimal.Zero,
	}); err != nil && h.opts.Logger != nil {
		h.opts.Logger.Error("settling a failed request", "error", err)
	}
}

func (h *Handler) writeResponse(ctx context.Context, w http.ResponseWriter,
	out *provider.Response, cost decimal.Decimal, result pricing.Result,
	keyID uuid.UUID) {
	if !result.Unpriced {
		w.Header().Set(HeaderCostUSD, cost.String())
	}
	if out.InjectedMaxTokens > 0 {
		// The client did not ask for this ceiling, so it is told that one
		// was applied rather than left to wonder why a response stopped.
		w.Header().Set(HeaderInjectedMaxTokens, fmt.Sprintf("%d", out.InjectedMaxTokens))
	}
	if remaining, ok := h.remainingBudget(ctx, keyID); ok {
		w.Header().Set(HeaderBudgetRemaining, remaining)
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(out.StatusCode)
	_, _ = w.Write(out.Body)
}

func (h *Handler) remainingBudget(ctx context.Context, keyID uuid.UUID) (string, bool) {
	remaining, err := h.opts.DB.RemainingBudget(ctx, keyID)
	if err != nil || remaining == nil {
		return "", false
	}
	return remaining.String(), true
}

// writeUpstreamError answers a provider failure.
//
// A body already in the OpenAI shape travels unchanged, so a client's own
// error handling keeps working. Everything else is re-described. In both
// cases the body passes through redaction first: a provider echoing our
// credential back inside an error message is a real occurrence, not a
// hypothetical one.
func (h *Handler) writeUpstreamError(w http.ResponseWriter, err error, providerName string) {
	if unsupported, ok := provider.AsUnsupported(err); ok {
		message := fmt.Sprintf("%q is not supported by %s; costlane refuses rather than "+
			"silently dropping it", unsupported.Parameter, providerName)
		if unsupported.Detail != "" {
			// The parameter is supported and the value is not, which is a
			// different thing to be told: the caller needs the reason and
			// the levels this model does have, not just a name.
			message = fmt.Sprintf("%q is not supported by %s as sent: %s",
				unsupported.Parameter, providerName, unsupported.Detail)
		}
		api.WriteError(w, http.StatusBadRequest, "unsupported_parameter", message)
		return
	}

	upstream, ok := provider.AsUpstream(err)
	if !ok {
		api.WriteError(w, http.StatusBadGateway, api.ErrorTypeUpstreamTimeout,
			"the upstream provider could not be reached")
		return
	}

	if upstream.RetryAfter != "" {
		// The client's SDK knows what to do with this; withholding it
		// would make a retryable failure look permanent.
		w.Header().Set("Retry-After", upstream.RetryAfter)
	}

	body := h.opts.Redactor.Redact(upstream.Body)
	if upstream.Native {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(upstream.StatusCode)
		_, _ = w.Write(body)
		return
	}

	api.WriteError(w, upstream.StatusCode, upstreamErrorType(upstream.StatusCode),
		fmt.Sprintf("the %s API returned %d", providerName, upstream.StatusCode))
}

func upstreamErrorType(status int) string {
	switch {
	case status == http.StatusTooManyRequests:
		return "rate_limit_error"
	case status >= 500:
		return "upstream_error"
	default:
		return api.ErrorTypeInvalidRequest
	}
}
