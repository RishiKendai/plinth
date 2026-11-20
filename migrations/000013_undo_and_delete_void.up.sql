-- Undo a void, and delete (hide) a voided transaction. Owner request 2026-11-20; docs/balance-rules.md.
--
-- voided_from remembers what a transaction was before it was voided, so Undo can restore exactly that.
-- deleted_at hides a closed transaction from the user's lists, search and exports. The row and its ledger entries stay (they net to
-- zero, and the ledger is append-only), so balances stay explainable. Only closed statuses can be deleted (enforced here).
ALTER TABLE transactions
    ADD COLUMN voided_from text CHECK (voided_from IN ('pending', 'needs_review', 'confirmed')),
    ADD COLUMN deleted_at  timestamptz,
    ADD CONSTRAINT transactions_deleted_closed CHECK (deleted_at IS NULL OR status IN ('failed', 'cancelled', 'reversed'));

-- Voids made before this migration: a reversed one was confirmed; a cancelled one is assumed to have been pending.
UPDATE transactions SET voided_from = CASE status WHEN 'reversed' THEN 'confirmed' ELSE 'pending' END WHERE status IN ('reversed', 'cancelled');
