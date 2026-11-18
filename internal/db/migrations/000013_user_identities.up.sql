-- Google sign-in (P2-8): accounts created through an identity provider have no password.
ALTER TABLE users ALTER COLUMN password_hash DROP NOT NULL;

CREATE TABLE user_identities (
    provider   text        NOT NULL,          -- 'google'
    subject    text        NOT NULL,          -- the provider's stable user id (OIDC "sub")
    user_id    uuid        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    email      citext      NOT NULL,          -- email at link time, for support/debugging
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (provider, subject)
);
CREATE INDEX user_identities_user_id_idx ON user_identities (user_id);
