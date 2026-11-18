-- Phase 5: groups, split expenses, settlements (CLAUDE.md 3.11, 3.12).
--
-- Three things stay separate: the expense (what was paid), the obligations (each member's share) and settlements
-- (actual repayments). A group is its own entity: group tables never reference accounts or personal transactions
-- except through an optional link the owner of that transaction chose to make, so a group admin has no path to a
-- member's personal finances.

CREATE TABLE groups (
    id          uuid PRIMARY KEY,
    name        text        NOT NULL CHECK (length(name) BETWEEN 1 AND 80),
    currency    char(3)     NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    created_by  uuid        NOT NULL REFERENCES users (id) ON DELETE RESTRICT,
    archived_at timestamptz,
    version     integer     NOT NULL DEFAULT 1,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);

-- A member is a person in the group. user_id is set when that person uses Plinth; people who do not use Plinth are
-- manual members with only a display name.
CREATE TABLE group_members (
    id           uuid PRIMARY KEY,
    group_id     uuid        NOT NULL REFERENCES groups (id) ON DELETE CASCADE,
    user_id      uuid        REFERENCES users (id) ON DELETE RESTRICT,
    display_name text        NOT NULL CHECK (length(display_name) BETWEEN 1 AND 60),
    role         text        NOT NULL DEFAULT 'member' CHECK (role IN ('admin', 'member')),
    removed_at   timestamptz,
    created_at   timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX group_members_user_key ON group_members (group_id, user_id) WHERE user_id IS NOT NULL AND removed_at IS NULL;
CREATE INDEX group_members_group_idx ON group_members (group_id);
CREATE INDEX group_members_user_idx ON group_members (user_id) WHERE removed_at IS NULL;

-- One-time invite codes bind a member slot to a Plinth user. Only a hash of the code is stored.
CREATE TABLE group_invites (
    id         uuid PRIMARY KEY,
    group_id   uuid        NOT NULL REFERENCES groups (id) ON DELETE CASCADE,
    member_id  uuid        NOT NULL REFERENCES group_members (id) ON DELETE CASCADE,
    code_hash  bytea       NOT NULL UNIQUE,
    created_by uuid        NOT NULL REFERENCES users (id),
    expires_at timestamptz NOT NULL,
    used_at    timestamptz,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE group_expenses (
    id                uuid PRIMARY KEY,
    group_id          uuid        NOT NULL REFERENCES groups (id) ON DELETE CASCADE,
    paid_by           uuid        NOT NULL REFERENCES group_members (id),
    created_by        uuid        NOT NULL REFERENCES group_members (id),
    title             text        NOT NULL CHECK (length(title) BETWEEN 1 AND 120),
    amount            bigint      NOT NULL CHECK (amount > 0),
    currency          char(3)     NOT NULL,
    split_method      text        NOT NULL CHECK (split_method IN ('equal', 'exact', 'percent', 'shares')),
    occurred_at       timestamptz NOT NULL,
    notes             text        NOT NULL DEFAULT '' CHECK (length(notes) <= 1000),
    voided_at         timestamptz,
    -- The payer's own ledger transaction, if the payer is a Plinth user who chose to record the payment.
    transaction_id    uuid        REFERENCES transactions (id),
    created_at        timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX group_expenses_group_idx ON group_expenses (group_id, occurred_at DESC, id DESC);

-- What each member owes for an expense. The rows of one expense sum to exactly group_expenses.amount (enforced in the
-- split engine and checked by a deferred constraint trigger below).
CREATE TABLE expense_shares (
    expense_id uuid   NOT NULL REFERENCES group_expenses (id) ON DELETE CASCADE,
    member_id  uuid   NOT NULL REFERENCES group_members (id),
    amount     bigint NOT NULL CHECK (amount >= 0),
    input      bigint NOT NULL DEFAULT 0, -- what was typed: exact amount, basis points, or share weight
    PRIMARY KEY (expense_id, member_id)
);

CREATE FUNCTION expense_shares_reconcile() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE total bigint; want bigint;
BEGIN
    SELECT COALESCE(SUM(amount), 0) INTO total FROM expense_shares WHERE expense_id = NEW.expense_id;
    SELECT amount INTO want FROM group_expenses WHERE id = NEW.expense_id;
    IF want IS NOT NULL AND total <> want THEN
        RAISE EXCEPTION 'shares of expense % sum to % but the expense is %', NEW.expense_id, total, want;
    END IF;
    RETURN NULL;
END $$;
CREATE CONSTRAINT TRIGGER expense_shares_sum AFTER INSERT ON expense_shares
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION expense_shares_reconcile();

-- A settlement is a real repayment between two members. It is not an expense or income of the group.
CREATE TABLE group_settlements (
    id             uuid PRIMARY KEY,
    group_id       uuid        NOT NULL REFERENCES groups (id) ON DELETE CASCADE,
    from_member    uuid        NOT NULL REFERENCES group_members (id),
    to_member      uuid        NOT NULL REFERENCES group_members (id),
    created_by     uuid        NOT NULL REFERENCES group_members (id),
    amount         bigint      NOT NULL CHECK (amount > 0),
    currency       char(3)     NOT NULL,
    occurred_at    timestamptz NOT NULL,
    note           text        NOT NULL DEFAULT '' CHECK (length(note) <= 500),
    voided_at      timestamptz,
    transaction_id uuid        REFERENCES transactions (id), -- the recording user's own ledger entry, if they made one
    created_at     timestamptz NOT NULL DEFAULT now(),
    CHECK (from_member <> to_member)
);
CREATE INDEX group_settlements_group_idx ON group_settlements (group_id, occurred_at DESC);
