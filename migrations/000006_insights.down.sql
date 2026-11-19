DROP TRIGGER IF EXISTS accounts_touch ON accounts;
DROP TRIGGER IF EXISTS transactions_touch ON transactions;
DROP FUNCTION IF EXISTS plinth_touch_updated_at();
DROP INDEX IF EXISTS accounts_user_updated_idx;
DROP INDEX IF EXISTS transactions_user_updated_idx;
DROP TABLE IF EXISTS reports;
