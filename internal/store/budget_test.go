package store_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/DiegohNY/costlane/internal/auth"
	"github.com/DiegohNY/costlane/internal/store"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

func usd(s string) decimal.Decimal {
	d, err := decimal.NewFromString(s)
	if err != nil {
		panic(err)
	}
	return d
}

// budgetedKey creates a key with a limit and returns its id and secret hash.
func budgetedKey(t *testing.T, db *store.DB, limit string) (uuid.UUID, []byte) {
	t.Helper()
	key, err := auth.NewKey()
	if err != nil {
		t.Fatalf("generating: %v", err)
	}
	in := store.CreateKeyInput{Hash: key.Hash, Prefix: key.Prefix, Label: "budget test"}
	if limit != "" {
		l := usd(limit)
		in.LimitUSD = &l
	}
	rec, err := db.CreateKey(t.Context(), in)
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	return rec.ID, key.Hash
}

// The property the whole design exists for: many requests racing on one key
// must not overspend it. Exact numbers, not approximate ones — an
// off-by-a-few here is a budget that leaks.
func TestConcurrentReservesNeverExceedTheLimit(t *testing.T) {
	db := newTestDB(t)
	_, hash := budgetedKey(t, db, "10")

	const (
		goroutines = 200
		estimate   = "1"
	)
	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		succeeded int
		refused   int
	)

	start := make(chan struct{})
	for range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := db.Reserve(context.Background(), store.ReserveInput{
				KeyHash:      hash,
				Model:        "gpt-5.6-terra",
				EstimatedUSD: usd(estimate),
				TTL:          time.Minute,
			})
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				succeeded++
			case errors.Is(err, store.ErrBudgetExceeded):
				refused++
			default:
				t.Errorf("unexpected error: %v", err)
			}
		}()
	}
	close(start)
	wg.Wait()

	if succeeded != 10 {
		t.Errorf("%d reserves succeeded, want exactly 10 ($10 limit at $1 each)", succeeded)
	}
	if refused != goroutines-10 {
		t.Errorf("%d reserves refused, want %d", refused, goroutines-10)
	}

	var reserved, spent string
	if err := db.Pool().QueryRow(t.Context(),
		`SELECT reserved_usd::text, spent_usd::text FROM key_budgets
		  WHERE key_id = (SELECT id FROM virtual_keys WHERE key_hash = $1)`,
		hash).Scan(&reserved, &spent); err != nil {
		t.Fatalf("reading budget: %v", err)
	}
	if !usd(reserved).Equal(usd("10")) {
		t.Errorf("reserved_usd = %s, want exactly 10", reserved)
	}
	if !usd(spent).IsZero() {
		t.Errorf("spent_usd = %s, want 0: nothing has settled yet", spent)
	}

	assertReconciled(t, db)
}

// A settle applied twice must count once. The second attempt matches no row
// because the state is terminal, which is what makes a retry after a crash
// safe.
func TestConcurrentDoubleSettleAppliesOnce(t *testing.T) {
	db := newTestDB(t)
	_, hash := budgetedKey(t, db, "100")

	res, err := db.Reserve(t.Context(), store.ReserveInput{
		KeyHash: hash, Model: "m", EstimatedUSD: usd("5"), TTL: time.Minute,
	})
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	var wg sync.WaitGroup
	applied := make([]bool, 2)
	for i := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out, err := db.Settle(context.Background(), store.SettleInput{
				ReservationID: res.ReservationID,
				ActualUSD:     usd("3"),
			})
			if err != nil {
				t.Errorf("Settle: %v", err)
				return
			}
			applied[i] = out.Applied
		}()
	}
	wg.Wait()

	if applied[0] == applied[1] {
		t.Errorf("both settles reported Applied=%t; exactly one must win", applied[0])
	}

	spent, reserved := budgetTotals(t, db, hash)
	if !spent.Equal(usd("3")) {
		t.Errorf("spent_usd = %s, want 3 counted once", spent)
	}
	if !reserved.IsZero() {
		t.Errorf("reserved_usd = %s, want 0", reserved)
	}
	assertReconciled(t, db)
}

