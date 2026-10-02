package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v2"
	stripe "github.com/stripe/stripe-go/v82"
	"github.com/stripe/stripe-go/v82/webhook"
)

// No real Stripe account or payment is involved: events are built locally
// and signed with a test-only secret using Stripe's own test helper.
const testWebhookSecret = "whsec_phase0_test_only"

func stripeTestHandler(t *testing.T) *StripeHandler {
	cfg := testConfig()
	cfg.StripeWebhookSecret = testWebhookSecret
	return &StripeHandler{DB: testDB(t), Cfg: cfg}
}

type testSession struct {
	ID, Type, EventID, Email, PaymentStatus string
	Amount                                  int64
}

func signedEvent(t *testing.T, eventID, eventType string, s testSession) (string, string) {
	t.Helper()
	payload, _ := json.Marshal(map[string]any{
		"id": eventID, "object": "event", "api_version": stripe.APIVersion, "type": eventType,
		"data": map[string]any{"object": map[string]any{
			"id": s.ID, "object": "checkout.session", "payment_status": s.PaymentStatus,
			"currency": "usd", "amount_total": s.Amount,
			"metadata": map[string]string{"type": s.Type, "event_id": s.EventID, "guest_email": s.Email},
		}},
	})
	signed := webhook.GenerateTestSignedPayload(&webhook.UnsignedPayload{Payload: payload, Secret: testWebhookSecret})
	return string(signed.Payload), signed.Header
}

func deliver(t *testing.T, h *StripeHandler, body, sigHeader string) int {
	t.Helper()
	app := fiber.New()
	app.Post("/webhooks/stripe", h.Webhook)
	req := httptest.NewRequest("POST", "/webhooks/stripe", strings.NewReader(body))
	req.Header.Set("Stripe-Signature", sigHeader)
	resp, err := app.Test(req, -1)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode
}

func webhookRecord(t *testing.T, eventID string) (status string, attempts int, lastErr *string) {
	t.Helper()
	if err := testDB(t).QueryRow(context.Background(),
		`SELECT status, attempts, last_error FROM stripe_webhook_events WHERE event_id = $1`, eventID,
	).Scan(&status, &attempts, &lastErr); err != nil {
		t.Fatalf("webhook record %s: %v", eventID, err)
	}
	return
}

func sessionTickets(t *testing.T, sessionID string) int {
	t.Helper()
	var n int
	_ = testDB(t).QueryRow(context.Background(), `SELECT count(*) FROM tickets WHERE stripe_checkout_session_id = $1`, sessionID).Scan(&n)
	return n
}

func paidTicketSession(t *testing.T, event string) testSession {
	return testSession{ID: "cs_test_" + uniq(), Type: "ticket", EventID: event,
		Email: "buyer-" + uniq() + "@phase0.test", PaymentStatus: "paid", Amount: 2500}
}

func TestStripeWebhookSuccessIssuesOneTicketAndAttachesClaim(t *testing.T) {
	h := stripeTestHandler(t)
	ctx := context.Background()
	host := createTestHost(t, h.DB)
	event := createTestEvent(t, h.DB, host, testEvent{Price: 25, Paid: true, Active: true})
	sess := paidTicketSession(t, event)

	claim, hash, _ := newClaimToken()
	if err := insertTicketClaim(ctx, h.DB, hash, event, claimTarget{StripeSessionID: &sess.ID}); err != nil {
		t.Fatal(err)
	}

	evtID := "evt_" + uniq()
	body, sig := signedEvent(t, evtID, "checkout.session.completed", sess)
	if got := deliver(t, h, body, sig); got != 200 {
		t.Fatalf("got %d", got)
	}
	if n := sessionTickets(t, sess.ID); n != 1 {
		t.Fatalf("%d tickets", n)
	}
	var ledger int
	_ = h.DB.QueryRow(ctx, `SELECT count(*) FROM ledger_entries l JOIN tickets t ON t.id = l.ticket_id WHERE t.stripe_checkout_session_id = $1`, sess.ID).Scan(&ledger)
	if ledger != 1 {
		t.Fatalf("%d ledger entries", ledger)
	}
	if status, attempts, _ := webhookRecord(t, evtID); status != "processed" || attempts != 1 {
		t.Fatalf("record: %s / %d", status, attempts)
	}
	// The buyer's claim now resolves to the ticket.
	app := ticketTestApp(&TicketHandler{DB: h.DB, Cfg: h.Cfg})
	if status, _ := doReq(t, app, "GET", "/tickets/claim/"+claim, ""); status != 200 {
		t.Fatalf("claim after fulfilment: %d", status)
	}
}

