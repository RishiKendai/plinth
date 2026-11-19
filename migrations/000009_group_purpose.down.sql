DROP TRIGGER IF EXISTS groups_purpose_fixed ON groups;
DROP FUNCTION IF EXISTS plinth_groups_purpose_fixed();
ALTER TABLE groups DROP CONSTRAINT IF EXISTS groups_purpose_limit_check;
ALTER TABLE groups DROP COLUMN IF EXISTS purpose;
