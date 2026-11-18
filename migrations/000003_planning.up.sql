-- Phase 3: planning. Recurring schedules and their occurrences (expectations, never balance changes), reminders,
-- budgets (limits derived from transactions) and spending trackers (no limit).
--
-- Table ownership: recurring_*  -> recurrence; reminders -> reminders; budgets, budget_limits -> budgets;
-- trackers, tracker_transactions -> trackers. None of these tables ever writes accounts or ledger_entries:
-- money only moves through the transactions module (CLAUDE.md 3.8, 3.14).

CREATE TABLE recurring_schedules (
    id               uuid PRIMARY KEY,
    user_id          uuid        NOT NULL REFERENCES users (id) ON DELETE RESTRICT,
    account_id       uuid        NOT NULL REFERENCES accounts (id),
    type             text        NOT NULL CHECK (type IN ('expense', 'income')),
    title            text        NOT NULL CHECK (length(title) BETWEEN 1 AND 120),
    amount           bigint      NOT NULL CHECK (amount > 0),
    currency         char(3)     NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    category_id      uuid        REFERENCES categories (id),
    payment_method   text        NOT NULL DEFAULT '' CHECK (payment_method IN
                       ('', 'cash', 'upi', 'debit_card', 'credit_card', 'bank_transfer', 'netbanking', 'wallet', 'cheque', 'other')),
    payee_name       text        NOT NULL DEFAULT '' CHECK (length(payee_name) <= 120),
    notes            text        NOT NULL DEFAULT '' CHECK (length(notes) <= 2000),
    -- "Every <interval> <frequency>": interval > 1 is a custom interval (every 2 weeks, every 6 months).
    frequency        text        NOT NULL CHECK (frequency IN ('daily', 'weekly', 'monthly', 'quarterly', 'yearly')),
    interval         integer     NOT NULL DEFAULT 1 CHECK (interval BETWEEN 1 AND 365),
    start_date       date        NOT NULL, -- local civil date in `timezone`; the anchor for day-of-month. Never edited.
    end_date         date,
    timezone         text        NOT NULL CHECK (length(timezone) BETWEEN 1 AND 64),
    time_of_day      time        NOT NULL DEFAULT '09:00',
    reminder_days_before integer CHECK (reminder_days_before BETWEEN 0 AND 30),
    status           text        NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'paused', 'cancelled')),
    -- Occurrences with a due date up to and including this date have been generated.
    generated_through date       NOT NULL,
    version          integer     NOT NULL DEFAULT 1,
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now(),
    CHECK (end_date IS NULL OR end_date >= start_date)
);
CREATE INDEX recurring_schedules_user_idx ON recurring_schedules (user_id, status);
CREATE INDEX recurring_schedules_active_idx ON recurring_schedules (generated_through) WHERE status = 'active';

-- An expected payment or receipt. Confirming it records a real transaction through the transactions module.
CREATE TABLE recurring_occurrences (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    schedule_id     uuid        NOT NULL REFERENCES recurring_schedules (id) ON DELETE CASCADE,
    user_id         uuid        NOT NULL REFERENCES users (id) ON DELETE RESTRICT,
    due_date        date        NOT NULL,
    due_at          timestamptz NOT NULL,
    expected_amount bigint      NOT NULL CHECK (expected_amount > 0),
    currency        char(3)     NOT NULL,
    status          text        NOT NULL DEFAULT 'expected' CHECK (status IN ('expected', 'confirmed', 'skipped')),
    transaction_id  uuid        REFERENCES transactions (id),
    resolved_at     timestamptz,
    created_at      timestamptz NOT NULL DEFAULT now(),
    CHECK ((status = 'confirmed') = (transaction_id IS NOT NULL))
);
-- Idempotent generation: a (schedule, date) pair can exist only once, however often the generator runs.
CREATE UNIQUE INDEX recurring_occurrences_schedule_date_key ON recurring_occurrences (schedule_id, due_date);
CREATE INDEX recurring_occurrences_user_due_idx ON recurring_occurrences (user_id, due_at) WHERE status = 'expected';

