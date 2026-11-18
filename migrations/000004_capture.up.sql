-- Phase 4: capture and automation. Attachments (R2), CSV imports, bank-SMS events, UPI payment attempts.
--
-- Table ownership: attachments -> attachments module; import_* -> imports; sms_events -> bankmsg;
-- upi_payments -> upi. All of them create money only through the transactions module.

-- Two columns the transactions module gains for Phases 4 and 5:
--  * source 'settlement' (a group settlement payment) and 'group' (a group expense) join the allowed sources.
--  * counted_amount: the part of a transaction that counts as the user's own spending in budgets, trackers and
--    analytics. NULL means all of it. A group expense the user paid in full debits the whole amount from the
--    account (the money really left) but counts only the user's own share as spending.
ALTER TABLE transactions DROP CONSTRAINT transactions_source_check;
ALTER TABLE transactions ADD CONSTRAINT transactions_source_check CHECK (source IN
    ('manual', 'upi', 'sms', 'csv', 'recurring', 'group', 'receipt', 'reconcile', 'settlement'));
ALTER TABLE transactions ADD COLUMN counted_amount bigint;
ALTER TABLE transactions ADD CONSTRAINT transactions_counted_amount_check CHECK (counted_amount IS NULL OR (counted_amount >= 0 AND counted_amount <= amount));

CREATE TABLE attachments (
    id               uuid PRIMARY KEY,
    user_id          uuid        NOT NULL REFERENCES users (id) ON DELETE RESTRICT,
    entity_type      text        NOT NULL CHECK (entity_type IN ('transaction')),
    entity_id        uuid        NOT NULL,
    -- u/<user id>/a/<attachment id>: no filename, so no personal text in object keys; the prefix proves ownership.
    object_key       text        NOT NULL UNIQUE,
    original_filename text       NOT NULL DEFAULT '' CHECK (length(original_filename) <= 200),
    mime_type        text        NOT NULL CHECK (mime_type IN ('image/jpeg', 'image/png', 'image/webp', 'application/pdf')),
    size_bytes       bigint      NOT NULL CHECK (size_bytes BETWEEN 1 AND 2097152), -- 2 MB
    checksum_sha256  text        NOT NULL DEFAULT '' CHECK (checksum_sha256 ~ '^([0-9a-f]{64})?$'),
    status           text        NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'uploaded', 'rejected', 'deleted')),
    created_at       timestamptz NOT NULL DEFAULT now(),
    uploaded_at      timestamptz,
    deleted_at       timestamptz,
    object_removed   boolean     NOT NULL DEFAULT false -- the object is gone from storage (cleanup retries until true)
);
CREATE INDEX attachments_entity_idx ON attachments (entity_type, entity_id) WHERE status = 'uploaded';
CREATE INDEX attachments_cleanup_idx ON attachments (created_at) WHERE status = 'pending';
CREATE INDEX attachments_delete_idx ON attachments (created_at) WHERE status IN ('deleted', 'rejected') AND NOT object_removed;

CREATE TABLE import_batches (
    id             uuid PRIMARY KEY,
    user_id        uuid        NOT NULL REFERENCES users (id) ON DELETE RESTRICT,
    account_id     uuid        NOT NULL REFERENCES accounts (id),
    filename       text        NOT NULL DEFAULT '' CHECK (length(filename) <= 200),
    content        text        NOT NULL,
    status         text        NOT NULL DEFAULT 'uploaded' CHECK (status IN ('uploaded', 'mapped', 'confirmed', 'undone')),
    mapping        jsonb,
    total_rows     integer     NOT NULL DEFAULT 0,
    valid_rows     integer     NOT NULL DEFAULT 0,
    invalid_rows   integer     NOT NULL DEFAULT 0,
    duplicate_rows integer     NOT NULL DEFAULT 0,
    total_debit    bigint      NOT NULL DEFAULT 0,
    total_credit   bigint      NOT NULL DEFAULT 0,
    created_count  integer     NOT NULL DEFAULT 0,
    created_at     timestamptz NOT NULL DEFAULT now(),
    confirmed_at   timestamptz,
    undone_at      timestamptz
);
CREATE INDEX import_batches_user_idx ON import_batches (user_id, created_at DESC);

