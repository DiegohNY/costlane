-- +goose Up
CREATE TABLE virtual_keys (
    id                 uuid        PRIMARY KEY,
    -- SHA-256 of the presented key. Verification sits on the critical path
    -- of every request and the key is already high-entropy, so a
    -- deliberately slow KDF would tax the proxy for no gain.
    key_hash           bytea       NOT NULL UNIQUE,
    key_prefix         text        NOT NULL,
    label              text        NOT NULL,
    -- Free-form attribution: team, feature, customer. Aggregated at query
    -- time rather than modelled as a rigid hierarchy.
    metadata           jsonb       NOT NULL DEFAULT '{}',
    allowed_models     text[],
    disconnect_policy  text        NOT NULL DEFAULT 'cancel',
    drain_timeout_ms   integer,
    revoked_at         timestamptz,
    created_at         timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT virtual_keys_disconnect_policy_check
        CHECK (disconnect_policy IN ('cancel', 'drain')),
    CONSTRAINT virtual_keys_drain_timeout_positive
        CHECK (drain_timeout_ms IS NULL OR drain_timeout_ms > 0),
    CONSTRAINT virtual_keys_key_prefix_nonempty
        CHECK (length(key_prefix) > 0)
);

CREATE TABLE key_budgets (
    key_id        uuid           PRIMARY KEY REFERENCES virtual_keys (id) ON DELETE CASCADE,
    -- Budgets run on UTC months. Without an explicit zone the reset would
    -- move with the database session's timezone.
    window_start  date           NOT NULL DEFAULT date_trunc('month', now() AT TIME ZONE 'UTC')::date,
    limit_usd     numeric(20,10),
    spent_usd     numeric(20,10) NOT NULL DEFAULT 0,
    reserved_usd  numeric(20,10) NOT NULL DEFAULT 0,

    -- These three make an accounting bug a database error at the first
    -- test rather than a wrong number found by re-reading SQL. A double
    -- release, for instance, drives reserved_usd below zero.
    CONSTRAINT key_budgets_limit_non_negative    CHECK (limit_usd IS NULL OR limit_usd >= 0),
    CONSTRAINT key_budgets_spent_non_negative    CHECK (spent_usd >= 0),
    CONSTRAINT key_budgets_reserved_non_negative CHECK (reserved_usd >= 0)
);

-- +goose Down
DROP TABLE key_budgets;
DROP TABLE virtual_keys;
