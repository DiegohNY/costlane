package store_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/DiegohNY/costlane/internal/auth"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/DiegohNY/costlane/internal/store"
)

func newKeyInput(t *testing.T) (auth.Key, store.CreateKeyInput) {
	t.Helper()
	key, err := auth.NewKey()
	if err != nil {
		t.Fatalf("generating: %v", err)
	}
	return key, store.CreateKeyInput{
		Hash:   key.Hash,
		Prefix: key.Prefix,
		Label:  "test key",
	}
}

// Creating a key must leave a budget row behind it. Without one, the fused
// reserve of F4 returns zero rows and the diagnostic classifies it as 401 —
// an error that lies about what went wrong.
func TestCreateKeyAlsoCreatesItsBudget(t *testing.T) {
	db := newTestDB(t)
	_, in := newKeyInput(t)

	rec, err := db.CreateKey(t.Context(), in)
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}

	var budgets int
	if err := db.Pool().QueryRow(t.Context(),
		`SELECT count(*) FROM key_budgets WHERE key_id = $1`, rec.ID).Scan(&budgets); err != nil {
		t.Fatalf("counting budgets: %v", err)
	}
	if budgets != 1 {
		t.Errorf("a new key has %d budget rows, want exactly 1", budgets)
	}
}

// If either insert fails the other must not survive: a half-created key is
// the state that produces the lying 401.
func TestFailedCreateLeavesNothingBehind(t *testing.T) {
	db := newTestDB(t)
	key, in := newKeyInput(t)
	if _, err := db.CreateKey(t.Context(), in); err != nil {
		t.Fatalf("first create: %v", err)
	}

	// The same hash again violates the unique index, failing the first
	// statement of the transaction.
	duplicate := in
	duplicate.Label = "duplicate"
	if _, err := db.CreateKey(t.Context(), duplicate); err == nil {
		t.Fatal("creating a key with a duplicate hash must fail")
	}

	var keys int
	if err := db.Pool().QueryRow(t.Context(),
		`SELECT count(*) FROM virtual_keys WHERE key_hash = $1`, key.Hash).Scan(&keys); err != nil {
		t.Fatalf("counting keys: %v", err)
	}
	if keys != 1 {
		t.Errorf("%d keys share the hash, want the original 1", keys)
	}
}

func TestFindKeyByHash(t *testing.T) {
	db := newTestDB(t)
	key, in := newKeyInput(t)
	created, err := db.CreateKey(t.Context(), in)
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}

	found, err := db.FindKeyByHash(t.Context(), key.Hash)
	if err != nil {
		t.Fatalf("FindKeyByHash: %v", err)
	}
	if found.ID != created.ID {
		t.Errorf("found %s, want %s", found.ID, created.ID)
	}
	if found.RevokedAt != nil {
		t.Error("a new key must not be revoked")
	}
}

func TestFindUnknownKeyReportsNotFound(t *testing.T) {
	db := newTestDB(t)
	_, err := db.FindKeyByHash(t.Context(), []byte("a hash that was never stored 00"))
	if !errors.Is(err, store.ErrKeyNotFound) {
		t.Errorf("err = %v, want store.ErrKeyNotFound", err)
	}
}

// Revocation is soft, so spend history stays readable and attributed.
func TestRevokeIsSoftAndIdempotent(t *testing.T) {
	db := newTestDB(t)
	key, in := newKeyInput(t)
	created, _ := db.CreateKey(t.Context(), in)

	if err := db.RevokeKey(t.Context(), created.ID); err != nil {
		t.Fatalf("first revoke: %v", err)
	}
	// A second revoke is not an error: the caller's intent is already
	// satisfied, and reporting 404 would make retries look like failures.
	if err := db.RevokeKey(t.Context(), created.ID); err != nil {
		t.Errorf("second revoke must succeed, got: %v", err)
	}

	found, err := db.FindKeyByHash(t.Context(), key.Hash)
	if err != nil {
		t.Fatalf("a revoked key must still be findable: %v", err)
	}
	if found.RevokedAt == nil {
		t.Error("the key is not marked revoked")
	}

	// The row survives, so historical usage stays attributed.
	var rows int
	if err := db.Pool().QueryRow(t.Context(),
		`SELECT count(*) FROM virtual_keys WHERE id = $1`, created.ID).Scan(&rows); err != nil {
		t.Fatalf("counting: %v", err)
	}
	if rows != 1 {
		t.Error("revocation must not delete the row")
	}
}

