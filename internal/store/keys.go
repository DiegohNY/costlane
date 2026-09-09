package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"
)

// ErrKeyNotFound reports a key that does not exist.
var ErrKeyNotFound = errors.New("store: key not found")

// maxMetadataBytes caps the attribution map. Metadata drives the spend
// aggregations, which stay simple queries only while it stays small and flat.
const maxMetadataBytes = 4 << 10

// KeyRecord is a virtual key as stored. It never carries the secret, which
// exists only in the response that creates it.
type KeyRecord struct {
	ID               uuid.UUID
	Prefix           string
	Label            string
	Metadata         map[string]string
	AllowedModels    []string
	DisconnectPolicy string
	DrainTimeoutMS   *int
	// A decimal rather than a formatted string: NUMERIC(20,10) renders as
	// "5.0000000000", and leaving every consumer to normalise that invites
	// exactly the comparison bugs this type exists to prevent.
	LimitUSD  *decimal.Decimal
	RevokedAt *time.Time
	CreatedAt time.Time

	// Hash is populated only by lookups that need it, never by listings.
	Hash []byte
}

// CreateKeyInput describes a key to create.
type CreateKeyInput struct {
	Hash             []byte
	Prefix           string
	Label            string
	Metadata         map[string]string
	AllowedModels    []string
	DisconnectPolicy string
	DrainTimeoutMS   *int
	LimitUSD         *decimal.Decimal
}

// FieldValue expresses the three intentions a JSON field can carry: absent,
// explicitly null, or a value. Conflating them would make it impossible to
// clear a field, or to say "all models" as opposed to "none".
type FieldValue[T any] struct {
	Set   bool
	Null  bool
	Value T
}

// UpdateKeyInput describes a partial update. A nil pointer means the field
// was absent from the request and must not be touched.
type UpdateKeyInput struct {
	Label            *string
	Metadata         *FieldValue[map[string]string]
	AllowedModels    *FieldValue[[]string]
	DisconnectPolicy *string
	DrainTimeoutMS   *FieldValue[int]
	LimitUSD         *FieldValue[decimal.Decimal]

	// MaxDrainMS bounds a per-key drain timeout, mirroring the boot-time
	// validation. A drain window wider than the request it drains cannot be
	// honoured, whether it is configured globally or per key.
	MaxDrainMS int
}

