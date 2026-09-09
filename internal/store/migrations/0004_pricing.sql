-- +goose Up
-- Prices are stored long: one row per (model, provider, token kind, context
-- tier, validity window). A wide table would need a migration every time a
-- provider invents a billing class, and they do — OpenAI added a cache-write
-- charge on GPT-5.6, and Anthropic prices cache writes differently for a
-- 5-minute and a 1-hour TTL. Here each of those is a row.
CREATE TABLE model_prices (
    id                 uuid           PRIMARY KEY,
    model              text           NOT NULL,
    provider           text           NOT NULL,
    -- A closed list guarded by a CHECK rather than an enum: adding a kind
    -- is one line here, and an awkward migration against an enum type.
    token_kind         text           NOT NULL,
    -- The tier that was SERVED, not the one requested. OpenAI echoes
    -- service_tier in its response, and pricing follows what actually ran,
    -- the same way it follows served_model rather than requested_model.
    service_tier       text           NOT NULL DEFAULT 'standard',
    usd_per_mtok       numeric(20,10) NOT NULL,
    -- Context tiers, as a half-open range. OpenAI prices long context at
    -- twice the short rate; Gemini has its own boundary.
    input_tokens_from  bigint         NOT NULL DEFAULT 0,
    input_tokens_to    bigint,
    effective_from     timestamptz    NOT NULL,
    effective_until    timestamptz,
    -- Provenance is mandatory: a rate nobody can trace back to a published
    -- page is a rate nobody can defend.
    source_url         text           NOT NULL,
    fetched_at         timestamptz    NOT NULL,
    created_at         timestamptz    NOT NULL DEFAULT now(),

    CONSTRAINT model_prices_token_kind_check CHECK (token_kind IN (
        'input',
        'output',
        'cached_read',
        'cache_write_5m',
        'cache_write_1h',
        'reasoning',
        'audio_input',
        'audio_output'
    )),
    CONSTRAINT model_prices_service_tier_check CHECK (service_tier IN (
        'standard',
        'flex',
        'priority',
        'fast',
        'batch',
        'geo_us'
    )),
    CONSTRAINT model_prices_rate_non_negative CHECK (usd_per_mtok >= 0),
    CONSTRAINT model_prices_tier_bounds CHECK (
        input_tokens_from >= 0
        AND (input_tokens_to IS NULL OR input_tokens_to > input_tokens_from)
    ),
    CONSTRAINT model_prices_validity_ordered
        CHECK (effective_until IS NULL OR effective_until > effective_from),
    CONSTRAINT model_prices_source_url_present CHECK (length(source_url) > 0)
);

-- The invariant lives in the database, not in the loader. Two rows for the
-- same model, provider and token kind may not cover the same tier over the
-- same period.
ALTER TABLE model_prices ADD CONSTRAINT model_prices_no_overlap
    EXCLUDE USING gist (
        model        WITH =,
        provider     WITH =,
        token_kind   WITH =,
        service_tier WITH =,
        int8range(input_tokens_from, input_tokens_to, '[)') WITH &&,
        tstzrange(effective_from, effective_until, '[)')    WITH &&
    );

CREATE INDEX model_prices_lookup_idx
    ON model_prices (model, provider, service_tier, token_kind, effective_from DESC);

CREATE TABLE model_aliases (
    alias            text        NOT NULL,
    canonical_model  text        NOT NULL,
    provider         text        NOT NULL,
    effective_from   timestamptz NOT NULL,
    effective_until  timestamptz,
    created_at       timestamptz NOT NULL DEFAULT now(),

    -- Historised: an undated name has pointed at different snapshots over
    -- time, and a request from March must resolve with March's mapping.
    -- Note that from the Claude 4.6 generation on, a dateless id is itself
    -- the pinned snapshot rather than a moving pointer, so not every
    -- undated name needs an alias row at all.
    PRIMARY KEY (alias, effective_from),

    CONSTRAINT model_aliases_validity_ordered
        CHECK (effective_until IS NULL OR effective_until > effective_from)
);

ALTER TABLE model_aliases ADD CONSTRAINT model_aliases_no_overlap
    EXCLUDE USING gist (
        alias    WITH =,
        provider WITH =,
        tstzrange(effective_from, effective_until, '[)') WITH &&
    );

-- +goose Down
DROP TABLE model_aliases;
DROP TABLE model_prices;
