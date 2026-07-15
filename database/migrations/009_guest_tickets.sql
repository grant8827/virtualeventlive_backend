ALTER TABLE tickets ALTER COLUMN buyer_id DROP NOT NULL;
ALTER TABLE tickets ADD COLUMN IF NOT EXISTS guest_email TEXT;
CREATE INDEX IF NOT EXISTS idx_tickets_guest_email ON tickets(guest_email);
