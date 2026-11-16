DROP INDEX IF EXISTS events_block_action_idx;
ALTER TABLE events DROP COLUMN IF EXISTS action;
