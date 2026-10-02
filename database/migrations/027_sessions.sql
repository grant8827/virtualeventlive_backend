-- Server-side sign-in sessions (AUTH-03). The browser holds a random session
-- ID in an HttpOnly, Secure, SameSite cookie; only its SHA-256 is stored, so
-- the table can't be used to sign in. Sessions are revocable, expire after
-- idle and absolute limits, and rotate their ID periodically. During a short
-- grace window after rotation the previous ID still works, so requests that
-- were already in flight don't fail.
CREATE TABLE IF NOT EXISTS sessions (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    token_hash VARCHAR(64) UNIQUE NOT NULL,
    previous_token_hash VARCHAR(64),
    previous_valid_until TIMESTAMP WITH TIME ZONE,
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT CURRENT_TIMESTAMP,
    rotated_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT CURRENT_TIMESTAMP,
    last_seen_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT CURRENT_TIMESTAMP,
    expires_at TIMESTAMP WITH TIME ZONE NOT NULL,
    revoked_at TIMESTAMP WITH TIME ZONE,
    revoked_reason VARCHAR(50),
    ip VARCHAR(64),
    user_agent VARCHAR(300)
);
CREATE INDEX IF NOT EXISTS idx_sessions_user_active ON sessions (user_id) WHERE revoked_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_sessions_previous_token_hash ON sessions (previous_token_hash) WHERE previous_token_hash IS NOT NULL;