func TestStripeWebhookDuplicatesDoNotDuplicateTickets(t *testing.T) {
	h := stripeTestHandler(t)
	host := createTestHost(t, h.DB)
	event := createTestEvent(t, h.DB, host, testEvent{Price: 25, Paid: true, Active: true})
	sess := paidTicketSession(t, event)

	evtID := "evt_" + uniq()
	body, sig := signedEvent(t, evtID, "checkout.session.completed", sess)
	for i := 0; i < 3; i++ {
		if got := deliver(t, h, body, sig); got != 200 {
			t.Fatalf("delivery %d: got %d", i+1, got)
		}
	}
	// A second event for the same session (async success) is also a no-op.
	body2, sig2 := signedEvent(t, "evt_"+uniq(), "checkout.session.async_payment_succeeded", sess)
	if got := deliver(t, h, body2, sig2); got != 200 {
		t.Fatalf("async event: got %d", got)
	}
	if n := sessionTickets(t, sess.ID); n != 1 {
		t.Fatalf("%d tickets after duplicate deliveries", n)
	}
	if status, attempts, _ := webhookRecord(t, evtID); status != "processed" || attempts != 3 {
		t.Fatalf("record: %s / %d attempts", status, attempts)
	}
}

// A temporary failure is reported to Stripe (non-2xx, so it retries) and
// recorded; the retry then fulfils exactly once.
func TestStripeWebhookFailureIsRetriedThenSucceeds(t *testing.T) {
	h := stripeTestHandler(t)
	host := createTestHost(t, h.DB)
	event := createTestEvent(t, h.DB, host, testEvent{Price: 25, Paid: true, Active: true})
	sess := paidTicketSession(t, event)

	calls := 0
	h.process = func(ctx context.Context, e stripe.Event) (webhookOutcome, error) {
		calls++
		if calls == 1 {
			return "", errors.New("database unavailable (simulated)")
		}
		return h.processStripeEvent(ctx, e)
	}

	evtID := "evt_" + uniq()
	body, sig := signedEvent(t, evtID, "checkout.session.completed", sess)
	if got := deliver(t, h, body, sig); got != 500 {
		t.Fatalf("failing fulfilment acknowledged with %d; Stripe would not retry", got)
	}
	status, _, lastErr := webhookRecord(t, evtID)
	if status != "failed" || lastErr == nil || !strings.Contains(*lastErr, "simulated") {
		t.Fatalf("failure not recorded: %s %v", status, lastErr)
	}
	if n := sessionTickets(t, sess.ID); n != 0 {
		t.Fatalf("%d tickets after failure", n)
	}

	// Stripe retries the same event.
	if got := deliver(t, h, body, sig); got != 200 {
		t.Fatalf("retry: got %d", got)
	}
	if status, attempts, _ := webhookRecord(t, evtID); status != "processed" || attempts != 2 {
		t.Fatalf("after retry: %s / %d", status, attempts)
	}
	if n := sessionTickets(t, sess.ID); n != 1 {
		t.Fatalf("%d tickets after retry", n)
	}
}

// Paid for an event cancelled since checkout: no ticket, not retried
// forever, flagged for a manual refund.
func TestStripeWebhookClosedEventNeedsAttentionNotTicket(t *testing.T) {
	h := stripeTestHandler(t)
	host := createTestHost(t, h.DB)
	for name, ev := range closedEvents {
		event := createTestEvent(t, h.DB, host, ev)
		sess := paidTicketSession(t, event)
		evtID := "evt_" + uniq()
		body, sig := signedEvent(t, evtID, "checkout.session.completed", sess)
		if got := deliver(t, h, body, sig); got != 200 {
			t.Errorf("%s: got %d", name, got)
		}
		if n := sessionTickets(t, sess.ID); n != 0 {
			t.Errorf("%s: %d tickets issued", name, n)
		}
		if status, _, _ := webhookRecord(t, evtID); status != "needs_attention" {
			t.Errorf("%s: recorded %s", name, status)
		}
	}
}

