-- Phase 1: identity, sessions, settings, audit.
-- Table ownership: the auth module owns users (credentials), sessions and email_tokens.
-- The users module reads/writes profile columns only; settings owns user_settings.

CREATE TABLE users (
    id                    uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    email                 text        NOT NULL CHECK (email = lower(email) AND length(email) <= 254),
    email_verified_at     timestamptz,
    password_hash         text        NOT NULL,
    display_name          text        NOT NULL DEFAULT '' CHECK (length(display_name) <= 100),
    failed_logins         integer     NOT NULL DEFAULT 0 CHECK (failed_logins >= 0),
    locked_until          timestamptz,
    deletion_requested_at timestamptz,
    created_at            timestamptz NOT NULL DEFAULT now(),
    updated_at            timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX users_email_key ON users (email);

CREATE TABLE user_settings (
    user_id               uuid PRIMARY KEY REFERENCES users (id) ON DELETE CASCADE,
    currency              char(3)     NOT NULL DEFAULT 'INR' CHECK (currency ~ '^[A-Z]{3}$'),
    date_format           text        NOT NULL DEFAULT 'dmy' CHECK (date_format IN ('dmy', 'mdy', 'ymd')),
    number_format         text        NOT NULL DEFAULT 'indian' CHECK (number_format IN ('indian', 'international')),
    timezone              text        NOT NULL DEFAULT 'Asia/Kolkata' CHECK (length(timezone) BETWEEN 1 AND 64),
    theme                 text        NOT NULL DEFAULT 'system' CHECK (theme IN ('light', 'dark', 'system')),
    notifications_enabled boolean     NOT NULL DEFAULT true,
    hide_amounts          boolean     NOT NULL DEFAULT false,
    show_decimals         boolean     NOT NULL DEFAULT true,
    version               integer     NOT NULL DEFAULT 1,
    updated_at            timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE sessions (
    id                 uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id            uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    access_hash        bytea       NOT NULL,
    refresh_hash       bytea       NOT NULL,
    prev_refresh_hash  bytea,
    access_expires_at  timestamptz NOT NULL,
    refresh_expires_at timestamptz NOT NULL,
    device_name        text        NOT NULL DEFAULT '' CHECK (length(device_name) <= 100),
    platform           text        NOT NULL DEFAULT '' CHECK (length(platform) <= 30),
    created_at         timestamptz NOT NULL DEFAULT now(),
    last_used_at       timestamptz NOT NULL DEFAULT now(),
    revoked_at         timestamptz
);
CREATE UNIQUE INDEX sessions_access_hash_key ON sessions (access_hash);
CREATE UNIQUE INDEX sessions_refresh_hash_key ON sessions (refresh_hash);
CREATE INDEX sessions_prev_refresh_hash_idx ON sessions (prev_refresh_hash) WHERE prev_refresh_hash IS NOT NULL;
CREATE INDEX sessions_user_idx ON sessions (user_id) WHERE revoked_at IS NULL;

-- One-time codes sent by email. Only an HMAC of the code is stored.
CREATE TABLE email_tokens (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id     uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    purpose     text        NOT NULL CHECK (purpose IN ('verify_email', 'reset_password')),
    code_hash   bytea       NOT NULL,
    attempts    integer     NOT NULL DEFAULT 0,
    expires_at  timestamptz NOT NULL,
    consumed_at timestamptz,
    created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX email_tokens_lookup_idx ON email_tokens (user_id, purpose, created_at DESC);

CREATE TABLE audit_events (
    id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    user_id    uuid REFERENCES users (id) ON DELETE SET NULL,
    event      text        NOT NULL,
    metadata   jsonb       NOT NULL DEFAULT '{}',
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX audit_events_user_idx ON audit_events (user_id, created_at DESC);
