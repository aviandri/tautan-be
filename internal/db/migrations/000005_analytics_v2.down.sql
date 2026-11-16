DROP TABLE analytics_salts;
DROP INDEX events_block_created_idx;
ALTER TABLE events DROP COLUMN visitor_hash, DROP COLUMN browser, DROP COLUMN os,
    DROP COLUMN utm_source, DROP COLUMN utm_medium, DROP COLUMN utm_campaign, DROP COLUMN channel;
