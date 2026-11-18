-- Payments (b): orders paid on the creator's own provider account, and the
-- webhook deliveries we've processed (one row per provider event: idempotency).
CREATE TABLE orders (
    id              uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    code            text        NOT NULL UNIQUE,            -- short code shown to the buyer, e.g. K7Q2-M9XD
    profile_id      uuid        NOT NULL REFERENCES profiles(id) ON DELETE CASCADE,
    buyer_email     citext      NOT NULL,
    buyer_name      text        NOT NULL DEFAULT '',
    items           jsonb       NOT NULL,                   -- snapshot: name, price, qty, the file to deliver
    amount          bigint      NOT NULL CHECK (amount > 0), -- minor units
    currency        text        NOT NULL CHECK (currency IN ('IDR', 'USD')),
    provider        text        NOT NULL CHECK (provider IN ('xendit', 'midtrans', 'stripe')),
    mode            text        NOT NULL CHECK (mode IN ('sandbox', 'live')),
    status          text        NOT NULL DEFAULT 'pending'
                                CHECK (status IN ('pending', 'paid', 'expired', 'failed', 'refunded')),
    provider_ref    text,                                   -- invoice id / Midtrans order id / checkout session id
    payment_ref     text,                                   -- Stripe payment_intent (refunds arrive by it)
    checkout_url    text,
    contact_id      uuid        REFERENCES contacts(id) ON DELETE SET NULL,
    downloads       integer     NOT NULL DEFAULT 0,
    expires_at      timestamptz NOT NULL,
    paid_at         timestamptz,
    refunded_at     timestamptz,
    last_checked_at timestamptz,                            -- last GetStatus reconcile
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX orders_profile_created_idx ON orders (profile_id, created_at DESC, id DESC);
CREATE UNIQUE INDEX orders_provider_ref_idx ON orders (provider, provider_ref) WHERE provider_ref IS NOT NULL;
CREATE INDEX orders_payment_ref_idx ON orders (provider, payment_ref) WHERE payment_ref IS NOT NULL;

CREATE TABLE payment_events (
    id          bigserial   PRIMARY KEY,
    profile_id  uuid        NOT NULL REFERENCES profiles(id) ON DELETE CASCADE,
    provider    text        NOT NULL,
    event_key   text        NOT NULL,
    order_id    uuid        REFERENCES orders(id) ON DELETE SET NULL,
    type        text        NOT NULL DEFAULT '',
    status      text        NOT NULL DEFAULT '',   -- the status the event asked for, or a note (ignored, amount_mismatch)
    received_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (profile_id, provider, event_key)
);
CREATE INDEX payment_events_order_idx ON payment_events (order_id, received_at);
