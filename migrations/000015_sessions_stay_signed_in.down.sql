-- The old 30-day expiries cannot be restored; sign-ins simply keep the extended date. Only the added column is removed.
ALTER TABLE sessions DROP COLUMN IF EXISTS rotated_at;
