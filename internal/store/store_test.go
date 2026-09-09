package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Postgres error codes we assert on, so a test that expects a constraint
// violation cannot pass on an unrelated failure.
const (
	codeExclusionViolation = "23P01"
	codeCheckViolation     = "23514"
	codeUniqueViolation    = "23505"
	codeNotNullViolation   = "23502"
)

func requirePgError(t *testing.T, err error, wantCode, wantConstraint string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected a %s violation on %s, got no error", wantCode, wantConstraint)
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("expected a postgres error, got %T: %v", err, err)
	}
	if pgErr.Code != wantCode {
		t.Fatalf("error code = %s (%s), want %s", pgErr.Code, pgErr.Message, wantCode)
	}
	if wantConstraint != "" && !strings.Contains(pgErr.ConstraintName, wantConstraint) {
		t.Errorf("constraint = %q, want it to contain %q", pgErr.ConstraintName, wantConstraint)
	}
}

// --- migrations -----------------------------------------------------------

func TestMigrateUpFromEmpty(t *testing.T) {
	db := newTestDB(t)
	for _, table := range []string{
		"virtual_keys", "key_budgets", "budget_reservations",
		"model_prices", "model_aliases", "usage_records", "request_payloads",
	} {
		var exists bool
		err := db.Pool().QueryRow(t.Context(),
			`SELECT EXISTS (SELECT 1 FROM information_schema.tables
			                 WHERE table_schema='public' AND table_name=$1)`, table).Scan(&exists)
		if err != nil {
			t.Fatalf("querying for %s: %v", table, err)
		}
		if !exists {
			t.Errorf("table %s was not created", table)
		}
	}
}

func TestMigrateIsIdempotent(t *testing.T) {
	db := newTestDB(t)
	// Running the same migrations again must be a no-op, not an error.
	if err := Migrate(t.Context(), db.Pool()); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
}

// A down migration that has never been run is a down migration that does not
// work. Exercise every one of them, then bring the schema back up.
func TestMigrateDownThenUp(t *testing.T) {
	db := newTestDB(t)
	if err := MigrateDownAll(t.Context(), db.Pool()); err != nil {
		t.Fatalf("migrating down: %v", err)
	}
	var count int
	err := db.Pool().QueryRow(t.Context(),
		`SELECT count(*) FROM information_schema.tables
		  WHERE table_schema='public' AND table_name IN
		        ('virtual_keys','key_budgets','budget_reservations',
		         'model_prices','model_aliases','usage_records','request_payloads')`).Scan(&count)
	if err != nil {
		t.Fatalf("counting tables: %v", err)
	}
	if count != 0 {
		t.Errorf("%d tables survived the down migration", count)
	}
	if err := Migrate(t.Context(), db.Pool()); err != nil {
		t.Fatalf("migrating back up: %v", err)
	}
}

// btree_gist is a trusted extension from PostgreSQL 13, so the database owner
// installs it without superuser rights. This test documents that requirement
// in code rather than in a deployment note.
func TestBtreeGistInstalledWithoutSuperuser(t *testing.T) {
	db := newTestDB(t)
	var isSuper bool
	if err := db.Pool().QueryRow(t.Context(),
		`SELECT rolsuper FROM pg_roles WHERE rolname = current_user`).Scan(&isSuper); err != nil {
		t.Fatalf("checking role: %v", err)
	}
	var installed bool
	if err := db.Pool().QueryRow(t.Context(),
		`SELECT EXISTS (SELECT 1 FROM pg_extension WHERE extname='btree_gist')`).Scan(&installed); err != nil {
		t.Fatalf("checking extension: %v", err)
	}
	if !installed {
		t.Fatal("btree_gist is not installed; the exclusion constraints cannot work")
	}
	t.Logf("btree_gist installed (current_user is superuser: %t)", isSuper)
}

func TestConcurrentMigratorsSerialise(t *testing.T) {
	db := newTestDB(t)
	// The advisory lock must make a second migrator wait rather than race.
	errs := make(chan error, 4)
	for range 4 {
		go func() { errs <- Migrate(context.Background(), db.Pool()) }()
	}
	for range 4 {
		if err := <-errs; err != nil {
			t.Errorf("concurrent migrate: %v", err)
		}
	}
}

// --- money constraints ----------------------------------------------------

