CREATE TABLE contacts (
    id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    profile_id       uuid NOT NULL REFERENCES profiles(id) ON DELETE CASCADE,
    email            citext NOT NULL,
    name             text NOT NULL DEFAULT '',
    subscribed       boolean NOT NULL DEFAULT false,
    subscribed_at    timestamptz,
    source_block_ids uuid[] NOT NULL DEFAULT '{}',
    responses        int NOT NULL DEFAULT 0,
    first_seen_at    timestamptz NOT NULL DEFAULT now(),
    last_activity_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (profile_id, email)
);
CREATE INDEX contacts_activity_idx ON contacts(profile_id, last_activity_at DESC, id DESC);

CREATE TABLE form_submissions (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    profile_id uuid NOT NULL REFERENCES profiles(id) ON DELETE CASCADE,
    block_id   uuid REFERENCES blocks(id) ON DELETE SET NULL,
    contact_id uuid REFERENCES contacts(id) ON DELETE SET NULL,
    answers    jsonb NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX form_submissions_block_idx ON form_submissions(block_id, created_at DESC, id DESC);
CREATE INDEX form_submissions_contact_idx ON form_submissions(contact_id, created_at DESC);

ALTER TABLE contact_submissions ADD COLUMN contact_id uuid REFERENCES contacts(id) ON DELETE SET NULL;
