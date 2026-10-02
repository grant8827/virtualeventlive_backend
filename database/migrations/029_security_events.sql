-- Security audit log (Phase 1). Append-only record of sign-ins, failures,
-- lockouts, password and email changes, role/status changes, invitations and
-- superuser actions. Holds IDs, IP and user agent and small non-secret
-- details — never passwords, tokens or plain email addresses (emails are
-- hashed when a failed attempt has no matching account).
CREATE TABLE IF NOT EXISTS security_events (
    id BIGSERIAL PRIMARY KEY,
    occurred_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT CURRENT_TIMESTAMP,
    event_type VARCHAR(60) NOT NULL,
    actor_user_id UUID REFERENCES users(id) ON DELETE SET NULL,
    target_user_id UUID REFERENCES users(id) ON DELETE SET NULL,
    ip VARCHAR(64),
    user_agent VARCHAR(300),
    details JSONB NOT NULL DEFAULT '{}'::jsonb
);
CREATE INDEX IF NOT EXISTS idx_security_events_occurred_at ON security_events (occurred_at DESC);
CREATE INDEX IF NOT EXISTS idx_security_events_type ON security_events (event_type, occurred_at DESC);
CREATE INDEX IF NOT EXISTS idx_security_events_actor ON security_events (actor_user_id, occurred_at DESC);
CREATE INDEX IF NOT EXISTS idx_security_events_target ON security_events (target_user_id, occurred_at DESC);
