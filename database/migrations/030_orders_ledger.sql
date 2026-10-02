-- Phase 2: orders, payments, refunds, disputes and an immutable ledger.
-- All money is integer cents (PAY-06). Migrations run on every start, so
-- everything here is idempotent and the one-off backfill is flag-guarded.
--
-- The schema part runs once (guarded by a schema_flags row): re-running
-- ALTER TABLE / CREATE INDEX on every start takes table locks even when
-- there is nothing to do, which can deadlock with live traffic on another
-- instance. The ledger trigger functions are (re)defined first.

DO $migration$
BEGIN
    IF EXISTS (SELECT 1 FROM schema_flags WHERE name = 'migration_030_orders_ledger') THEN
        RETURN;
    END IF;

    -- One order per checkout: a ticket purchase or an event booking fee
    -- (PAY-03). It records what was charged, under which fee rule (PAY-07),
    -- through which provider, and its state:
    --   pending          checkout started; holds its tickets until reserved_until
    --   paid             money received and fulfilled
    --   failed           payment failed or checkout could not be created
    --   expired          checkout abandoned
    --   needs_attention  money received but not fulfillable (e.g. the event was
    --                    cancelled meanwhile, or the amount didn't match):
    --                    an operator must refund or fix it
    CREATE TABLE IF NOT EXISTS orders (
        id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
        kind VARCHAR(20) NOT NULL CHECK (kind IN ('ticket', 'booking_fee')),
        event_id UUID NOT NULL REFERENCES events(id) ON DELETE RESTRICT,
        host_id UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
        buyer_id UUID REFERENCES users(id) ON DELETE SET NULL,
        buyer_email VARCHAR(255) NOT NULL,
        quantity INTEGER NOT NULL DEFAULT 1 CHECK (quantity > 0),
        currency CHAR(3) NOT NULL DEFAULT 'USD',
        unit_amount_cents BIGINT NOT NULL CHECK (unit_amount_cents >= 0),
        amount_cents BIGINT NOT NULL CHECK (amount_cents >= 0),
        fee_rule VARCHAR(40) NOT NULL,
        platform_fee_bps INTEGER NOT NULL CHECK (platform_fee_bps BETWEEN 0 AND 10000),
        platform_fee_cents BIGINT NOT NULL CHECK (platform_fee_cents >= 0 AND platform_fee_cents <= amount_cents),
        provider VARCHAR(20) NOT NULL CHECK (provider IN ('stripe', 'paypal', 'free', 'legacy')),
        -- Stripe Checkout Session ID / PayPal order ID.
        provider_checkout_id VARCHAR(255),
        -- Stripe PaymentIntent ID / PayPal capture ID.
        provider_payment_id VARCHAR(255),
        -- Actual processing fee charged to the platform, once reconciled.
        provider_fee_cents BIGINT,
        -- How the host is paid: instantly at checkout ('stripe' destination
        -- charge, 'paypal' multiparty) or by a later manual payout.
        payout_gateway VARCHAR(20),
        status VARCHAR(20) NOT NULL
            CHECK (status IN ('pending', 'paid', 'failed', 'expired', 'needs_attention')),
        refunded_cents BIGINT NOT NULL DEFAULT 0 CHECK (refunded_cents >= 0 AND refunded_cents <= amount_cents),
        platform_fee_refunded_cents BIGINT NOT NULL DEFAULT 0 CHECK (platform_fee_refunded_cents >= 0),
        -- Currently held by open/lost disputes (withdrawn minus reinstated).
        disputed_cents BIGINT NOT NULL DEFAULT 0 CHECK (disputed_cents >= 0),
        reserved_until TIMESTAMP WITH TIME ZONE,
        failure_reason TEXT,
        created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT CURRENT_TIMESTAMP,
        updated_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT CURRENT_TIMESTAMP,
        paid_at TIMESTAMP WITH TIME ZONE
    );
    CREATE UNIQUE INDEX IF NOT EXISTS idx_orders_provider_checkout
        ON orders (provider, provider_checkout_id) WHERE provider_checkout_id IS NOT NULL;
    CREATE UNIQUE INDEX IF NOT EXISTS idx_orders_provider_payment
        ON orders (provider, provider_payment_id) WHERE provider_payment_id IS NOT NULL;
    CREATE INDEX IF NOT EXISTS idx_orders_event_status ON orders (event_id, status);
    CREATE INDEX IF NOT EXISTS idx_orders_status_created ON orders (status, created_at DESC);
    CREATE INDEX IF NOT EXISTS idx_orders_host ON orders (host_id);
    CREATE INDEX IF NOT EXISTS idx_orders_event_buyer ON orders (event_id, lower(buyer_email));
    -- Free tickets: one per email per event (PAY-11).
    CREATE UNIQUE INDEX IF NOT EXISTS idx_orders_one_free_ticket
        ON orders (event_id, lower(buyer_email))
        WHERE kind = 'ticket' AND provider = 'free' AND status = 'paid';

    -- Tickets belong to an order; refunded or charged-back tickets are voided
    -- (kept for the record, but no longer admit anyone).
    ALTER TABLE tickets ADD COLUMN IF NOT EXISTS order_id UUID REFERENCES orders(id) ON DELETE RESTRICT;
    ALTER TABLE tickets ADD COLUMN IF NOT EXISTS voided_at TIMESTAMP WITH TIME ZONE;
    ALTER TABLE tickets ADD COLUMN IF NOT EXISTS void_reason VARCHAR(40);
    CREATE INDEX IF NOT EXISTS idx_tickets_order_id ON tickets (order_id);

    -- (paypal_ticket_orders.order_id is PayPal's order ID; ours is app_order_id.)
    ALTER TABLE paypal_ticket_orders ADD COLUMN IF NOT EXISTS app_order_id UUID REFERENCES orders(id) ON DELETE RESTRICT;

    -- One row per provider refund, keyed by the provider's refund ID, so
    -- duplicate and out-of-order notifications converge (PAY-05).
    --   posted    the refund is in the ledger
    --   reversed  it later failed/was cancelled and the ledger was reversed
    CREATE TABLE IF NOT EXISTS payment_refunds (
        id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
        order_id UUID NOT NULL REFERENCES orders(id) ON DELETE RESTRICT,
        provider VARCHAR(20) NOT NULL,
        provider_refund_id VARCHAR(255) NOT NULL,
        amount_cents BIGINT NOT NULL CHECK (amount_cents > 0),
        -- The part of the refund taken from platform commission (the rest is
        -- the host's share), fixed when posted so a reversal undoes it exactly.
        platform_fee_cents BIGINT NOT NULL DEFAULT 0 CHECK (platform_fee_cents >= 0),
        status VARCHAR(20) NOT NULL CHECK (status IN ('pending', 'succeeded', 'failed', 'canceled')),
        reason TEXT,
        requested_by UUID REFERENCES users(id) ON DELETE SET NULL,
        posted BOOLEAN NOT NULL DEFAULT FALSE,
        reversed BOOLEAN NOT NULL DEFAULT FALSE,
        created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT CURRENT_TIMESTAMP,
        updated_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT CURRENT_TIMESTAMP,
        UNIQUE (provider, provider_refund_id)
    );
    CREATE INDEX IF NOT EXISTS idx_payment_refunds_order ON payment_refunds (order_id);

    -- Disputes / chargebacks. withdrawn_cents is what the provider currently
    -- holds back; outcome is open, won or lost.
    CREATE TABLE IF NOT EXISTS payment_disputes (
        id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
        order_id UUID NOT NULL REFERENCES orders(id) ON DELETE RESTRICT,
        provider VARCHAR(20) NOT NULL,
        provider_dispute_id VARCHAR(255) NOT NULL,
        amount_cents BIGINT NOT NULL CHECK (amount_cents >= 0),
        withdrawn_cents BIGINT NOT NULL DEFAULT 0 CHECK (withdrawn_cents >= 0),
        fee_cents BIGINT NOT NULL DEFAULT 0 CHECK (fee_cents >= 0),
        -- Balance transactions applied so far; an older notification (fewer
        -- transactions) never rolls the dispute back.
        balance_txns_seen INTEGER NOT NULL DEFAULT 0,
        provider_status VARCHAR(40) NOT NULL,
        outcome VARCHAR(10) NOT NULL DEFAULT 'open' CHECK (outcome IN ('open', 'won', 'lost')),
        reason VARCHAR(100),
        created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT CURRENT_TIMESTAMP,
        updated_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT CURRENT_TIMESTAMP,
        closed_at TIMESTAMP WITH TIME ZONE,
        UNIQUE (provider, provider_dispute_id)
    );
    CREATE INDEX IF NOT EXISTS idx_payment_disputes_order ON payment_disputes (order_id);

    -- The ledger (PAY-05): append-only, double-entry. Each journal is one
    -- financial fact (a sale, a refund, a fee...) with a unique idempotency key,
    -- so replaying a webhook can never post it twice. Its postings are signed
    -- cents (debit positive, credit negative) and must sum to zero.
    --
    -- Accounts:
    --   cash_stripe / cash_paypal / cash_legacy   platform funds at a provider
    --   host_payable          owed to hosts (zero once settled to them)
    --   platform_commission   ticket commission revenue
    --   booking_fee_revenue   event booking fees
    --   processing_fees       provider fees charged to the platform
    --   dispute_losses        amounts withdrawn by disputes
    CREATE TABLE IF NOT EXISTS ledger_journal (
        id BIGSERIAL PRIMARY KEY,
        idempotency_key VARCHAR(255) NOT NULL UNIQUE,
        kind VARCHAR(30) NOT NULL CHECK (kind IN (
            'sale', 'host_transfer', 'booking_fee', 'refund', 'host_transfer_reversal',
            'refund_reversal', 'host_transfer_restore', 'processing_fee',
            'dispute_withdrawal', 'dispute_reinstatement', 'dispute_fee')),
        order_id UUID NOT NULL REFERENCES orders(id) ON DELETE RESTRICT,
        event_id UUID NOT NULL REFERENCES events(id) ON DELETE RESTRICT,
        host_id UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
        currency CHAR(3) NOT NULL DEFAULT 'USD',
        memo TEXT,
        created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT CURRENT_TIMESTAMP
    );
    CREATE INDEX IF NOT EXISTS idx_ledger_journal_order ON ledger_journal (order_id);
    CREATE INDEX IF NOT EXISTS idx_ledger_journal_event ON ledger_journal (event_id);
    CREATE INDEX IF NOT EXISTS idx_ledger_journal_created ON ledger_journal (created_at);

    CREATE TABLE IF NOT EXISTS ledger_postings (
        id BIGSERIAL PRIMARY KEY,
        journal_id BIGINT NOT NULL REFERENCES ledger_journal(id) ON DELETE RESTRICT,
        account VARCHAR(30) NOT NULL CHECK (account IN (
            'cash_stripe', 'cash_paypal', 'cash_legacy', 'host_payable', 'platform_commission',
            'booking_fee_revenue', 'processing_fees', 'dispute_losses')),
        amount_cents BIGINT NOT NULL CHECK (amount_cents <> 0)
    );
    CREATE INDEX IF NOT EXISTS idx_ledger_postings_journal ON ledger_postings (journal_id);
    CREATE INDEX IF NOT EXISTS idx_ledger_postings_account ON ledger_postings (account);

    -- Immutable: corrections are new, reversing journals.
    CREATE OR REPLACE FUNCTION ledger_refuse_change() RETURNS trigger AS $fn$
    BEGIN
        RAISE EXCEPTION 'ledger rows are immutable; post a reversing journal instead';
    END;
    $fn$ LANGUAGE plpgsql;

    -- Triggers are created once (not dropped and re-created on every start,
    -- which would lock the ledger tables while other instances are writing).
    IF NOT EXISTS (SELECT 1 FROM pg_trigger WHERE tgname = 'ledger_journal_immutable') THEN
        CREATE TRIGGER ledger_journal_immutable BEFORE UPDATE OR DELETE ON ledger_journal
            FOR EACH ROW EXECUTE FUNCTION ledger_refuse_change();
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_trigger WHERE tgname = 'ledger_postings_immutable') THEN
        CREATE TRIGGER ledger_postings_immutable BEFORE UPDATE OR DELETE ON ledger_postings
            FOR EACH ROW EXECUTE FUNCTION ledger_refuse_change();
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_trigger WHERE tgname = 'ledger_journal_no_truncate') THEN
        CREATE TRIGGER ledger_journal_no_truncate BEFORE TRUNCATE ON ledger_journal
            FOR EACH STATEMENT EXECUTE FUNCTION ledger_refuse_change();
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_trigger WHERE tgname = 'ledger_postings_no_truncate') THEN
        CREATE TRIGGER ledger_postings_no_truncate BEFORE TRUNCATE ON ledger_postings
            FOR EACH STATEMENT EXECUTE FUNCTION ledger_refuse_change();
    END IF;

    -- Balanced: checked at commit, so a journal and its postings are written in
    -- one transaction and a journal can never be left unbalanced or empty.
    CREATE OR REPLACE FUNCTION ledger_check_balanced() RETURNS trigger AS $fn$
    DECLARE
        jid BIGINT;
        total BIGINT;
        lines INTEGER;
    BEGIN
        IF TG_TABLE_NAME = 'ledger_journal' THEN
            jid := NEW.id;
        ELSE
            jid := NEW.journal_id;
        END IF;
        SELECT COALESCE(SUM(amount_cents), 0), count(*) INTO total, lines
        FROM ledger_postings WHERE journal_id = jid;
        IF lines < 2 OR total <> 0 THEN
            RAISE EXCEPTION 'ledger journal % is unbalanced (% postings, sum %)', jid, lines, total;
        END IF;
        RETURN NULL;
    END;
    $fn$ LANGUAGE plpgsql;

    IF NOT EXISTS (SELECT 1 FROM pg_trigger WHERE tgname = 'ledger_postings_balanced') THEN
        CREATE CONSTRAINT TRIGGER ledger_postings_balanced AFTER INSERT ON ledger_postings
            DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION ledger_check_balanced();
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_trigger WHERE tgname = 'ledger_journal_balanced') THEN
        CREATE CONSTRAINT TRIGGER ledger_journal_balanced AFTER INSERT ON ledger_journal
            DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION ledger_check_balanced();
    END IF;

    -- PayPal webhook inbox, the same shape as stripe_webhook_events (PAY-02).
    CREATE TABLE IF NOT EXISTS paypal_webhook_events (
        event_id VARCHAR(255) PRIMARY KEY,
        event_type VARCHAR(100) NOT NULL,
        resource_id VARCHAR(255),
        status VARCHAR(20) NOT NULL
            CHECK (status IN ('processing', 'processed', 'ignored', 'needs_attention', 'failed')),
        attempts INTEGER NOT NULL DEFAULT 0,
        last_error TEXT,
        first_received_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT CURRENT_TIMESTAMP,
        last_attempt_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT CURRENT_TIMESTAMP,
        processed_at TIMESTAMP WITH TIME ZONE
    );
    CREATE INDEX IF NOT EXISTS idx_paypal_webhook_events_status ON paypal_webhook_events (status);

    -- Reconciliation runs: what was checked and what didn't match.
    CREATE TABLE IF NOT EXISTS reconciliation_runs (
        id BIGSERIAL PRIMARY KEY,
        kind VARCHAR(20) NOT NULL CHECK (kind IN ('books', 'provider')),
        started_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT CURRENT_TIMESTAMP,
        finished_at TIMESTAMP WITH TIME ZONE,
        checked INTEGER NOT NULL DEFAULT 0,
        issues INTEGER NOT NULL DEFAULT 0,
        report JSONB NOT NULL DEFAULT '{}'::jsonb
    );

    INSERT INTO schema_flags (name) VALUES ('migration_030_orders_ledger');
