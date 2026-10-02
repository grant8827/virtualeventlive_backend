-- One-time, short-lived claim links for the ticket-success page. A purchase
-- creates a claim; the buyer is sent to /ticket-success?claim=<token> and the
-- page exchanges the token for exactly that purchase's ticket. Replaces the
-- old ?email= lookup, which let anyone who knew an email address read that
-- customer's tickets and access codes. Only the token's SHA-256 is stored.
CREATE TABLE IF NOT EXISTS ticket_claims (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    token_hash VARCHAR(64) UNIQUE NOT NULL,
    event_id UUID NOT NULL REFERENCES events(id) ON DELETE CASCADE,
    ticket_id UUID REFERENCES tickets(id) ON DELETE CASCADE,
    stripe_session_id VARCHAR(255) UNIQUE,
    paypal_order_id VARCHAR(64) UNIQUE,
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT CURRENT_TIMESTAMP,
    expires_at TIMESTAMP WITH TIME ZONE NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_ticket_claims_expires_at ON ticket_claims (expires_at);
