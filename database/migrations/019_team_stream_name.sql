-- Optional on-air name for a team member, shown next to them when an event
-- is assigned on the Book Event form.
ALTER TABLE users ADD COLUMN IF NOT EXISTS stream_name VARCHAR(255);
