package store

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// BalanceDrift reports one key whose materialised balance disagrees with the
// reservation log.
type BalanceDrift struct {
	KeyID uuid.UUID

	MaterialisedSpent    decimal.Decimal
	MaterialisedReserved decimal.Decimal
	LogSpent             decimal.Decimal
	LogReserved          decimal.Decimal
}

// ReconcileReport is the outcome of the three checks.
//
// The materialised balance is a cache of the append-only log, and the whole
// budget design rests on the two agreeing. These queries are the cheapest way
// to know whether they still do, and every concurrency test ends with them.
type ReconcileReport struct {
	// BalanceDrift is empty when the balance matches the log.
	BalanceDrift []BalanceDrift

	// SettledWithoutUsageRecord counts reservations that closed but left no
	// usage record. It measures exactly what the asynchronous write buffer
	// lost in a crash: if it grows in production, the accepted risk does
	// not hold and the buffer needs to become durable.
	SettledWithoutUsageRecord int

	// CostWindowMismatch counts windows where the sum of recorded costs
	// disagrees with the spend attributed to that window.
	CostWindowMismatch int
}

// OK reports whether every check passed.
func (r ReconcileReport) OK() bool {
	return len(r.BalanceDrift) == 0 &&
		r.SettledWithoutUsageRecord == 0 &&
		r.CostWindowMismatch == 0
}

// Reconcile runs the three checks.
func (db *DB) Reconcile(ctx context.Context) (ReconcileReport, error) {
	var report ReconcileReport

	// 1. Recompute the balance from the log and compare.
	//
	// Spend is only counted where the reservation's window matches the
	// budget's current window, mirroring what the settle does: a
	// reservation that closed into a previous month contributed to that
	// month's records, not to this month's balance.
	rows, err := db.read.Query(ctx, `
		SELECT kb.key_id,
		       kb.spent_usd::text,
		       kb.reserved_usd::text,
		       COALESCE(log.settled, 0)::text,
		       COALESCE(log.pending, 0)::text
		  FROM key_budgets kb
		  LEFT JOIN (
		        SELECT r.key_id,
		               SUM(CASE WHEN r.state = 'settled' THEN r.actual_usd ELSE 0 END)
		                 FILTER (WHERE r.window_start = kb2.window_start) AS settled,
		               SUM(CASE WHEN r.state = 'pending' THEN r.estimated_usd ELSE 0 END) AS pending
		          FROM budget_reservations r
		          JOIN key_budgets kb2 ON kb2.key_id = r.key_id
		         GROUP BY r.key_id
		       ) log ON log.key_id = kb.key_id
		 WHERE kb.spent_usd <> COALESCE(log.settled, 0)
		    OR kb.reserved_usd <> COALESCE(log.pending, 0)`)
	if err != nil {
		return report, fmt.Errorf("store: reconciling balances: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			d                       BalanceDrift
			spent, reserved, ls, lr string
		)
		if err := rows.Scan(&d.KeyID, &spent, &reserved, &ls, &lr); err != nil {
			return report, fmt.Errorf("store: scanning drift: %w", err)
		}
		d.MaterialisedSpent = mustDecimal(spent)
		d.MaterialisedReserved = mustDecimal(reserved)
		d.LogSpent = mustDecimal(ls)
		d.LogReserved = mustDecimal(lr)
		report.BalanceDrift = append(report.BalanceDrift, d)
	}
	if err := rows.Err(); err != nil {
		return report, fmt.Errorf("store: reading drift: %w", err)
	}

	// 2. Settled reservations that produced no usage record.
	if err := db.read.QueryRow(ctx, `
		SELECT count(*)
		  FROM budget_reservations r
		  LEFT JOIN usage_records u ON u.reservation_id = r.id
		 WHERE r.state = 'settled' AND u.id IS NULL`).
		Scan(&report.SettledWithoutUsageRecord); err != nil {
		return report, fmt.Errorf("store: reconciling usage records: %w", err)
	}

	// 3. Recorded costs against the spend attributed to each window.
	if err := db.read.QueryRow(ctx, `
		SELECT count(*) FROM (
		    SELECT u.key_id, u.window_start
		      FROM usage_records u
		     WHERE u.cost_usd IS NOT NULL
		     GROUP BY u.key_id, u.window_start
		    HAVING SUM(u.cost_usd) <> COALESCE((
		        SELECT SUM(r.actual_usd) FROM budget_reservations r
		         WHERE r.key_id = u.key_id
		           AND r.window_start = u.window_start
		           AND r.state = 'settled'), 0)
		) mismatched`).Scan(&report.CostWindowMismatch); err != nil {
		return report, fmt.Errorf("store: reconciling costs: %w", err)
	}

	return report, nil
}
