package handlers

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/jackc/pgx/v5"

	"vertualeventlive/backend/database"
)

// Migration 030 turns the sales already recorded in ledger_entries into
// orders and balanced journals. This runs it against a scratch database
// shaped like production before Phase 2 (migrations 001–029 plus legacy
// rows), then checks every legacy sale reconciles.
func TestBackfillOfLegacySalesReconciles(t *testing.T) {
	testDB(t) // refuses anything but a local database named *test*
	ctx := context.Background()
	u, _ := url.Parse(os.Getenv("TEST_DATABASE_URL"))
	name := "vep_phase2_backfill_test"
	admin, err := pgx.Connect(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)
	_, _ = admin.Exec(ctx, `DROP DATABASE IF EXISTS `+name)
	if _, err := admin.Exec(ctx, `CREATE DATABASE `+name); err != nil {
		t.Skipf("cannot create scratch database: %v", err)
	}
	t.Cleanup(func() { _, _ = admin.Exec(context.Background(), `DROP DATABASE IF EXISTS `+name+` WITH (FORCE)`) })

	u.Path = "/" + name
	pool, err := database.ConnectPostgres(u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	// The schema as production has it today.
	files, _ := filepath.Glob("../database/migrations/*.sql")
	sort.Strings(files)
	for _, f := range files {
		if filepath.Base(f) >= "030" {
			break
		}
		sql, _ := os.ReadFile(f)
		if _, err := pool.Exec(ctx, string(sql)); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
	}

	// Legacy data: a Stripe sale (settled), a PayPal sale, a WiPay sale
	// still owed to the host, a free ticket, a paid booking fee and a
	// bypassed one.
	exec := func(sql string, args ...any) string {
		t.Helper()
		var id string
		if err := pool.QueryRow(ctx, sql+` RETURNING id::text`, args...).Scan(&id); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return id
	}
	host := exec(`INSERT INTO users (email, password_hash, role, status) VALUES ('legacy-host@phase2.test', 'x', 'host', 'active')`)
	buyer := exec(`INSERT INTO users (email, password_hash, role, status) VALUES ('legacy-buyer@phase2.test', 'x', 'buyer', 'active')`)
	paidEvent := exec(`INSERT INTO events (host_id, title, start_time, ends_at, ticket_price, venue_fee, venue_paid, is_active)
	                   VALUES ($1, 'Legacy', NOW() + INTERVAL '1 day', NOW() + INTERVAL '2 days', 19.99, 45.00, true, true)`, host)
	_ = exec(`INSERT INTO events (host_id, title, start_time, ends_at, ticket_price, venue_fee, venue_paid, venue_bypassed, is_active)
	          VALUES ($1, 'Bypassed', NOW() + INTERVAL '1 day', NOW() + INTERVAL '2 days', 0, 15.00, true, true, true)`, host)

	stripeTicket := exec(`INSERT INTO tickets (event_id, buyer_id, access_token, stripe_checkout_session_id) VALUES ($1, $2, 'LEGACY1', 'cs_legacy_1')`, paidEvent, buyer)
	exec(`INSERT INTO ledger_entries (ticket_id, event_id, gross_amount, stripe_fee, platform_fee, host_payout, payout_gateway, payout_status)
	      VALUES ($1, $2, 19.99, 0.88, 2.00, 17.99, 'stripe', 'paid')`, stripeTicket, paidEvent)
	paypalTicket := exec(`INSERT INTO tickets (event_id, guest_email, access_token) VALUES ($1, 'pp-guest@phase2.test', 'LEGACY2')`, paidEvent)
	exec(`INSERT INTO ledger_entries (ticket_id, event_id, gross_amount, stripe_fee, platform_fee, host_payout, payout_gateway, payout_status)
	      VALUES ($1, $2, 19.99, 0, 2.00, 17.99, 'paypal', 'paid')`, paypalTicket, paidEvent)
	if _, err := pool.Exec(ctx, `INSERT INTO paypal_ticket_orders (order_id, event_id, buyer_email, amount, status, ticket_id)
	                             VALUES ('PPLEGACY', $1, 'pp-guest@phase2.test', 19.99, 'completed', $2)`, paidEvent, paypalTicket); err != nil {
		t.Fatal(err)
	}
	wipayTicket := exec(`INSERT INTO tickets (event_id, guest_email, access_token) VALUES ($1, 'wp@phase2.test', 'LEGACY3')`, paidEvent)
	exec(`INSERT INTO ledger_entries (ticket_id, event_id, gross_amount, stripe_fee, platform_fee, host_payout, payout_gateway, payout_status)
	      VALUES ($1, $2, 10.00, 0.59, 1.00, 8.41, 'wipay', 'pending')`, wipayTicket, paidEvent)
	exec(`INSERT INTO tickets (event_id, guest_email, access_token) VALUES ($1, 'free@phase2.test', 'LEGACY4')`, paidEvent)

	// Phase 2 migrations, twice (every start re-runs them).
	for i := 0; i < 2; i++ {
		if err := database.RunMigrations(pool); err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
	}

	var orders, orphans int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM orders`).Scan(&orders)
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM tickets WHERE order_id IS NULL`).Scan(&orphans)
	if orders != 5 || orphans != 0 {
		t.Fatalf("%d orders (want 4 tickets + 1 booking fee), %d tickets without an order", orders, orphans)
	}
	var provider, checkout string
	_ = pool.QueryRow(ctx, `SELECT o.provider, o.provider_checkout_id FROM orders o JOIN tickets t ON t.order_id = o.id WHERE t.id = $1`, paypalTicket).Scan(&provider, &checkout)
	if provider != "paypal" || checkout != "PPLEGACY" {
		t.Fatalf("PayPal sale backfilled as %s / %s", provider, checkout)
	}

	rows, err := pool.Query(ctx, `SELECT p.account, SUM(p.amount_cents) FROM ledger_postings p GROUP BY p.account`)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int64{}
	for rows.Next() {
		var a string
		var v int64
		_ = rows.Scan(&a, &v)
		if v != 0 {
			got[a] = v
		}
	}
	rows.Close()
	// Stripe and PayPal hosts were paid at checkout; the WiPay host is
	// still owed 9.00 (10.00 less the 1.00 commission).
	wantAccounts(t, "backfilled ledger", got, map[string]int64{
		"cash_stripe": 200, "cash_paypal": 200, "cash_legacy": 1000 + 4500,
		"platform_commission": -(200 + 200 + 100), "host_payable": -900, "booking_fee_revenue": -4500,
	})

	r, err := ReconcileBooks(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Issues) != 0 || r.OrdersChecked != 5 {
		t.Fatalf("backfilled books: %d orders, issues %v", r.OrdersChecked, r.Issues)
	}
}
