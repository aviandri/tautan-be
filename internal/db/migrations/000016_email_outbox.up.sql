-- Transactional email: the outbox a worker delivers from, and password resets.
CREATE TABLE email_outbox (
    id              bigserial   PRIMARY KEY,
    kind            text        NOT NULL,               -- 'password_reset', 'verify_email', ...
    to_email        citext      NOT NULL,
    subject         text        NOT NULL,
    text_body       text        NOT NULL,               -- cleared once sent (holds live links)
    html_body       text        NOT NULL DEFAULT '',
    status          text        NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'sent', 'failed')),
    attempts        int         NOT NULL DEFAULT 0,
    next_attempt_at timestamptz NOT NULL DEFAULT now(),  -- also the claim lease while sending
    last_error      text,
    created_at      timestamptz NOT NULL DEFAULT now(),
    sent_at         timestamptz
);
CREATE INDEX email_outbox_due_idx ON email_outbox (next_attempt_at) WHERE status = 'pending';

CREATE TABLE password_resets (
    token_hash text        PRIMARY KEY,                  -- sha256 of the emailed token
    user_id    uuid        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    expires_at timestamptz NOT NULL,
    used_at    timestamptz,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX password_resets_user_idx ON password_resets (user_id, created_at);
