ALTER TABLE profiles
    ADD COLUMN socials jsonb NOT NULL DEFAULT '[]'::jsonb,
    ADD COLUMN socials_position text NOT NULL DEFAULT 'top' CHECK (socials_position IN ('top', 'bottom'));
