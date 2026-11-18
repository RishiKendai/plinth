-- Phase 2: core ledger. Accounts, categories, tags, transactions, transfers, ledger entries, idempotency.
--
-- Table ownership:
--   accounts          accounts module
--   categories, tags  categories module
--   transactions, transfers, transaction_tags   transactions module
--   ledger_entries    ledger module (append-only; written only through internal/ledger)
--   idempotency_keys  idempotency module
--
-- Balance rule (owner decision, docs/balance-rules.md): pending and confirmed transactions count toward an
-- account's balance. Failed, cancelled and needs-review never do. Leaving a counting state writes a reversing
-- ledger entry; nothing is ever updated or deleted in ledger_entries.

CREATE EXTENSION IF NOT EXISTS pg_trgm;

CREATE TABLE accounts (
    id                 uuid PRIMARY KEY,
    user_id            uuid        NOT NULL REFERENCES users (id) ON DELETE RESTRICT,
    name               text        NOT NULL CHECK (length(name) BETWEEN 1 AND 80),
    type               text        NOT NULL CHECK (type IN ('savings', 'current', 'cash', 'credit_card', 'wallet', 'fixed_deposit', 'other')),
    currency           char(3)     NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    opening_balance    bigint      NOT NULL DEFAULT 0,
    opening_balance_date date      NOT NULL,
    institution        text        NOT NULL DEFAULT '' CHECK (length(institution) <= 80),
    -- Last four characters only. Full account numbers are never stored.
    masked_identifier  text        NOT NULL DEFAULT '' CHECK (masked_identifier ~ '^[0-9A-Za-z]{0,4}$'),
    -- Cached balance = opening_balance + SUM(ledger_entries.amount). Always changed in the same transaction as
    -- the ledger entry; ledger.VerifyAccount proves it.
    balance            bigint      NOT NULL,
    reconciled_at      timestamptz,
    reconciled_balance bigint,
    archived_at        timestamptz,
    version            integer     NOT NULL DEFAULT 1,
    created_at         timestamptz NOT NULL DEFAULT now(),
    updated_at         timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX accounts_user_name_key ON accounts (user_id, lower(name)) WHERE archived_at IS NULL;
CREATE INDEX accounts_user_idx ON accounts (user_id, created_at);

CREATE TABLE categories (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id     uuid REFERENCES users (id) ON DELETE RESTRICT, -- NULL = built-in default, visible to everyone, read-only
    parent_id   uuid REFERENCES categories (id),
    kind        text        NOT NULL CHECK (kind IN ('expense', 'income')),
    name        text        NOT NULL CHECK (length(name) BETWEEN 1 AND 60),
    icon        text        NOT NULL DEFAULT '' CHECK (length(icon) <= 30),
    archived_at timestamptz,
    created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX categories_name_key ON categories (
    coalesce(user_id, '00000000-0000-0000-0000-000000000000'),
    coalesce(parent_id, '00000000-0000-0000-0000-000000000000'),
    kind, lower(name));
CREATE INDEX categories_user_idx ON categories (user_id);

-- Built-in categories. Subcategories hang off a parent of the same kind. A function so tests can re-seed
-- after TRUNCATE ... CASCADE (which also empties this table).
CREATE FUNCTION plinth_seed_categories() RETURNS void LANGUAGE sql AS $fn$
INSERT INTO categories (kind, name, icon) VALUES
    ('expense', 'Food and dining', 'food'), ('expense', 'Groceries', 'cart'), ('expense', 'Transport', 'car'),
    ('expense', 'Housing', 'home'), ('expense', 'Utilities', 'bolt'), ('expense', 'Shopping', 'bag'),
    ('expense', 'Health', 'heart'), ('expense', 'Entertainment', 'film'), ('expense', 'Travel', 'plane'),
    ('expense', 'Education', 'book'), ('expense', 'Subscriptions', 'repeat'), ('expense', 'Insurance', 'shield'),
    ('expense', 'Personal care', 'user'), ('expense', 'Gifts and donations', 'gift'), ('expense', 'Fees and taxes', 'receipt'),
    ('expense', 'Other', 'tag'),
    ('income', 'Salary', 'wallet'), ('income', 'Business', 'briefcase'), ('income', 'Interest', 'trend'),
    ('income', 'Refund', 'repeat'), ('income', 'Gift', 'gift'), ('income', 'Other income', 'tag');
INSERT INTO categories (kind, name, parent_id)
SELECT 'expense', s.name, p.id FROM (VALUES
    ('Food and dining', 'Restaurants'), ('Food and dining', 'Delivery'), ('Food and dining', 'Coffee and snacks'),
    ('Transport', 'Fuel'), ('Transport', 'Cab and auto'), ('Transport', 'Public transport'),
    ('Housing', 'Rent'), ('Housing', 'Maintenance'),
    ('Utilities', 'Electricity'), ('Utilities', 'Water'), ('Utilities', 'Internet and phone'), ('Utilities', 'Gas')
) AS s (parent, name) JOIN categories p ON p.name = s.parent AND p.user_id IS NULL AND p.parent_id IS NULL AND p.kind = 'expense';
$fn$;
SELECT plinth_seed_categories();

CREATE TABLE tags (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id    uuid        NOT NULL REFERENCES users (id) ON DELETE RESTRICT,
    name       text        NOT NULL CHECK (length(name) BETWEEN 1 AND 40),
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX tags_user_name_key ON tags (user_id, lower(name));

CREATE TABLE transfers (
    id              uuid PRIMARY KEY,
    user_id         uuid        NOT NULL REFERENCES users (id) ON DELETE RESTRICT,
    from_account_id uuid        NOT NULL REFERENCES accounts (id),
    to_account_id   uuid        NOT NULL REFERENCES accounts (id),
    amount          bigint      NOT NULL CHECK (amount > 0),
    currency        char(3)     NOT NULL,
    occurred_at     timestamptz NOT NULL,
    notes           text        NOT NULL DEFAULT '' CHECK (length(notes) <= 2000),
    created_at      timestamptz NOT NULL DEFAULT now(),
    CHECK (from_account_id <> to_account_id)
);
CREATE INDEX transfers_user_idx ON transfers (user_id, occurred_at DESC);

-- One row per account leg. An expense or income has one row; a transfer has two rows sharing transfer_id.
CREATE TABLE transactions (
    id                 uuid PRIMARY KEY,
    user_id            uuid        NOT NULL REFERENCES users (id) ON DELETE RESTRICT,
    account_id         uuid        NOT NULL REFERENCES accounts (id),
    type               text        NOT NULL CHECK (type IN ('expense', 'income', 'transfer')),
    amount             bigint      NOT NULL CHECK (amount > 0),
    currency           char(3)     NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    title              text        NOT NULL CHECK (length(title) BETWEEN 1 AND 120),
    description        text        NOT NULL DEFAULT '' CHECK (length(description) <= 500),
    category_id        uuid        REFERENCES categories (id),
    payment_method     text        NOT NULL DEFAULT '' CHECK (payment_method IN
                         ('', 'cash', 'upi', 'debit_card', 'credit_card', 'bank_transfer', 'netbanking', 'wallet', 'cheque', 'other')),
    payer_name         text        NOT NULL DEFAULT '' CHECK (length(payer_name) <= 120),
    payee_name         text        NOT NULL DEFAULT '' CHECK (length(payee_name) <= 120),
    status             text        NOT NULL CHECK (status IN ('pending', 'confirmed', 'failed', 'cancelled', 'reversed', 'needs_review')),
    occurred_at        timestamptz NOT NULL,
    posted_at          timestamptz,
    timezone           text        NOT NULL CHECK (length(timezone) BETWEEN 1 AND 64),
    notes              text        NOT NULL DEFAULT '' CHECK (length(notes) <= 2000),
    location           text        NOT NULL DEFAULT '' CHECK (length(location) <= 200),
    latitude           numeric(9, 6) CHECK (latitude BETWEEN -90 AND 90),
    longitude          numeric(9, 6) CHECK (longitude BETWEEN -180 AND 180),
    source             text        NOT NULL DEFAULT 'manual' CHECK (source IN
                         ('manual', 'upi', 'sms', 'csv', 'recurring', 'group', 'receipt', 'reconcile')),
    external_ref       text        NOT NULL DEFAULT '' CHECK (length(external_ref) <= 120),
    transfer_id        uuid        REFERENCES transfers (id),
    transfer_direction text        CHECK (transfer_direction IN ('out', 'in')),
    refund_of          uuid        REFERENCES transactions (id),
    replaces_id        uuid        REFERENCES transactions (id),
    version            integer     NOT NULL DEFAULT 1,
    created_at         timestamptz NOT NULL DEFAULT now(),
    updated_at         timestamptz NOT NULL DEFAULT now(),
    search             tsvector GENERATED ALWAYS AS (to_tsvector('simple',
                         title || ' ' || description || ' ' || notes || ' ' || payee_name || ' ' || payer_name)) STORED,
    CHECK ((type = 'transfer') = (transfer_id IS NOT NULL)),
    CHECK ((type = 'transfer') = (transfer_direction IS NOT NULL)),
    CHECK (refund_of IS NULL OR type = 'income'),
    CHECK ((latitude IS NULL) = (longitude IS NULL))
);
-- Source-event uniqueness (CLAUDE.md 3.7): the same bank/UPI/SMS/CSV event can back at most one live transaction.
CREATE UNIQUE INDEX transactions_source_event_key ON transactions (user_id, source, external_ref)
    WHERE external_ref <> '' AND status NOT IN ('reversed', 'cancelled', 'failed');
CREATE INDEX transactions_user_date_idx ON transactions (user_id, occurred_at DESC, id DESC);
CREATE INDEX transactions_account_date_idx ON transactions (account_id, occurred_at DESC, id DESC);
CREATE INDEX transactions_user_amount_idx ON transactions (user_id, amount DESC, id DESC);
CREATE INDEX transactions_user_category_idx ON transactions (user_id, category_id) WHERE category_id IS NOT NULL;
CREATE INDEX transactions_transfer_idx ON transactions (transfer_id) WHERE transfer_id IS NOT NULL;
CREATE INDEX transactions_refund_idx ON transactions (refund_of) WHERE refund_of IS NOT NULL;
CREATE INDEX transactions_replaces_idx ON transactions (replaces_id) WHERE replaces_id IS NOT NULL;
CREATE INDEX transactions_search_idx ON transactions USING gin (search);
CREATE INDEX transactions_title_trgm_idx ON transactions USING gin (title gin_trgm_ops);
CREATE INDEX transactions_payee_trgm_idx ON transactions USING gin (payee_name gin_trgm_ops);

CREATE TABLE transaction_tags (
    transaction_id uuid NOT NULL REFERENCES transactions (id) ON DELETE CASCADE,
    tag_id         uuid NOT NULL REFERENCES tags (id) ON DELETE CASCADE,
    PRIMARY KEY (transaction_id, tag_id)
);
CREATE INDEX transaction_tags_tag_idx ON transaction_tags (tag_id);

-- Append-only record of every balance movement.
CREATE TABLE ledger_entries (
    id             bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    user_id        uuid        NOT NULL REFERENCES users (id) ON DELETE RESTRICT,
    account_id     uuid        NOT NULL REFERENCES accounts (id),
    transaction_id uuid        NOT NULL REFERENCES transactions (id),
    amount         bigint      NOT NULL CHECK (amount <> 0), -- signed minor units: negative leaves the account
    currency       char(3)     NOT NULL,
    kind           text        NOT NULL CHECK (kind IN ('movement', 'reversal')),
    effective_at   timestamptz NOT NULL, -- the transaction's occurred_at, for balance-over-time
    created_at     timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX ledger_entries_account_idx ON ledger_entries (account_id, effective_at);
CREATE INDEX ledger_entries_transaction_idx ON ledger_entries (transaction_id);

CREATE FUNCTION ledger_entries_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'ledger_entries is append-only';
END $$;
CREATE TRIGGER ledger_entries_no_change BEFORE UPDATE OR DELETE ON ledger_entries
    FOR EACH ROW EXECUTE FUNCTION ledger_entries_immutable();

-- A retry carrying the same key replays the original result instead of creating another movement.
CREATE TABLE idempotency_keys (
    user_id       uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    key           text        NOT NULL CHECK (length(key) BETWEEN 8 AND 64),
    scope         text        NOT NULL,
    request_hash  bytea       NOT NULL,
    resource_type text        NOT NULL DEFAULT '',
    resource_id   uuid,
    created_at    timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, key)
);