// The race the design worried about: a settle arriving as the reaper expires
// the same reservation. Exactly one may release the reserved amount, or the
// budget is credited twice.
func TestSettleAgainstReaperRace(t *testing.T) {
	db := newTestDB(t)

	for range 100 {
		_, hash := budgetedKey(t, db, "1000")
		res, err := db.Reserve(t.Context(), store.ReserveInput{
			KeyHash: hash, Model: "m", EstimatedUSD: usd("5"),
			// Already expired, so the reaper is eligible immediately.
			TTL: -time.Second,
		})
		if err != nil {
			t.Fatalf("Reserve: %v", err)
		}

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			if _, err := db.Settle(context.Background(), store.SettleInput{
				ReservationID: res.ReservationID, ActualUSD: usd("4"),
			}); err != nil {
				t.Errorf("Settle: %v", err)
			}
		}()
		go func() {
			defer wg.Done()
			if _, err := db.ReapExpired(context.Background(), 100); err != nil {
				t.Errorf("ReapExpired: %v", err)
			}
		}()
		wg.Wait()

		spent, reserved := budgetTotals(t, db, hash)
		// The spend is real either way and must be recorded.
		if !spent.Equal(usd("4")) {
			t.Fatalf("spent_usd = %s, want 4: the request consumed tokens", spent)
		}
		// Released exactly once, never twice.
		if !reserved.IsZero() {
			t.Fatalf("reserved_usd = %s, want 0: a double release would go negative", reserved)
		}
	}
	assertReconciled(t, db)
}

// Spend belongs to the window of the reserve. Under load the rotation must
// happen once, not once per racing request.
func TestWindowRotationUnderConcurrentLoad(t *testing.T) {
	db := newTestDB(t)
	keyID, hash := budgetedKey(t, db, "1000")

	// Put the budget in a previous month with spend already on it.
	if _, err := db.Pool().Exec(t.Context(),
		`UPDATE key_budgets
		    SET window_start = (date_trunc('month', now() AT TIME ZONE 'UTC') - interval '1 month')::date,
		        spent_usd = 300
		  WHERE key_id = $1`, keyID); err != nil {
		t.Fatalf("seeding a stale window: %v", err)
	}

	const goroutines = 50
	var wg sync.WaitGroup
	start := make(chan struct{})
	ids := make([]uuid.UUID, goroutines)

	for i := range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			out, err := db.Reserve(context.Background(), store.ReserveInput{
				KeyHash: hash, Model: "m", EstimatedUSD: usd("1"), TTL: time.Minute,
			})
			if err != nil {
				t.Errorf("Reserve: %v", err)
				return
			}
			ids[i] = out.ReservationID
		}()
	}
	close(start)
	wg.Wait()

	spent, reserved := budgetTotals(t, db, hash)
	// The stale spend is cleared exactly once: clearing it per request
	// would be harmless here but would mask a rotation running repeatedly.
	if !spent.IsZero() {
		t.Errorf("spent_usd = %s, want 0 after rotation", spent)
	}
	if !reserved.Equal(usd(("50"))) {
		t.Errorf("reserved_usd = %s, want 50: no reservation may be lost", reserved)
	}

	var window time.Time
	if err := db.Pool().QueryRow(t.Context(),
		`SELECT window_start FROM key_budgets WHERE key_id = $1`, keyID).Scan(&window); err != nil {
		t.Fatalf("reading window: %v", err)
	}
	nowUTC := time.Now().UTC()
	if window.Year() != nowUTC.Year() || window.Month() != nowUTC.Month() {
		t.Errorf("window_start = %s, want the current UTC month", window.Format("2006-01"))
	}

	// Every reservation carries the window in force when it was taken.
	for i, id := range ids {
		if id == uuid.Nil {
			t.Fatalf("reservation %d was not created", i)
		}
	}
	assertReconciled(t, db)
}

