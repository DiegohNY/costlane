package store_test

import (
	"testing"
	"time"

	"github.com/DiegohNY/costlane/internal/pricing"
	"github.com/DiegohNY/costlane/internal/store"
	"github.com/DiegohNY/costlane/internal/usage"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// A batch is one statement: it either lands whole or not at all, so
// reconciliation never counts a partial success as a gap.
func TestWriteBatchPersistsEveryRecord(t *testing.T) {
	db := newTestDB(t)
	keyID, _ := budgetedKey(t, db, "100")

	cost := decimal.RequireFromString("0.0025")
	records := make([]usage.Record, 0, 50)
	for i := range 50 {
		records = append(records, usage.Record{
			ID: uuid.New(), KeyID: keyID, RequestID: uuid.NewString(),
			RequestedModel: "gpt-6-astra", ServedModel: "gpt-6-astra",
			Provider: "openai", WindowStart: today(),
			TokenDetail: pricing.Counts{
				pricing.KindInput: int64(100 + i), pricing.KindOutput: 20,
			},
			CostUSD: &cost, UsageSource: "provider", StatusCode: 200,
		})
	}

	if err := db.WriteBatch(t.Context(), records); err != nil {
		t.Fatalf("WriteBatch: %v", err)
	}

	var n int
	if err := db.Pool().QueryRow(t.Context(),
		`SELECT count(*) FROM usage_records`).Scan(&n); err != nil {
		t.Fatalf("counting: %v", err)
	}
	if n != 50 {
		t.Errorf("%d records persisted, want 50", n)
	}

	// The aggregates must still be derived from the detail.
	var mismatches int
	if err := db.Pool().QueryRow(t.Context(), `
		SELECT count(*) FROM usage_records
		 WHERE input_tokens <> COALESCE((token_detail->>'input')::bigint, 0)`).
		Scan(&mismatches); err != nil {
		t.Fatalf("reconciling: %v", err)
	}
	if mismatches != 0 {
		t.Errorf("%d records whose aggregates disagree with their detail", mismatches)
	}
}

// One bad record must not let the rest through silently: a partial batch
// would be a gap that looks like a crash.
func TestWriteBatchIsAllOrNothing(t *testing.T) {
	db := newTestDB(t)
	keyID, _ := budgetedKey(t, db, "100")

	good := usage.Record{
		ID: uuid.New(), KeyID: keyID, RequestID: "ok",
		RequestedModel: "m", Provider: "openai", WindowStart: today(),
		UsageSource: "provider", StatusCode: 200,
	}
	// An invalid usage_source, which the CHECK constraint rejects.
	bad := good
	bad.ID = uuid.New()
	bad.RequestID = "bad"
	bad.UsageSource = "invented"

	if err := db.WriteBatch(t.Context(), []usage.Record{good, bad}); err == nil {
		t.Fatal("a batch containing an invalid record must fail")
	}

	var n int
	if err := db.Pool().QueryRow(t.Context(),
		`SELECT count(*) FROM usage_records`).Scan(&n); err != nil {
		t.Fatalf("counting: %v", err)
	}
	if n != 0 {
		t.Errorf("%d records survived a failed batch: it was not atomic", n)
	}
}

func TestWriteBatchOfNothingIsFine(t *testing.T) {
	db := newTestDB(t)
	if err := db.WriteBatch(t.Context(), nil); err != nil {
		t.Errorf("an empty batch must not be an error: %v", err)
	}
}

// Payloads and records are kept for different periods, because they answer
// different questions and carry different risk: a record is an accounting
// entry, while a stored prompt is a copy of a customer's data.
func TestRetentionKeepsRecordsLongerThanPayloads(t *testing.T) {
	db := newTestDB(t)
	keyID, _ := budgetedKey(t, db, "100")

	old := usage.Record{
		ID: uuid.New(), KeyID: keyID, RequestID: "old",
		RequestedModel: "m", Provider: "openai", WindowStart: today(),
		UsageSource: "provider", StatusCode: 200,
		CreatedAt: time.Now().UTC().Add(-72 * time.Hour),
	}
	recent := old
	recent.ID = uuid.New()
	recent.RequestID = "recent"
	recent.CreatedAt = time.Now().UTC().Add(-time.Hour)
	// The shorter payload retention is what makes a stored prompt outlive
	// its debugging session by hours rather than by days.

	if err := db.WriteBatchAt(t.Context(), []usage.Record{old, recent}); err != nil {
		t.Fatalf("seeding: %v", err)
	}
	for _, id := range []uuid.UUID{old.ID, recent.ID} {
		if _, err := db.Pool().Exec(t.Context(),
			`INSERT INTO request_payloads (usage_record_id, request_body, created_at)
			 VALUES ($1, '{"prompt":"secret"}', (SELECT created_at FROM usage_records WHERE id = $1))`,
			id); err != nil {
			t.Fatalf("seeding payload: %v", err)
		}
	}

	// Payloads are kept two hours and records two days. Of the two rows,
	// only the 72-hour-old one is past either threshold — so its payload
	// goes and its record goes, while the hour-old pair stays untouched.
	result, err := db.ApplyRetention(t.Context(), store.RetentionPolicy{
		RequestPayloads: 2 * time.Hour,
		UsageRecords:    48 * time.Hour,
	})
	if err != nil {
		t.Fatalf("ApplyRetention: %v", err)
	}
	if result.RequestPayloads != 1 {
		t.Errorf("%d payloads pruned, want the one past two hours", result.RequestPayloads)
	}
	if result.UsageRecords != 1 {
		t.Errorf("%d records pruned, want only the 72-hour-old one", result.UsageRecords)
	}

	var records, payloads int
	if err := db.Pool().QueryRow(t.Context(),
		`SELECT (SELECT count(*) FROM usage_records),
		        (SELECT count(*) FROM request_payloads)`).Scan(&records, &payloads); err != nil {
		t.Fatalf("counting: %v", err)
	}
	if records != 1 || payloads != 1 {
		t.Errorf("%d records and %d payloads survive, want the recent pair",
			records, payloads)
	}
}

// Retention with no policy set must do nothing rather than empty the table.
func TestRetentionWithNoPolicyDeletesNothing(t *testing.T) {
	db := newTestDB(t)
	keyID, _ := budgetedKey(t, db, "100")

	if err := db.WriteBatch(t.Context(), []usage.Record{{
		ID: uuid.New(), KeyID: keyID, RequestID: "keep",
		RequestedModel: "m", Provider: "openai", WindowStart: today(),
		UsageSource: "provider", StatusCode: 200,
	}}); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	result, err := db.ApplyRetention(t.Context(), store.RetentionPolicy{})
	if err != nil {
		t.Fatalf("ApplyRetention: %v", err)
	}
	if result.UsageRecords != 0 || result.RequestPayloads != 0 {
		t.Errorf("an empty policy deleted %+v", result)
	}
}
