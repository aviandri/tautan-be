-- P3-A: what a click did (buy, wa_order, copy_account, marketplace:shopee ...).
ALTER TABLE events ADD COLUMN IF NOT EXISTS action text NOT NULL DEFAULT '';
CREATE INDEX IF NOT EXISTS events_block_action_idx ON events (block_id, action) WHERE action <> '';