// CreateKey inserts a key and its budget in one transaction.
//
// Both rows or neither: a key without a budget row would make the fused
// reserve return zero rows, which the diagnostic classifies as an unknown
// key — an error that lies about what went wrong.
func (db *DB) CreateKey(ctx context.Context, in CreateKeyInput) (KeyRecord, error) {
	if err := validateMetadata(in.Metadata); err != nil {
		return KeyRecord{}, err
	}
	if in.DisconnectPolicy == "" {
		in.DisconnectPolicy = "cancel"
	}

	metadata, err := marshalMetadata(in.Metadata)
	if err != nil {
		return KeyRecord{}, err
	}

	tx, err := db.write.Begin(ctx)
	if err != nil {
		return KeyRecord{}, fmt.Errorf("store: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	id := uuid.New()
	if _, err := tx.Exec(ctx,
		`INSERT INTO virtual_keys
		   (id, key_hash, key_prefix, label, metadata, allowed_models,
		    disconnect_policy, drain_timeout_ms)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		id, in.Hash, in.Prefix, in.Label, metadata, in.AllowedModels,
		in.DisconnectPolicy, in.DrainTimeoutMS); err != nil {
		return KeyRecord{}, fmt.Errorf("store: inserting key: %w", err)
	}

	if _, err := tx.Exec(ctx,
		`INSERT INTO key_budgets (key_id, limit_usd) VALUES ($1, $2)`,
		id, in.LimitUSD); err != nil {
		return KeyRecord{}, fmt.Errorf("store: inserting budget: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return KeyRecord{}, fmt.Errorf("store: commit: %w", err)
	}
	return db.keyByID(ctx, id)
}

// FindKeyByHash looks a key up by the hash of the presented credential.
// Revoked keys are returned so the caller can distinguish "unknown" from
// "revoked" rather than reporting both as the same failure.
func (db *DB) FindKeyByHash(ctx context.Context, hash []byte) (KeyRecord, error) {
	return db.scanKey(ctx, db.write.QueryRow(ctx, keySelect+` WHERE vk.key_hash = $1`, hash), true)
}

func (db *DB) keyByID(ctx context.Context, id uuid.UUID) (KeyRecord, error) {
	return db.scanKey(ctx, db.write.QueryRow(ctx, keySelect+` WHERE vk.id = $1`, id), false)
}

// ListKeys returns every key without secrets or hashes. A stored hash is
// enough to verify a guess offline, so it never travels.
func (db *DB) ListKeys(ctx context.Context) ([]KeyRecord, error) {
	rows, err := db.read.Query(ctx, keySelect+` ORDER BY vk.created_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("store: listing keys: %w", err)
	}
	defer rows.Close()

	var out []KeyRecord
	for rows.Next() {
		rec, err := scanKeyRow(rows, false)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

// RevokeKey marks a key revoked. It is idempotent: revoking twice satisfies
// the caller's intent both times, and reporting an error on the second would
// make a retry look like a failure.
func (db *DB) RevokeKey(ctx context.Context, id uuid.UUID) error {
	tag, err := db.write.Exec(ctx,
		`UPDATE virtual_keys SET revoked_at = COALESCE(revoked_at, now()) WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("store: revoking: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrKeyNotFound
	}
	return nil
}

// UpdateKey applies a partial update.
func (db *DB) UpdateKey(ctx context.Context, id uuid.UUID, in UpdateKeyInput) (KeyRecord, error) {
	if in.Metadata != nil && in.Metadata.Set && !in.Metadata.Null {
		if err := validateMetadata(in.Metadata.Value); err != nil {
			return KeyRecord{}, err
		}
	}
	if in.DrainTimeoutMS != nil && in.DrainTimeoutMS.Set && !in.DrainTimeoutMS.Null {
		if in.MaxDrainMS > 0 && in.DrainTimeoutMS.Value > in.MaxDrainMS {
			return KeyRecord{}, fmt.Errorf(
				"store: drain_timeout_ms (%d) must not exceed the provider timeout (%d)",
				in.DrainTimeoutMS.Value, in.MaxDrainMS)
		}
		if in.DrainTimeoutMS.Value <= 0 {
			return KeyRecord{}, errors.New("store: drain_timeout_ms must be positive")
		}
	}

	tx, err := db.write.Begin(ctx)
	if err != nil {
		return KeyRecord{}, fmt.Errorf("store: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	var exists bool
	if err := tx.QueryRow(ctx,
		`SELECT true FROM virtual_keys WHERE id = $1 FOR UPDATE`, id).Scan(&exists); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return KeyRecord{}, ErrKeyNotFound
		}
		return KeyRecord{}, fmt.Errorf("store: locking key: %w", err)
	}

	if in.Label != nil {
		if _, err := tx.Exec(ctx,
			`UPDATE virtual_keys SET label = $2 WHERE id = $1`, id, *in.Label); err != nil {
			return KeyRecord{}, fmt.Errorf("store: updating label: %w", err)
		}
	}
	if in.DisconnectPolicy != nil {
		if _, err := tx.Exec(ctx,
			`UPDATE virtual_keys SET disconnect_policy = $2 WHERE id = $1`,
			id, *in.DisconnectPolicy); err != nil {
			return KeyRecord{}, fmt.Errorf("store: updating disconnect policy: %w", err)
		}
	}
	if f := in.Metadata; f != nil && f.Set {
		value := []byte(`{}`)
		if !f.Null {
			if value, err = marshalMetadata(f.Value); err != nil {
				return KeyRecord{}, err
			}
		}
		if _, err := tx.Exec(ctx,
			`UPDATE virtual_keys SET metadata = $2 WHERE id = $1`, id, value); err != nil {
			return KeyRecord{}, fmt.Errorf("store: updating metadata: %w", err)
		}
	}
	if f := in.AllowedModels; f != nil && f.Set {
		// NULL means every model; an empty array means none. They are
		// opposite intentions and must not collapse into one another.
		var value any
		if !f.Null {
			value = f.Value
			if f.Value == nil {
				value = []string{}
			}
		}
		if _, err := tx.Exec(ctx,
			`UPDATE virtual_keys SET allowed_models = $2 WHERE id = $1`, id, value); err != nil {
			return KeyRecord{}, fmt.Errorf("store: updating allowed models: %w", err)
		}
	}
	if f := in.DrainTimeoutMS; f != nil && f.Set {
		var value any
		if !f.Null {
			value = f.Value
		}
		if _, err := tx.Exec(ctx,
			`UPDATE virtual_keys SET drain_timeout_ms = $2 WHERE id = $1`, id, value); err != nil {
			return KeyRecord{}, fmt.Errorf("store: updating drain timeout: %w", err)
		}
	}
	if f := in.LimitUSD; f != nil && f.Set {
		// Lowering a limit below committed spend is allowed: in-flight
		// requests settle and new ones are refused. Refusing the change
		// would leave an operator unable to stop a runaway key.
		var value any
		if !f.Null {
			value = f.Value
		}
		if _, err := tx.Exec(ctx,
			`UPDATE key_budgets SET limit_usd = $2 WHERE key_id = $1`, id, value); err != nil {
			return KeyRecord{}, fmt.Errorf("store: updating limit: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return KeyRecord{}, fmt.Errorf("store: commit: %w", err)
	}
	return db.keyByID(ctx, id)
}

const keySelect = `
	SELECT vk.id, vk.key_prefix, vk.label, vk.metadata, vk.allowed_models,
	       vk.disconnect_policy, vk.drain_timeout_ms, kb.limit_usd::text,
	       vk.revoked_at, vk.created_at, vk.key_hash
	  FROM virtual_keys vk
	  LEFT JOIN key_budgets kb ON kb.key_id = vk.id`

type scanner interface {
	Scan(dest ...any) error
}

func (db *DB) scanKey(_ context.Context, row scanner, withHash bool) (KeyRecord, error) {
	rec, err := scanKeyRow(row, withHash)
	if errors.Is(err, pgx.ErrNoRows) {
		return KeyRecord{}, ErrKeyNotFound
	}
	return rec, err
}

func scanKeyRow(row scanner, withHash bool) (KeyRecord, error) {
	var (
		rec      KeyRecord
		metadata []byte
		hash     []byte
		limit    *string
	)
	if err := row.Scan(&rec.ID, &rec.Prefix, &rec.Label, &metadata, &rec.AllowedModels,
		&rec.DisconnectPolicy, &rec.DrainTimeoutMS, &limit,
		&rec.RevokedAt, &rec.CreatedAt, &hash); err != nil {
		return KeyRecord{}, err
	}
	if limit != nil {
		d, err := decimal.NewFromString(*limit)
		if err != nil {
			return KeyRecord{}, fmt.Errorf("store: decoding limit: %w", err)
		}
		rec.LimitUSD = &d
	}
	if len(metadata) > 0 {
		if err := json.Unmarshal(metadata, &rec.Metadata); err != nil {
			return KeyRecord{}, fmt.Errorf("store: decoding metadata: %w", err)
		}
	}
	if withHash {
		rec.Hash = hash
	}
	return rec, nil
}

// validateMetadata keeps the attribution map flat and small, so the spend
// aggregations stay ordinary queries rather than jsonb traversals.
func validateMetadata(m map[string]string) error {
	if len(m) == 0 {
		return nil
	}
	for k := range m {
		if k == "" {
			return errors.New("store: metadata keys must not be empty")
		}
	}
	encoded, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("store: encoding metadata: %w", err)
	}
	if len(encoded) > maxMetadataBytes {
		return fmt.Errorf("store: metadata is %d bytes, over the %d byte limit",
			len(encoded), maxMetadataBytes)
	}
	return nil
}

func marshalMetadata(m map[string]string) ([]byte, error) {
	if m == nil {
		return []byte(`{}`), nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("store: encoding metadata: %w", err)
	}
	return b, nil
}