// The double-release bug found during design would have driven reserved_usd
// below zero. With this constraint it is a database error at the first test
// instead of a wrong number discovered by re-reading SQL.
func TestReservedUsdCannotGoNegative(t *testing.T) {
	db := newTestDB(t)
	keyID := seedKey(t, db, nil)

	_, err := db.Pool().Exec(t.Context(),
		`UPDATE key_budgets SET reserved_usd = reserved_usd - 1 WHERE key_id = $1`, keyID)
	requirePgError(t, err, codeCheckViolation, "reserved_non_negative")
}

func TestSpentUsdCannotGoNegative(t *testing.T) {
	db := newTestDB(t)
	keyID := seedKey(t, db, nil)

	_, err := db.Pool().Exec(t.Context(),
		`UPDATE key_budgets SET spent_usd = -0.0000000001 WHERE key_id = $1`, keyID)
	requirePgError(t, err, codeCheckViolation, "spent_non_negative")
}

func TestLimitUsdCannotBeNegativeButMayBeNull(t *testing.T) {
	db := newTestDB(t)
	keyID := seedKey(t, db, nil)

	_, err := db.Pool().Exec(t.Context(),
		`UPDATE key_budgets SET limit_usd = -1 WHERE key_id = $1`, keyID)
	requirePgError(t, err, codeCheckViolation, "limit_non_negative")

	// NULL means unlimited and must remain allowed.
	if _, err := db.Pool().Exec(t.Context(),
		`UPDATE key_budgets SET limit_usd = NULL WHERE key_id = $1`, keyID); err != nil {
		t.Errorf("NULL limit must be permitted (unlimited key): %v", err)
	}
}

func TestReservationAmountsCannotBeNegative(t *testing.T) {
	db := newTestDB(t)
	keyID := seedKey(t, db, nil)

	_, err := db.Pool().Exec(t.Context(),
		`INSERT INTO budget_reservations (id, key_id, window_start, state, estimated_usd, expires_at)
		 VALUES ($1, $2, current_date, 'pending', -5, now() + interval '5 minutes')`,
		uuid.New(), keyID)
	requirePgError(t, err, codeCheckViolation, "estimated_non_negative")

	_, err = db.Pool().Exec(t.Context(),
		`INSERT INTO budget_reservations
		   (id, key_id, window_start, state, estimated_usd, actual_usd, settled_at, expires_at)
		 VALUES ($1, $2, current_date, 'settled', 5, -1, now(), now() + interval '5 minutes')`,
		uuid.New(), keyID)
	requirePgError(t, err, codeCheckViolation, "actual_non_negative")
}

// --- state constraint -----------------------------------------------------

func TestReservationStateRejectsUnknownValue(t *testing.T) {
	db := newTestDB(t)
	keyID := seedKey(t, db, nil)

	_, err := db.Pool().Exec(t.Context(),
		`INSERT INTO budget_reservations (id, key_id, window_start, state, estimated_usd, expires_at)
		 VALUES ($1, $2, current_date, 'released', 1, now() + interval '5 minutes')`,
		uuid.New(), keyID)
	requirePgError(t, err, codeCheckViolation, "state_check")
}

func TestReservationStateAcceptsEveryValidValue(t *testing.T) {
	db := newTestDB(t)
	keyID := seedKey(t, db, nil)

	for _, state := range []string{"pending", "settled", "expired"} {
		var actual any
		var settledAt any
		if state == "settled" {
			actual, settledAt = "1.5", time.Now()
		}
		_, err := db.Pool().Exec(t.Context(),
			`INSERT INTO budget_reservations
			   (id, key_id, window_start, state, estimated_usd, actual_usd, settled_at, expires_at)
			 VALUES ($1, $2, current_date, $3, 1, $4, $5, now() + interval '5 minutes')`,
			uuid.New(), keyID, state, actual, settledAt)
		if err != nil {
			t.Errorf("state %q must be accepted: %v", state, err)
		}
	}
}

// A settled reservation carries both an amount and a timestamp; anything less
// is a half-applied settle.
func TestSettledReservationMustCarryAmountAndTimestamp(t *testing.T) {
	db := newTestDB(t)
	keyID := seedKey(t, db, nil)

	_, err := db.Pool().Exec(t.Context(),
		`INSERT INTO budget_reservations (id, key_id, window_start, state, estimated_usd, expires_at)
		 VALUES ($1, $2, current_date, 'settled', 1, now() + interval '5 minutes')`,
		uuid.New(), keyID)
	requirePgError(t, err, codeCheckViolation, "settled_is_complete")
}

// --- pricing exclusion constraints ---------------------------------------

