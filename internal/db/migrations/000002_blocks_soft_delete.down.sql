DELETE FROM blocks WHERE deleted_at IS NOT NULL;
ALTER TABLE blocks DROP COLUMN deleted_at;
