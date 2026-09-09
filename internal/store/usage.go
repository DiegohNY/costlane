package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/DiegohNY/costlane/internal/pricing"
	"github.com/DiegohNY/costlane/internal/usage"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// WriteBatch persists many records in one round trip.
//
// A per-record insert would put a round trip on the critical path of every
// request; sending them together is what makes the asynchronous buffer worth
// having. The whole batch is one statement, so it either lands or it does
// not — a half-written batch would leave the reconciliation counting a gap
// that is really a partial success.
func (db *DB) WriteBatch(ctx context.Context, records []usage.Record) error {
	if len(records) == 0 {
		return nil
	}

	rows := make([][]any, 0, len(records))
	for _, r := range records {
		row, err := usageRow(r)
		if err != nil {
			return err
		}
		rows = append(rows, row)
	}

	_, err := db.write.CopyFrom(ctx,
		pgx.Identifier{"usage_records"}, usageColumns, pgx.CopyFromRows(rows))
	if err != nil {
		return fmt.Errorf("store: writing %d usage records: %w", len(records), err)
	}
	return nil
}

var usageColumns = []string{
	"id", "key_id", "reservation_id", "request_id",
	"requested_model", "served_model", "provider", "service_tier",
	"provider_request_id", "window_start", "token_detail",
	"input_tokens", "cached_read_tokens", "cache_write_tokens",
	"output_tokens", "reasoning_tokens",
	"cost_usd", "usage_source", "unpriced", "partially_priced", "streamed",
	"client_disconnected", "drain_timeout", "parse_errors",
	"finish_reason", "status_code", "error_code", "latency_ms", "ttft_us",
}

func usageRow(r usage.Record) ([]any, error) {
	// A nil map marshals to JSON null, which the object constraint rejects
	// — and rightly: a request that consumed nothing still has a token
	// detail, it is just empty. This happens for a request that failed
	// before the provider replied.
	if r.TokenDetail == nil {
		r.TokenDetail = pricing.Counts{}
	}
	detail, err := json.Marshal(r.TokenDetail)
	if err != nil {
		return nil, fmt.Errorf("store: encoding token detail: %w", err)
	}
	input, cachedRead, cacheWrite, output, reasoning := r.Aggregates()

	var cost any
	if r.CostUSD != nil {
		cost = r.CostUSD.String()
	}
	tier := r.ServiceTier
	if tier == "" {
		tier = "standard"
	}

	return []any{
		r.ID, r.KeyID, r.ReservationID, r.RequestID,
		r.RequestedModel, nullIfEmpty(r.ServedModel), r.Provider, tier,
		nullIfEmpty(r.ProviderRequestID), r.WindowStart, detail,
		input, cachedRead, cacheWrite, output, reasoning,
		cost, r.UsageSource, r.Unpriced, r.PartiallyPriced, r.Streamed,
		r.ClientDisconnected, r.DrainTimeout, r.ParseErrors,
		nullIfEmpty(r.FinishReason), r.StatusCode, nullIfEmpty(r.ErrorCode),
		nullIfZero(r.LatencyMS), nullIfZero64(r.TTFTMicros),
	}, nil
}

