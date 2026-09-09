-- +goose Up
CREATE TABLE budget_reservations (
    id                    uuid           PRIMARY KEY,
    key_id                uuid           NOT NULL REFERENCES virtual_keys (id) ON DELETE CASCADE,
    -- The window in force when the reservation was taken. Spend belongs to
    -- the window of the reserve, not of the settle, so a reservation
    -- spanning a month boundary settles into the month it started in.
    window_start          date           NOT NULL,
    state                 text           NOT NULL,
    estimated_usd         numeric(20,10) NOT NULL,
    actual_usd            numeric(20,10),
    expires_at            timestamptz    NOT NULL,
    overshoot             boolean        NOT NULL DEFAULT false,
    settled_after_expiry  boolean        NOT NULL DEFAULT false,
    created_at            timestamptz    NOT NULL DEFAULT now(),
    settled_at            timestamptz,

    -- A CHECK rather than an enum: adding a state later is a one-line
    -- migration here and an awkward one against a Postgres enum type.
    CONSTRAINT budget_reservations_state_check
        CHECK (state IN ('pending', 'settled', 'expired')),
    CONSTRAINT budget_reservations_estimated_non_negative CHECK (estimated_usd >= 0),
    CONSTRAINT budget_reservations_actual_non_negative
        CHECK (actual_usd IS NULL OR actual_usd >= 0),
    -- A settled reservation has an amount and a timestamp; anything else
    -- is a half-applied settle.
    CONSTRAINT budget_reservations_settled_is_complete
        CHECK (state <> 'settled' OR (actual_usd IS NOT NULL AND settled_at IS NOT NULL))
);

-- The reaper scans pending reservations only, never the whole history.
CREATE INDEX budget_reservations_pending_expiry_idx
    ON budget_reservations (expires_at)
    WHERE state = 'pending';

CREATE INDEX budget_reservations_key_window_idx
    ON budget_reservations (key_id, window_start);

-- +goose Down
DROP TABLE budget_reservations;
