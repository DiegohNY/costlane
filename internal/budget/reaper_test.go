package budget_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/DiegohNY/costlane/internal/auth"
	"github.com/DiegohNY/costlane/internal/budget"
	"github.com/DiegohNY/costlane/internal/store"
	"github.com/DiegohNY/costlane/internal/storetest"
	"github.com/shopspring/decimal"
)

type recordingMetrics struct {
	mu      sync.Mutex
	reaped  int
	sweeps  int
	retries int
}

func (m *recordingMetrics) ReservationsReaped(n int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.reaped += n
}
func (m *recordingMetrics) ReapDuration(time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sweeps++
}
func (m *recordingMetrics) DeadlockRetries(n int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.retries += n
}

func (m *recordingMetrics) totals() (reaped, sweeps, retries int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.reaped, m.sweeps, m.retries
}

func expiredReservation(t *testing.T, db *store.DB) {
	t.Helper()
	key, err := auth.NewKey()
	if err != nil {
		t.Fatalf("generating: %v", err)
	}
	limit := decimal.NewFromInt(1000)
	rec, err := db.CreateKey(t.Context(), store.CreateKeyInput{
		Hash: key.Hash, Prefix: key.Prefix, Label: "reaper test", LimitUSD: &limit,
	})
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	if _, err := db.Reserve(t.Context(), store.ReserveInput{
		KeyHash: key.Hash, Model: "m", EstimatedUSD: decimal.NewFromInt(5),
		TTL: -time.Second,
	}); err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	_ = rec
}

func TestReaperReleasesExpiredReservations(t *testing.T) {
	db := storetest.NewTestDB(t)
	for range 5 {
		expiredReservation(t, db)
	}

	metrics := &recordingMetrics{}
	r := budget.NewReaper(budget.Options{DB: db, Batch: 100, Metrics: metrics})
	r.Sweep(t.Context())

	var pending int
	if err := db.Pool().QueryRow(t.Context(),
		`SELECT count(*) FROM budget_reservations WHERE state = 'pending'`).Scan(&pending); err != nil {
		t.Fatalf("counting: %v", err)
	}
	if pending != 0 {
		t.Errorf("%d reservations are still pending", pending)
	}

	// Budget held by an abandoned request must come back.
	var reserved string
	if err := db.Pool().QueryRow(t.Context(),
		`SELECT COALESCE(SUM(reserved_usd), 0)::text FROM key_budgets`).Scan(&reserved); err != nil {
		t.Fatalf("summing: %v", err)
	}
	if d, _ := decimal.NewFromString(reserved); !d.IsZero() {
		t.Errorf("reserved_usd totals %s, want 0 after reaping", reserved)
	}

	if reaped, sweeps, _ := metrics.totals(); reaped != 5 || sweeps != 1 {
		t.Errorf("metrics: reaped=%d sweeps=%d, want 5 and 1", reaped, sweeps)
	}
}

// A reservation that has not expired must survive: reaping a live request
// would release budget still in use, and its settle would then land after
// expiry.
func TestReaperLeavesLiveReservationsAlone(t *testing.T) {
	db := storetest.NewTestDB(t)
	key, err := auth.NewKey()
	if err != nil {
		t.Fatalf("generating: %v", err)
	}
	limit := decimal.NewFromInt(100)
	if _, err := db.CreateKey(t.Context(), store.CreateKeyInput{
		Hash: key.Hash, Prefix: key.Prefix, Label: "live", LimitUSD: &limit,
	}); err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	if _, err := db.Reserve(t.Context(), store.ReserveInput{
		KeyHash: key.Hash, Model: "m", EstimatedUSD: decimal.NewFromInt(5),
		TTL: time.Hour,
	}); err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	budget.NewReaper(budget.Options{DB: db, Batch: 100}).Sweep(t.Context())

	var pending int
	if err := db.Pool().QueryRow(t.Context(),
		`SELECT count(*) FROM budget_reservations WHERE state = 'pending'`).Scan(&pending); err != nil {
		t.Fatalf("counting: %v", err)
	}
	if pending != 1 {
		t.Errorf("%d pending reservations, want the live one to survive", pending)
	}
}

// On shutdown the reaper finishes the batch it is running rather than
// abandoning a transaction that has already claimed rows.
func TestReaperStopsOnCancellation(t *testing.T) {
	db := storetest.NewTestDB(t)
	expiredReservation(t, db)

	r := budget.NewReaper(budget.Options{DB: db, Interval: 50 * time.Millisecond, Batch: 10})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		r.Run(ctx)
		close(done)
	}()

	time.Sleep(200 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the reaper did not stop within five seconds of cancellation")
	}

	// Whatever it swept is committed, not half-applied.
	report, err := db.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(report.BalanceDrift) != 0 {
		t.Errorf("a cancelled sweep left the balance diverged: %+v", report.BalanceDrift)
	}
}

// An empty sweep is ordinary and must not be reported as work.
func TestEmptySweepIsQuiet(t *testing.T) {
	db := storetest.NewTestDB(t)
	metrics := &recordingMetrics{}
	budget.NewReaper(budget.Options{DB: db, Batch: 10, Metrics: metrics}).Sweep(t.Context())

	if reaped, sweeps, _ := metrics.totals(); reaped != 0 || sweeps != 1 {
		t.Errorf("metrics: reaped=%d sweeps=%d, want 0 reaped and 1 sweep", reaped, sweeps)
	}
}