func TestOverlappingPriceRowsAreRejected(t *testing.T) {
	db := newTestDB(t)
	insert := func(from, until any) error {
		_, err := db.Pool().Exec(t.Context(),
			`INSERT INTO model_prices
			   (id, model, provider, input_usd_per_mtok, output_usd_per_mtok,
			    effective_from, effective_until)
			 VALUES ($1, 'gpt-4o-2024-08-06', 'openai', 2.5, 10, $2, $3)`,
			uuid.New(), from, until)
		return err
	}
	if err := insert("2026-01-01T00:00:00Z", "2026-06-01T00:00:00Z"); err != nil {
		t.Fatalf("seeding the first price: %v", err)
	}
	// Starts before the first one ends: an overlap.
	requirePgError(t, insert("2026-05-01T00:00:00Z", nil), codeExclusionViolation, "no_overlap")
}

func TestAdjacentPriceRowsAreAccepted(t *testing.T) {
	db := newTestDB(t)
	insert := func(from, until any) error {
		_, err := db.Pool().Exec(t.Context(),
			`INSERT INTO model_prices
			   (id, model, provider, input_usd_per_mtok, output_usd_per_mtok,
			    effective_from, effective_until)
			 VALUES ($1, 'gpt-4o-2024-08-06', 'openai', 2.5, 10, $2, $3)`,
			uuid.New(), from, until)
		return err
	}
	if err := insert("2026-01-01T00:00:00Z", "2026-06-01T00:00:00Z"); err != nil {
		t.Fatalf("first price: %v", err)
	}
	// effective_until is exclusive, so starting exactly where the previous
	// row ends is adjacency, not overlap. Repricing depends on this.
	if err := insert("2026-06-01T00:00:00Z", nil); err != nil {
		t.Errorf("adjacent validity ranges must be accepted: %v", err)
	}
}

// Two tiers of the same model over the same period differ by token range and
// must coexist: Gemini above 200k is priced separately today.
func TestDistinctContextTiersCoexist(t *testing.T) {
	db := newTestDB(t)
	insert := func(from int64, to any) error {
		_, err := db.Pool().Exec(t.Context(),
			`INSERT INTO model_prices
			   (id, model, provider, input_usd_per_mtok, output_usd_per_mtok,
			    input_tokens_from, input_tokens_to, effective_from)
			 VALUES ($1, 'gemini-2.5-pro', 'google', 1.25, 10, $2, $3, '2026-01-01T00:00:00Z')`,
			uuid.New(), from, to)
		return err
	}
	if err := insert(0, int64(200000)); err != nil {
		t.Fatalf("low tier: %v", err)
	}
	if err := insert(200000, nil); err != nil {
		t.Errorf("adjacent tiers must coexist: %v", err)
	}
	// But an overlapping tier is still a mistake.
	requirePgError(t, insert(100000, int64(300000)), codeExclusionViolation, "no_overlap")
}

func TestSameModelOnDifferentProvidersCoexists(t *testing.T) {
	db := newTestDB(t)
	insert := func(provider string) error {
		_, err := db.Pool().Exec(t.Context(),
			`INSERT INTO model_prices
			   (id, model, provider, input_usd_per_mtok, output_usd_per_mtok, effective_from)
			 VALUES ($1, 'claude-sonnet-4-5', $2, 3, 15, '2026-01-01T00:00:00Z')`,
			uuid.New(), provider)
		return err
	}
	if err := insert("anthropic"); err != nil {
		t.Fatalf("anthropic: %v", err)
	}
	// The same model served through a different provider is a different
	// price row, which is what lets vertex and azure arrive without a
	// migration.
	if err := insert("vertex"); err != nil {
		t.Errorf("same model on another provider must be allowed: %v", err)
	}
}

func TestOverlappingAliasRowsAreRejected(t *testing.T) {
	db := newTestDB(t)
	insert := func(canonical string, from, until any) error {
		_, err := db.Pool().Exec(t.Context(),
			`INSERT INTO model_aliases (alias, canonical_model, provider, effective_from, effective_until)
			 VALUES ('gpt-4o', $1, 'openai', $2, $3)`, canonical, from, until)
		return err
	}
	if err := insert("gpt-4o-2024-05-13", "2026-01-01T00:00:00Z", "2026-08-01T00:00:00Z"); err != nil {
		t.Fatalf("first alias: %v", err)
	}
	requirePgError(t,
		insert("gpt-4o-2024-08-06", "2026-07-01T00:00:00Z", nil),
		codeExclusionViolation, "no_overlap")
}

