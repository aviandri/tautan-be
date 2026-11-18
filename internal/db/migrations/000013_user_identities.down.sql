DROP TABLE IF EXISTS user_identities;
-- Password-less accounts keep an empty hash, which never matches at login.
UPDATE users SET password_hash = '' WHERE password_hash IS NULL;
ALTER TABLE users ALTER COLUMN password_hash SET NOT NULL;
