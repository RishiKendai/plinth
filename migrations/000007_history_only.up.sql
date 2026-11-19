-- A transaction can be recorded for history only: it is a real expense or income (it shows in lists, budgets and analytics) but it
-- adds no ledger entry and so never changes the account balance. For older transactions the user forgot to add while the balance
-- already reflects them; recording them normally would count them twice. The ledger stays authoritative: balance = opening
-- balance + ledger entries, and a history-only transaction simply has no entries.
ALTER TABLE transactions ADD COLUMN history_only boolean NOT NULL DEFAULT false;
ALTER TABLE transactions ADD CONSTRAINT transactions_history_only_check CHECK (NOT history_only OR type IN ('expense', 'income'));
