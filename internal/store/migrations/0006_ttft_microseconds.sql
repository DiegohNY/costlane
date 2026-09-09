-- +goose Up
-- Time to first byte is the number this gateway is judged on, and on a fast
-- path it is well under a millisecond: rounding to whole milliseconds
-- discards the measurement precisely where it is most flattering, and where
-- a regression would first appear.
ALTER TABLE usage_records ADD COLUMN ttft_us bigint;
ALTER TABLE usage_records ADD CONSTRAINT usage_records_ttft_non_negative
    CHECK (ttft_us IS NULL OR ttft_us >= 0);

-- Backfill from the millisecond column so history stays comparable.
UPDATE usage_records SET ttft_us = ttft_ms * 1000 WHERE ttft_ms IS NOT NULL;

ALTER TABLE usage_records DROP COLUMN ttft_ms;

-- +goose Down
ALTER TABLE usage_records ADD COLUMN ttft_ms integer;
UPDATE usage_records SET ttft_ms = (ttft_us / 1000)::integer WHERE ttft_us IS NOT NULL;
ALTER TABLE usage_records DROP CONSTRAINT usage_records_ttft_non_negative;
ALTER TABLE usage_records DROP COLUMN ttft_us;
