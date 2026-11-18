-- Changing the account email: a confirm link goes to the new address; once it
-- is used, the old address gets a notice with an undo link (kind 'revert').
CREATE TABLE email_change_tokens (
    token_hash text        PRIMARY KEY,                  -- sha256 of the emailed token
    user_id    uuid        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    kind       text        NOT NULL CHECK (kind IN ('confirm', 'revert')),
    old_email  citext      NOT NULL,
    new_email  citext      NOT NULL,
    expires_at timestamptz NOT NULL,
    used_at    timestamptz,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX email_change_tokens_user_idx ON email_change_tokens (user_id, created_at);
