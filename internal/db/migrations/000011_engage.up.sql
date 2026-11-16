-- P3-C engage cards.
CREATE TABLE IF NOT EXISTS poll_votes (
    block_id     uuid NOT NULL REFERENCES blocks(id) ON DELETE CASCADE,
    visitor_hash text NOT NULL,
    day          date NOT NULL,          -- UTC day, matching the daily analytics salt
    option_id    text NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (block_id, visitor_hash, day)
);
CREATE TABLE IF NOT EXISTS questions (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    profile_id  uuid NOT NULL REFERENCES profiles(id) ON DELETE CASCADE,
    block_id    uuid NOT NULL REFERENCES blocks(id) ON DELETE CASCADE,
    question    text NOT NULL,
    answer      text NOT NULL DEFAULT '',
    answered_at timestamptz,
    pinned      boolean NOT NULL DEFAULT false,
    created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS questions_profile_idx ON questions (profile_id, created_at DESC);
CREATE INDEX IF NOT EXISTS questions_block_pinned_idx ON questions (block_id) WHERE pinned;
CREATE TABLE IF NOT EXISTS giveaway_entries (
    block_id   uuid NOT NULL REFERENCES blocks(id) ON DELETE CASCADE,
    contact_id uuid REFERENCES contacts(id) ON DELETE SET NULL,
    email      citext NOT NULL,
    name       text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (block_id, email)
);
CREATE TABLE IF NOT EXISTS giveaway_winners (
    block_id uuid PRIMARY KEY REFERENCES blocks(id) ON DELETE CASCADE,
    email    citext NOT NULL,
    name     text NOT NULL DEFAULT '',
    display  text NOT NULL,
    drawn_at timestamptz NOT NULL
);
