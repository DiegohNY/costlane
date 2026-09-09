-- +goose Up
CREATE TABLE usage_records (
    id                   uuid           PRIMARY KEY,
    key_id               uuid           NOT NULL REFERENCES virtual_keys (id) ON DELETE CASCADE,
    -- No foreign key to budget_reservations: reconciliation measures
    -- settled reservations that never produced a usage record, and a
    -- constraint would prevent recording the very gap it should surface.
    reservation_id       uuid,
    request_id           text           NOT NULL,
    requested_model      text           NOT NULL,
    served_model         text,
    provider             text           NOT NULL,
    -- The tier the provider reports having served. Recorded alongside
    -- served_model for the same reason: cost follows what ran.
    service_tier         text           NOT NULL DEFAULT 'standard',
    -- The provider's own request id: the only key for disputing an invoice
    -- or opening a ticket, so it is captured even on failure.
    provider_request_id  text,
    window_start         date           NOT NULL,

    -- The exact per-kind token counts, and the sole input to the cost
    -- calculation. Providers keep inventing billing classes — a cache
    -- write charge here, a 1-hour cache TTL there — and a jsonb map
    -- absorbs a new one without a migration.
    token_detail         jsonb          NOT NULL DEFAULT '{}',

    -- Aggregates derived from token_detail, for reporting only. cache_write
    -- is the sum of both TTLs; reasoning is already counted inside output.
    -- Never used to compute cost.
    input_tokens         bigint         NOT NULL DEFAULT 0,
    cached_read_tokens   bigint         NOT NULL DEFAULT 0,
    cache_write_tokens   bigint         NOT NULL DEFAULT 0,
    output_tokens        bigint         NOT NULL DEFAULT 0,
    -- Billed as output by every provider, so it does not change the
    -- arithmetic. Recorded because "why did this cost three times as much"
    -- is a question the product exists to answer.
    reasoning_tokens     bigint         NOT NULL DEFAULT 0,

    -- NULL means unpriced, never free.
    cost_usd             numeric(20,10),
    usage_source         text           NOT NULL,
    -- No price at all for this model: cost is unknown, never zero.
    unpriced             boolean        NOT NULL DEFAULT false,
    -- Some token kinds were priced and others were not. The request spent
    -- real money, so charging zero would be the largest budget hole in the
    -- system; charging only the known part, and saying so, is the one
    -- fail-closed option. token_detail carries unpriced_kinds.
    partially_priced     boolean        NOT NULL DEFAULT false,
    streamed             boolean        NOT NULL DEFAULT false,
    client_disconnected  boolean        NOT NULL DEFAULT false,
    drain_timeout        boolean        NOT NULL DEFAULT false,
    parse_errors         integer        NOT NULL DEFAULT 0,
    finish_reason        text,
    status_code          integer        NOT NULL,
    -- Separate from status_code: a stream that fails after headers are
    -- sent still reports 200 to the client.
    error_code           text,
    latency_ms           integer,
    ttft_ms              integer,
    created_at           timestamptz    NOT NULL DEFAULT now(),

    CONSTRAINT usage_records_usage_source_check
        CHECK (usage_source IN ('provider', 'tokenizer', 'estimate')),
    CONSTRAINT usage_records_service_tier_check CHECK (service_tier IN (
        'standard', 'flex', 'priority', 'fast', 'batch', 'geo_us'
    )),
    CONSTRAINT usage_records_tokens_non_negative CHECK (
        input_tokens >= 0 AND cached_read_tokens >= 0
        AND cache_write_tokens >= 0 AND output_tokens >= 0
        AND reasoning_tokens >= 0
    ),
    CONSTRAINT usage_records_cost_non_negative
        CHECK (cost_usd IS NULL OR cost_usd >= 0),
    CONSTRAINT usage_records_parse_errors_non_negative CHECK (parse_errors >= 0),
    CONSTRAINT usage_records_unpriced_has_no_cost
        CHECK ((unpriced AND cost_usd IS NULL) OR (NOT unpriced)),
    -- Wholly unpriced and partially priced are different states: the first
    -- has no cost, the second has a cost that is known to be incomplete.
    CONSTRAINT usage_records_partial_excludes_unpriced
        CHECK (NOT (partially_priced AND unpriced)),
    CONSTRAINT usage_records_partial_has_a_cost
        CHECK (NOT partially_priced OR cost_usd IS NOT NULL),
    CONSTRAINT usage_records_token_detail_is_object
        CHECK (jsonb_typeof(token_detail) = 'object')
);

-- BRIN on an append-only table costs a fraction of a btree and is what range
-- queries need; btree carries the per-key access path.
CREATE INDEX usage_records_created_brin_idx ON usage_records USING brin (created_at);
CREATE INDEX usage_records_key_created_idx  ON usage_records (key_id, created_at DESC);
CREATE INDEX usage_records_window_idx       ON usage_records (window_start);

-- Prompts live apart so the table can be dropped without touching spend
-- queries, and only exist when logging is explicitly enabled.
CREATE TABLE request_payloads (
    usage_record_id uuid        PRIMARY KEY REFERENCES usage_records (id) ON DELETE CASCADE,
    request_body    jsonb,
    response_body   jsonb,
    created_at      timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE request_payloads;
DROP TABLE usage_records;
