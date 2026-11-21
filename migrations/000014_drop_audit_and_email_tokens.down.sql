-- Recreates the empty tables (the audit history deleted by the up migration cannot come back).
DROP INDEX IF EXISTS idempotency_keys_created_idx;
CREATE TABLE IF NOT EXISTS email_tokens (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id     uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    purpose     text        NOT NULL CHECK (purpose IN ('verify_email', 'reset_password')),
    code_hash   bytea       NOT NULL,
    attempts    integer     NOT NULL DEFAULT 0,
    expires_at  timestamptz NOT NULL,
    consumed_at timestamptz,
    created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS email_tokens_lookup_idx ON email_tokens (user_id, purpose, created_at DESC);
CREATE TABLE IF NOT EXISTS audit_events (
    id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    user_id    uuid REFERENCES users (id) ON DELETE SET NULL,
    event      text        NOT NULL,
    metadata   jsonb       NOT NULL DEFAULT '{}',
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS audit_events_user_idx ON audit_events (user_id, created_at DESC);
