-- Payments (c): shop cart orders with physical items. The address is the
-- buyer's personal data: owner-only (dashboard, CSV), never logged, never
-- sent to providers.
ALTER TABLE orders
    ADD COLUMN shipping     jsonb,                                  -- {name, phone, address, city, postal_code, country, notes}
    ADD COLUMN shipping_fee bigint      NOT NULL DEFAULT 0 CHECK (shipping_fee >= 0),
    ADD COLUMN fulfilment   text        CHECK (fulfilment IN ('unfulfilled', 'shipped')), -- null: nothing to ship
    ADD COLUMN tracking     text        NOT NULL DEFAULT '',
    ADD COLUMN shipped_at   timestamptz;
