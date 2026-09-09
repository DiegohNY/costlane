// Package usage records and persists per-request token and cost usage.
package usage

import (
	"time"

	"github.com/DiegohNY/costlane/internal/pricing"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// Record is one request's accounting.
//
// TokenDetail is the source of truth for cost; the aggregate fields exist for
// reporting and are derived from it. Keeping both means a reporting query
// stays an ordinary sum while a new billing class still lands without a
// migration.
type Record struct {
	ID            uuid.UUID
	KeyID         uuid.UUID
	ReservationID *uuid.UUID
	RequestID     string

	RequestedModel    string
	ServedModel       string
	Provider          string
	ServiceTier       string
	ProviderRequestID string
	WindowStart       time.Time

	TokenDetail pricing.Counts

	CostUSD         *decimal.Decimal
	UsageSource     string
	Unpriced        bool
	PartiallyPriced bool
	Streamed        bool

	ClientDisconnected bool
	DrainTimeout       bool
	ParseErrors        int
	FinishReason       string
	StatusCode         int
	ErrorCode          string
	LatencyMS          int
	TTFTMS             int

	CreatedAt time.Time
}

// Aggregates returns the reporting columns implied by the detail.
//
// cache_write is the sum of both TTLs, and reasoning is already inside
// output, so neither is added twice.
func (r Record) Aggregates() (input, cachedRead, cacheWrite, output, reasoning int64) {
	d := r.TokenDetail
	return d[pricing.KindInput],
		d[pricing.KindCachedRead],
		d[pricing.KindCacheWrite5m] + d[pricing.KindCacheWrite1h],
		d[pricing.KindOutput],
		d[pricing.KindReasoning]
}
