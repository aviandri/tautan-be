-- Access tokens issued before this instant are rejected (P2-8 hardening:
-- set when a Google link takes over an account whose email was never verified).
ALTER TABLE users ADD COLUMN sessions_revoked_at timestamptz;
