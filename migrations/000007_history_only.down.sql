ALTER TABLE transactions DROP CONSTRAINT IF EXISTS transactions_history_only_check;
ALTER TABLE transactions DROP COLUMN IF EXISTS history_only;