func TestRevokingAnUnknownKeyReportsNotFound(t *testing.T) {
	db := newTestDB(t)
	if err := db.RevokeKey(t.Context(), uuid.New()); !errors.Is(err, store.ErrKeyNotFound) {
		t.Errorf("err = %v, want store.ErrKeyNotFound", err)
	}
}

// --- metadata -------------------------------------------------------------

// Metadata drives the spend aggregations of F7, which stay simple queries
// only while the shape stays a flat string map.
func TestMetadataAcceptsAFlatStringMap(t *testing.T) {
	db := newTestDB(t)
	_, in := newKeyInput(t)
	in.Metadata = map[string]string{"team": "search", "customer": "acme", "feature": "autocomplete"}

	rec, err := db.CreateKey(t.Context(), in)
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	if rec.Metadata["team"] != "search" || rec.Metadata["customer"] != "acme" {
		t.Errorf("metadata came back as %v", rec.Metadata)
	}
}

func TestOversizedMetadataIsRejected(t *testing.T) {
	db := newTestDB(t)
	_, in := newKeyInput(t)
	in.Metadata = map[string]string{"blob": strings.Repeat("x", 5000)}

	_, err := db.CreateKey(t.Context(), in)
	if err == nil {
		t.Fatal("metadata beyond the size limit must be rejected")
	}
	if !strings.Contains(err.Error(), "metadata") {
		t.Errorf("the error should name the offending field, got: %v", err)
	}
}

func TestEmptyMetadataKeyIsRejected(t *testing.T) {
	db := newTestDB(t)
	_, in := newKeyInput(t)
	in.Metadata = map[string]string{"": "value"}

	if _, err := db.CreateKey(t.Context(), in); err == nil {
		t.Fatal("an empty metadata key must be rejected")
	}
}

// --- update ---------------------------------------------------------------

// Absent, null and empty are three different intentions, and conflating them
// would make it impossible to clear a field or to say "everything".
func TestUpdateDistinguishesAbsentFromNullFromEmpty(t *testing.T) {
	db := newTestDB(t)
	_, in := newKeyInput(t)
	in.AllowedModels = []string{"gpt-5.6-terra"}
	created, err := db.CreateKey(t.Context(), in)
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}

	// Absent: leave it alone.
	after, err := db.UpdateKey(t.Context(), created.ID, store.UpdateKeyInput{Label: strPtr("renamed")})
	if err != nil {
		t.Fatalf("UpdateKey: %v", err)
	}
	if len(after.AllowedModels) != 1 || after.AllowedModels[0] != "gpt-5.6-terra" {
		t.Errorf("an absent field was modified: %v", after.AllowedModels)
	}
	if after.Label != "renamed" {
		t.Errorf("Label = %q, want renamed", after.Label)
	}

	// Empty list: no model is permitted.
	after, err = db.UpdateKey(t.Context(), created.ID, store.UpdateKeyInput{
		AllowedModels: &store.FieldValue[[]string]{Set: true, Value: []string{}},
	})
	if err != nil {
		t.Fatalf("UpdateKey: %v", err)
	}
	if after.AllowedModels == nil {
		t.Error("an empty list means no models, which is not the same as NULL")
	}
	if len(after.AllowedModels) != 0 {
		t.Errorf("AllowedModels = %v, want empty", after.AllowedModels)
	}

	// Null: every model is permitted.
	after, err = db.UpdateKey(t.Context(), created.ID, store.UpdateKeyInput{
		AllowedModels: &store.FieldValue[[]string]{Set: true, Null: true},
	})
	if err != nil {
		t.Fatalf("UpdateKey: %v", err)
	}
	if after.AllowedModels != nil {
		t.Errorf("AllowedModels = %v, want nil meaning all models", after.AllowedModels)
	}
}

