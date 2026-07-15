CREATE TABLE IF NOT EXISTS advertisements (
  id         UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
  host_id    UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  event_id   UUID        REFERENCES events(id) ON DELETE SET NULL,
  headline   TEXT        NOT NULL,
  body       TEXT,
  image_url  TEXT,
  cta_text   TEXT        NOT NULL DEFAULT 'Get Tickets',
  is_active  BOOLEAN     NOT NULL DEFAULT true,
  created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
