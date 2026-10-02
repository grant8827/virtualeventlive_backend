package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v2"

	"vertualeventlive/backend/config"
)

// The Phase 2 exit criterion: after success, failure, retry, refund and
// dispute scenarios, the books reconcile exactly — internally, against the
// (fake) provider, and to the cent in every account. No real Stripe
// account, payment or card is involved.

type finEnv struct {
	t       *testing.T
	fs      *fakeStripe
	cfg     *config.Config
	app     *fiber.App
	stripeH *StripeHandler
	actor   string
	n       int
}

func newFinEnv(t *testing.T) *finEnv {
	db := testDB(t)
	e := &finEnv{t: t, fs: newFakeStripe(t)}
	e.cfg = testConfig()
	e.cfg.StripeSecretKey = "sk_test_local_fake"
	e.cfg.StripeWebhookSecret = testWebhookSecret
	e.actor = createTestHost(t, db)

	e.stripeH = &StripeHandler{DB: db, Cfg: e.cfg}
	ticketH := &TicketHandler{DB: db, Cfg: e.cfg}
	eventH := &EventHandler{DB: db, Cfg: e.cfg}
	finH := &FinanceHandler{SuperuserHandler: SuperuserHandler{DB: db}, Cfg: e.cfg}

	e.app = fiber.New()
	e.app.Post("/webhooks/stripe", e.stripeH.Webhook)
	e.app.Post("/tickets/guest-purchase", ticketH.GuestPurchase)
	e.app.Post("/as/:user/events/:id/checkout", func(c *fiber.Ctx) error {
		c.Locals("user_id", c.Params("user"))
		return eventH.Checkout(c)
	})
	e.app.Patch("/as/:user/events/:id/ticket", func(c *fiber.Ctx) error {
		c.Locals("user_id", c.Params("user"))
		return eventH.TicketSetup(c)
	})
	su := e.app.Group("/su", func(c *fiber.Ctx) error {
		c.Locals("user_id", e.actor)
		return c.Next()
	})
	su.Get("/orders", finH.ListOrders)
	su.Get("/orders/:id", finH.GetOrder)
	su.Post("/orders/:id/refund", finH.RefundOrder)
	su.Get("/finance/reconciliation", finH.Reconciliation)
	su.Post("/finance/reconcile", finH.RunProviderReconciliation)
	return e
}

// webhook delivers a signed Stripe event; id "" makes a fresh one.
func (e *finEnv) webhook(id, eventType string, object map[string]any) int {
	e.t.Helper()
	if id == "" {
		e.n++
		id = fmt.Sprintf("evt_fin_%d_%s", e.n, uniq())
	}
	body, sig := stripeEvent(e.t, id, eventType, object)
	status, _ := doReq2(e.t, e.app, "POST", "/webhooks/stripe", body, map[string]string{"Stripe-Signature": sig})
	return status
}

// buy starts a guest checkout and returns the fake session and order ID.
func (e *finEnv) buy(eventID, email string) (*fakeSession, string) {
	e.t.Helper()
	status, body := doReq(e.t, e.app, "POST", "/tickets/guest-purchase",
		fmt.Sprintf(`{"event_id":%q,"email":%q}`, eventID, email))
	if status != 200 || !strings.Contains(body, "checkout_url") {
		e.t.Fatalf("checkout for %s: %d %s", email, status, body)
	}
	s := e.fs.lastSession()
	return s, s.Metadata["order_id"]
}

// paid completes checkout and delivers the webhook.
func (e *finEnv) paid(s *fakeSession, fee int64) {
	e.t.Helper()
	e.fs.pay(s.ID, fee)
	if got := e.webhook("", "checkout.session.completed", e.fs.sessionEvent(s)); got != 200 {
		e.t.Fatalf("completion webhook: %d", got)
	}
}

func orderState(t *testing.T, id string) (status string, refunded, feeRefunded, disputed int64) {
	t.Helper()
	if err := testDB(t).QueryRow(context.Background(),
		`SELECT status, refunded_cents, platform_fee_refunded_cents, disputed_cents FROM orders WHERE id = $1`, id,
	).Scan(&status, &refunded, &feeRefunded, &disputed); err != nil {
		t.Fatalf("order %s: %v", id, err)
	}
	return
}

func orderTickets(t *testing.T, id string) (valid, all int) {
	t.Helper()
	_ = testDB(t).QueryRow(context.Background(),
		`SELECT count(*) FILTER (WHERE voided_at IS NULL), count(*) FROM tickets WHERE order_id = $1`, id).Scan(&valid, &all)
	return
}

