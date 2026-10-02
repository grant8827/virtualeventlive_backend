package handlers

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/gofiber/fiber/v2"
)

// PAY-09 / PAY-11: capacity with reservations, per-buyer limits, one free
// ticket per email, exact prices.

func TestPaidCheckoutReservesTheLastTicket(t *testing.T) {
	e := newFinEnv(t)
	host := createStripeHost(t)
	one := 1
	event := createTestEvent(t, testDB(t), host, testEvent{Price: 25, Paid: true, Active: true, MaxTix: &one})

	s1, o1 := e.buy(event, "first-"+uniq()+"@phase2.test")
	// The first buyer's checkout holds the only ticket: nobody else can
	// start paying for it.
	status, body := doReq(t, e.app, "POST", "/tickets/guest-purchase",
		fmt.Sprintf(`{"event_id":%q,"email":"second-%s@phase2.test"}`, event, uniq()))
	if status != 409 || !strings.Contains(body, "sold out") {
		t.Fatalf("second buyer: %d %s, want 409 sold out", status, body)
	}
	if n := e.fs.sessionCount(); n != 1 {
		t.Fatalf("%d Stripe sessions created, want 1", n)
	}
	// Abandoned: the reservation is released and the ticket is on sale again.
	e.webhook("", "checkout.session.expired", e.fs.sessionEvent(s1))
	if st, _, _, _ := orderState(t, o1); st != "expired" {
		t.Fatalf("first order is %s", st)
	}
	s3, o3 := e.buy(event, "third-"+uniq()+"@phase2.test")
	e.paid(s3, 0)
	if v, _ := orderTickets(t, o3); v != 1 {
		t.Fatal("third buyer got no ticket")
	}
	if n := ticketCount(t, event); n != 1 {
		t.Fatalf("%d tickets for a 1-ticket event", n)
	}
}

func TestReservationExpiresByTime(t *testing.T) {
	e := newFinEnv(t)
	host := createStripeHost(t)
	one := 1
	event := createTestEvent(t, testDB(t), host, testEvent{Price: 25, Paid: true, Active: true, MaxTix: &one})
	_, o1 := e.buy(event, "slow-"+uniq()+"@phase2.test")
	// Stripe's expiry notice never comes; the reservation lapses anyway.
	if _, err := testDB(t).Exec(context.Background(), `UPDATE orders SET reserved_until = NOW() - INTERVAL '1 second' WHERE id = $1`, o1); err != nil {
		t.Fatal(err)
	}
	e.buy(event, "next-"+uniq()+"@phase2.test")
}

// A buyer who paid while their reservation lapsed still gets a ticket if one
// is left; if the event sold out meanwhile, the payment is booked and
// flagged for a refund, never oversold.
func TestLatePaymentAfterSellOutNeedsAttentionNotOversell(t *testing.T) {
	e := newFinEnv(t)
	host := createStripeHost(t)
	one := 1
	event := createTestEvent(t, testDB(t), host, testEvent{Price: 25, Paid: true, Active: true, MaxTix: &one})
	slow, oSlow := e.buy(event, "slow-"+uniq()+"@phase2.test")
	_, _ = testDB(t).Exec(context.Background(), `UPDATE orders SET reserved_until = NOW() - INTERVAL '1 second' WHERE id = $1`, oSlow)
	fast, oFast := e.buy(event, "fast-"+uniq()+"@phase2.test")
	e.paid(fast, 0)
	e.paid(slow, 0)
	if v, _ := orderTickets(t, oFast); v != 1 {
		t.Fatal("fast buyer has no ticket")
	}
	if st, _, _, _ := orderState(t, oSlow); st != "needs_attention" {
		t.Fatalf("late order is %s, want needs_attention", st)
	}
	if n := ticketCount(t, event); n != 1 {
		t.Fatalf("%d tickets for a 1-ticket event", n)
	}
	// The late money is still in the books, so it can be refunded.
	if got := accounts(t, oSlow); got["cash_stripe"] != 250 {
		t.Fatalf("late payment not booked: %v", got)
	}
}