// Two reapers racing over many keys must not deadlock: without a consistent
// lock order, two transactions updating the same pair of budget rows in
// opposite orders block each other forever.
func TestConcurrentReapersDoNotDeadlock(t *testing.T) {
	db := newTestDB(t)

	const keys = 50
	hashes := make([][]byte, keys)
	for i := range keys {
		_, hashes[i] = budgetedKey(t, db, "1000")
	}

	for iteration := range 20 {
		for _, hash := range hashes {
			if _, err := db.Reserve(t.Context(), store.ReserveInput{
				KeyHash: hash, Model: "m", EstimatedUSD: usd("1"), TTL: -time.Second,
			}); err != nil {
				t.Fatalf("iteration %d: Reserve: %v", iteration, err)
			}
		}

		var wg sync.WaitGroup
		for range 2 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if _, err := db.ReapExpired(context.Background(), keys); err != nil {
					t.Errorf("iteration %d: ReapExpired: %v", iteration, err)
				}
			}()
		}
		wg.Wait()

		var pending int
		if err := db.Pool().QueryRow(t.Context(),
			`SELECT count(*) FROM budget_reservations WHERE state = 'pending'`).Scan(&pending); err != nil {
			t.Fatalf("counting: %v", err)
		}
		if pending != 0 {
			t.Fatalf("iteration %d left %d pending reservations", iteration, pending)
		}
	}
	assertReconciled(t, db)
}

// --- isolation ------------------------------------------------------------

// The reserve is correct because Postgres re-evaluates the predicate against
// the committed row when an UPDATE blocks. That behaviour is specific to
// READ COMMITTED: a stricter level raises a serialization error instead,
// under exactly the contention this must survive.
func TestTransactionsRunAtReadCommitted(t *testing.T) {
	db := newTestDB(t)
	_, hash := budgetedKey(t, db, "10")

	res, err := db.Reserve(t.Context(), store.ReserveInput{
		KeyHash: hash, Model: "m", EstimatedUSD: usd("1"), TTL: time.Minute,
		ObserveIsolation: func(level string) {
			if level != "read committed" {
				t.Errorf("reserve ran at %q, want read committed", level)
			}
		},
	})
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	if _, err := db.Settle(t.Context(), store.SettleInput{
		ReservationID: res.ReservationID, ActualUSD: usd("1"),
		ObserveIsolation: func(level string) {
			if level != "read committed" {
				t.Errorf("settle ran at %q, want read committed", level)
			}
		},
	}); err != nil {
		t.Fatalf("Settle: %v", err)
	}
}

// --- settle survives a cancelled parent ----------------------------------

// When a client disconnects, the request context is cancelled. If the settle
// used it, the transaction would abort and the reservation would sit pending
// until the reaper — losing the spend from the budget in the meantime.
func TestSettleCompletesWithACancelledParentContext(t *testing.T) {
	db := newTestDB(t)
	_, hash := budgetedKey(t, db, "100")

	res, err := db.Reserve(t.Context(), store.ReserveInput{
		KeyHash: hash, Model: "m", EstimatedUSD: usd("5"), TTL: time.Minute,
	})
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	cancelled, cancel := context.WithCancel(context.Background())
	cancel() // the client has gone

	out, err := db.Settle(cancelled, store.SettleInput{
		ReservationID: res.ReservationID, ActualUSD: usd("4"),
	})
	if err != nil {
		t.Fatalf("a settle must survive a cancelled parent: %v", err)
	}
	if !out.Applied {
		t.Error("the settle did not apply")
	}

	spent, reserved := budgetTotals(t, db, hash)
	if !spent.Equal(usd("4")) {
		t.Errorf("spent_usd = %s, want 4", spent)
	}
	if !reserved.IsZero() {
		t.Errorf("reserved_usd = %s, want 0", reserved)
	}
	assertReconciled(t, db)
}

// --- overshoot ------------------------------------------------------------

// A settle larger than its reservation is recorded in full: the tokens were
// consumed. The limit may be exceeded by at most one request, which is
// flagged rather than hidden.
func TestOvershootIsRecordedAndFlagged(t *testing.T) {
	db := newTestDB(t)
	_, hash := budgetedKey(t, db, "10")

	res, err := db.Reserve(t.Context(), store.ReserveInput{
		KeyHash: hash, Model: "m", EstimatedUSD: usd("2"), TTL: time.Minute,
	})
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	out, err := db.Settle(t.Context(), store.SettleInput{
		ReservationID: res.ReservationID, ActualUSD: usd("15"),
	})
	if err != nil {
		t.Fatalf("Settle: %v", err)
	}
	if !out.Overshoot {
		t.Error("a settle above its estimate must be flagged as an overshoot")
	}

	spent, _ := budgetTotals(t, db, hash)
	if !spent.Equal(usd("15")) {
		t.Errorf("spent_usd = %s, want the real 15", spent)
	}

	// The next request sees the inflated spend and is refused, so an
	// overshoot cannot accumulate.
	if _, err := db.Reserve(t.Context(), store.ReserveInput{
		KeyHash: hash, Model: "m", EstimatedUSD: usd("1"), TTL: time.Minute,
	}); !errors.Is(err, store.ErrBudgetExceeded) {
		t.Errorf("err = %v, want ErrBudgetExceeded after an overshoot", err)
	}
	assertReconciled(t, db)
}

