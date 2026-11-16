ALTER TABLE events
    ADD COLUMN visitor_hash text NOT NULL DEFAULT '',
    ADD COLUMN browser      text NOT NULL DEFAULT '',
    ADD COLUMN os           text NOT NULL DEFAULT '',
    ADD COLUMN utm_source   text NOT NULL DEFAULT '',
    ADD COLUMN utm_medium   text NOT NULL DEFAULT '',
    ADD COLUMN utm_campaign text NOT NULL DEFAULT '',
    ADD COLUMN channel      text NOT NULL DEFAULT '';
CREATE INDEX events_block_created_idx ON events(block_id, created_at) WHERE block_id IS NOT NULL;

-- One random salt per UTC day for visitor hashes; old salts are deleted so
-- hashes can't be linked across days.
CREATE TABLE analytics_salts (
    day  date PRIMARY KEY,
    salt bytea NOT NULL
);