func TestOneFreeTicketPerEmail(t *testing.T) {
	db := testDB(t)
	host := createTestHost(t, db)
	event := createTestEvent(t, db, host, onSale)
	h := &TicketHandler{DB: db, Cfg: testConfig()}
	app := fiber.New()
	app.Post("/tickets/guest-purchase", h.GuestPurchase)
	email := "free-" + uniq() + "@phase2.test"

	var ok, refused atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// Same address, varying case.
			addr := email
			if i%2 == 1 {
				addr = strings.ToUpper(email)
			}
			status, _ := doReq(t, app, "POST", "/tickets/guest-purchase", fmt.Sprintf(`{"event_id":%q,"email":%q}`, event, addr))
			switch status {
			case 200:
				ok.Add(1)
			case 409:
				refused.Add(1)
			}
		}(i)
	}
	wg.Wait()
	if ok.Load() != 1 || refused.Load() != 7 {
		t.Fatalf("%d issued, %d refused; want 1 and 7", ok.Load(), refused.Load())
	}
	// Someone else can still claim one.
	if status, _ := doReq(t, app, "POST", "/tickets/guest-purchase", fmt.Sprintf(`{"event_id":%q,"email":"other-%s@phase2.test"}`, event, uniq())); status != 200 {
		t.Fatalf("other email: %d", status)
	}
	if status, _ := doReq(t, app, "POST", "/tickets/guest-purchase", fmt.Sprintf(`{"event_id":%q,"email":"not-an-email"}`, event)); status != 400 {
		t.Fatalf("invalid email: %d, want 400", status)
	}
}

func TestPaidTicketsPerBuyerLimit(t *testing.T) {
	e := newFinEnv(t)
	e.cfg.MaxTicketsPerBuyer = 2
	host := createStripeHost(t)
	event := createTestEvent(t, testDB(t), host, testEvent{Price: 25, Paid: true, Active: true})
	email := "limit-" + uniq() + "@phase2.test"
	s1, _ := e.buy(event, email)
	e.paid(s1, 0)
	e.buy(event, email) // second: reserved, not yet paid
	status, body := doReq(t, e.app, "POST", "/tickets/guest-purchase", fmt.Sprintf(`{"event_id":%q,"email":%q}`, event, strings.ToUpper(email)))
	if status != 409 || !strings.Contains(body, "limit") {
		t.Fatalf("third ticket: %d %s, want 409 limit", status, body)
	}
}