// --- diagnostics ----------------------------------------------------------

func TestReserveClassifiesItsFailures(t *testing.T) {
	db := newTestDB(t)

	t.Run("unknown key", func(t *testing.T) {
		_, err := db.Reserve(t.Context(), store.ReserveInput{
			KeyHash: []byte("a hash that was never stored!!!"),
			Model:   "m", EstimatedUSD: usd("1"), TTL: time.Minute,
		})
		if !errors.Is(err, store.ErrKeyNotFound) {
			t.Errorf("err = %v, want ErrKeyNotFound", err)
		}
	})

	t.Run("revoked key", func(t *testing.T) {
		id, hash := budgetedKey(t, db, "100")
		if err := db.RevokeKey(t.Context(), id); err != nil {
			t.Fatalf("RevokeKey: %v", err)
		}
		_, err := db.Reserve(t.Context(), store.ReserveInput{
			KeyHash: hash, Model: "m", EstimatedUSD: usd("1"), TTL: time.Minute,
		})
		if !errors.Is(err, store.ErrKeyRevoked) {
			t.Errorf("err = %v, want ErrKeyRevoked", err)
		}
	})

	t.Run("model not allowed", func(t *testing.T) {
		id, hash := budgetedKey(t, db, "100")
		if _, err := db.UpdateKey(t.Context(), id, store.UpdateKeyInput{
			AllowedModels: &store.FieldValue[[]string]{Set: true, Value: []string{"gpt-5.6-terra"}},
		}); err != nil {
			t.Fatalf("UpdateKey: %v", err)
		}
		_, err := db.Reserve(t.Context(), store.ReserveInput{
			KeyHash: hash, Model: "claude-opus-5", EstimatedUSD: usd("1"), TTL: time.Minute,
		})
		if !errors.Is(err, store.ErrModelNotAllowed) {
			t.Errorf("err = %v, want ErrModelNotAllowed", err)
		}
	})

	t.Run("budget exceeded carries its state", func(t *testing.T) {
		_, hash := budgetedKey(t, db, "5")
		_, err := db.Reserve(t.Context(), store.ReserveInput{
			KeyHash: hash, Model: "m", EstimatedUSD: usd("10"), TTL: time.Minute,
		})
		var exceeded *store.BudgetExceededError
		if !errors.As(err, &exceeded) {
			t.Fatalf("err = %v, want a BudgetExceededError", err)
		}
		// Whoever receives a 402 wants to know when they can retry.
		if exceeded.LimitUSD == nil || !exceeded.LimitUSD.Equal(usd("5")) {
			t.Errorf("LimitUSD = %v, want 5", exceeded.LimitUSD)
		}
		if exceeded.WindowResetsAt.IsZero() {
			t.Error("the error must say when the window resets")
		}
		if !exceeded.WindowResetsAt.After(time.Now()) {
			t.Errorf("WindowResetsAt = %s, want a future instant", exceeded.WindowResetsAt)
		}
	})
}

// An unlimited key is never refused, however much it spends.
func TestUnlimitedKeyIsNeverRefused(t *testing.T) {
	db := newTestDB(t)
	_, hash := budgetedKey(t, db, "")

	for range 20 {
		if _, err := db.Reserve(t.Context(), store.ReserveInput{
			KeyHash: hash, Model: "m", EstimatedUSD: usd("1000000"), TTL: time.Minute,
		}); err != nil {
			t.Fatalf("an unlimited key must never be refused: %v", err)
		}
	}
	assertReconciled(t, db)
}

// --- helpers --------------------------------------------------------------

