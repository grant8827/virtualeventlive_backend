package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v2"

	"vertualeventlive/backend/services"
)

// PAY-08: the PayPal booking-fee return and WiPay payouts were never
// verified end to end; they are retired and can't move or mark money.

func TestRetiredPayPalBookingReturnNeverCaptures(t *testing.T) {
	db := testDB(t)
	fp, svc := newFakePayPal(t)
	host := createTestHost(t, db)
	event := createTestEvent(t, db, host, testEvent{Paid: false})
	h := &EventHandler{DB: db, Cfg: testConfig(), PayPal: svc}
	app := fiber.New()
	app.Get("/events/:id/paypal/complete", h.PayPalComplete)

	status, _ := doReq(t, app, "GET", "/events/"+event+"/paypal/complete?state=1.abc&token=ANYORDER", "")
	if status != 302 {
		t.Fatalf("got %d", status)
	}
	if fp.calls["capture"] != 0 {
		t.Fatal("the retired route captured a PayPal payment")
	}
	var paid bool
	_ = db.QueryRow(context.Background(), `SELECT venue_paid FROM events WHERE id = $1`, event).Scan(&paid)
	if paid {
		t.Fatal("the retired route activated an event")
	}
}

func TestWiPayPayoutIsDisabled(t *testing.T) {
	db := testDB(t)
	called := false
	wipay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true }))
	defer wipay.Close()
	host := createTestHost(t, db)
	if _, err := db.Exec(context.Background(),
		`INSERT INTO connected_accounts (user_id, wipay_account_id, payout_gateway) VALUES ($1, 'WIPAY1', 'wipay')`, host); err != nil {
		t.Fatal(err)
	}
	h := &PayoutHandler{DB: db, Cfg: testConfig(), WiPay: &services.WiPayService{APIBaseURL: wipay.URL, APIKey: "k"}}
	app := fiber.New()
	app.Post("/payout", func(c *fiber.Ctx) error { c.Locals("user_id", host); return h.Payout(c) })
	status, body := doReq(t, app, "POST", "/payout", "")
	if status != 503 || !strings.Contains(body, "aren't available") {
		t.Fatalf("payout: %d %s", status, body)
	}
	if called {
		t.Fatal("WiPay was called")
	}
}

// A second booking fee paid for the same event is booked (the money did
// arrive) and flagged for a refund; the event isn't charged twice silently.
func TestDuplicateBookingFeeNeedsAttention(t *testing.T) {
	e := newFinEnv(t)
	host := createStripeHost(t)
	event := createTestEvent(t, testDB(t), host, testEvent{Paid: false})
	_, _ = testDB(t).Exec(context.Background(), `UPDATE events SET venue_fee = 30 WHERE id = $1`, event)

	doReq(t, e.app, "POST", "/as/"+host+"/events/"+event+"/checkout", "")
	first := e.fs.lastSession()
	doReq(t, e.app, "POST", "/as/"+host+"/events/"+event+"/checkout", "") // second tab
	second := e.fs.lastSession()
	e.paid(first, 0)
	e.paid(second, 0)

	if st, _, _, _ := orderState(t, first.Metadata["order_id"]); st != "paid" {
		t.Fatalf("first booking fee is %s", st)
	}
	if st, _, _, _ := orderState(t, second.Metadata["order_id"]); st != "needs_attention" {
		t.Fatalf("second booking fee is %s, want needs_attention", st)
	}
	if got := accounts(t, second.Metadata["order_id"]); got["cash_stripe"] != 3000 {
		t.Fatalf("duplicate payment not booked: %v", got)
	}
}