END $migration$;

-- One-off backfill: every sale recorded in the old ledger_entries table
-- becomes an order with balanced journals, and paid booking fees become
-- booking-fee orders. Old rows are left untouched (read-only history).
DO $$
DECLARE
    le RECORD;
    oid UUID;
    jid BIGINT;
    gross BIGINT;
    fee BIGINT;
    prov VARCHAR(20);
    cash VARCHAR(30);
    checkout VARCHAR(255);
BEGIN
    IF EXISTS (SELECT 1 FROM schema_flags WHERE name = 'backfill_orders_from_ledger_v1') THEN
        RETURN;
    END IF;

    FOR le IN
        SELECT l.id, l.ticket_id, l.event_id, l.gross_amount, l.platform_fee, l.payout_gateway,
               l.payout_status, l.settled_at, t.buyer_id, t.guest_email, t.stripe_checkout_session_id,
               e.host_id, u.email AS buyer_user_email, p.order_id AS paypal_order_id
        FROM ledger_entries l
        JOIN tickets t ON t.id = l.ticket_id
        JOIN events e ON e.id = l.event_id
        LEFT JOIN users u ON u.id = t.buyer_id
        LEFT JOIN paypal_ticket_orders p ON p.ticket_id = t.id
        WHERE t.order_id IS NULL
        ORDER BY l.settled_at, l.id
    LOOP
        gross := round(le.gross_amount * 100)::bigint;
        fee := LEAST(round(le.platform_fee * 100)::bigint, gross);
        IF le.stripe_checkout_session_id IS NOT NULL THEN
            prov := 'stripe'; checkout := le.stripe_checkout_session_id;
        ELSIF le.paypal_order_id IS NOT NULL THEN
            prov := 'paypal'; checkout := le.paypal_order_id;
        ELSE
            prov := 'legacy'; checkout := NULL;
        END IF;
        cash := 'cash_' || prov;

        INSERT INTO orders (kind, event_id, host_id, buyer_id, buyer_email, quantity, unit_amount_cents,
                            amount_cents, fee_rule, platform_fee_bps, platform_fee_cents, provider,
                            provider_checkout_id, payout_gateway, status, created_at, updated_at, paid_at)
        VALUES ('ticket', le.event_id, le.host_id, le.buyer_id,
                COALESCE(le.buyer_user_email, le.guest_email, 'unknown'), 1, gross, gross,
                'legacy-ledger', 1000, fee, prov, checkout, le.payout_gateway, 'paid',
                COALESCE(le.settled_at, NOW()), NOW(), COALESCE(le.settled_at, NOW()))
        RETURNING id INTO oid;
        UPDATE tickets SET order_id = oid WHERE id = le.ticket_id;
        UPDATE paypal_ticket_orders SET app_order_id = oid WHERE ticket_id = le.ticket_id;

        IF gross > 0 THEN
            INSERT INTO ledger_journal (idempotency_key, kind, order_id, event_id, host_id, memo, created_at)
            VALUES ('sale:' || oid, 'sale', oid, le.event_id, le.host_id, 'backfilled from ledger_entries ' || le.id,
                    COALESCE(le.settled_at, NOW()))
            RETURNING id INTO jid;
            INSERT INTO ledger_postings (journal_id, account, amount_cents) VALUES (jid, cash, gross);
            IF gross - fee > 0 THEN
                INSERT INTO ledger_postings (journal_id, account, amount_cents) VALUES (jid, 'host_payable', -(gross - fee));
            END IF;
            IF fee > 0 THEN
                INSERT INTO ledger_postings (journal_id, account, amount_cents) VALUES (jid, 'platform_commission', -fee);
            END IF;

            -- Settled to the host already (destination charge, PayPal
            -- multiparty, or a completed manual payout).
            IF le.payout_status = 'paid' AND gross - fee > 0 THEN
                INSERT INTO ledger_journal (idempotency_key, kind, order_id, event_id, host_id, memo, created_at)
                VALUES ('host_transfer:' || oid, 'host_transfer', oid, le.event_id, le.host_id,
                        'backfilled from ledger_entries ' || le.id, COALESCE(le.settled_at, NOW()))
                RETURNING id INTO jid;
                INSERT INTO ledger_postings (journal_id, account, amount_cents)
                VALUES (jid, 'host_payable', gross - fee), (jid, cash, -(gross - fee));
            END IF;
        END IF;
    END LOOP;

    -- Free tickets and any other ticket without a ledger row (no money).
    FOR le IN
        SELECT t.id AS ticket_id, t.event_id, t.buyer_id, t.guest_email, t.purchased_at,
               e.host_id, u.email AS buyer_user_email
        FROM tickets t
        JOIN events e ON e.id = t.event_id
        LEFT JOIN users u ON u.id = t.buyer_id
        WHERE t.order_id IS NULL
    LOOP
        INSERT INTO orders (kind, event_id, host_id, buyer_id, buyer_email, quantity, unit_amount_cents,
                            amount_cents, fee_rule, platform_fee_bps, platform_fee_cents, provider,
                            status, created_at, updated_at, paid_at)
        VALUES ('ticket', le.event_id, le.host_id, le.buyer_id,
                COALESCE(le.buyer_user_email, le.guest_email, 'unknown'), 1, 0, 0,
                'legacy-free', 0, 0, 'legacy', 'paid',
                COALESCE(le.purchased_at, NOW()), NOW(), COALESCE(le.purchased_at, NOW()))
        RETURNING id INTO oid;
        UPDATE tickets SET order_id = oid WHERE id = le.ticket_id;
    END LOOP;

    -- Booking fees really paid (not bypassed by a superuser or dev switch).
    FOR le IN
        SELECT e.id AS event_id, e.host_id, e.venue_fee, e.created_at, u.email
        FROM events e JOIN users u ON u.id = e.host_id
        WHERE e.venue_paid AND NOT e.venue_bypassed AND e.venue_fee > 0
          AND NOT EXISTS (SELECT 1 FROM orders o WHERE o.event_id = e.id AND o.kind = 'booking_fee')
    LOOP
        gross := round(le.venue_fee * 100)::bigint;
        INSERT INTO orders (kind, event_id, host_id, buyer_id, buyer_email, quantity, unit_amount_cents,
                            amount_cents, fee_rule, platform_fee_bps, platform_fee_cents, provider,
                            status, created_at, updated_at, paid_at)
        VALUES ('booking_fee', le.event_id, le.host_id, le.host_id, le.email, 1, gross, gross,
                'booking-fee', 10000, gross, 'legacy', 'paid', le.created_at, NOW(), le.created_at)
        RETURNING id INTO oid;
        INSERT INTO ledger_journal (idempotency_key, kind, order_id, event_id, host_id, memo, created_at)
        VALUES ('booking_fee:' || oid, 'booking_fee', oid, le.event_id, le.host_id,
                'backfilled from events.venue_fee', le.created_at)
        RETURNING id INTO jid;
        INSERT INTO ledger_postings (journal_id, account, amount_cents)
        VALUES (jid, 'cash_legacy', gross), (jid, 'booking_fee_revenue', -gross);
    END LOOP;

    INSERT INTO schema_flags (name) VALUES ('backfill_orders_from_ledger_v1');
END $$;
