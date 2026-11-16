ALTER TABLE profiles
    ADD COLUMN announcement jsonb,
    ADD COLUMN settings jsonb NOT NULL DEFAULT '{}'::jsonb;

-- Old usernames keep redirecting to the profile after a rename.
CREATE TABLE username_redirects (
    old_username citext PRIMARY KEY,
    profile_id   uuid NOT NULL REFERENCES profiles(id) ON DELETE CASCADE,
    created_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX username_redirects_profile_idx ON username_redirects(profile_id, created_at DESC);
