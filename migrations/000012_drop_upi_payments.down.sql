-- Recreates the empty table as it was in 000004. The rows that were dropped are not restored.
CREATE TABLE IF NOT EXISTS upi_payments (
    id               uuid PRIMARY KEY,
    user_id          uuid        NOT NULL REFERENCES users (id) ON DELETE RESTRICT,
    account_id       uuid        NOT NULL REFERENCES accounts (id),
    payee_vpa        text        NOT NULL CHECK (payee_vpa ~ '^[A-Za-z0-9._-]{2,255}@[A-Za-z]{2,64}$'),
    payee_name       text        NOT NULL DEFAULT '' CHECK (length(payee_name) <= 120),
    amount           bigint      NOT NULL CHECK (amount > 0),
    currency         char(3)     NOT NULL DEFAULT 'INR',
    note             text        NOT NULL DEFAULT '' CHECK (length(note) <= 120),
    txn_ref          text        NOT NULL DEFAULT '' CHECK (length(txn_ref) <= 60),
    status           text        NOT NULL DEFAULT 'started' CHECK (status IN ('started', 'handed_off', 'paid', 'failed', 'uncertain')),
    transaction_id   uuid        REFERENCES transactions (id),
    bank_verified_at timestamptz,
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS upi_payments_user_idx ON upi_payments (user_id, created_at DESC);
