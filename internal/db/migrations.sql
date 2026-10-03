CREATE TABLE IF NOT EXISTS booking_users (
    user_id TEXT PRIMARY KEY
);

CREATE TABLE IF NOT EXISTS shows (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name            TEXT        NOT NULL,
    price_paise     BIGINT      NOT NULL CHECK (price_paise >= 0),
    per_user_limit  INT         NOT NULL DEFAULT 4 CHECK (per_user_limit > 0),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- One row per physical seat. The UNIQUE (show_id, label) constraint makes a
-- seat a unique thing: it can never be duplicated for a show.
CREATE TABLE IF NOT EXISTS seats (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    show_id         UUID        NOT NULL REFERENCES shows(id) ON DELETE CASCADE,
    label           TEXT        NOT NULL,
    status          TEXT        NOT NULL DEFAULT 'available'
                        CHECK (status IN ('available', 'held', 'confirmed')),
    held_by         TEXT,
    reservation_id  UUID,
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (show_id, label)
);

CREATE INDEX IF NOT EXISTS idx_seats_show_status ON seats (show_id, status);
CREATE INDEX IF NOT EXISTS idx_seats_show_heldby ON seats (show_id, held_by);

-- Reservations record the outcome of a reserve call.
-- UNIQUE (user_id, idempotency_key) is the exactly-once guard: a retry with the
-- same key cannot create a second reservation even under full concurrency.
CREATE TABLE IF NOT EXISTS reservations (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    show_id          UUID        NOT NULL REFERENCES shows(id) ON DELETE CASCADE,
    user_id          TEXT        NOT NULL,
    seats            TEXT[]      NOT NULL,
    amount_paise     BIGINT      NOT NULL,
    status           TEXT        NOT NULL CHECK (status IN ('confirmed', 'cancelled')),
    idempotency_key  TEXT        NOT NULL,
    request_hash     TEXT        NOT NULL,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (user_id, idempotency_key)
);

CREATE INDEX IF NOT EXISTS idx_reservations_show_user ON reservations (show_id, user_id);

ALTER TABLE reservations ADD CONSTRAINT reservations_amount_nonnegative CHECK (amount_paise >= 0);
ALTER TABLE reservations ADD CONSTRAINT reservations_nonempty_seats CHECK (cardinality(seats) > 0);
ALTER TABLE reservations ADD CONSTRAINT reservations_show_identity UNIQUE (show_id, id);
ALTER TABLE seats ADD CONSTRAINT seats_reservation_fk FOREIGN KEY (show_id, reservation_id) REFERENCES reservations (show_id, id);
ALTER TABLE seats ADD CONSTRAINT seats_ownership_consistent CHECK (
    (status = 'available' AND held_by IS NULL AND reservation_id IS NULL) OR
    (status IN ('held', 'confirmed') AND held_by IS NOT NULL AND reservation_id IS NOT NULL)
);
