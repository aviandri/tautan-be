ALTER TABLE blocks ADD COLUMN deleted_at timestamptz;
CREATE INDEX blocks_deleted_idx ON blocks(profile_id, deleted_at) WHERE deleted_at IS NOT NULL;
