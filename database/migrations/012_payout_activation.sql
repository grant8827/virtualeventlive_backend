-- A connected payout account can be retained without being active. A NULL
-- gateway pauses new ticket sales while preserving the host's account IDs.
ALTER TABLE connected_accounts ALTER COLUMN payout_gateway DROP NOT NULL;