func TestUpdateLimitDistinguishesNullFromZero(t *testing.T) {
	db := newTestDB(t)
	_, in := newKeyInput(t)
	created, _ := db.CreateKey(t.Context(), in)

	// Null: unlimited.
	after, err := db.UpdateKey(t.Context(), created.ID, store.UpdateKeyInput{
		LimitUSD: &store.FieldValue[decimal.Decimal]{Set: true, Null: true},
	})
	if err != nil {
		t.Fatalf("UpdateKey: %v", err)
	}
	if after.LimitUSD != nil {
		t.Errorf("LimitUSD = %v, want nil meaning unlimited", after.LimitUSD)
	}

	// Zero: a real limit that refuses everything.
	after, err = db.UpdateKey(t.Context(), created.ID, store.UpdateKeyInput{
		LimitUSD: &store.FieldValue[decimal.Decimal]{Set: true, Value: decimal.Zero},
	})
	if err != nil {
		t.Fatalf("UpdateKey: %v", err)
	}
	if after.LimitUSD == nil || !after.LimitUSD.IsZero() {
		t.Errorf("LimitUSD = %v, want a limit of 0", after.LimitUSD)
	}
}

// Lowering a limit below what is already committed is allowed: in-flight
// requests finish and settle, and new ones are refused. Rejecting the change
// would leave an operator unable to stop a runaway key.
func TestLimitMayBeLoweredBelowCommittedSpend(t *testing.T) {
	db := newTestDB(t)
	_, in := newKeyInput(t)
	created, _ := db.CreateKey(t.Context(), in)

	if _, err := db.Pool().Exec(t.Context(),
		`UPDATE key_budgets SET spent_usd = 50, reserved_usd = 10, limit_usd = 100
		  WHERE key_id = $1`, created.ID); err != nil {
		t.Fatalf("seeding spend: %v", err)
	}

	after, err := db.UpdateKey(t.Context(), created.ID, store.UpdateKeyInput{
		LimitUSD: &store.FieldValue[decimal.Decimal]{Set: true, Value: decimal.NewFromInt(5)},
	})
	if err != nil {
		t.Fatalf("lowering the limit must be allowed: %v", err)
	}
	if after.LimitUSD == nil || !after.LimitUSD.Equal(decimal.NewFromInt(5)) {
		t.Errorf("LimitUSD = %v, want 5", after.LimitUSD)
	}

	// The committed figures are untouched: history is not rewritten.
	var spent, reserved string
	if err := db.Pool().QueryRow(t.Context(),
		`SELECT spent_usd::text, reserved_usd::text FROM key_budgets WHERE key_id = $1`,
		created.ID).Scan(&spent, &reserved); err != nil {
		t.Fatalf("reading budget: %v", err)
	}
	if !strings.HasPrefix(spent, "50") || !strings.HasPrefix(reserved, "10") {
		t.Errorf("spend was altered: spent=%s reserved=%s", spent, reserved)
	}
}

// The per-key drain timeout is bounded by the provider timeout for the same
// reason as the global one: a drain window wider than the request it drains
// cannot be honoured.
func TestPerKeyDrainTimeoutIsValidated(t *testing.T) {
	db := newTestDB(t)
	_, in := newKeyInput(t)
	created, _ := db.CreateKey(t.Context(), in)

	_, err := db.UpdateKey(t.Context(), created.ID, store.UpdateKeyInput{
		DrainTimeoutMS: &store.FieldValue[int]{Set: true, Value: 900_000},
		MaxDrainMS:     60_000,
	})
	if err == nil {
		t.Fatal("a drain timeout beyond the provider timeout must be rejected")
	}
	if !strings.Contains(err.Error(), "drain") {
		t.Errorf("the error should name the field, got: %v", err)
	}
}

