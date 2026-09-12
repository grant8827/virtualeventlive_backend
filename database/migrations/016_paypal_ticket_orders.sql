-- Persist PayPal ticket orders so the browser return can be completed safely
-- and idempotently. The PayPal order ID is the authoritative external payment
-- reference; no buyer identity or amount is trusted from the return URL.
CREATE TABLE IF NOT EXISTS paypal_ticket_orders (
    order_id VARCHAR(64) PRIMARY KEY,
    event_id UUID NOT NULL REFERENCES events(id) ON DELETE CASCADE,
    buyer_id UUID REFERENCES users(id) ON DELETE SET NULL,
    buyer_email TEXT NOT NULL,
    amount NUMERIC(10, 2) NOT NULL,
    merchant_id VARCHAR(32),
    status VARCHAR(20) NOT NULL DEFAULT 'created'
        CHECK (status IN ('created', 'completed')),
    ticket_id UUID REFERENCES tickets(id) ON DELETE SET NULL,
    created_at TIMESTAMP WITH TIME ZONE DEFAULT CURRENT_TIMESTAMP,
    completed_at TIMESTAMP WITH TIME ZONE
);

ALTER TABLE paypal_ticket_orders ADD COLUMN IF NOT EXISTS merchant_id VARCHAR(32);

CREATE INDEX IF NOT EXISTS idx_paypal_ticket_orders_event_id
    ON paypal_ticket_orders(event_id);
