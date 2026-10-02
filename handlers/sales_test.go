package handlers

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"

	"vertualeventlive/backend/services"
)

func TestSalesRule(t *testing.T) {
	now := time.Now()
	one, two := 1, 2
	ok := saleState{VenuePaid: true, Active: true, EndsAt: now.Add(time.Hour)}
	with := func(f func(*saleState)) saleState { s := ok; f(&s); return s }

	cases := map[string]struct {
		s    saleState
		want error
	}{
		"on sale":         {ok, nil},
		"disabled":        {with(func(s *saleState) { s.Active = false }), errSaleDisabled},
		"cancelled":       {with(func(s *saleState) { s.Cancelled = true }), errSaleCancelled},
		"ended":           {with(func(s *saleState) { s.EndsAt = now.Add(-time.Minute) }), errSaleEnded},
		"ends right now":  {with(func(s *saleState) { s.EndsAt = now }), errSaleEnded},
		"venue unpaid":    {with(func(s *saleState) { s.VenuePaid = false }), errSaleVenueUnpaid},
		"sold out":        {with(func(s *saleState) { s.MaxTickets = &one; s.Sold = 1 }), errSaleSoldOut},
		"last ticket":     {with(func(s *saleState) { s.MaxTickets = &two; s.Sold = 1 }), nil},
		"two of last one": {with(func(s *saleState) { s.MaxTickets = &two; s.Sold = 1; s.Quantity = 2 }), errSaleSoldOut},
		"two of two":      {with(func(s *saleState) { s.MaxTickets = &two; s.Quantity = 2 }), nil},
		// Cancellation wins over everything else.
		"cancelled+ended": {with(func(s *saleState) { s.Cancelled = true; s.EndsAt = now.Add(-time.Hour) }), errSaleCancelled},
	}
	for name, tc := range cases {
		if got := checkCanSell(tc.s, now); !errors.Is(got, tc.want) {
			t.Errorf("%s: got %v, want %v", name, got, tc.want)
		}
	}
}

func ticketCount(t *testing.T, eventID string) int {
	t.Helper()
	var n int
	if err := testDB(t).QueryRow(context.Background(), `SELECT count(*) FROM tickets WHERE event_id = $1`, eventID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

var closedEvents = map[string]testEvent{
	"disabled":  {Paid: true, Active: false},
	"cancelled": {Paid: true, Active: true, Cancelled: true},
	"ended":     {Paid: true, Active: true, Ended: true},
	"unpaid":    {Paid: false, Active: true},
}

// Free tickets (guest and signed-in) can't be issued for a closed event.
func TestClosedEventsCannotIssueFreeTickets(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	host := createTestHost(t, db)
	h := &TicketHandler{DB: db, Cfg: testConfig()}
	email := "buyer-" + uniq() + "@phase0.test"

	for name, ev := range closedEvents {
		event := createTestEvent(t, db, host, ev)
		if _, _, err := h.issueFreeTicket(ctx, event, nil, email); !isSaleRuleError(err) {
			t.Errorf("%s (guest): got %v, want a sales-rule refusal", name, err)
		}
		if _, _, err := h.issueFreeTicket(ctx, event, &host, "host-"+uniq()+"@phase0.test"); !isSaleRuleError(err) {
			t.Errorf("%s (signed-in): got %v, want a sales-rule refusal", name, err)
		}
		if n := ticketCount(t, event); n != 0 {
			t.Errorf("%s: %d tickets issued", name, n)
		}
	}
}

// Through the public HTTP endpoint: closed events refuse with 409 and issue
// nothing; an event on sale still works.
func TestGuestPurchaseEndpointHonoursSalesRule(t *testing.T) {
	db := testDB(t)
	host := createTestHost(t, db)
	app := fiber.New()
	app.Post("/tickets/guest-purchase", (&TicketHandler{DB: db, Cfg: testConfig()}).GuestPurchase)
	body := func(event string) string {
		return `{"event_id":"` + event + `","email":"buyer-` + uniq() + `@phase0.test"}`
	}

	for name, ev := range closedEvents {
		event := createTestEvent(t, db, host, ev)
		status, resp := doReq(t, app, "POST", "/tickets/guest-purchase", body(event))
		if status == 200 || strings.Contains(resp, "access_token") {
			t.Errorf("%s: got %d %s", name, status, resp)
		}
		if n := ticketCount(t, event); n != 0 {
			t.Errorf("%s: %d tickets issued", name, n)
		}
	}

	open := createTestEvent(t, db, host, onSale)
	if status, resp := doReq(t, app, "POST", "/tickets/guest-purchase", body(open)); status != 200 || !strings.Contains(resp, `"claim"`) {
		t.Fatalf("event on sale: got %d %s", status, resp)
	}
}

// Ten simultaneous requests for the last free ticket issue exactly one.
func TestCapacityCannotBeOversold(t *testing.T) {
	db := testDB(t)
	host := createTestHost(t, db)
	one := 1
	event := createTestEvent(t, db, host, testEvent{Paid: true, Active: true, MaxTix: &one})
	h := &TicketHandler{DB: db, Cfg: testConfig()}

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			email := "rush-" + uniq() + "@phase0.test"
			_, _, _ = h.issueFreeTicket(context.Background(), event, nil, email)
		}()
	}
	wg.Wait()
	if n := ticketCount(t, event); n != 1 {
		t.Fatalf("%d tickets issued for a 1-ticket event", n)
	}
}

// PayPal re-checks the rule before capturing: an order for an event that
// was cancelled after checkout began is refused before any money moves.
func TestPayPalRefusesClosedEventBeforeCapture(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	host := createTestHost(t, db)
	for name, ev := range closedEvents {
		event := createTestEvent(t, db, host, ev)
		orderID := "PP" + uniq()
		if _, err := db.Exec(ctx,
			`INSERT INTO paypal_ticket_orders (order_id, event_id, buyer_email, amount, merchant_id)
			 VALUES ($1, $2, 'pp@phase0.test', 10, 'MERCHANT')`, orderID, event); err != nil {
			t.Fatal(err)
		}
		// An unconfigured PayPal client: if the rule didn't stop us first we'd
		// get a "not configured" capture error instead of a sales refusal.
		h := &TicketHandler{DB: db, Cfg: testConfig(), PayPal: &services.PayPalService{}}
		if _, _, err := h.completePayPalTicketOrder(ctx, orderID); !isSaleRuleError(err) {
			t.Errorf("%s: got %v, want a sales-rule refusal before capture", name, err)
		}
		if n := ticketCount(t, event); n != 0 {
			t.Errorf("%s: %d tickets issued", name, n)
		}
	}
}
