-- An income can be filed under a group (a trip refund, a contribution, a payout). It is only a label on the person's own income
-- transaction: it is never split, never changes member balances, the group's spent total or a budget's remaining amount, and the
-- transaction and the account balance are untouched. A transaction is in at most one group at a time.
CREATE TABLE group_incomes (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    group_id       uuid        NOT NULL REFERENCES groups (id) ON DELETE CASCADE,
    transaction_id uuid        NOT NULL REFERENCES transactions (id),
    created_by     uuid        NOT NULL REFERENCES group_members (id),
    title          text        NOT NULL,
    amount         bigint      NOT NULL CHECK (amount > 0),
    currency       char(3)     NOT NULL,
    occurred_at    timestamptz NOT NULL,
    voided_at      timestamptz,
    created_at     timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX group_incomes_transaction_key ON group_incomes (transaction_id) WHERE voided_at IS NULL;
CREATE INDEX group_incomes_group_idx ON group_incomes (group_id, occurred_at DESC, id DESC);