// WriteUsageRecord persists one record. It exists for the synchronous
// fallback the buffer takes when its queue is full.
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
			finish_reason, status_code, error_code, latency_ms, ttft_us)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,
		        $17::numeric,$18,$19,$20,$21,$22,$23,$24,$25,$26,$27,$28,$29)`,
		r.ID, r.KeyID, r.ReservationID, r.RequestID,
		r.RequestedModel, nullIfEmpty(r.ServedModel), r.Provider, tier,
		nullIfEmpty(r.ProviderRequestID), r.WindowStart, detail,
		input, cachedRead, cacheWrite, output, reasoning,
		cost, r.UsageSource, r.Unpriced, r.PartiallyPriced, r.Streamed,
		r.ClientDisconnected, r.DrainTimeout, r.ParseErrors,
		nullIfEmpty(r.FinishReason), r.StatusCode, nullIfEmpty(r.ErrorCode),
		nullIfZero(r.LatencyMS), nullIfZero64(r.TTFTMicros))
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

func nullIfZero64(n int64) any {
	if n == 0 {
		return nil
	}
	return n
}

// WriteBatchAt persists records with their stated CreatedAt.
//
// The ordinary path lets the database set the timestamp, which is right for
// production and useless for a test that needs records spread over days.
func (db *DB) WriteBatchAt(ctx context.Context, records []usage.Record) error {
	if len(records) == 0 {
		return nil
	}
	columns := append(append([]string{}, usageColumns...), "created_at")
	rows := make([][]any, 0, len(records))
	for _, r := range records {
		row, err := usageRow(r)
		if err != nil {
			return err
		}
		at := r.CreatedAt
		if at.IsZero() {
			at = time.Now().UTC()
		}
		rows = append(rows, append(row, at))
	}
	if _, err := db.write.CopyFrom(ctx,
		pgx.Identifier{"usage_records"}, columns, pgx.CopyFromRows(rows)); err != nil {
		return fmt.Errorf("store: writing %d dated usage records: %w", len(records), err)
	}
	return nil
}

// RetentionPolicy says how long each kind of row is kept.
//
// Payloads and records are separate because they answer different questions
// and carry different risk. A usage record is an accounting entry worth
// keeping; a stored prompt is a copy of a customer's data, and it should not
// outlive the debugging session it was captured for.
type RetentionPolicy struct {
	UsageRecords    time.Duration
	RequestPayloads time.Duration
	// BatchSize bounds one delete, so retention never takes a lock long
	// enough to block live traffic.
	BatchSize int
}

// RetentionResult reports what one pass removed.
type RetentionResult struct {
	UsageRecords    int64
	RequestPayloads int64
}

// ApplyRetention deletes rows past their retention.
//
// Payloads go first: dropping a usage record would cascade to its payload
// anyway, and deleting the payload separately means a shortened payload
// retention takes effect even where the record is being kept.
func (db *DB) ApplyRetention(ctx context.Context, policy RetentionPolicy) (RetentionResult, error) {
	var result RetentionResult
	if policy.BatchSize <= 0 {
		policy.BatchSize = 10_000
	}

	if policy.RequestPayloads > 0 {
		tag, err := db.write.Exec(ctx, `
			DELETE FROM request_payloads
			 WHERE usage_record_id IN (
			     SELECT usage_record_id FROM request_payloads
			      WHERE created_at < now() - $1::interval
			      LIMIT $2)`,
			intervalOf(policy.RequestPayloads), policy.BatchSize)
		if err != nil {
			return result, fmt.Errorf("store: pruning request payloads: %w", err)
		}
		result.RequestPayloads = tag.RowsAffected()
	}

	if policy.UsageRecords > 0 {
		tag, err := db.write.Exec(ctx, `
			DELETE FROM usage_records
			 WHERE id IN (
			     SELECT id FROM usage_records
			      WHERE created_at < now() - $1::interval
			      LIMIT $2)`,
			intervalOf(policy.UsageRecords), policy.BatchSize)
		if err != nil {
			return result, fmt.Errorf("store: pruning usage records: %w", err)
		}
		result.UsageRecords = tag.RowsAffected()
	}

	return result, nil
}

func intervalOf(d time.Duration) string {
	return fmt.Sprintf("%d seconds", int64(d.Seconds()))
}

// WritePayload stores a request and response body against a usage record.
//
// It runs only when prompt logging is switched on, and writes to a table of
// its own so that it can be dropped, or given a much shorter retention,
// without touching the accounting. A prompt is a copy of a customer's data:
// it should exist for as long as someone is debugging, and no longer.
func (db *DB) WritePayload(ctx context.Context, usageRecordID uuid.UUID,
	request, response []byte) error {
	_, err := db.write.Exec(ctx, `
		INSERT INTO request_payloads (usage_record_id, request_body, response_body)
		VALUES ($1, $2, $3)
		ON CONFLICT (usage_record_id) DO NOTHING`,
		usageRecordID, nullIfNoBytes(request), nullIfNoBytes(response))
	if err != nil {
		return fmt.Errorf("store: writing request payload: %w", err)
	}
	return nil
}

func nullIfNoBytes(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return b
}
