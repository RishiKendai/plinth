-- Owner decision 2026-11-21: the audit log and the unused email-code table go. The database keeps only what the app needs.
--   audit_events  an event log (sign-ins, edits, ...). Nothing in the app reads it. The history it held is deleted by this migration.
--   email_tokens  one-time email codes. No email is sent, so nothing ever wrote to it.
-- Financial records are untouched: the ledger, transactions and accounts do not depend on either table.
DROP TABLE IF EXISTS audit_events;
DROP TABLE IF EXISTS email_tokens;

-- The worker deletes idempotency keys older than 30 days (see internal/idempotency); this index makes that delete cheap.
CREATE INDEX IF NOT EXISTS idempotency_keys_created_idx ON idempotency_keys (created_at);
