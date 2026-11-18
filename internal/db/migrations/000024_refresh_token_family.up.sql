-- Refresh-token reuse detection (security audit 2026-11, M3). Every login
-- starts a family; each rotation keeps the family and marks the old token
-- rotated_at. Presenting a rotated token again means it was copied: the API
-- revokes the whole family and ends the account's access tokens.
ALTER TABLE refresh_tokens
    ADD COLUMN family_id  uuid,
    ADD COLUMN rotated_at timestamptz;
UPDATE refresh_tokens SET family_id = id WHERE family_id IS NULL;
ALTER TABLE refresh_tokens
    ALTER COLUMN family_id SET DEFAULT gen_random_uuid(),
    ALTER COLUMN family_id SET NOT NULL;
CREATE INDEX refresh_tokens_family_idx ON refresh_tokens (family_id);