// Delayed payment methods: nothing on "completed" (unpaid), the ticket on
// "async_payment_succeeded".
func TestStripeWebhookWaitsForDelayedPayment(t *testing.T) {
	h := stripeTestHandler(t)
	host := createTestHost(t, h.DB)
	event := createTestEvent(t, h.DB, host, testEvent{Price: 25, Paid: true, Active: true})
	sess := paidTicketSession(t, event)

	unpaid := sess
	unpaid.PaymentStatus = "unpaid"
	body, sig := signedEvent(t, "evt_"+uniq(), "checkout.session.completed", unpaid)
	if got := deliver(t, h, body, sig); got != 200 || sessionTickets(t, sess.ID) != 0 {
		t.Fatal("unpaid session must be acknowledged without a ticket")
	}
	body, sig = signedEvent(t, "evt_"+uniq(), "checkout.session.async_payment_succeeded", sess)
	if got := deliver(t, h, body, sig); got != 200 || sessionTickets(t, sess.ID) != 1 {
		t.Fatal("async success must issue the ticket")
	}
}

func TestStripeWebhookRejectsBadSignature(t *testing.T) {
	h := stripeTestHandler(t)
	evtID := "evt_" + uniq()
	body, _ := signedEvent(t, evtID, "checkout.session.completed", testSession{ID: "cs_x", PaymentStatus: "paid"})
	forged := webhook.GenerateTestSignedPayload(&webhook.UnsignedPayload{Payload: []byte(body), Secret: "whsec_wrong"})
	if got := deliver(t, h, body, forged.Header); got != 400 {
		t.Fatalf("forged signature: got %d", got)
	}
	var n int
	_ = h.DB.QueryRow(context.Background(), `SELECT count(*) FROM stripe_webhook_events WHERE event_id = $1`, evtID).Scan(&n)
	if n != 0 {
		t.Fatal("unverified event was recorded")
	}
}

// Booking fee: duplicate deliveries activate once; a cancelled event is
// never re-activated.
func TestStripeWebhookVenueFeeActivatesOnceAndNotCancelled(t *testing.T) {
	h := stripeTestHandler(t)
	ctx := context.Background()
	host := createTestHost(t, h.DB)

	event := createTestEvent(t, h.DB, host, testEvent{Paid: false, Active: false})
	if _, err := h.DB.Exec(ctx, `UPDATE events SET venue_fee = 30 WHERE id = $1`, event); err != nil {
		t.Fatal(err)
	}
	sess := testSession{ID: "cs_test_" + uniq(), Type: "venue_fee", EventID: event, PaymentStatus: "paid", Amount: 3000}
	body, sig := signedEvent(t, "evt_"+uniq(), "checkout.session.completed", sess)
	for i := 0; i < 2; i++ {
		if got := deliver(t, h, body, sig); got != 200 {
			t.Fatalf("delivery %d: %d", i+1, got)
		}
	}
	var paid, active bool
	_ = h.DB.QueryRow(ctx, `SELECT venue_paid, is_active FROM events WHERE id = $1`, event).Scan(&paid, &active)
	if !paid || !active {
		t.Fatal("booking fee not applied")
	}

	cancelled := createTestEvent(t, h.DB, host, testEvent{Paid: false, Active: false, Cancelled: true})
	_, _ = h.DB.Exec(ctx, `UPDATE events SET venue_fee = 30 WHERE id = $1`, cancelled)
	evtID := "evt_" + uniq()
	body, sig = signedEvent(t, evtID, "checkout.session.completed",
		testSession{ID: "cs_test_" + uniq(), Type: "venue_fee", EventID: cancelled, PaymentStatus: "paid", Amount: 3000})
	if got := deliver(t, h, body, sig); got != 200 {
		t.Fatalf("cancelled event: %d", got)
	}
	_ = h.DB.QueryRow(ctx, `SELECT is_active FROM events WHERE id = $1`, cancelled).Scan(&active)
	if active {
		t.Fatal("a cancelled event was re-activated by a booking-fee payment")
	}
	if status, _, _ := webhookRecord(t, evtID); status != "needs_attention" {
		t.Fatalf("cancelled booking fee recorded as %s", status)
	}
}