CREATE TABLE reminders (
    id             uuid PRIMARY KEY,
    user_id        uuid        NOT NULL REFERENCES users (id) ON DELETE RESTRICT,
    title          text        NOT NULL CHECK (length(title) BETWEEN 1 AND 120),
    notes          text        NOT NULL DEFAULT '' CHECK (length(notes) <= 2000),
    amount         bigint      CHECK (amount > 0),
    currency       char(3)     CHECK (currency ~ '^[A-Z]{3}$'),
    account_id     uuid        REFERENCES accounts (id),
    category_id    uuid        REFERENCES categories (id),
    timezone       text        NOT NULL CHECK (length(timezone) BETWEEN 1 AND 64),
    time_of_day    time        NOT NULL DEFAULT '09:00',
    anchor_date    date        NOT NULL, -- first due date; the day-of-month anchor for repeats
    due_date       date        NOT NULL, -- the current due date
    frequency      text        CHECK (frequency IN ('daily', 'weekly', 'monthly', 'quarterly', 'yearly')),
    interval       integer     NOT NULL DEFAULT 1 CHECK (interval BETWEEN 1 AND 365),
    status         text        NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'paid', 'cancelled')),
    snoozed_until  timestamptz,
    last_paid_at   timestamptz,
    transaction_id uuid        REFERENCES transactions (id), -- the transaction that settled it (last one for repeats)
    version        integer     NOT NULL DEFAULT 1,
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now(),
    CHECK ((amount IS NULL) = (currency IS NULL))
);
CREATE INDEX reminders_user_due_idx ON reminders (user_id, due_date) WHERE status = 'active';

CREATE TABLE budgets (
    id           uuid PRIMARY KEY,
    user_id      uuid        NOT NULL REFERENCES users (id) ON DELETE RESTRICT,
    name         text        NOT NULL CHECK (length(name) BETWEEN 1 AND 80),
    account_id   uuid        REFERENCES accounts (id),
    category_id  uuid        REFERENCES categories (id),
    currency     char(3)     NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    -- Calendar-aligned periods: weekly (Monday to Sunday), monthly, quarterly (Jan-Mar...), yearly, or one custom range.
    period       text        NOT NULL CHECK (period IN ('weekly', 'monthly', 'quarterly', 'yearly', 'custom')),
    start_date   date        NOT NULL,
    end_date     date,
    rollover     boolean     NOT NULL DEFAULT false,
    alert_percent integer    CHECK (alert_percent BETWEEN 1 AND 200),
    timezone     text        NOT NULL CHECK (length(timezone) BETWEEN 1 AND 64),
    archived_at  timestamptz,
    version      integer     NOT NULL DEFAULT 1,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),
    CHECK ((period = 'custom') = (end_date IS NOT NULL)),
    CHECK (end_date IS NULL OR end_date >= start_date),
    CHECK (NOT (period = 'custom' AND rollover))
);
CREATE INDEX budgets_user_idx ON budgets (user_id) WHERE archived_at IS NULL;

-- Past periods keep the limit they had: the limit for a period is the row with the latest effective_from
-- on or before the period's first day.
CREATE TABLE budget_limits (
    budget_id      uuid        NOT NULL REFERENCES budgets (id) ON DELETE CASCADE,
    effective_from date        NOT NULL,
    amount         bigint      NOT NULL CHECK (amount > 0),
    created_at     timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (budget_id, effective_from)
);

CREATE TABLE trackers (
    id          uuid PRIMARY KEY,
    user_id     uuid        NOT NULL REFERENCES users (id) ON DELETE RESTRICT,
    name        text        NOT NULL CHECK (length(name) BETWEEN 1 AND 80),
    notes       text        NOT NULL DEFAULT '' CHECK (length(notes) <= 2000),
    currency    char(3)     NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    archived_at timestamptz,
    version     integer     NOT NULL DEFAULT 1,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX trackers_user_idx ON trackers (user_id) WHERE archived_at IS NULL;

CREATE TABLE tracker_transactions (
    tracker_id     uuid        NOT NULL REFERENCES trackers (id) ON DELETE CASCADE,
    transaction_id uuid        NOT NULL REFERENCES transactions (id),
    added_at       timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tracker_id, transaction_id)
);
CREATE INDEX tracker_transactions_tx_idx ON tracker_transactions (transaction_id);
