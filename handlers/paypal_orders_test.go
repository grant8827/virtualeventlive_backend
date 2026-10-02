package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gofiber/fiber/v2"

	"vertualeventlive/backend/services"
)

// A local stand-in for PayPal's REST API. Nothing leaves the machine.
type fakePayPal struct {
	t   *testing.T
	srv *httptest.Server

	mu       sync.Mutex
	n        int
	orders   map[string]map[string]any // order ID → purchase unit we were sent
	captures map[string]*ppCapture     // capture ID → state
	byOrder  map[string]string         // order ID → capture ID
	refunds  []map[string]any
	calls    map[string]int
	// Tampering, to simulate a capture that doesn't match the order.
	captureValue string
	capturePayee string
	// failCaptures makes the next N captures fail with a 500.
	failCaptures int
}

type ppCapture struct {
	Amount, Refunded int64
	Status           string
}

func newFakePayPal(t *testing.T) (*fakePayPal, *services.PayPalService) {
	f := &fakePayPal{t: t, orders: map[string]map[string]any{}, captures: map[string]*ppCapture{},
		byOrder: map[string]string{}, calls: map[string]int{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f, &services.PayPalService{ClientID: "client", ClientSecret: "secret", Environment: "sandbox",
		PartnerMerchantID: "PARTNER", PartnerAttributionID: "BN", WebhookID: "WH", APIBase: f.srv.URL}
}

func (f *fakePayPal) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	p := r.URL.Path
	switch {
	case p == "/v1/oauth2/token":
		writeJSON(w, 200, map[string]any{"access_token": "fake-token"})
	case p == "/v1/notifications/verify-webhook-signature":
		status := "FAILURE"
		if body["transmission_sig"] == "good" {
			status = "SUCCESS"
		}
		writeJSON(w, 200, map[string]any{"verification_status": status})
	case p == "/v2/checkout/orders" && r.Method == http.MethodPost:
		f.n++
		id := fmt.Sprintf("PPORDER%d%s", f.n, uniq())
		unit := body["purchase_units"].([]any)[0].(map[string]any)
		f.orders[id] = unit
		writeJSON(w, 201, map[string]any{"id": id, "links": []any{map[string]any{"rel": "approve", "href": "https://paypal.test/approve/" + id}}})
	case strings.HasPrefix(p, "/v2/checkout/orders/") && strings.HasSuffix(p, "/capture"):
		f.calls["capture"]++
		if f.failCaptures > 0 {
			f.failCaptures--
			writeJSON(w, 500, map[string]any{"name": "INTERNAL_SERVER_ERROR"})
			return
		}
		id := strings.TrimSuffix(strings.TrimPrefix(p, "/v2/checkout/orders/"), "/capture")
		unit := f.orders[id]
		amount := unit["amount"].(map[string]any)["value"].(string)
		payee := unit["payee"].(map[string]any)["merchant_id"].(string)
		if f.captureValue != "" {
			amount = f.captureValue
		}
		if f.capturePayee != "" {
			payee = f.capturePayee
		}
		capID, ok := f.byOrder[id]
		if !ok {
			f.n++
			capID = fmt.Sprintf("CAP%d%s", f.n, uniq())
			cents, _ := services.ParseDecimalCents(amount)
			f.captures[capID] = &ppCapture{Amount: cents, Status: "COMPLETED"}
			f.byOrder[id] = capID
		}
		writeJSON(w, 201, map[string]any{"id": id, "status": "COMPLETED", "purchase_units": []any{map[string]any{
			"reference_id": unit["reference_id"], "custom_id": unit["custom_id"],
			"payee": map[string]any{"merchant_id": payee},
			"payments": map[string]any{"captures": []any{map[string]any{
				"id": capID, "status": "COMPLETED", "amount": map[string]any{"currency_code": "USD", "value": amount},
			}}},
		}}})
	case strings.HasPrefix(p, "/v2/payments/captures/") && strings.HasSuffix(p, "/refund"):
		capID := strings.TrimSuffix(strings.TrimPrefix(p, "/v2/payments/captures/"), "/refund")
		c := f.captures[capID]
		cents, _ := services.ParseDecimalCents(body["amount"].(map[string]any)["value"].(string))
		c.Refunded += cents
		c.Status = "PARTIALLY_REFUNDED"
		if c.Refunded == c.Amount {
			c.Status = "REFUNDED"
		}
		f.n++
		ref := map[string]any{"id": fmt.Sprintf("REF%d%s", f.n, uniq()), "status": "COMPLETED",
			"amount": body["amount"], "request": body}
		f.refunds = append(f.refunds, ref)
		writeJSON(w, 201, ref)
	case strings.HasPrefix(p, "/v2/payments/captures/") && r.Method == http.MethodGet:
		capID := strings.TrimPrefix(p, "/v2/payments/captures/")
		c := f.captures[capID]
		writeJSON(w, 200, map[string]any{"id": capID, "status": c.Status,
			"amount":                      map[string]any{"currency_code": "USD", "value": services.FormatCents(c.Amount)},
			"seller_receivable_breakdown": map[string]any{"paypal_fee": map[string]any{"currency_code": "USD", "value": "0.88"}}})
	default:
		f.t.Errorf("fake PayPal: unexpected %s %s", r.Method, p)
		writeJSON(w, 404, map[string]any{})
	}
}

// reverseOutside records a chargeback reversal made by PayPal.
func (f *fakePayPal) reverseOutside(capID string, cents int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.captures[capID]
	c.Refunded += cents
	c.Status = "REVERSED"
}

type ppEnv struct {
	t     *testing.T
	fp    *fakePayPal
	app   *fiber.App
	actor string
	n     int
}

func newPPEnv(t *testing.T) *ppEnv {
	db := testDB(t)
	fp, svc := newFakePayPal(t)
	cfg := testConfig()
	e := &ppEnv{t: t, fp: fp, actor: createTestHost(t, db)}
	ticketH := &TicketHandler{DB: db, Cfg: cfg, PayPal: svc}
	finH := &FinanceHandler{SuperuserHandler: SuperuserHandler{DB: db}, Cfg: cfg, PayPal: svc}
	e.app = fiber.New()
	e.app.Post("/tickets/guest-purchase", ticketH.GuestPurchase)
	e.app.Get("/tickets/paypal/complete", ticketH.PayPalComplete)
	e.app.Post("/webhooks/paypal", ticketH.PayPalWebhook)
	su := e.app.Group("/su", func(c *fiber.Ctx) error { c.Locals("user_id", e.actor); return c.Next() })
	su.Post("/orders/:id/refund", finH.RefundOrder)
	su.Post("/finance/reconcile", finH.RunProviderReconciliation)
	return e
}

func createPayPalHost(t *testing.T) string {
	t.Helper()
	db := testDB(t)
	host := createTestHost(t, db)
	if _, err := db.Exec(context.Background(),
		`INSERT INTO connected_accounts (user_id, paypal_account_id, payout_gateway) VALUES ($1, 'HOSTMERCHANT', 'paypal')`, host); err != nil {
		t.Fatal(err)
	}
	return host
}

func (e *ppEnv) buy(eventID string) (ppOrderID, orderID string) {
	e.t.Helper()
	status, body := doReq(e.t, e.app, "POST", "/tickets/guest-purchase",
		fmt.Sprintf(`{"event_id":%q,"email":"pp-%s@phase2.test"}`, eventID, uniq()))
	if status != 200 || !strings.Contains(body, "paypal") {
		e.t.Fatalf("PayPal checkout: %d %s", status, body)
	}
	e.fp.mu.Lock()
	defer e.fp.mu.Unlock()
	for id, unit := range e.fp.orders {
		if _, seen := e.fp.byOrder[id]; !seen && unit["custom_id"] != nil {
			var ours bool
			_ = testDB(e.t).QueryRow(context.Background(),
				`SELECT EXISTS(SELECT 1 FROM orders WHERE provider_checkout_id = $1 AND status = 'pending')`, id).Scan(&ours)
			if ours {
				return id, unit["custom_id"].(string)
			}
		}
	}
	e.t.Fatal("no PayPal order recorded")
	return "", ""
}

func (e *ppEnv) webhook(id, eventType string, resource map[string]any, sig string) int {
	e.t.Helper()
	if id == "" {
		e.n++
		id = fmt.Sprintf("WH-%d-%s", e.n, uniq())
	}
	body, _ := json.Marshal(map[string]any{"id": id, "event_type": eventType, "resource": resource})
	status, _ := doReq2(e.t, e.app, "POST", "/webhooks/paypal", string(body), map[string]string{"PayPal-Transmission-Sig": sig})
	return status
}

func captureOf(t *testing.T, orderID string) string {
	t.Helper()
	var c *string
	_ = testDB(t).QueryRow(context.Background(), `SELECT provider_payment_id FROM orders WHERE id = $1`, orderID).Scan(&c)
	if c == nil {
		return ""
	}
	return *c
}

// PAY-04: exact amounts in, capture checked and recorded, booked once.
func TestPayPalCheckoutCapturesExactlyAndBooksOnce(t *testing.T) {
	e := newPPEnv(t)
	host := createPayPalHost(t)
	event := createTestEvent(t, testDB(t), host, testEvent{Price: 19.99, Paid: true, Active: true})
	ppID, orderID := e.buy(event)

	e.fp.mu.Lock()
	unit := e.fp.orders[ppID]
	e.fp.mu.Unlock()
	fee := unit["payment_instruction"].(map[string]any)["platform_fees"].([]any)[0].(map[string]any)["amount"].(map[string]any)["value"]
	if unit["amount"].(map[string]any)["value"] != "19.99" || fee != "2.00" || unit["custom_id"] != orderID {
		t.Fatalf("PayPal order sent: %v", unit)
	}

	if status, _ := doReq(t, e.app, "GET", "/tickets/paypal/complete?token="+ppID+"&claim=x", ""); status != 302 {
		t.Fatalf("return: %d", status)
	}
	// The same approval arriving by webhook, twice, changes nothing.
	approved := map[string]any{"id": ppID}
	e.webhook("WH-approved-"+ppID, "CHECKOUT.ORDER.APPROVED", approved, "good")
	if status := e.webhook("WH-approved-"+ppID, "CHECKOUT.ORDER.APPROVED", approved, "good"); status != 200 {
		t.Fatalf("duplicate webhook: %d", status)
	}
	var status string
	_ = testDB(t).QueryRow(context.Background(), `SELECT status FROM paypal_webhook_events WHERE event_id = $1`, "WH-approved-"+ppID).Scan(&status)
	if status != "processed" {
		t.Fatalf("inbox status %q", status)
	}

	if st, _, _, _ := orderState(t, orderID); st != "paid" {
		t.Fatalf("order is %s", st)
	}
	if captureOf(t, orderID) == "" {
		t.Fatal("capture ID not stored on the order")
	}
	if v, all := orderTickets(t, orderID); v != 1 || all != 1 {
		t.Fatalf("%d/%d tickets", v, all)
	}
	if e.fp.calls["capture"] != 1 {
		t.Fatalf("captured %d times", e.fp.calls["capture"])
	}
	wantAccounts(t, "PayPal sale", accounts(t, orderID), map[string]int64{"cash_paypal": 200, "platform_commission": -200})
}

func TestPayPalCaptureMismatchIsNotFulfilled(t *testing.T) {
	for name, tamper := range map[string]func(f *fakePayPal){
		"short amount":           func(f *fakePayPal) { f.captureValue = "1.00" },
		"paid to another seller": func(f *fakePayPal) { f.capturePayee = "ATTACKER" },
	} {
		t.Run(name, func(t *testing.T) {
			e := newPPEnv(t)
			host := createPayPalHost(t)
			event := createTestEvent(t, testDB(t), host, testEvent{Price: 19.99, Paid: true, Active: true})
			ppID, orderID := e.buy(event)
			tamper(e.fp)
			doReq(t, e.app, "GET", "/tickets/paypal/complete?token="+ppID, "")
			if st, _, _, _ := orderState(t, orderID); st != "needs_attention" {
				t.Fatalf("order is %s, want needs_attention", st)
			}
			if _, all := orderTickets(t, orderID); all != 0 {
				t.Fatal("a ticket was issued for a mismatched capture")
			}
			if got := accounts(t, orderID); len(got) != 0 {
				t.Fatalf("mismatched capture was booked: %v", got)
			}
		})
	}
}

func TestPayPalRefundsReversalsAndDisputes(t *testing.T) {
	e := newPPEnv(t)
	host := createPayPalHost(t)
	event := createTestEvent(t, testDB(t), host, testEvent{Price: 19.99, Paid: true, Active: true})
	ppID, orderID := e.buy(event)
	doReq(t, e.app, "GET", "/tickets/paypal/complete?token="+ppID, "")
	capID := captureOf(t, orderID)

	// Console refund: the seller refunds 5.00 and the platform fee 0.50.
	if status, body := doReq(t, e.app, "POST", "/su/orders/"+orderID+"/refund", `{"amount_cents":500,"reason":"asked"}`); status != 200 {
		t.Fatalf("refund: %d %s", status, body)
	}
	e.fp.mu.Lock()
	ref := e.fp.refunds[0]
	e.fp.mu.Unlock()
	req := ref["request"].(map[string]any)
	if req["amount"].(map[string]any)["value"] != "5.00" ||
		req["payment_instruction"].(map[string]any)["platform_fees"].([]any)[0].(map[string]any)["amount"].(map[string]any)["value"] != "0.50" {
		t.Fatalf("PayPal refund request: %v", req)
	}
	// PayPal's notice of the same refund is not booked twice.
	refunded := map[string]any{"id": ref["id"], "status": "COMPLETED", "amount": map[string]any{"currency_code": "USD", "value": "5.00"},
		"links": []any{map[string]any{"rel": "up", "href": e.fp.srv.URL + "/v2/payments/captures/" + capID}}}
	if status := e.webhook("", "PAYMENT.CAPTURE.REFUNDED", refunded, "good"); status != 200 {
		t.Fatalf("refund webhook: %d", status)
	}
	if _, r, fr, _ := orderState(t, orderID); r != 500 || fr != 50 {
		t.Fatalf("refunded %d (fee %d), want 500 (50)", r, fr)
	}

	// A dispute opens (status only), then PayPal reverses the rest.
	dispute := map[string]any{"dispute_id": "PP-D-" + uniq(), "status": "OPEN", "reason": "MERCHANDISE_OR_SERVICE_NOT_RECEIVED",
		"dispute_amount":        map[string]any{"currency_code": "USD", "value": "14.99"},
		"disputed_transactions": []any{map[string]any{"seller_transaction_id": capID}}}
	if status := e.webhook("", "CUSTOMER.DISPUTE.CREATED", dispute, "good"); status != 200 {
		t.Fatalf("dispute webhook: %d", status)
	}
	if v, _ := orderTickets(t, orderID); v != 1 {
		t.Fatal("an open dispute voided the ticket")
	}
	e.fp.reverseOutside(capID, 1499)
	reversal := map[string]any{"id": "REV-" + uniq(), "status": "COMPLETED", "amount": map[string]any{"currency_code": "USD", "value": "14.99"},
		"links": []any{map[string]any{"rel": "up", "href": e.fp.srv.URL + "/v2/payments/captures/" + capID}}}
	if status := e.webhook("", "PAYMENT.CAPTURE.REVERSED", reversal, "good"); status != 200 {
		t.Fatalf("reversal webhook: %d", status)
	}
	if _, r, fr, _ := orderState(t, orderID); r != 1999 || fr != 200 {
		t.Fatalf("after reversal refunded %d (fee %d), want 1999 (200)", r, fr)
	}
	if v, _ := orderTickets(t, orderID); v != 0 {
		t.Fatal("reversal left the ticket valid")
	}
	wantAccounts(t, "PayPal after refund+reversal", accounts(t, orderID), map[string]int64{})

	// Forged webhooks are refused and leave no trace.
	if status := e.webhook("WH-forged", "PAYMENT.CAPTURE.REFUNDED", refunded, "bad"); status != 401 {
		t.Fatalf("forged webhook: %d, want 401", status)
	}

	// Provider reconciliation agrees with the books.
	status, body := doReq(t, e.app, "POST", "/su/finance/reconcile", "")
	if status != 200 {
		t.Fatalf("reconcile: %d %s", status, body)
	}
	var pr ReconcileReport
	_ = json.Unmarshal([]byte(body), &pr)
	for _, is := range pr.Issues {
		if is.OrderID == orderID {
			t.Errorf("provider issue: %+v", is)
		}
	}
	br, err := ReconcileBooks(context.Background(), testDB(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, is := range br.Issues {
		if is.OrderID == orderID {
			t.Errorf("books issue: %+v", is)
		}
	}
}

// PAY-02 for PayPal: a temporary failure answers 500 (PayPal retries) and
// the retry completes the order exactly once.
func TestPayPalWebhookRetriesTransientFailure(t *testing.T) {
	e := newPPEnv(t)
	host := createPayPalHost(t)
	event := createTestEvent(t, testDB(t), host, testEvent{Price: 12, Paid: true, Active: true})
	ppID, orderID := e.buy(event)
	e.fp.mu.Lock()
	e.fp.failCaptures = 1
	e.fp.mu.Unlock()
	whID := "WH-retry-" + ppID
	if status := e.webhook(whID, "CHECKOUT.ORDER.APPROVED", map[string]any{"id": ppID}, "good"); status != 500 {
		t.Fatalf("first delivery: %d, want 500", status)
	}
	var status string
	var attempts int
	_ = testDB(t).QueryRow(context.Background(), `SELECT status, attempts FROM paypal_webhook_events WHERE event_id = $1`, whID).Scan(&status, &attempts)
	if status != "failed" {
		t.Fatalf("inbox after failure: %s", status)
	}
	if code := e.webhook(whID, "CHECKOUT.ORDER.APPROVED", map[string]any{"id": ppID}, "good"); code != 200 {
		t.Fatalf("retry: %d", code)
	}
	_ = testDB(t).QueryRow(context.Background(), `SELECT status, attempts FROM paypal_webhook_events WHERE event_id = $1`, whID).Scan(&status, &attempts)
	if status != "processed" || attempts != 2 {
		t.Fatalf("inbox after retry: %s / %d", status, attempts)
	}
	if v, all := orderTickets(t, orderID); v != 1 || all != 1 {
		t.Fatalf("%d/%d tickets after retry", v, all)
	}
}
