-- Email verification (P2-8 follow-up). Unverified accounts can't publish an
-- unpublished page; pages already published stay published (grandfathered:
-- the gate only applies to the unpublished → published transition).
ALTER TABLE users ADD COLUMN email_verified_at timestamptz;
-- Google already proved these addresses.
UPDATE users SET email_verified_at = i.created_at
FROM (SELECT user_id, min(created_at) AS created_at FROM user_identities GROUP BY user_id) i
WHERE i.user_id = users.id;

CREATE TABLE email_verifications (
    token_hash text        PRIMARY KEY,                  -- sha256 of the emailed token
    user_id    uuid        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    expires_at timestamptz NOT NULL,
    used_at    timestamptz,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX email_verifications_user_idx ON email_verifications (user_id, created_at);