// accounts sums ledger postings per account over the given orders.
func accounts(t *testing.T, orderIDs ...string) map[string]int64 {
	t.Helper()
	rows, err := testDB(t).Query(context.Background(),
		`SELECT p.account, SUM(p.amount_cents) FROM ledger_postings p JOIN ledger_journal j ON j.id = p.journal_id
		 WHERE j.order_id::text = ANY($1) GROUP BY p.account`, orderIDs)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var a string
		var v int64
		if err := rows.Scan(&a, &v); err != nil {
			t.Fatal(err)
		}
		if v != 0 {
			out[a] = v
		}
	}
	return out
}

func journalCount(t *testing.T, orderID, kind string) int {
	t.Helper()
	var n int
	_ = testDB(t).QueryRow(context.Background(),
		`SELECT count(*) FROM ledger_journal WHERE order_id = $1 AND kind = $2`, orderID, kind).Scan(&n)
	return n
}

func wantAccounts(t *testing.T, label string, got map[string]int64, want map[string]int64) {
	t.Helper()
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s: %s = %d cents, want %d (all: %v)", label, k, got[k], v, got)
		}
	}
	for k, v := range got {
		if _, ok := want[k]; !ok {
			t.Errorf("%s: unexpected %s = %d cents", label, k, v)
		}
	}
}

