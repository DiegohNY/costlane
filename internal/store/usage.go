package store

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/DiegohNY/costlane/internal/usage"
)

// WriteUsageRecord persists one request's accounting.
//
// It is synchronous for now. The asynchronous buffer arrives in F7, along
// with the reconciliation that measures what a crash costs; until then a
// record is either written or the error is reported, and nothing is lost
// quietly.
func (db *DB) WriteUsageRecord(ctx context.Context, r usage.Record) error {
	detail, err := json.Marshal(r.TokenDetail)
	if err != nil {
		return fmt.Errorf("store: encoding token detail: %w", err)
	}
	input, cachedRead, cacheWrite, output, reasoning := r.Aggregates()

	var cost *string
	if r.CostUSD != nil {
		text := r.CostUSD.String()
		cost = &text
	}
	tier := r.ServiceTier
	if tier == "" {
		tier = "standard"
	}

	_, err = db.write.Exec(ctx, `
		INSERT INTO usage_records (
			id, key_id, reservation_id, request_id,
			requested_model, served_model, provider, service_tier,
			provider_request_id, window_start, token_detail,
			input_tokens, cached_read_tokens, cache_write_tokens,
			output_tokens, reasoning_tokens,
			cost_usd, usage_source, unpriced, partially_priced, streamed,
			client_disconnected, drain_timeout, parse_errors,
			finish_reason, status_code, error_code, latency_ms, ttft_ms)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,
		        $17::numeric,$18,$19,$20,$21,$22,$23,$24,$25,$26,$27,$28,$29)`,
		r.ID, r.KeyID, r.ReservationID, r.RequestID,
		r.RequestedModel, nullIfEmpty(r.ServedModel), r.Provider, tier,
		nullIfEmpty(r.ProviderRequestID), r.WindowStart, detail,
		input, cachedRead, cacheWrite, output, reasoning,
		cost, r.UsageSource, r.Unpriced, r.PartiallyPriced, r.Streamed,
		r.ClientDisconnected, r.DrainTimeout, r.ParseErrors,
		nullIfEmpty(r.FinishReason), r.StatusCode, nullIfEmpty(r.ErrorCode),
		nullIfZero(r.LatencyMS), nullIfZero(r.TTFTMS))
	if err != nil {
		return fmt.Errorf("store: writing usage record: %w", err)
	}
	return nil
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullIfZero(n int) any {
	if n == 0 {
		return nil
	}
	return n
}
