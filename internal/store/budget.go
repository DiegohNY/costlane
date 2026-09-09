package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/shopspring/decimal"
)

// Failures a reserve can report. They are distinct because they call for
// different responses: an unknown key is 401, a disallowed model 403, an
// exhausted budget 402.
var (
	ErrKeyRevoked      = errors.New("store: key revoked")
	ErrModelNotAllowed = errors.New("store: model not allowed for this key")
	ErrBudgetExceeded  = errors.New("store: budget exceeded")
)

// deadlockDetected is the SQLSTATE Postgres raises when it breaks a deadlock
// by aborting one of the transactions.
const deadlockDetected = "40P01"

// settleTimeout bounds a settle that runs after its request has gone.
const settleTimeout = 15 * time.Second

// BudgetExceededError carries the budget state at the moment of refusal.
//
// Whoever receives a 402 wants to know when they can try again, and the
// diagnostic query has already read the numbers, so returning them costs
// nothing.
type BudgetExceededError struct {
	LimitUSD       *decimal.Decimal
	SpentUSD       decimal.Decimal
	ReservedUSD    decimal.Decimal
	WindowResetsAt time.Time
}

func (e *BudgetExceededError) Error() string {
	return fmt.Sprintf("store: budget exceeded (spent %s, reserved %s, limit %v)",
		e.SpentUSD, e.ReservedUSD, e.LimitUSD)
}

// Is lets errors.Is match this against the sentinel, so callers can test for
// the category without unwrapping to read the numbers.
func (e *BudgetExceededError) Is(target error) bool { return target == ErrBudgetExceeded }

// ReserveInput describes a reservation to take.
type ReserveInput struct {
	KeyHash      []byte
	Model        string
	EstimatedUSD decimal.Decimal
	TTL          time.Duration

	// ObserveIsolation, when set, is called with the transaction's
	// isolation level. Tests use it to assert the level the correctness
	// argument depends on.
	ObserveIsolation func(level string)
}

// ReserveOutput is what a successful reservation yields.
type ReserveOutput struct {
	ReservationID    uuid.UUID
	KeyID            uuid.UUID
	WindowStart      time.Time
	DisconnectPolicy string
	DrainTimeoutMS   *int
}

