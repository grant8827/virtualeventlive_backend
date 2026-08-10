-- Physical venue address, shown on "Virtual + Location" tickets.
ALTER TABLE events ADD COLUMN IF NOT EXISTS venue_address TEXT;

-- Sequential per-ticket serial number — the human-typable fallback for the
-- door QR code, independent of access_token (the short code used to unlock
-- the virtual stream).
CREATE SEQUENCE IF NOT EXISTS tickets_serial_no_seq;
ALTER TABLE tickets ADD COLUMN IF NOT EXISTS serial_no BIGINT NOT NULL DEFAULT nextval('tickets_serial_no_seq');
ALTER SEQUENCE tickets_serial_no_seq OWNED BY tickets.serial_no;
CREATE UNIQUE INDEX IF NOT EXISTS idx_tickets_serial_no ON tickets (serial_no);

-- One-time redemption tracking for "Virtual + Location" tickets: whichever
-- channel (virtual stream entry or physical door scan) claims the ticket
-- first locks it — the other channel then reports "already used".
ALTER TABLE tickets ADD COLUMN IF NOT EXISTS checked_in_at TIMESTAMP WITH TIME ZONE;
ALTER TABLE tickets ADD COLUMN IF NOT EXISTS checked_in_channel TEXT;
