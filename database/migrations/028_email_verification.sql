-- Verified email (AUTH-05).
--   email_verified_at  when the address was proven (link click, or the invite
--                      link a host registered through)
--   pending_email      a requested new address, applied only once verified
ALTER TABLE users ADD COLUMN IF NOT EXISTS email_verified_at TIMESTAMP WITH TIME ZONE;
ALTER TABLE users ADD COLUMN IF NOT EXISTS pending_email VARCHAR(255);

-- One-time verification links; only the token's SHA-256 is stored.
--   purpose 'verify' confirms the account's current email
--   purpose 'change' confirms a new address (users.pending_email)
CREATE TABLE IF NOT EXISTS email_verifications (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    email VARCHAR(255) NOT NULL,
    purpose VARCHAR(10) NOT NULL CHECK (purpose IN ('verify', 'change')),
    token_hash VARCHAR(64) UNIQUE NOT NULL,
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT CURRENT_TIMESTAMP,
    expires_at TIMESTAMP WITH TIME ZONE NOT NULL,
    used_at TIMESTAMP WITH TIME ZONE
);
CREATE INDEX IF NOT EXISTS idx_email_verifications_user ON email_verifications (user_id);

-- Migrations run on every start, so one-off data changes are guarded by a
-- flag. Accounts that existed before verification was introduced are
-- treated as verified (grandfathered) exactly once; later sign-ups are not.
CREATE TABLE IF NOT EXISTS schema_flags (
    name VARCHAR(100) PRIMARY KEY,
    applied_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT CURRENT_TIMESTAMP
);
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM schema_flags WHERE name = 'grandfather_email_verified') THEN
        UPDATE users SET email_verified_at = COALESCE(created_at, NOW()) WHERE email_verified_at IS NULL;
        INSERT INTO schema_flags (name) VALUES ('grandfather_email_verified');
    END IF;
END $$;
