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

	// PassthroughHeaderPrefixes names request headers that may be relayed
	// upstream. It is empty in production; tests set it so a fake provider
	// can be driven end to end.
	PassthroughHeaderPrefixes []string
	TierGuard                 float64
	ReservationTTL            time.Duration
}

// Logger is the subset of structured logging the proxy needs.
type Logger interface {
	Error(msg string, args ...any)
	Warn(msg string, args ...any)
}

// Metrics receives what a request did.
type Metrics interface {
	RequestCompleted(model, providerName, status string, d time.Duration)
	TokensCounted(model string, counts pricing.Counts)
	Unpriced(model string)
	ModelMismatch(requested, served string)
}

// Handler serves chat completions.
type Handler struct {
	opts Options
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
	return &Handler{opts: opts}
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
	if req.Stream {
		// Streaming arrives in F6. Refusing plainly beats silently
		// answering a different question.
		api.WriteError(w, http.StatusNotImplemented, api.ErrorTypeInvalidRequest,
			"streaming is not yet supported by this build")
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
	})

	h.writeResponse(r.Context(), w, out, cost, costResult, res.KeyID)

	if h.opts.Metrics != nil {
		h.opts.Metrics.RequestCompleted(servedModel, upstream.Name(), "200", time.Since(started))
		h.opts.Metrics.TokensCounted(servedModel, out.Counts)
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

	if err := h.opts.DB.WriteUsageRecord(ctx, record); err != nil && h.opts.Logger != nil {
		h.opts.Logger.Error("writing the usage record failed",
			"error", err, "request_id", in.requestID)
	}
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
		api.WriteError(w, http.StatusBadRequest, "unsupported_parameter",
			fmt.Sprintf("%q is not supported by %s; costlane refuses rather than "+
				"silently dropping it", unsupported.Parameter, providerName))
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

	body := obs.RedactSecrets(upstream.Body)
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
