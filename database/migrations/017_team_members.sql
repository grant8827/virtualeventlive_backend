-- Team members: logins a host adds to their own account. A member's role is
-- 'staff' or 'admin' and account_owner_id points at the host whose events,
-- tickets and chat they work on. Deleting the host removes their team.
ALTER TABLE users ADD COLUMN IF NOT EXISTS account_owner_id UUID REFERENCES users(id) ON DELETE CASCADE;
ALTER TABLE users ADD COLUMN IF NOT EXISTS status VARCHAR(20) NOT NULL DEFAULT 'active';
CREATE INDEX IF NOT EXISTS idx_users_account_owner_id ON users (account_owner_id);
