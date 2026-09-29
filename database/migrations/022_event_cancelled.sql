-- Set when a superuser cancels an event, so it reads "Cancelled" rather than
-- just "Ended" on the platform dashboard.
ALTER TABLE events ADD COLUMN IF NOT EXISTS cancelled_at TIMESTAMP WITH TIME ZONE;
