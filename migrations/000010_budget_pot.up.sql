-- A budget group is a pot of money, not a monthly limit: it starts with the amount given at creation (budget_limit, now the opening
-- amount), grows with top-ups, shrinks with the group's expenses, and expenses are refused once nothing is left. Each top-up is a
-- recorded transaction of the group and may also be recorded in the member's own account.
ALTER TABLE transactions DROP CONSTRAINT transactions_source_check;
ALTER TABLE transactions ADD CONSTRAINT transactions_source_check CHECK (source IN
    ('manual', 'upi', 'sms', 'csv', 'recurring', 'group', 'receipt', 'reconcile', 'settlement', 'topup'));

CREATE TABLE group_topups (
    id             uuid PRIMARY KEY,
    group_id       uuid        NOT NULL REFERENCES groups (id) ON DELETE CASCADE,
    member_id      uuid        NOT NULL REFERENCES group_members (id),
    amount         bigint      NOT NULL CHECK (amount > 0),
    note           text        NOT NULL DEFAULT '' CHECK (length(note) <= 500),
    occurred_at    timestamptz NOT NULL,
    -- The member's own ledger transaction (source 'topup'), if they chose to record the money leaving one of their accounts.
    transaction_id uuid        REFERENCES transactions (id),
    voided_at      timestamptz,
    created_at     timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX group_topups_group_idx ON group_topups (group_id, occurred_at DESC, id DESC);
