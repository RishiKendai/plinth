-- A group can switch splitting off: it is then only a place to collect transactions (nobody owes anyone). Splits created from a
-- transaction (kind 'split') always split. Groups that have only one active member cannot split anything, so they start with it off.
ALTER TABLE groups ADD COLUMN split_enabled boolean NOT NULL DEFAULT true;
UPDATE groups g SET split_enabled = false
 WHERE g.kind = 'group' AND (SELECT count(*) FROM group_members m WHERE m.group_id = g.id AND m.removed_at IS NULL) <= 1;
