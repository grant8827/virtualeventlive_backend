-- Hosts can now receive payouts via Stripe Connect, WiPay, or PayPal instead
-- of only Stripe. stripe_account_id is no longer the sole payout identifier.
-- One row per host across all gateways, so upserts target user_id.
ALTER TABLE connected_accounts ALTER COLUMN stripe_account_id DROP NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS connected_accounts_user_id_key ON connected_accounts(user_id);
ALTER TABLE connected_accounts ADD COLUMN IF NOT EXISTS payout_gateway VARCHAR(20) NOT NULL DEFAULT 'stripe' CHECK (payout_gateway IN ('stripe', 'wipay', 'paypal'));
ALTER TABLE connected_accounts ADD COLUMN IF NOT EXISTS wipay_account_id VARCHAR(255);
ALTER TABLE connected_accounts ADD COLUMN IF NOT EXISTS paypal_account_id VARCHAR(255);

-- Stripe hosts are paid out automatically at checkout time via destination
-- charges, so their ledger rows are 'paid' immediately. WiPay/PayPal hosts
-- are charged into the platform's Stripe balance first and paid out in a
-- separate batch, so their rows start 'pending' until /connect/payout runs.
ALTER TABLE ledger_entries ADD COLUMN IF NOT EXISTS payout_gateway VARCHAR(20) NOT NULL DEFAULT 'stripe' CHECK (payout_gateway IN ('stripe', 'wipay', 'paypal'));
ALTER TABLE ledger_entries ADD COLUMN IF NOT EXISTS payout_status VARCHAR(20) NOT NULL DEFAULT 'paid' CHECK (payout_status IN ('pending', 'paid', 'failed'));
ALTER TABLE ledger_entries ADD COLUMN IF NOT EXISTS paid_out_at TIMESTAMP WITH TIME ZONE;

CREATE INDEX IF NOT EXISTS idx_ledger_payout_status ON ledger_entries(payout_status);
