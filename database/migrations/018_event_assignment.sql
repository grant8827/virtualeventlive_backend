-- Optional team member responsible for an event. Staff only see events
-- assigned to them; the owner and admins see every event on the account.
-- Deleting the member leaves the event unassigned.
ALTER TABLE events ADD COLUMN IF NOT EXISTS assigned_to UUID REFERENCES users(id) ON DELETE SET NULL;
CREATE INDEX IF NOT EXISTS idx_events_assigned_to ON events (assigned_to);
