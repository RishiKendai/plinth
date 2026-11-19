-- Phase 6 and 7: background reports and incremental sync.

CREATE TABLE reports (
    id           uuid PRIMARY KEY,
    user_id      uuid        NOT NULL REFERENCES users (id) ON DELETE RESTRICT,
    kind         text        NOT NULL CHECK (kind IN ('statement', 'year_in_review')),
    account_id   uuid        REFERENCES accounts (id),
    period_from  date        NOT NULL,
    period_to    date        NOT NULL CHECK (period_to >= period_from),
    status       text        NOT NULL DEFAULT 'queued' CHECK (status IN ('queued', 'running', 'ready', 'failed')),
    attempts     integer     NOT NULL DEFAULT 0,
    error        text        NOT NULL DEFAULT '',
    content      text,
    row_count    integer     NOT NULL DEFAULT 0,
    created_at   timestamptz NOT NULL DEFAULT now(),
    started_at   timestamptz,
    finished_at  timestamptz
);
CREATE INDEX reports_user_idx ON reports (user_id, created_at DESC);
CREATE INDEX reports_queue_idx ON reports (created_at) WHERE status IN ('queued', 'running');

-- Incremental sync pulls "everything changed since (updated_at, id)". Accounts and categories are small; transactions are the
-- large set, so only they need a dedicated index for the per-user cursor.
CREATE INDEX transactions_user_updated_idx ON transactions (user_id, updated_at, id);
CREATE INDEX accounts_user_updated_idx ON accounts (user_id, updated_at, id);

-- Safety net for sync: any change to a row moves its updated_at, even if some code path forgets to. A pull that asks for
-- "changed since" therefore never misses an edit.
CREATE FUNCTION plinth_touch_updated_at() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW IS DISTINCT FROM OLD AND NEW.updated_at IS NOT DISTINCT FROM OLD.updated_at THEN
        NEW.updated_at := clock_timestamp();
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER transactions_touch BEFORE UPDATE ON transactions FOR EACH ROW EXECUTE FUNCTION plinth_touch_updated_at();
CREATE TRIGGER accounts_touch BEFORE UPDATE ON accounts FOR EACH ROW EXECUTE FUNCTION plinth_touch_updated_at();
