-- One row per Stripe webhook event (PAY-02), so deliveries are idempotent and
-- failures are diagnosable without logging payment details. Stores only the
-- Stripe event/object IDs, our outcome, attempt count and a short error.
--   processed        fulfilled; later deliveries are acknowledged as duplicates
--   ignored          nothing to do (e.g. payment still pending, unrelated type)
--   needs_attention  can't be fulfilled by retrying (e.g. paid for an event
--                    cancelled since checkout) — refund/fix by hand
--   failed           temporary failure; Stripe retries and we try again
--   processing       in progress
CREATE TABLE IF NOT EXISTS stripe_webhook_events (
    event_id VARCHAR(255) PRIMARY KEY,
    event_type VARCHAR(100) NOT NULL,
    object_id VARCHAR(255),
    status VARCHAR(20) NOT NULL
        CHECK (status IN ('processing', 'processed', 'ignored', 'needs_attention', 'failed')),
    attempts INTEGER NOT NULL DEFAULT 0,
    last_error TEXT,
    first_received_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT CURRENT_TIMESTAMP,
    last_attempt_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT CURRENT_TIMESTAMP,
    processed_at TIMESTAMP WITH TIME ZONE
);
CREATE INDEX IF NOT EXISTS idx_stripe_webhook_events_status ON stripe_webhook_events (status);
