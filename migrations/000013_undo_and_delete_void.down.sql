-- Deleted (hidden) transactions become visible again; nothing is lost.
ALTER TABLE transactions DROP CONSTRAINT IF EXISTS transactions_deleted_closed, DROP COLUMN IF EXISTS deleted_at, DROP COLUMN IF EXISTS voided_from;