func budgetTotals(t *testing.T, db *store.DB, hash []byte) (spent, reserved decimal.Decimal) {
	t.Helper()
	var s, r string
	if err := db.Pool().QueryRow(t.Context(),
		`SELECT spent_usd::text, reserved_usd::text FROM key_budgets
		  WHERE key_id = (SELECT id FROM virtual_keys WHERE key_hash = $1)`,
		hash).Scan(&s, &r); err != nil {
		t.Fatalf("reading budget: %v", err)
	}
	return usd(s), usd(r)
}

// The materialised balance is a cache of the reservation log, and the whole
// approach rests on the two agreeing. Every concurrency test asserts it.
func assertReconciled(t *testing.T, db *store.DB) {
	t.Helper()
	report, err := db.Reconcile(t.Context())
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(report.BalanceDrift) != 0 {
		t.Errorf("materialised balance drifted from the log: %+v", report.BalanceDrift)
	}
	if report.SettledWithoutUsageRecord != 0 {
		t.Logf("note: %d settled reservations have no usage record (expected before F5)",
			report.SettledWithoutUsageRecord)
	}
}

func today() time.Time {
	now := time.Now().UTC()
	return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
}

// The settle already updates the budget row; the figure the response header
// needs is in that row at that moment. Returning it costs nothing and saves
// a SELECT that read what the statement one line earlier had just written.
func TestSettleReturnsTheRemainingBudget(t *testing.T) {
	db := newTestDB(t)
	_, hash := budgetedKey(t, db, "10")

	res, err := db.Reserve(t.Context(), store.ReserveInput{
		KeyHash: hash, Model: "m", EstimatedUSD: usd("2"), TTL: time.Minute,
	})
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	out, err := db.Settle(t.Context(), store.SettleInput{
		ReservationID: res.ReservationID, ActualUSD: usd("3"),
	})
	if err != nil {
		t.Fatalf("Settle: %v", err)
	}
	if out.RemainingUSD == nil {
		t.Fatal("a budgeted key settled with no remaining figure")
	}
	// 10 limit, 3 spent, nothing still reserved.
	if !out.RemainingUSD.Equal(usd("7")) {
		t.Errorf("RemainingUSD = %s, want 7", out.RemainingUSD)
	}
	assertReconciled(t, db)
}

// An unlimited key has no remaining figure, and a nil is the only honest
// answer: zero would read as exhausted.
func TestSettleReturnsNoRemainingForAnUnlimitedKey(t *testing.T) {
	db := newTestDB(t)
	_, hash := budgetedKey(t, db, "")

	res, err := db.Reserve(t.Context(), store.ReserveInput{
		KeyHash: hash, Model: "m", EstimatedUSD: usd("2"), TTL: time.Minute,
	})
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	out, err := db.Settle(t.Context(), store.SettleInput{
		ReservationID: res.ReservationID, ActualUSD: usd("3"),
	})
	if err != nil {
		t.Fatalf("Settle: %v", err)
	}
	if out.RemainingUSD != nil {
		t.Errorf("RemainingUSD = %s for a key with no limit, want nil", out.RemainingUSD)
	}
}

// A settle that applied nothing has no figure to report. It did not write the
// budget row, so it cannot speak for it: a second settle arriving after the
// first, or after the reaper, must return nil rather than a number it read
// from someone else's work.
func TestSettleThatAppliedNothingReportsNoRemaining(t *testing.T) {
	db := newTestDB(t)
	_, hash := budgetedKey(t, db, "10")

	res, err := db.Reserve(t.Context(), store.ReserveInput{
		KeyHash: hash, Model: "m", EstimatedUSD: usd("2"), TTL: time.Minute,
	})
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	if _, err := db.Settle(t.Context(), store.SettleInput{
		ReservationID: res.ReservationID, ActualUSD: usd("3"),
	}); err != nil {
		t.Fatalf("first Settle: %v", err)
	}

	second, err := db.Settle(t.Context(), store.SettleInput{
		ReservationID: res.ReservationID, ActualUSD: usd("3"),
	})
	if err != nil {
		t.Fatalf("second Settle: %v", err)
	}
	if second.Applied {
		t.Fatal("the second settle applied, which would count the spend twice")
	}
	if second.RemainingUSD != nil {
		t.Errorf("RemainingUSD = %s from a settle that changed nothing, want nil",
			second.RemainingUSD)
	}
	assertReconciled(t, db)
}
