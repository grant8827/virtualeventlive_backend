-- PayPal Commerce Platform seller onboarding. Never store seller passwords,
-- bank details, or API secrets; PayPal returns a merchant ID after hosted
-- onboarding and remains the system of record for eligibility.
ALTER TABLE connected_accounts ADD COLUMN IF NOT EXISTS paypal_merchant_id VARCHAR(255);
ALTER TABLE connected_accounts ADD COLUMN IF NOT EXISTS paypal_tracking_id VARCHAR(255);
ALTER TABLE connected_accounts ADD COLUMN IF NOT EXISTS paypal_payments_receivable BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE connected_accounts ADD COLUMN IF NOT EXISTS paypal_email_confirmed BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE connected_accounts ADD COLUMN IF NOT EXISTS paypal_onboarding_complete BOOLEAN NOT NULL DEFAULT FALSE;

CREATE UNIQUE INDEX IF NOT EXISTS connected_accounts_paypal_merchant_id_key
    ON connected_accounts(paypal_merchant_id)
    WHERE paypal_merchant_id IS NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS connected_accounts_paypal_tracking_id_key
    ON connected_accounts(paypal_tracking_id)
    WHERE paypal_tracking_id IS NOT NULL;

-- Orders are recorded before redirecting to a provider. This makes capture
-- idempotent and preserves the original processor for refunds and disputes.
CREATE TABLE IF NOT EXISTS payment_orders (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    provider VARCHAR(20) NOT NULL CHECK (provider IN ('stripe', 'paypal')),
    provider_order_id VARCHAR(255) UNIQUE NOT NULL,
    event_id UUID NOT NULL REFERENCES events(id) ON DELETE CASCADE,
    buyer_id UUID REFERENCES users(id) ON DELETE SET NULL,
    buyer_email VARCHAR(255) NOT NULL,
    gross_amount NUMERIC(10, 2) NOT NULL,
    platform_fee NUMERIC(10, 2) NOT NULL,
    status VARCHAR(20) NOT NULL DEFAULT 'created'
        CHECK (status IN ('created', 'approved', 'completed', 'cancelled', 'refunded', 'failed')),
    provider_capture_id VARCHAR(255),
    ticket_id UUID REFERENCES tickets(id) ON DELETE SET NULL,
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT CURRENT_TIMESTAMP,
    completed_at TIMESTAMP WITH TIME ZONE
);

CREATE INDEX IF NOT EXISTS idx_payment_orders_event_id ON payment_orders(event_id);
CREATE INDEX IF NOT EXISTS idx_payment_orders_buyer_email ON payment_orders(buyer_email);

ALTER TABLE ledger_entries ADD COLUMN IF NOT EXISTS payment_provider VARCHAR(20) NOT NULL DEFAULT 'stripe';
ALTER TABLE ledger_entries ADD COLUMN IF NOT EXISTS processor_fee NUMERIC(10, 2);
ALTER TABLE ledger_entries ADD COLUMN IF NOT EXISTS provider_transaction_id VARCHAR(255);