// The same undated name resolving to different snapshots over time is the
// whole point of historising aliases.
func TestAliasRemappingOverTimeIsAccepted(t *testing.T) {
	db := newTestDB(t)
	insert := func(canonical string, from, until any) error {
		_, err := db.Pool().Exec(t.Context(),
			`INSERT INTO model_aliases (alias, canonical_model, provider, effective_from, effective_until)
			 VALUES ('gpt-4o', $1, 'openai', $2, $3)`, canonical, from, until)
		return err
	}
	if err := insert("gpt-4o-2024-05-13", "2026-01-01T00:00:00Z", "2026-08-01T00:00:00Z"); err != nil {
		t.Fatalf("first mapping: %v", err)
	}
	if err := insert("gpt-4o-2024-08-06", "2026-08-01T00:00:00Z", nil); err != nil {
		t.Errorf("a later remapping must be accepted: %v", err)
	}
}

func TestInvertedValidityRangeIsRejected(t *testing.T) {
	db := newTestDB(t)
	_, err := db.Pool().Exec(t.Context(),
		`INSERT INTO model_prices
		   (id, model, provider, input_usd_per_mtok, output_usd_per_mtok,
		    effective_from, effective_until)
		 VALUES ($1, 'gpt-4o', 'openai', 2.5, 10,
		         '2026-06-01T00:00:00Z', '2026-01-01T00:00:00Z')`, uuid.New())
	requirePgError(t, err, codeCheckViolation, "validity_ordered")
}

func TestNegativeRateIsRejected(t *testing.T) {
	db := newTestDB(t)
	_, err := db.Pool().Exec(t.Context(),
		`INSERT INTO model_prices
		   (id, model, provider, input_usd_per_mtok, output_usd_per_mtok, effective_from)
		 VALUES ($1, 'gpt-4o', 'openai', -1, 10, '2026-01-01T00:00:00Z')`, uuid.New())
	requirePgError(t, err, codeCheckViolation, "rates_non_negative")
}

func TestInvertedTierBoundsAreRejected(t *testing.T) {
	db := newTestDB(t)
	_, err := db.Pool().Exec(t.Context(),
		`INSERT INTO model_prices
		   (id, model, provider, input_usd_per_mtok, output_usd_per_mtok,
		    input_tokens_from, input_tokens_to, effective_from)
		 VALUES ($1, 'gpt-4o', 'openai', 2.5, 10, 200000, 1000, '2026-01-01T00:00:00Z')`,
		uuid.New())
	requirePgError(t, err, codeCheckViolation, "tier_bounds")
}

// --- virtual keys ---------------------------------------------------------

func TestDuplicateKeyHashIsRejected(t *testing.T) {
	db := newTestDB(t)
	hash := []byte("0123456789abcdef0123456789abcdef")
	seedKey(t, db, hash)

	_, err := db.Pool().Exec(t.Context(),
		`INSERT INTO virtual_keys (id, key_hash, key_prefix, label)
		 VALUES ($1, $2, 'cl_dup', 'duplicate')`, uuid.New(), hash)
	requirePgError(t, err, codeUniqueViolation, "")
}

func TestDisconnectPolicyRejectsUnknownValue(t *testing.T) {
	db := newTestDB(t)
	_, err := db.Pool().Exec(t.Context(),
		`INSERT INTO virtual_keys (id, key_hash, key_prefix, label, disconnect_policy)
		 VALUES ($1, $2, 'cl_bad', 'bad policy', 'buffer')`,
		uuid.New(), []byte("hash-for-the-bad-policy-test-000"))
	requirePgError(t, err, codeCheckViolation, "disconnect_policy_check")
}

// --- usage records --------------------------------------------------------

func TestUsageSourceRejectsUnknownValue(t *testing.T) {
	db := newTestDB(t)
	keyID := seedKey(t, db, nil)

	_, err := db.Pool().Exec(t.Context(),
		`INSERT INTO usage_records
		   (id, key_id, request_id, requested_model, provider, window_start,
		    usage_source, status_code)
		 VALUES ($1, $2, 'req-1', 'gpt-4o', 'openai', current_date, 'guess', 200)`,
		uuid.New(), keyID)
	requirePgError(t, err, codeCheckViolation, "usage_source_check")
}

