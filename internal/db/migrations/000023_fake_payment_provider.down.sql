DELETE FROM payment_events WHERE provider = 'fake';
DELETE FROM orders WHERE provider = 'fake';
DELETE FROM payment_providers WHERE provider = 'fake';
ALTER TABLE payment_providers DROP CONSTRAINT payment_providers_provider_check;
ALTER TABLE payment_providers ADD CONSTRAINT payment_providers_provider_check
    CHECK (provider IN ('xendit', 'midtrans', 'stripe'));
ALTER TABLE orders DROP CONSTRAINT orders_provider_check;
ALTER TABLE orders ADD CONSTRAINT orders_provider_check
    CHECK (provider IN ('xendit', 'midtrans', 'stripe'));
