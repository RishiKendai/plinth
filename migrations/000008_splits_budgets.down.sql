DROP INDEX IF EXISTS group_expenses_transaction_key;
DROP INDEX IF EXISTS groups_kind_idx;
ALTER TABLE groups DROP COLUMN IF EXISTS budget_limit;
ALTER TABLE groups DROP COLUMN IF EXISTS kind;
