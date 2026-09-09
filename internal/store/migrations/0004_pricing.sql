-- +goose Up
CREATE TABLE model_prices (
    id                        uuid           PRIMARY KEY,
    model                     text           NOT NULL,
    provider                  text           NOT NULL,
    -- Four separate rates. Prompt caching changes the price of input, and
    -- Anthropic prices a cache write differently from a cache read, so a
    -- single averaged rate cannot be correct for either.
    input_usd_per_mtok        numeric(20,10) NOT NULL,
    cached_read_usd_per_mtok  numeric(20,10),
    cache_write_usd_per_mtok  numeric(20,10),
    output_usd_per_mtok       numeric(20,10) NOT NULL,
    max_output_tokens         integer,
    -- Context tiers: Gemini above 200k and Anthropic with extended context
    -- already price by tier today.
    input_tokens_from         bigint         NOT NULL DEFAULT 0,
    input_tokens_to           bigint,
    effective_from            timestamptz    NOT NULL,
    effective_until           timestamptz,
    source_url                text,
    version                   integer        NOT NULL DEFAULT 1,
    created_at                timestamptz    NOT NULL DEFAULT now(),

    CONSTRAINT model_prices_rates_non_negative CHECK (
        input_usd_per_mtok >= 0
        AND output_usd_per_mtok >= 0
        AND (cached_read_usd_per_mtok IS NULL OR cached_read_usd_per_mtok >= 0)
        AND (cache_write_usd_per_mtok IS NULL OR cache_write_usd_per_mtok >= 0)
    ),
    CONSTRAINT model_prices_tier_bounds CHECK (
        input_tokens_from >= 0
        AND (input_tokens_to IS NULL OR input_tokens_to > input_tokens_from)
    ),
    CONSTRAINT model_prices_validity_ordered
        CHECK (effective_until IS NULL OR effective_until > effective_from)
);

-- The invariant lives in the database, not in the loader. A YAML file that
-- introduces an overlap fails at boot with a constraint violation rather
-- than passing because the loading code had a bug.
ALTER TABLE model_prices ADD CONSTRAINT model_prices_no_overlap
    EXCLUDE USING gist (
        model    WITH =,
        provider WITH =,
        int8range(input_tokens_from, input_tokens_to, '[)') WITH &&,
        tstzrange(effective_from, effective_until, '[)')    WITH &&
    );

CREATE INDEX model_prices_lookup_idx ON model_prices (model, provider, effective_from DESC);

CREATE TABLE model_aliases (
    alias            text        NOT NULL,
    canonical_model  text        NOT NULL,
    provider         text        NOT NULL,
    effective_from   timestamptz NOT NULL,
    effective_until  timestamptz,
    created_at       timestamptz NOT NULL DEFAULT now(),

    -- Historised, because an undated name such as gpt-4o has pointed at
    -- different dated snapshots over time, and a request from March must
    -- resolve with March's mapping.
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
