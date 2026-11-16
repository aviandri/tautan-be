ALTER TABLE profiles ADD COLUMN onboarding jsonb NOT NULL DEFAULT '{"completed": false, "username_claimed": true}'::jsonb;
-- Accounts created before onboarding existed have nothing left to do.
UPDATE profiles SET onboarding = '{"completed": true, "username_claimed": true}'::jsonb;
