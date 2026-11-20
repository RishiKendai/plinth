-- Account deletion (owner request 2026-11-19): after a grace period the worker permanently erases a user who asked to be deleted.
-- The ledger is append-only for everyone except that purge, which sets a transaction-local flag. Nothing else sets it.
CREATE OR REPLACE FUNCTION ledger_entries_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' AND current_setting('plinth.account_purge', true) = 'on' THEN
        RETURN OLD;
    END IF;
    RAISE EXCEPTION 'ledger_entries is append-only';
END $$;
