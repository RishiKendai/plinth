-- Budgets and spending trackers are not separate features: they are what a group is for. Every group is either a budget (a monthly
-- limit on its total spending, set when it is created) or a spending tracker (an open-ended running total with no limit). The
-- choice is made at creation and never changes. Groups that already had a limit become budgets; all others become trackers.
ALTER TABLE groups ADD COLUMN purpose text NOT NULL DEFAULT 'tracker' CHECK (purpose IN ('budget', 'tracker'));
UPDATE groups SET purpose = 'budget' WHERE budget_limit IS NOT NULL;
ALTER TABLE groups ADD CONSTRAINT groups_purpose_limit_check CHECK ((purpose = 'budget') = (budget_limit IS NOT NULL));
-- A purpose can never be changed after creation.
CREATE FUNCTION plinth_groups_purpose_fixed() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.purpose IS DISTINCT FROM OLD.purpose THEN RAISE EXCEPTION 'a group''s purpose cannot be changed'; END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER groups_purpose_fixed BEFORE UPDATE ON groups FOR EACH ROW EXECUTE FUNCTION plinth_groups_purpose_fixed();
