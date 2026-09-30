-- Stripe retries webhook deliveries until they succeed. Keep the Checkout
-- Session ID on the issued ticket so one paid session can never mint more
-- than one ticket.
ALTER TABLE tickets ADD COLUMN IF NOT EXISTS stripe_checkout_session_id TEXT;

CREATE UNIQUE INDEX IF NOT EXISTS idx_tickets_stripe_checkout_session_id
    ON tickets (stripe_checkout_session_id);