// Reserve authenticates the key, checks the model, rotates the spending
// window if a month has passed, and takes a reservation — in one statement
// inside one transaction.
//
// Fusing them keeps the common path to a single round trip. The predicate
// lives inside the UPDATE rather than in a preceding SELECT, so there is no
// window between checking and writing: when two transactions target the same
// row, the second blocks and then re-evaluates the predicate against the
// committed value. That re-evaluation is specific to READ COMMITTED, which
// is why the isolation level is set explicitly rather than inherited.
//
// Zero rows means one of several things, so only then does a second query
// run to say which.
func (db *DB) Reserve(ctx context.Context, in ReserveInput) (ReserveOutput, error) {
	tx, err := db.write.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return ReserveOutput{}, fmt.Errorf("store: begin reserve: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	if in.ObserveIsolation != nil {
		var level string
		if err := tx.QueryRow(ctx, `SHOW transaction_isolation`).Scan(&level); err != nil {
			return ReserveOutput{}, fmt.Errorf("store: reading isolation: %w", err)
		}
		in.ObserveIsolation(level)
	}

	var out ReserveOutput
	err = tx.QueryRow(ctx, reserveSQL, in.KeyHash, in.Model, in.EstimatedUSD.String()).
		Scan(&out.KeyID, &out.WindowStart, &out.DisconnectPolicy, &out.DrainTimeoutMS)
	if errors.Is(err, pgx.ErrNoRows) {
		// The rare path pays a second query to classify the refusal; the
		// common path never reaches it.
		return ReserveOutput{}, db.diagnoseReserve(ctx, in)
	}
	if err != nil {
		return ReserveOutput{}, fmt.Errorf("store: reserving: %w", err)
	}

	out.ReservationID = uuid.New()
	if _, err := tx.Exec(ctx,
		`INSERT INTO budget_reservations
		   (id, key_id, window_start, state, estimated_usd, expires_at)
		 VALUES ($1, $2, $3, 'pending', $4, now() + $5::interval)`,
		out.ReservationID, out.KeyID, out.WindowStart, in.EstimatedUSD.String(),
		fmt.Sprintf("%d milliseconds", in.TTL.Milliseconds())); err != nil {
		return ReserveOutput{}, fmt.Errorf("store: inserting reservation: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return ReserveOutput{}, fmt.Errorf("store: committing reserve: %w", err)
	}
	return out, nil
}

// The window rotates lazily, inside the reserve. A scheduled job would race
// with concurrent reserves; doing it here makes rotation part of the same
// atomic step. A key with no traffic never rotates, which does not matter
// because reporting reads usage records by window rather than this balance.
const reserveSQL = `
	UPDATE key_budgets kb
	   SET spent_usd = CASE
	           WHEN kb.window_start < date_trunc('month', now() AT TIME ZONE 'UTC')::date
	           THEN 0 ELSE kb.spent_usd END,
	       window_start = GREATEST(kb.window_start,
	                               date_trunc('month', now() AT TIME ZONE 'UTC')::date),
	       reserved_usd = kb.reserved_usd + $3::numeric
	  FROM virtual_keys vk
	 WHERE vk.key_hash = $1
	   AND vk.revoked_at IS NULL
	   AND (vk.allowed_models IS NULL OR $2 = ANY(vk.allowed_models))
	   AND kb.key_id = vk.id
	   AND (kb.limit_usd IS NULL OR
	        (CASE WHEN kb.window_start < date_trunc('month', now() AT TIME ZONE 'UTC')::date
	              THEN 0 ELSE kb.spent_usd END) + kb.reserved_usd + $3::numeric <= kb.limit_usd)
	RETURNING kb.key_id, kb.window_start, vk.disconnect_policy, vk.drain_timeout_ms`

// diagnoseReserve explains a reserve that matched nothing.
func (db *DB) diagnoseReserve(ctx context.Context, in ReserveInput) error {
	var (
		revoked       *time.Time
		allowedModels []string
		limit         *string
		spent         string
		reserved      string
		windowStart   time.Time
	)
	err := db.read.QueryRow(ctx, `
		SELECT vk.revoked_at, vk.allowed_models,
		       kb.limit_usd::text, kb.spent_usd::text, kb.reserved_usd::text, kb.window_start
		  FROM virtual_keys vk
		  JOIN key_budgets kb ON kb.key_id = vk.id
		 WHERE vk.key_hash = $1`, in.KeyHash).
		Scan(&revoked, &allowedModels, &limit, &spent, &reserved, &windowStart)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrKeyNotFound
	}
	if err != nil {
		return fmt.Errorf("store: diagnosing reserve: %w", err)
	}

	if revoked != nil {
		return ErrKeyRevoked
	}
	if allowedModels != nil && !contains(allowedModels, in.Model) {
		return ErrModelNotAllowed
	}

	exceeded := &BudgetExceededError{
		SpentUSD:       mustDecimal(spent),
		ReservedUSD:    mustDecimal(reserved),
		WindowResetsAt: nextWindowStart(),
	}
	if limit != nil {
		l := mustDecimal(*limit)
		exceeded.LimitUSD = &l
	}
	return exceeded
}

// SettleInput describes the close of a reservation.
type SettleInput struct {
	ReservationID uuid.UUID
	ActualUSD     decimal.Decimal

	ObserveIsolation func(level string)
}

// SettleOutput reports what the settle did.
type SettleOutput struct {
	// Applied is false when the reservation was already terminal, which is
	// how a retried settle knows it lost.
	Applied            bool
	Overshoot          bool
	SettledAfterExpiry bool
	EstimatedUSD       decimal.Decimal
	WindowStart        time.Time
}

