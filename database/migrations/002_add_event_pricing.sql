ALTER TABLE events
  ADD COLUMN IF NOT EXISTS ends_at TIMESTAMPTZ,
  ADD COLUMN IF NOT EXISTS venue_fee NUMERIC(10,2) NOT NULL DEFAULT 0,
  ADD COLUMN IF NOT EXISTS venue_paid BOOLEAN NOT NULL DEFAULT FALSE;

-- Backfill ends_at for any existing rows
UPDATE events SET ends_at = start_time + INTERVAL '2 hours' WHERE ends_at IS NULL;
