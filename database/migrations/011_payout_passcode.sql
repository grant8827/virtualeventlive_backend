ALTER TABLE users ADD COLUMN IF NOT EXISTS payout_passcode_hash VARCHAR(255);
ALTER TABLE users ADD COLUMN IF NOT EXISTS payout_passcode_failed_attempts INTEGER NOT NULL DEFAULT 0;
ALTER TABLE users ADD COLUMN IF NOT EXISTS payout_passcode_locked_until TIMESTAMP WITH TIME ZONE;
ALTER TABLE users ADD COLUMN IF NOT EXISTS payout_passcode_created_at TIMESTAMP WITH TIME ZONE;

CREATE TABLE IF NOT EXISTS payout_security_events (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    event_type VARCHAR(50) NOT NULL,
    ip_address VARCHAR(100),
    user_agent TEXT,
    created_at TIMESTAMP WITH TIME ZONE DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_payout_security_events_user_id
    ON payout_security_events(user_id, created_at DESC);