func TestUpdatingAnUnknownKeyReportsNotFound(t *testing.T) {
	db := newTestDB(t)
	_, err := db.UpdateKey(t.Context(), uuid.New(), store.UpdateKeyInput{Label: strPtr("x")})
	if !errors.Is(err, store.ErrKeyNotFound) {
		t.Errorf("err = %v, want store.ErrKeyNotFound", err)
	}
}

// Listing is for humans choosing a key; it must never carry the secret,
// which exists only in the creation response.
func TestListKeysCarriesNoSecret(t *testing.T) {
	db := newTestDB(t)
	key, in := newKeyInput(t)
	if _, err := db.CreateKey(t.Context(), in); err != nil {
		t.Fatalf("CreateKey: %v", err)
	}

	keys, err := db.ListKeys(t.Context())
	if err != nil {
		t.Fatalf("ListKeys: %v", err)
	}
	if len(keys) != 1 {
		t.Fatalf("listed %d keys, want 1", len(keys))
	}
	if keys[0].Prefix != key.Prefix {
		t.Errorf("Prefix = %q, want %q", keys[0].Prefix, key.Prefix)
	}
	// The stored hash must not travel either: it is enough to verify a
	// guess offline.
	if keys[0].Hash != nil {
		t.Error("a listing must not carry the key hash")
	}
}

func strPtr(s string) *string { return &s }

// Keys are read from the database on every request rather than cached,
// precisely so that a revocation takes effect at once. A cache would create
// a window in which a revoked key still works, which a spend-control product
// cannot have — and it is why prices are cached here but keys are not.
func TestRevocationIsVisibleImmediately(t *testing.T) {
	db := newTestDB(t)
	key, in := newKeyInput(t)
	created, err := db.CreateKey(t.Context(), in)
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}

	before, err := db.FindKeyByHash(t.Context(), key.Hash)
	if err != nil {
		t.Fatalf("FindKeyByHash: %v", err)
	}
	if before.RevokedAt != nil {
		t.Fatal("a fresh key must not be revoked")
	}

	if err := db.RevokeKey(t.Context(), created.ID); err != nil {
		t.Fatalf("RevokeKey: %v", err)
	}

	// The very next lookup, with no delay and no cache invalidation step,
	// must already see the revocation.
	after, err := db.FindKeyByHash(t.Context(), key.Hash)
	if err != nil {
		t.Fatalf("FindKeyByHash after revoke: %v", err)
	}
	if after.RevokedAt == nil {
		t.Error("the revocation was not visible on the next lookup")
	}
}

// A revoked key is still findable, so the caller can answer "revoked" rather
// than "unknown". Both are 401 to the client, but only one is true in the
// logs, and an operator debugging a failing integration needs the difference.
func TestRevokedKeyRemainsDistinguishableFromUnknown(t *testing.T) {
	db := newTestDB(t)
	key, in := newKeyInput(t)
	created, _ := db.CreateKey(t.Context(), in)
	if err := db.RevokeKey(t.Context(), created.ID); err != nil {
		t.Fatalf("RevokeKey: %v", err)
	}

	found, err := db.FindKeyByHash(t.Context(), key.Hash)
	if err != nil {
		t.Fatalf("a revoked key must still be findable: %v", err)
	}
	if found.RevokedAt == nil {
		t.Error("the record does not show the revocation")
	}

	if _, err := db.FindKeyByHash(t.Context(), []byte("never stored this hash at all")); !errors.Is(err, store.ErrKeyNotFound) {
		t.Errorf("an unknown key must report ErrKeyNotFound, got %v", err)
	}
}
