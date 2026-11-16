-- Card-level gates (password hash, kind, options); never part of data.
ALTER TABLE blocks ADD COLUMN IF NOT EXISTS gate jsonb;
