-- Dev-only "fake" payment provider (internal/payments/fake.go). The API refuses
-- it when APP_ENV=production, so widening the CHECKs is harmless there.
ALTER TABLE payment_providers DROP CONSTRAINT payment_providers_provider_check;
ALTER TABLE payment_providers ADD CONSTRAINT payment_providers_provider_check
    CHECK (provider IN ('xendit', 'midtrans', 'stripe', 'fake'));
ALTER TABLE orders DROP CONSTRAINT orders_provider_check;
ALTER TABLE orders ADD CONSTRAINT orders_provider_check
    CHECK (provider IN ('xendit', 'midtrans', 'stripe', 'fake'));
