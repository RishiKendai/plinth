-- Owner decision 2026-11-21: a person stays signed in until they sign out. Sign-ins no longer expire after 30 days.
-- Existing sign-ins that have not been signed out are extended to match the new lifetime (internal/auth SessionLifetime).
UPDATE sessions SET refresh_expires_at = now() + interval '10 years' WHERE revoked_at IS NULL;

-- When the tokens were last swapped. The previous refresh token is accepted once more for two minutes after that (a lost reply to a
-- refresh must not sign the person out); using it any later is treated as theft. last_used_at cannot be used: it moves on every request.
ALTER TABLE sessions ADD COLUMN IF NOT EXISTS rotated_at timestamptz NOT NULL DEFAULT now();
