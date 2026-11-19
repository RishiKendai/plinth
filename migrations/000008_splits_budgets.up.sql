-- A split is a group of kind 'split': an expense shared with a few people that is not part of any named group. It reuses the
-- group machinery (members, shares, balances, settlements) so splits and groups can never disagree about the arithmetic.
ALTER TABLE groups ADD COLUMN kind text NOT NULL DEFAULT 'group' CHECK (kind IN ('group', 'split'));
-- A group may set a monthly limit on what the group spends in total (UTC calendar month of the expense date). Null means none.
ALTER TABLE groups ADD COLUMN budget_limit bigint CHECK (budget_limit IS NULL OR budget_limit > 0);
CREATE INDEX groups_kind_idx ON groups (kind);
-- An existing personal transaction can be shared into a group or split; it may back at most one live group expense.
CREATE UNIQUE INDEX group_expenses_transaction_key ON group_expenses (transaction_id) WHERE transaction_id IS NOT NULL AND voided_at IS NULL;
