package store

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/DiegohNY/costlane/internal/auth"
	"github.com/google/uuid"
)

// Dimension is a grouping a usage query may use.
//
// The set is closed and checked, so no caller can put a column name into a
// query. Two at a time is the limit: a third produces a result nobody reads
// and a scan nobody wants.
type Dimension string

// The dimensions a usage query may group by.
const (
	DimensionKey      Dimension = "key"
	DimensionModel    Dimension = "model"
	DimensionProvider Dimension = "provider"
	DimensionDay      Dimension = "day"
	DimensionHour     Dimension = "hour"
)

// dimensionSQL maps a dimension to the expression that produces it.
//
// Days and hours are cut on UTC boundaries, matching the budget windows. A
// day that shifted with the caller's timezone would give totals that never
// reconcile with spent_usd.
var dimensionSQL = map[Dimension]string{
	DimensionKey:      "u.key_id::text",
	DimensionModel:    "u.served_model",
	DimensionProvider: "u.provider",
	DimensionDay:      "to_char(date_trunc('day', u.created_at AT TIME ZONE 'UTC'), 'YYYY-MM-DD')",
	DimensionHour:     "to_char(date_trunc('hour', u.created_at AT TIME ZONE 'UTC'), 'YYYY-MM-DD\"T\"HH24:00:00\"Z\"')",
}

// Valid reports whether d is a known dimension.
func (d Dimension) Valid() bool { _, ok := dimensionSQL[d]; return ok }

// ErrTooManyDimensions and friends report a query that was refused.
var (
	ErrTooManyDimensions = errors.New("store: at most two group-by dimensions")
	ErrUnknownDimension  = errors.New("store: unknown group-by dimension")
	ErrRangeTooWide      = errors.New("store: the requested range is too wide")
	ErrBadCursor         = errors.New("store: the cursor could not be decoded")
)

// UsageQuery describes an aggregate request.
type UsageQuery struct {
	Scope auth.Scope
	From  time.Time
	To    time.Time

	GroupBy []Dimension

	// MaxWindow bounds the range, so no query can walk the whole table.
	MaxWindow time.Duration
}

// UsageBucket is one row of an aggregate result.
//
// Money travels as a decimal string. A float64 would round it on the way out,
// which for a product whose purpose is exact accounting is the one thing it
// must not do.
type UsageBucket struct {
	Group map[string]string `json:"group"`

	Requests         int64 `json:"requests"`
	InputTokens      int64 `json:"input_tokens"`
	CachedReadTokens int64 `json:"cached_read_tokens"`
	CacheWriteTokens int64 `json:"cache_write_tokens"`
	OutputTokens     int64 `json:"output_tokens"`
	ReasoningTokens  int64 `json:"reasoning_tokens"`

	CostUSD string `json:"cost_usd"`
	// UnpricedRequests counts requests whose cost is unknown, so a total
	// is never read as complete when it is not.
	UnpricedRequests int64 `json:"unpriced_requests"`
}