CREATE TABLE import_rows (
    batch_id       uuid    NOT NULL REFERENCES import_batches (id) ON DELETE CASCADE,
    row_no         integer NOT NULL,
    occurred_on    date,
    title          text    NOT NULL DEFAULT '',
    amount         bigint  NOT NULL DEFAULT 0,
    direction      text    NOT NULL DEFAULT '' CHECK (direction IN ('', 'debit', 'credit')),
    reference      text    NOT NULL DEFAULT '',
    category       text    NOT NULL DEFAULT '',
    status         text    NOT NULL CHECK (status IN ('valid', 'invalid', 'duplicate')),
    error          text    NOT NULL DEFAULT '',
    duplicate_of   uuid,
    fingerprint    text    NOT NULL DEFAULT '',
    transaction_id uuid    REFERENCES transactions (id),
    PRIMARY KEY (batch_id, row_no)
);

CREATE TABLE sms_events (
    id                   uuid PRIMARY KEY,
    user_id              uuid        NOT NULL REFERENCES users (id) ON DELETE RESTRICT,
    -- Hash of the message text, computed on the phone. The text itself is never sent to or stored by the server.
    fingerprint          text        NOT NULL CHECK (fingerprint ~ '^[0-9a-f]{64}$'),
    sender               text        NOT NULL DEFAULT '' CHECK (length(sender) <= 40),
    received_at          timestamptz NOT NULL,
    amount               bigint      NOT NULL CHECK (amount > 0),
    direction            text        NOT NULL CHECK (direction IN ('debit', 'credit')),
    currency             char(3)     NOT NULL DEFAULT 'INR',
    account_hint         text        NOT NULL DEFAULT '' CHECK (account_hint ~ '^[0-9A-Za-z]{0,4}$'),
    merchant             text        NOT NULL DEFAULT '' CHECK (length(merchant) <= 120),
    reference            text        NOT NULL DEFAULT '' CHECK (length(reference) <= 60),
    confidence           integer     NOT NULL DEFAULT 0 CHECK (confidence BETWEEN 0 AND 100),
    -- A suggestion only. account_id is set only when the user adds the transaction (never auto-assigned).
    suggested_account_id uuid        REFERENCES accounts (id),
    account_id           uuid        REFERENCES accounts (id),
    status               text        NOT NULL DEFAULT 'review' CHECK (status IN ('review', 'added', 'linked', 'duplicate', 'dismissed')),
    transaction_id       uuid        REFERENCES transactions (id),
    duplicate_of         uuid        REFERENCES transactions (id),
    created_at           timestamptz NOT NULL DEFAULT now()
);
-- The same message processed twice, or synced again, is one event.
CREATE UNIQUE INDEX sms_events_fingerprint_key ON sms_events (user_id, fingerprint);
CREATE INDEX sms_events_review_idx ON sms_events (user_id, created_at DESC) WHERE status = 'review';

CREATE TABLE upi_payments (
    id               uuid PRIMARY KEY,
    user_id          uuid        NOT NULL REFERENCES users (id) ON DELETE RESTRICT,
    account_id       uuid        NOT NULL REFERENCES accounts (id),
    payee_vpa        text        NOT NULL CHECK (payee_vpa ~ '^[A-Za-z0-9._-]{2,255}@[A-Za-z]{2,64}$'),
    payee_name       text        NOT NULL DEFAULT '' CHECK (length(payee_name) <= 120),
    amount           bigint      NOT NULL CHECK (amount > 0),
    currency         char(3)     NOT NULL DEFAULT 'INR',
    note             text        NOT NULL DEFAULT '' CHECK (length(note) <= 120),
    txn_ref          text        NOT NULL DEFAULT '' CHECK (length(txn_ref) <= 60),
    -- started -> handed_off (in the UPI app) -> paid | failed | uncertain (what the USER says). Nothing here is bank-verified
    -- unless bank_verified_at is set by a matching bank SMS. transaction_id is set only once the user's answer is recorded.
    status           text        NOT NULL DEFAULT 'started' CHECK (status IN ('started', 'handed_off', 'paid', 'failed', 'uncertain')),
    transaction_id   uuid        REFERENCES transactions (id),
    bank_verified_at timestamptz,
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX upi_payments_user_idx ON upi_payments (user_id, created_at DESC);
