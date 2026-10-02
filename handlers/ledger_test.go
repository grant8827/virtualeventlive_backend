package handlers

import (
	"context"
	"strings"
	"testing"
)

// PAY-05: the ledger is append-only and every journal balances. These are
// enforced by the database, so not even a bug in the Go code (or a manual
// SQL session) can rewrite history or post money out of nowhere.

func paidTestOrder(t *testing.T) *order {
	t.Helper()
	db := testDB(t)
	ctx := context.Background()
	host := createStripeHost(t)
	event := createTestEvent(t, db, host, testEvent{Price: 10, Paid: true, Active: true})
	h := &TicketHandler{DB: db, Cfg: testConfig()}
	o, err := createPendingOrder(ctx, db, newOrder{Kind: "ticket", EventID: event, BuyerEmail: "ledger-" + uniq() + "@phase2.test",
		Quantity: 1, UnitCents: 1000, Fee: feeRule(h.Cfg), Provider: "stripe", Reserve: true})
	if err != nil {
		t.Fatal(err)
	}
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := markOrderPaid(ctx, tx, o, fulfillment{CheckoutID: "cs_ledger_" + uniq(), PaymentID: "pi_ledger_" + uniq()}, nil); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return o
}

func TestLedgerRowsAreImmutable(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	o := paidTestOrder(t)

	for name, sql := range map[string]string{
		"update posting": `UPDATE ledger_postings SET amount_cents = amount_cents + 1
		                   WHERE journal_id IN (SELECT id FROM ledger_journal WHERE order_id = $1)`,
		"delete posting": `DELETE FROM ledger_postings WHERE journal_id IN (SELECT id FROM ledger_journal WHERE order_id = $1)`,
		"update journal": `UPDATE ledger_journal SET memo = 'edited' WHERE order_id = $1`,
		"delete journal": `DELETE FROM ledger_journal WHERE order_id = $1`,
	} {
		if _, err := db.Exec(ctx, sql, o.ID); err == nil || !strings.Contains(err.Error(), "immutable") {
			t.Errorf("%s: got %v, want refusal", name, err)
		}
	}
	for _, table := range []string{"ledger_postings", "ledger_journal"} {
		if _, err := db.Exec(ctx, `TRUNCATE `+table+` CASCADE`); err == nil {
			t.Errorf("TRUNCATE %s was allowed", table)
		}
	}
	// The order's money can't be removed by deleting the order either.
	if _, err := db.Exec(ctx, `DELETE FROM orders WHERE id = $1`, o.ID); err == nil {
		t.Error("deleting a booked order was allowed")
	}
	if got := accounts(t, o.ID); got["cash_stripe"] != 100 || got["platform_commission"] != -100 {
		t.Fatalf("ledger changed: %v", got)
	}
}

func TestLedgerRefusesUnbalancedOrEmptyJournals(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	o := paidTestOrder(t)

	try := func(name string, postings ...posting) {
		t.Helper()
		tx, err := db.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		var id int64
		if err := tx.QueryRow(ctx,
			`INSERT INTO ledger_journal (idempotency_key, kind, order_id, event_id, host_id)
			 VALUES ($1, 'refund', $2, $3, $4) RETURNING id`, "test:"+uniq(), o.ID, o.EventID, o.HostID,
		).Scan(&id); err != nil {
			t.Fatal(err)
		}
		for _, p := range postings {
			if _, err := tx.Exec(ctx, `INSERT INTO ledger_postings (journal_id, account, amount_cents) VALUES ($1, $2, $3)`,
				id, p.Account, p.Cents); err != nil {
				t.Fatal(err)
			}
		}
		if err := tx.Commit(ctx); err == nil || !strings.Contains(err.Error(), "unbalanced") {
			t.Errorf("%s: commit got %v, want unbalanced", name, err)
		}
	}
	try("money from nowhere", posting{"cash_stripe", 500}, posting{"platform_commission", -499})
	try("single posting", posting{"cash_stripe", 500})
	try("no postings")

	// The Go side refuses before the database has to.
	tx, _ := db.Begin(ctx)
	defer tx.Rollback(ctx)
	if _, err := postJournal(ctx, tx, journal{Key: "test:" + uniq(), Kind: "refund", Order: o,
		Postings: []posting{{"cash_stripe", 1}, {"host_payable", 0}}}); err == nil {
		t.Error("postJournal accepted an unbalanced journal")
	}
}

func TestJournalsPostOnceByKey(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	o := paidTestOrder(t)
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	// Booking the same sale again (a replayed webhook) posts nothing.
	if err := postSale(ctx, tx, o); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if n := journalCount(t, o.ID, "sale"); n != 1 {
		t.Fatalf("%d sale journals", n)
	}
}