// QueryUsage aggregates usage.
func (db *DB) QueryUsage(ctx context.Context, q UsageQuery) ([]UsageBucket, error) {
	if !q.Scope.Valid() {
		return nil, auth.ErrNoScope
	}
	if len(q.GroupBy) > 2 {
		return nil, ErrTooManyDimensions
	}
	for _, d := range q.GroupBy {
		if !d.Valid() {
			return nil, fmt.Errorf("%w: %q", ErrUnknownDimension, d)
		}
	}
	if q.MaxWindow > 0 && q.To.Sub(q.From) > q.MaxWindow {
		return nil, ErrRangeTooWide
	}

	var (
		selects []string
		groups  []string
	)
	for i, d := range q.GroupBy {
		expr := dimensionSQL[d]
		alias := fmt.Sprintf("g%d", i)
		selects = append(selects, expr+" AS "+alias)
		groups = append(groups, expr)
	}

	query := `SELECT `
	if len(selects) > 0 {
		query += strings.Join(selects, ", ") + ", "
	}
	query += `count(*) AS requests,
	          COALESCE(SUM(u.input_tokens), 0),
	          COALESCE(SUM(u.cached_read_tokens), 0),
	          COALESCE(SUM(u.cache_write_tokens), 0),
	          COALESCE(SUM(u.output_tokens), 0),
	          COALESCE(SUM(u.reasoning_tokens), 0),
	          COALESCE(SUM(u.cost_usd), 0)::text,
	          count(*) FILTER (WHERE u.cost_usd IS NULL)
	     FROM usage_records u
	    WHERE u.created_at >= $1 AND u.created_at < $2`

	args := []any{q.From, q.To}
	// The scope is applied here rather than by the caller: a handler that
	// forgot to filter would otherwise expose every key's spend.
	if !q.Scope.IsMaster() {
		args = append(args, q.Scope.KeyID())
		query += fmt.Sprintf(" AND u.key_id = $%d", len(args))
	}
	if len(groups) > 0 {
		query += " GROUP BY " + strings.Join(groups, ", ")
		query += " ORDER BY " + strings.Join(groups, ", ")
	}

	rows, err := db.read.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: querying usage: %w", err)
	}
	defer rows.Close()

	var out []UsageBucket
	for rows.Next() {
		bucket := UsageBucket{Group: map[string]string{}}
		values := make([]string, len(q.GroupBy))
		dest := make([]any, 0, len(q.GroupBy)+8)
		for i := range values {
			dest = append(dest, &values[i])
		}
		dest = append(dest,
			&bucket.Requests, &bucket.InputTokens, &bucket.CachedReadTokens,
			&bucket.CacheWriteTokens, &bucket.OutputTokens, &bucket.ReasoningTokens,
			&bucket.CostUSD, &bucket.UnpricedRequests)

		if err := rows.Scan(dest...); err != nil {
			return nil, fmt.Errorf("store: scanning usage: %w", err)
		}
		for i, d := range q.GroupBy {
			bucket.Group[string(d)] = values[i]
		}
		out = append(out, bucket)
	}
	return out, rows.Err()
}

// RequestQuery describes a paginated listing.
type RequestQuery struct {
	Scope  auth.Scope
	From   time.Time
	To     time.Time
	Limit  int
	Cursor string

	MaxWindow time.Duration
}