func TestFinancialScenariosReconcileExactly(t *testing.T) {
	e := newFinEnv(t)
	host := createStripeHost(t)
	// $19.99 is the price the old float code charged as $19.98.
	event := createTestEvent(t, testDB(t), host, testEvent{Price: 19.99, Paid: true, Active: true})
	mail := func(who string) string { return who + "-" + uniq() + "@phase2.test" }

	// ── Success, with retries ──────────────────────────────────────────
	sA, orderA := e.buy(event, mail("a"))
	if sA.Unit != 1999 || sA.Qty != 1 || sA.AppFee != 200 || sA.Destination == "" || sA.PIMetadata["order_id"] != orderA {
		t.Fatalf("Stripe was asked for unit %d x%d, fee %d, destination %q, PI order %q; want 1999 x1, fee 200",
			sA.Unit, sA.Qty, sA.AppFee, sA.Destination, sA.PIMetadata["order_id"])
	}
	e.fs.pay(sA.ID, 88)
	obj := e.fs.sessionEvent(sA)
	if got := e.webhook("evt_A_"+orderA, "checkout.session.completed", obj); got != 200 {
		t.Fatalf("A completed: %d", got)
	}
	e.webhook("evt_A_"+orderA, "checkout.session.completed", obj)  // duplicate delivery
	e.webhook("", "checkout.session.async_payment_succeeded", obj) // second event, same payment
	if st, _, _, _ := orderState(t, orderA); st != "paid" {
		t.Fatalf("A is %s", st)
	}
	if v, all := orderTickets(t, orderA); v != 1 || all != 1 {
		t.Fatalf("A has %d/%d tickets after retries", v, all)
	}
	if journalCount(t, orderA, "sale") != 1 || journalCount(t, orderA, "host_transfer") != 1 {
		t.Fatal("A was booked more than once")
	}
	wantAccounts(t, "A after sale", accounts(t, orderA), map[string]int64{
		"cash_stripe": 200, "platform_commission": -200,
	})

	// ── Failure and abandonment ───────────────────────────────────────
	sB, orderB := e.buy(event, mail("b"))
	e.webhook("", "checkout.session.async_payment_failed", e.fs.sessionEvent(sB))
	sC, orderC := e.buy(event, mail("c"))
	e.webhook("", "checkout.session.expired", e.fs.sessionEvent(sC))
	for name, id := range map[string]string{"failed": orderB, "expired": orderC} {
		if st, _, _, _ := orderState(t, id); st != name {
			t.Errorf("order should be %s, is %s", name, st)
		}
		if v, all := orderTickets(t, id); all != 0 || v != 0 {
			t.Errorf("%s order has tickets", name)
		}
		if got := accounts(t, id); len(got) != 0 {
			t.Errorf("%s order is in the ledger: %v", name, got)
		}
	}

	// ── Out of order: a refund notice before the payment notice ───────
	sD, orderD := e.buy(event, mail("d"))
	e.fs.pay(sD.ID, 88)
	e.fs.refundOutside(sD.PI, 500)
	early := refundObject("re_early_"+orderD, sD.PI, "", 500, "succeeded")
	if got := e.webhook("evt_D_refund_"+orderD, "refund.created", early); got != 500 {
		t.Fatalf("refund before payment: got %d, want 500 so Stripe retries", got)
	}
	if got := e.webhook("", "checkout.session.completed", e.fs.sessionEvent(sD)); got != 200 {
		t.Fatalf("D completed: %d", got)
	}
	if got := e.webhook("evt_D_refund_"+orderD, "refund.created", early); got != 200 {
		t.Fatalf("refund retry: %d", got)
	}
	if _, refunded, feeRefunded, _ := orderState(t, orderD); refunded != 500 || feeRefunded != 50 {
		t.Fatalf("D refunded %d (fee %d), want 500 (50)", refunded, feeRefunded)
	}

	// ── Refunds from the console: partial, repeat notice, the rest ────
	status, body := doReq(t, e.app, "POST", "/su/orders/"+orderA+"/refund", `{"amount_cents":500,"reason":"customer asked"}`)
	if status != 200 {
		t.Fatalf("partial refund: %d %s", status, body)
	}
	refunds := e.fs.refundsFor(sA.PI)
	if len(refunds) != 1 || refunds[0]["reverse_transfer"] != "true" || refunds[0]["refund_application_fee"] != "true" {
		t.Fatalf("Stripe refund didn't claw back the host share and fee: %v", refunds)
	}
	// Stripe's own notice for the same refund changes nothing.
	e.webhook("", "refund.updated", refundObject(refunds[0]["id"].(string), sA.PI, orderA, 500, "succeeded"))
	if _, refunded, feeRefunded, _ := orderState(t, orderA); refunded != 500 || feeRefunded != 50 {
		t.Fatalf("A after partial refund: %d (fee %d)", refunded, feeRefunded)
	}
	if v, _ := orderTickets(t, orderA); v != 1 {
		t.Fatal("a partial refund voided the ticket")
	}
	if status, _ := doReq(t, e.app, "POST", "/su/orders/"+orderA+"/refund", `{"amount_cents":1500,"reason":"too much"}`); status != 400 {
		t.Fatalf("over-refund: got %d, want 400", status)
	}
	if status, body := doReq(t, e.app, "POST", "/su/orders/"+orderA+"/refund", `{"reason":"event moved"}`); status != 200 {
		t.Fatalf("refund the rest: %d %s", status, body)
	}
	if _, refunded, feeRefunded, _ := orderState(t, orderA); refunded != 1999 || feeRefunded != 200 {
		t.Fatalf("A fully refunded: %d (fee %d), want 1999 (200)", refunded, feeRefunded)
	}
	if v, _ := orderTickets(t, orderA); v != 0 {
		t.Fatal("a full refund left the ticket valid")
	}

	// ── A refund that fails after being booked ────────────────────────
	sE, orderE := e.buy(event, mail("e"))
	e.paid(sE, 88)
	failing := "re_fail_" + orderE
	e.webhook("", "refund.created", refundObject(failing, sE.PI, orderE, 1999, "pending"))
	if v, _ := orderTickets(t, orderE); v != 0 {
		t.Fatal("pending full refund should void the ticket")
	}
	e.webhook("", "refund.failed", refundObject(failing, sE.PI, orderE, 1999, "failed"))
	e.webhook("", "refund.created", refundObject(failing, sE.PI, orderE, 1999, "pending")) // stale notice
	if _, refunded, _, _ := orderState(t, orderE); refunded != 0 {
		t.Fatalf("failed refund left %d refunded", refunded)
	}
	if v, _ := orderTickets(t, orderE); v != 1 {
		t.Fatal("ticket not restored after the refund failed")
	}

	// ── Dispute won (with an out-of-order stale notice) ───────────────
	sF, orderF := e.buy(event, mail("f"))
	e.paid(sF, 88)
	e.fs.setDisputed(sF.PI)
	dpF := "dp_won_" + orderF
	e.webhook("", "charge.dispute.created", disputeObject(dpF, sF.PI, 1999, "needs_response"))
	e.webhook("", "charge.dispute.funds_withdrawn", disputeObject(dpF, sF.PI, 1999, "needs_response", [2]int64{-1999, 1500}))
	e.webhook("", "charge.dispute.created", disputeObject(dpF, sF.PI, 1999, "needs_response")) // stale
	if _, _, _, disputed := orderState(t, orderF); disputed != 1999 {
		t.Fatalf("F disputed %d, want 1999", disputed)
	}
	e.webhook("", "charge.dispute.closed", disputeObject(dpF, sF.PI, 1999, "won", [2]int64{-1999, 1500}, [2]int64{1999, 0}))
	e.webhook("", "charge.dispute.funds_withdrawn", disputeObject(dpF, sF.PI, 1999, "needs_response", [2]int64{-1999, 1500})) // stale
	if _, _, _, disputed := orderState(t, orderF); disputed != 0 {
		t.Fatalf("F still disputed %d after winning", disputed)
	}
	if v, _ := orderTickets(t, orderF); v != 1 {
		t.Fatal("won dispute voided the ticket")
	}

	// ── Dispute lost ───────────────────────────────────────────────────
	sG, orderG := e.buy(event, mail("g"))
	e.paid(sG, 88)
	e.fs.setDisputed(sG.PI)
	dpG := "dp_lost_" + orderG
	e.webhook("", "charge.dispute.funds_withdrawn", disputeObject(dpG, sG.PI, 1999, "needs_response", [2]int64{-1999, 1500}))
	e.webhook("", "charge.dispute.closed", disputeObject(dpG, sG.PI, 1999, "lost", [2]int64{-1999, 1500}))
	if v, _ := orderTickets(t, orderG); v != 0 {
		t.Fatal("lost dispute left the ticket valid")
	}

	// ── A payment whose webhook never arrives ─────────────────────────
	sH, orderH := e.buy(event, mail("h"))
	e.fs.pay(sH.ID, 88)

	// ── Booking fee ────────────────────────────────────────────────────
	unpaid := createTestEvent(t, testDB(t), host, testEvent{Paid: false, Active: false})
	if _, err := testDB(t).Exec(context.Background(), `UPDATE events SET venue_fee = 45.00 WHERE id = $1`, unpaid); err != nil {
		t.Fatal(err)
	}
	if status, body := doReq(t, e.app, "POST", "/as/"+host+"/events/"+unpaid+"/checkout", ""); status != 200 {
		t.Fatalf("booking checkout: %d %s", status, body)
	}
	sBook := e.fs.lastSession()
	orderBook := sBook.Metadata["order_id"]
	if sBook.Unit != 4500 || sBook.PIMetadata["order_id"] != orderBook {
		t.Fatalf("booking session unit %d", sBook.Unit)
	}
	e.paid(sBook, 160)
	var venuePaid bool
	_ = testDB(t).QueryRow(context.Background(), `SELECT venue_paid FROM events WHERE id = $1`, unpaid).Scan(&venuePaid)
	if !venuePaid {
		t.Fatal("booking fee did not activate the event")
	}

	// ── Provider reconciliation: repairs H, books actual fees ─────────
	status, body = doReq(t, e.app, "POST", "/su/finance/reconcile", "")
	if status != 200 {
		t.Fatalf("provider reconciliation: %d %s", status, body)
	}
	var pr ReconcileReport
	_ = json.Unmarshal([]byte(body), &pr)
	ours := map[string]bool{orderA: true, orderB: true, orderC: true, orderD: true, orderE: true, orderF: true, orderG: true, orderH: true, orderBook: true}
	for _, is := range pr.Issues {
		if ours[is.OrderID] {
			t.Errorf("provider reconciliation issue: %+v", is)
		}
	}
	if !strings.Contains(strings.Join(pr.Repaired, " "), sH.ID) {
		t.Fatalf("missed payment %s not repaired: %v", sH.ID, pr.Repaired)
	}
	if st, _, _, _ := orderState(t, orderH); st != "paid" {
		t.Fatalf("H is %s after repair", st)
	}
	// A second run finds nothing new and books no fee twice.
	doReq(t, e.app, "POST", "/su/finance/reconcile", "")
	for _, id := range []string{orderA, orderD, orderE, orderF, orderG, orderH, orderBook} {
		if n := journalCount(t, id, "processing_fee"); n != 1 {
			t.Errorf("order %s has %d processing-fee journals", id, n)
		}
	}

	// ── Books reconcile ────────────────────────────────────────────────
	status, body = doReq(t, e.app, "GET", "/su/finance/reconciliation", "")
	if status != 200 {
		t.Fatalf("books: %d %s", status, body)
	}
	var books struct {
		Books ReconcileReport `json:"books"`
	}
	_ = json.Unmarshal([]byte(body), &books)
	for _, is := range books.Books.Issues {
		if ours[is.OrderID] || is.Check == "ledger_balanced" || is.Check == "journal_balanced" {
			t.Errorf("books issue: %+v", is)
		}
	}

	// ── Exact totals, to the cent ──────────────────────────────────────
	wantAccounts(t, "A (fully refunded)", accounts(t, orderA), map[string]int64{"cash_stripe": -88, "processing_fees": 88})
	wantAccounts(t, "D (500 refunded)", accounts(t, orderD), map[string]int64{
		"cash_stripe": 200 - 50 - 88, "platform_commission": -150, "processing_fees": 88})
	wantAccounts(t, "E (refund failed)", accounts(t, orderE), map[string]int64{
		"cash_stripe": 200 - 88, "platform_commission": -200, "processing_fees": 88})
	wantAccounts(t, "F (dispute won)", accounts(t, orderF), map[string]int64{
		"cash_stripe": 200 - 1500 - 88, "platform_commission": -200, "processing_fees": 1588})
	wantAccounts(t, "G (dispute lost)", accounts(t, orderG), map[string]int64{
		"cash_stripe": 200 - 1999 - 1500 - 88, "platform_commission": -200, "processing_fees": 1588, "dispute_losses": 1999})
	wantAccounts(t, "H (repaired)", accounts(t, orderH), map[string]int64{
		"cash_stripe": 200 - 88, "platform_commission": -200, "processing_fees": 88})
	wantAccounts(t, "booking fee", accounts(t, orderBook), map[string]int64{
		"cash_stripe": 4500 - 160, "booking_fee_revenue": -4500, "processing_fees": 160})

	all := accounts(t, orderA, orderB, orderC, orderD, orderE, orderF, orderG, orderH, orderBook)
	var sum int64
	for _, v := range all {
		sum += v
	}
	if sum != 0 {
		t.Fatalf("ledger for these orders sums to %d, not 0", sum)
	}
	wantAccounts(t, "all orders", all, map[string]int64{
		"cash_stripe":         -88 + 62 + 112 - 1388 - 3387 + 112 + 4340,
		"platform_commission": -(150 + 200 + 200 + 200 + 200),
		"booking_fee_revenue": -4500,
		"processing_fees":     88*6 + 1500*2 + 160,
		"dispute_losses":      1999,
	})
}