// Settle closes a reservation and moves its amount from reserved to spent.
//
// It always runs on a context detached from the caller's. A settle happens
// at the end of a request, and by then the client may have disconnected and
// cancelled the request context; using it would abort the transaction and
// leave the reservation pending until the reaper, with the spend missing
// from the budget in the meantime.
//
// The state becomes terminal in every case, so a retried settle after a
// crash matches nothing and cannot count the same amount twice.
func (db *DB) Settle(ctx context.Context, in SettleInput) (SettleOutput, error) {
	settleCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), settleTimeout)
	defer cancel()

	tx, err := db.write.BeginTx(settleCtx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return SettleOutput{}, fmt.Errorf("store: begin settle: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(settleCtx)) }()

	if in.ObserveIsolation != nil {
		var level string
		if err := tx.QueryRow(settleCtx, `SHOW transaction_isolation`).Scan(&level); err != nil {
			return SettleOutput{}, fmt.Errorf("store: reading isolation: %w", err)
		}
		in.ObserveIsolation(level)
	}

	var (
		out       SettleOutput
		keyID     uuid.UUID
		estimated string
	)
	// settled_after_expiry is computed in the SET clause from the old value,
	// because RETURNING yields the new one: after this statement the state
	// is 'settled' either way, so it cannot tell us what it used to be.
	err = tx.QueryRow(settleCtx, `
		UPDATE budget_reservations
		   SET state = 'settled',
		       actual_usd = $2::numeric,
		       settled_at = now(),
		       overshoot = ($2::numeric > estimated_usd),
		       settled_after_expiry = (state = 'expired')
		 WHERE id = $1 AND state IN ('pending', 'expired')
		RETURNING key_id, estimated_usd::text, window_start, overshoot, settled_after_expiry`,
		in.ReservationID, in.ActualUSD.String()).
		Scan(&keyID, &estimated, &out.WindowStart, &out.Overshoot, &out.SettledAfterExpiry)
	if errors.Is(err, pgx.ErrNoRows) {
		// Already terminal: someone else settled it, or the reaper expired
		// it and a later settle already ran. Committing an empty
		// transaction is the correct no-op.
		return SettleOutput{Applied: false}, tx.Commit(settleCtx)
	}
	if err != nil {
		return SettleOutput{}, fmt.Errorf("store: settling: %w", err)
	}
	out.Applied = true
	out.EstimatedUSD = mustDecimal(estimated)

	// The reserved amount is released only if the reaper has not already
	// released it. Subtracting twice would drive reserved_usd negative,
	// which the CHECK constraint would catch — but catching it here means
	// never writing it.
	if _, err := tx.Exec(settleCtx, `
		UPDATE key_budgets
		   SET reserved_usd = reserved_usd - CASE WHEN $2 THEN 0 ELSE $3::numeric END,
		       spent_usd = spent_usd + CASE WHEN window_start = $4 THEN $5::numeric ELSE 0 END
		 WHERE key_id = $1`,
		keyID, out.SettledAfterExpiry, estimated, out.WindowStart, in.ActualUSD.String()); err != nil {
		return SettleOutput{}, fmt.Errorf("store: applying settle to budget: %w", err)
	}

	if err := tx.Commit(settleCtx); err != nil {
		return SettleOutput{}, fmt.Errorf("store: committing settle: %w", err)
	}
	return out, nil
}

// ReapOutput reports what one sweep did.
type ReapOutput struct {
	Reaped          int
	DeadlockRetries int
}

// ReapExpired expires reservations whose TTL has passed and releases the
// budget they held.
//
// Without it, a process that dies between reserve and settle leaves a
// reservation pending forever and the key's budget permanently understated.
//
// Budget rows are locked in key order before being updated. Two reapers
// touching the same pair of keys in opposite orders would otherwise deadlock;
// Postgres would break it by aborting one, which is survivable but wasteful.
// A deadlock is retried once regardless, since the ordering cannot rule out
// contention with a concurrent settle.
func (db *DB) ReapExpired(ctx context.Context, limit int) (ReapOutput, error) {
	var out ReapOutput
	for attempt := range 2 {
		reaped, err := db.reapOnce(ctx, limit)
		if err == nil {
			out.Reaped = reaped
			return out, nil
		}
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == deadlockDetected && attempt == 0 {
			out.DeadlockRetries++
			continue
		}
		return out, err
	}
	return out, nil
}

func (db *DB) reapOnce(ctx context.Context, limit int) (int, error) {
	tx, err := db.write.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return 0, fmt.Errorf("store: begin reap: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	// Claim the expired reservations. SKIP LOCKED lets several replicas
	// sweep at once without contending for the same rows.
	rows, err := tx.Query(ctx, `
		SELECT id, key_id, estimated_usd::text
		  FROM budget_reservations
		 WHERE state = 'pending' AND expires_at < now()
		 ORDER BY expires_at
		 LIMIT $1
		 FOR UPDATE SKIP LOCKED`, limit)
	if err != nil {
		return 0, fmt.Errorf("store: selecting expired: %w", err)
	}

	type claim struct {
		id        uuid.UUID
		keyID     uuid.UUID
		estimated decimal.Decimal
	}
	var claims []claim
	for rows.Next() {
		var c claim
		var estimated string
		if err := rows.Scan(&c.id, &c.keyID, &estimated); err != nil {
			rows.Close()
			return 0, fmt.Errorf("store: scanning expired: %w", err)
		}
		c.estimated = mustDecimal(estimated)
		claims = append(claims, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("store: reading expired: %w", err)
	}
	if len(claims) == 0 {
		return 0, tx.Commit(ctx)
	}

	// Sum per key, then lock the budget rows in a deterministic order.
	// Without the ordering, two reapers holding different halves of the
	// same pair would wait on each other.
	totals := map[uuid.UUID]decimal.Decimal{}
	for _, c := range claims {
		totals[c.keyID] = totals[c.keyID].Add(c.estimated)
	}
	keyIDs := make([]uuid.UUID, 0, len(totals))
	for id := range totals {
		keyIDs = append(keyIDs, id)
	}

	if _, err := tx.Exec(ctx,
		`SELECT 1 FROM key_budgets WHERE key_id = ANY($1) ORDER BY key_id FOR UPDATE`,
		keyIDs); err != nil {
		return 0, fmt.Errorf("store: locking budgets: %w", err)
	}

	ids := make([]uuid.UUID, 0, len(claims))
	for _, c := range claims {
		ids = append(ids, c.id)
	}
	// The state change and the release are in the same transaction: a crash
	// between them would leave the balance diverged from the log, and the
	// divergence would be invisible until reconciliation ran.
	tag, err := tx.Exec(ctx,
		`UPDATE budget_reservations SET state = 'expired'
		  WHERE id = ANY($1) AND state = 'pending'`, ids)
	if err != nil {
		return 0, fmt.Errorf("store: expiring reservations: %w", err)
	}

	for _, id := range keyIDs {
		if _, err := tx.Exec(ctx,
			`UPDATE key_budgets SET reserved_usd = reserved_usd - $2::numeric WHERE key_id = $1`,
			id, totals[id].String()); err != nil {
			return 0, fmt.Errorf("store: releasing budget: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("store: committing reap: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

func mustDecimal(s string) decimal.Decimal {
	d, err := decimal.NewFromString(s)
	if err != nil {
		return decimal.Zero
	}
	return d
}

// nextWindowStart is the instant the current UTC month rolls over, which is
// when a monthly budget frees up.
func nextWindowStart() time.Time {
	now := time.Now().UTC()
	return time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, 1, 0)
}