// RequestRow is one recorded request.
type RequestRow struct {
	ID                uuid.UUID `json:"id"`
	RequestID         string    `json:"request_id"`
	KeyID             uuid.UUID `json:"key_id"`
	RequestedModel    string    `json:"requested_model"`
	ServedModel       string    `json:"served_model"`
	Provider          string    `json:"provider"`
	ProviderRequestID string    `json:"provider_request_id"`

	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`

	CostUSD     *string `json:"cost_usd"`
	UsageSource string  `json:"usage_source"`
	Unpriced    bool    `json:"unpriced"`
	Streamed    bool    `json:"streamed"`
	StatusCode  int     `json:"status_code"`
	ErrorCode   *string `json:"error_code"`

	CreatedAt time.Time `json:"created_at"`
}

// RequestPage is a page of requests plus the cursor for the next one.
type RequestPage struct {
	Data       []RequestRow `json:"data"`
	NextCursor *string      `json:"next_cursor"`
}

// QueryRequests lists individual requests.
//
// Pagination is keyset on (created_at, id) rather than OFFSET: on a table
// that grows by a row per request, OFFSET 100000 is a scan, and rows shift
// under a reader as new ones arrive.
func (db *DB) QueryRequests(ctx context.Context, q RequestQuery) (RequestPage, error) {
	if !q.Scope.Valid() {
		return RequestPage{}, auth.ErrNoScope
	}
	if q.MaxWindow > 0 && q.To.Sub(q.From) > q.MaxWindow {
		return RequestPage{}, ErrRangeTooWide
	}
	if q.Limit <= 0 || q.Limit > 1000 {
		q.Limit = 100
	}

	args := []any{q.From, q.To}
	query := `
		SELECT u.id, u.request_id, u.key_id, u.requested_model,
		       COALESCE(u.served_model, ''), u.provider,
		       COALESCE(u.provider_request_id, ''),
		       u.input_tokens, u.output_tokens, u.cost_usd::text,
		       u.usage_source, u.unpriced, u.streamed, u.status_code,
		       u.error_code, u.created_at
		  FROM usage_records u
		 WHERE u.created_at >= $1 AND u.created_at < $2`

	if !q.Scope.IsMaster() {
		args = append(args, q.Scope.KeyID())
		query += fmt.Sprintf(" AND u.key_id = $%d", len(args))
	}

	if q.Cursor != "" {
		at, id, err := decodeCursor(q.Cursor)
		if err != nil {
			return RequestPage{}, err
		}
		args = append(args, at, id)
		query += fmt.Sprintf(" AND (u.created_at, u.id) < ($%d, $%d)",
			len(args)-1, len(args))
	}

	args = append(args, q.Limit+1)
	query += fmt.Sprintf(" ORDER BY u.created_at DESC, u.id DESC LIMIT $%d", len(args))

	rows, err := db.read.Query(ctx, query, args...)
	if err != nil {
		return RequestPage{}, fmt.Errorf("store: querying requests: %w", err)
	}
	defer rows.Close()

	page := RequestPage{Data: []RequestRow{}}
	for rows.Next() {
		var r RequestRow
		if err := rows.Scan(&r.ID, &r.RequestID, &r.KeyID, &r.RequestedModel,
			&r.ServedModel, &r.Provider, &r.ProviderRequestID,
			&r.InputTokens, &r.OutputTokens, &r.CostUSD,
			&r.UsageSource, &r.Unpriced, &r.Streamed, &r.StatusCode,
			&r.ErrorCode, &r.CreatedAt); err != nil {
			return RequestPage{}, fmt.Errorf("store: scanning requests: %w", err)
		}
		page.Data = append(page.Data, r)
	}
	if err := rows.Err(); err != nil {
		return RequestPage{}, err
	}

	// One row beyond the page size tells us there is more, without a
	// second count query.
	if len(page.Data) > q.Limit {
		last := page.Data[q.Limit-1]
		page.Data = page.Data[:q.Limit]
		cursor := encodeCursor(last.CreatedAt, last.ID)
		page.NextCursor = &cursor
	}
	return page, nil
}

// encodeCursor packs the keyset position opaquely, so callers cannot
// construct one and depend on its shape.
func encodeCursor(at time.Time, id uuid.UUID) string {
	raw := at.UTC().Format(time.RFC3339Nano) + "|" + id.String()
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func decodeCursor(cursor string) (time.Time, uuid.UUID, error) {
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return time.Time{}, uuid.Nil, fmt.Errorf("%w: not valid base64", ErrBadCursor)
	}
	at, id, found := strings.Cut(string(raw), "|")
	if !found {
		return time.Time{}, uuid.Nil, fmt.Errorf("%w: malformed", ErrBadCursor)
	}
	parsed, err := time.Parse(time.RFC3339Nano, at)
	if err != nil {
		return time.Time{}, uuid.Nil, fmt.Errorf("%w: bad timestamp", ErrBadCursor)
	}
	parsedID, err := uuid.Parse(id)
	if err != nil {
		return time.Time{}, uuid.Nil, fmt.Errorf("%w: bad identifier", ErrBadCursor)
	}
	return parsed, parsedID, nil
}

// BudgetStatus is what a key can see of its own budget.
type BudgetStatus struct {
	LimitUSD       *string   `json:"limit_usd"`
	SpentUSD       string    `json:"spent_usd"`
	ReservedUSD    string    `json:"reserved_usd"`
	RemainingUSD   *string   `json:"remaining_usd"`
	WindowStart    string    `json:"window_start"`
	WindowResetsAt time.Time `json:"window_resets_at"`
}

// BudgetStatusFor reports a key's budget.
func (db *DB) BudgetStatusFor(ctx context.Context, keyID uuid.UUID) (BudgetStatus, error) {
	var (
		status      BudgetStatus
		windowStart time.Time
	)
	err := db.read.QueryRow(ctx, `
		SELECT limit_usd::text, spent_usd::text, reserved_usd::text,
		       CASE WHEN limit_usd IS NULL THEN NULL
		            ELSE (limit_usd - spent_usd - reserved_usd)::text END,
		       window_start
		  FROM key_budgets WHERE key_id = $1`, keyID).
		Scan(&status.LimitUSD, &status.SpentUSD, &status.ReservedUSD,
			&status.RemainingUSD, &windowStart)
	if err != nil {
		return BudgetStatus{}, fmt.Errorf("store: reading budget: %w", err)
	}
	status.WindowStart = windowStart.Format("2006-01-02")
	status.WindowResetsAt = nextWindowStart()
	return status, nil
}