func TestTicketSetupPriceAndCapacity(t *testing.T) {
	e := newFinEnv(t)
	host := createStripeHost(t)
	db := testDB(t)
	event := createTestEvent(t, db, host, testEvent{Price: 25, Paid: true, Active: true})
	path := "/as/" + host + "/events/" + event + "/ticket"
	read := func() (price string, max *int) {
		_ = db.QueryRow(context.Background(), `SELECT ticket_price::text, max_tickets FROM events WHERE id = $1`, event).Scan(&price, &max)
		return
	}

	for body, want := range map[string]int{
		`{"ticket_price":19.999}`:                 400, // fraction of a cent
		`{"ticket_price":-1}`:                     400,
		`{"ticket_price":0.3}`:                    400, // below Stripe's minimum
		`{"ticket_price":1000000}`:                400, // above Stripe's maximum
		`{"ticket_price":19.99,"max_tickets":-5}`: 400,
	} {
		if status, resp := doReq(t, e.app, "PATCH", path, body); status != want {
			t.Errorf("%s: %d %s, want %d", body, status, resp, want)
		}
	}
	if status, resp := doReq(t, e.app, "PATCH", path, `{"ticket_price":19.99,"max_tickets":3}`); status != 200 {
		t.Fatalf("valid setup: %d %s", status, resp)
	}
	if price, max := read(); price != "19.99" || max == nil || *max != 3 {
		t.Fatalf("stored %s / %v", price, max)
	}
	// A client that doesn't send max_tickets keeps the cap.
	if status, _ := doReq(t, e.app, "PATCH", path, `{"ticket_price":5}`); status != 200 {
		t.Fatal("price-only update failed")
	}
	if _, max := read(); max == nil || *max != 3 {
		t.Fatal("price-only update cleared the capacity")
	}
	// Can't cap below what's sold or held.
	s, _ := e.buy(event, "cap1-"+uniq()+"@phase2.test")
	e.paid(s, 0)
	e.buy(event, "cap2-"+uniq()+"@phase2.test")
	if status, body := doReq(t, e.app, "PATCH", path, `{"ticket_price":5,"max_tickets":1}`); status != 400 || !strings.Contains(body, "2 tickets") {
		t.Fatalf("cap below sold: %d %s", status, body)
	}
	if status, _ := doReq(t, e.app, "PATCH", path, `{"ticket_price":5,"max_tickets":null}`); status != 200 {
		t.Fatal("removing the cap failed")
	}
	if _, max := read(); max != nil {
		t.Fatal("cap not removed")
	}
	// The charge is the exact price: $5.00 → 500 cents.
	e.buy(event, "exact-"+uniq()+"@phase2.test")
	if s := e.fs.lastSession(); s.Unit != 500 || s.AppFee != 50 {
		t.Fatalf("charged %d with fee %d, want 500 / 50", s.Unit, s.AppFee)
	}
}

// A capacity of 5 with 30 concurrent paid checkouts creates exactly 5.
func TestConcurrentCheckoutsNeverOversell(t *testing.T) {
	e := newFinEnv(t)
	host := createStripeHost(t)
	five := 5
	event := createTestEvent(t, testDB(t), host, testEvent{Price: 25, Paid: true, Active: true, MaxTix: &five})
	var ok atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			status, _ := doReq(t, e.app, "POST", "/tickets/guest-purchase",
				fmt.Sprintf(`{"event_id":%q,"email":"rush-%s@phase2.test"}`, event, uniq()))
			if status == 200 {
				ok.Add(1)
			}
		}()
	}
	wg.Wait()
	if ok.Load() != 5 || e.fs.sessionCount() != 5 {
		t.Fatalf("%d checkouts / %d sessions for 5 tickets", ok.Load(), e.fs.sessionCount())
	}
}

// The ticket-success page stops waiting when the checkout ended without a
// ticket.
func TestClaimReportsEndedCheckouts(t *testing.T) {
	e := newFinEnv(t)
	host := createStripeHost(t)
	event := createTestEvent(t, testDB(t), host, testEvent{Price: 25, Paid: true, Active: true})
	ticketH := &TicketHandler{DB: testDB(t), Cfg: e.cfg}
	e.app.Get("/tickets/claim/:token", ticketH.Claim)

	// The buyer's claim token only lives in Stripe's success URL, so the
	// test swaps in a claim it knows for the same session.
	s2, _ := e.buy(event, "claim2-"+uniq()+"@phase2.test")
	if _, err := testDB(t).Exec(context.Background(), `DELETE FROM ticket_claims WHERE stripe_session_id = $1`, s2.ID); err != nil {
		t.Fatal(err)
	}
	claim2, hash2, _ := newClaimToken()
	if err := insertTicketClaim(context.Background(), testDB(t), hash2, event, claimTarget{StripeSessionID: &s2.ID}); err != nil {
		t.Fatal(err)
	}
	if status, _ := doReq(t, e.app, "GET", "/tickets/claim/"+claim2, ""); status != 202 {
		t.Fatalf("pending checkout: %d, want 202", status)
	}
	e.webhook("", "checkout.session.expired", e.fs.sessionEvent(s2))
	if status, body := doReq(t, e.app, "GET", "/tickets/claim/"+claim2, ""); status != 410 || !strings.Contains(body, "weren't charged") {
		t.Fatalf("expired checkout: %d %s, want 410", status, body)
	}
}
