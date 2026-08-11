-- Host-uploaded logo shown in the corner of the ticket. Distinct from
-- card_bg_image (the ticket's background/flyer art) — when unset, the
-- ticket simply shows no logo at all rather than a placeholder.
ALTER TABLE events ADD COLUMN IF NOT EXISTS logo_image TEXT;