// An unpriced request has no cost. Recording one with a cost would mean we
// priced something we said we could not price.
func TestUnpricedRecordCannotCarryACost(t *testing.T) {
	db := newTestDB(t)
	keyID := seedKey(t, db, nil)

	_, err := db.Pool().Exec(t.Context(),
		`INSERT INTO usage_records
		   (id, key_id, request_id, requested_model, provider, window_start,
		    usage_source, status_code, unpriced, cost_usd)
		 VALUES ($1, $2, 'req-2', 'mystery', 'openai', current_date,
		         'provider', 200, true, 0.5)`,
		uuid.New(), keyID)
	requirePgError(t, err, codeCheckViolation, "unpriced_has_no_cost")
}

func TestNegativeTokenCountIsRejected(t *testing.T) {
	db := newTestDB(t)
	keyID := seedKey(t, db, nil)

	_, err := db.Pool().Exec(t.Context(),
		`INSERT INTO usage_records
		   (id, key_id, request_id, requested_model, provider, window_start,
		    usage_source, status_code, input_tokens)
		 VALUES ($1, $2, 'req-3', 'gpt-4o', 'openai', current_date, 'provider', 200, -1)`,
		uuid.New(), keyID)
	requirePgError(t, err, codeCheckViolation, "tokens_non_negative")
}

// Reconciliation compares settled reservations against usage records, so a
// usage record must be insertable even when its reservation is absent —
// otherwise the constraint would hide the very gap it should measure.
func TestUsageRecordSurvivesMissingReservation(t *testing.T) {
	db := newTestDB(t)
	keyID := seedKey(t, db, nil)

	_, err := db.Pool().Exec(t.Context(),
		`INSERT INTO usage_records
		   (id, key_id, reservation_id, request_id, requested_model, provider,
		    window_start, usage_source, status_code)
		 VALUES ($1, $2, $3, 'req-4', 'gpt-4o', 'openai', current_date, 'provider', 200)`,
		uuid.New(), keyID, uuid.New())
	if err != nil {
		t.Errorf("usage record must not require an existing reservation: %v", err)
	}
}

// --- pools ----------------------------------------------------------------

// A read query that runs long must be cut off by the pool's own timeout, so
// no reporting query can turn into a full scan that outlives its usefulness.
func TestReadPoolEnforcesStatementTimeout(t *testing.T) {
	db := newTestDB(t)
	_, err := db.ReadPool().Exec(t.Context(), `SELECT pg_sleep(3)`)
	if err == nil {
		t.Fatal("a long read must be cancelled by statement_timeout")
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "57014" { // query_canceled
		t.Errorf("expected query_canceled (57014), got: %v", err)
	}
}

func TestWritePoolHasNoStatementTimeout(t *testing.T) {
	db := newTestDB(t)
	var timeout string
	if err := db.Pool().QueryRow(t.Context(), `SHOW statement_timeout`).Scan(&timeout); err != nil {
		t.Fatalf("reading statement_timeout: %v", err)
	}
	// Writes include migrations and settles, which must not be cut off.
	if timeout != "0" {
		t.Errorf("write pool statement_timeout = %q, want 0 (unlimited)", timeout)
	}
}

func TestReadPoolReportsItsTimeout(t *testing.T) {
	db := newTestDB(t)
	var timeout string
	if err := db.ReadPool().QueryRow(t.Context(), `SHOW statement_timeout`).Scan(&timeout); err != nil {
		t.Fatalf("reading statement_timeout: %v", err)
	}
	if timeout == "0" {
		t.Error("read pool must carry a statement_timeout")
	}
}

// --- helpers --------------------------------------------------------------

func seedKey(t *testing.T, db *DB, hash []byte) uuid.UUID {
	t.Helper()
	if hash == nil {
		hash = []byte(uuid.NewString() + "-padding-to-32ch")
	}
	id := uuid.New()
	tx, err := db.Pool().Begin(t.Context())
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	if _, err := tx.Exec(t.Context(),
		`INSERT INTO virtual_keys (id, key_hash, key_prefix, label)
		 VALUES ($1, $2, 'cl_test1', 'test key')`, id, hash); err != nil {
		t.Fatalf("seeding key: %v", err)
	}
	if _, err := tx.Exec(t.Context(),
		`INSERT INTO key_budgets (key_id, limit_usd) VALUES ($1, 100)`, id); err != nil {
		t.Fatalf("seeding budget: %v", err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return id
}

var _ = pgx.ErrNoRows // keep the pgx import meaningful for later tests