// The payments console lists and explains orders without exposing ticket
// access codes, and the dashboard totals come from the ledger.
func TestPaymentsConsoleViews(t *testing.T) {
	e := newFinEnv(t)
	host := createStripeHost(t)
	event := createTestEvent(t, testDB(t), host, testEvent{Price: 30, Paid: true, Active: true})
	email := "console-" + uniq() + "@phase2.test"
	s, orderID := e.buy(event, email)
	e.paid(s, 0)
	doReq(t, e.app, "POST", "/su/orders/"+orderID+"/refund", `{"amount_cents":1000,"reason":"partial"}`)

	status, body := doReq(t, e.app, "GET", "/su/orders?status=refunded&q="+email, "")
	if status != 200 || !strings.Contains(body, orderID) {
		t.Fatalf("list refunded: %d %s", status, body)
	}
	if status, body := doReq(t, e.app, "GET", "/su/orders?status=bogus", ""); status != 400 {
		t.Fatalf("bad filter: %d %s", status, body)
	}
	status, body = doReq(t, e.app, "GET", "/su/orders/"+orderID, "")
	if status != 200 || !strings.Contains(body, `"refund:stripe:`) || !strings.Contains(body, `"refundable_cents":2000`) {
		t.Fatalf("detail: %d %s", status, body)
	}
	var code string
	_ = testDB(t).QueryRow(context.Background(), `SELECT access_token FROM tickets WHERE order_id = $1`, orderID).Scan(&code)
	if code == "" || strings.Contains(body, code) {
		t.Fatal("order detail exposed the ticket access code")
	}
	if status, _ := doReq(t, e.app, "POST", "/su/orders/"+orderID+"/refund", `{"amount_cents":100}`); status != 400 {
		t.Fatalf("refund without a reason: %d, want 400", status)
	}

	app := fiber.New()
	app.Get("/overview", (&SuperuserHandler{DB: testDB(t)}).Overview)
	status, body = doReq(t, app, "GET", "/overview", "")
	if status != 200 || !strings.Contains(body, `"refunds"`) || !strings.Contains(body, `"needs_attention_orders"`) {
		t.Fatalf("overview: %d %s", status, body)
	}
	var ev struct {
		Revenue struct {
			Refunds float64 `json:"refunds"`
		} `json:"revenue"`
	}
	_ = json.Unmarshal([]byte(body), &ev)
	if ev.Revenue.Refunds < 10 {
		t.Fatalf("overview refunds %.2f don't include this $10 refund", ev.Revenue.Refunds)
	}
}
