-- Payments (a): the creator's own provider accounts. Tautan never holds funds.
-- credentials is AES-256-GCM ciphertext (see internal/payments/seal.go) of
-- {"secret_key", "webhook_secret"}; hints keeps only the last 4 characters.
CREATE TABLE payment_providers (
    profile_id  uuid        NOT NULL REFERENCES profiles(id) ON DELETE CASCADE,
    provider    text        NOT NULL CHECK (provider IN ('xendit', 'midtrans', 'stripe')),
    mode        text        NOT NULL CHECK (mode IN ('sandbox', 'live')),
    enabled     boolean     NOT NULL DEFAULT false,
    credentials bytea       NOT NULL,
    hints       jsonb       NOT NULL DEFAULT '{}',
    verified_at timestamptz,          -- last successful test connection with these keys
    last_error  jsonb,                -- {code, message} of the last failed test, null after a pass
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (profile_id, provider)
);
