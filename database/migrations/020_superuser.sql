-- Platform superusers have users.role = 'superuser' and are created with
-- `go run ./cmd/superuser`, never through sign-up.
--
-- venue_bypassed marks events activated without a real venue-fee payment
-- (the dev bypass, a superuser activation, or no payment provider being
-- configured) so platform revenue only counts fees that were actually paid.
ALTER TABLE events ADD COLUMN IF NOT EXISTS venue_bypassed BOOLEAN NOT NULL DEFAULT false;
CREATE INDEX IF NOT EXISTS idx_users_role ON users (role);
CREATE INDEX IF NOT EXISTS idx_users_created_at ON users (created_at);
CREATE INDEX IF NOT EXISTS idx_tickets_purchased_at ON tickets (purchased_at);
CREATE INDEX IF NOT EXISTS idx_ledger_settled_at ON ledger_entries (settled_at);
