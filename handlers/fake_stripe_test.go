package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"
	stripe "github.com/stripe/stripe-go/v82"
	"github.com/stripe/stripe-go/v82/webhook"
)

// fakeStripe is a local stand-in for the parts of Stripe's API the backend
// calls: Checkout Sessions, Refunds and PaymentIntents. No request leaves the
// machine and no money moves; it lets tests run the real purchase, refund
// and reconciliation code end to end.
type fakeStripe struct {
	t   *testing.T
	srv *httptest.Server

	mu       sync.Mutex
	n        int
	sessions map[string]*fakeSession
	order    []string
	pis      map[string]*fakePI
	idem     map[string][]byte
	// requests counts calls by "METHOD /path-prefix".
	requests map[string]int
}

type fakeSession struct {
	ID, PI      string
	Unit, Qty   int64
	AppFee      int64
	Destination string
	Metadata    map[string]string
	PIMetadata  map[string]string
	Email       string
	Paid        bool
	Created     int64
}

func (s *fakeSession) amount() int64 { return s.Unit * s.Qty }

type fakePI struct {
	Amount   int64
	Refunded int64
	Disputed bool
	Fee      int64
	Refunds  []map[string]any
}

// newFakeStripe starts the fake and points stripe-go at it for the test.
func newFakeStripe(t *testing.T) *fakeStripe {
	f := &fakeStripe{t: t, sessions: map[string]*fakeSession{}, pis: map[string]*fakePI{},
		idem: map[string][]byte{}, requests: map[string]int{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	orig := stripe.GetBackend(stripe.APIBackend)
	stripe.SetBackend(stripe.APIBackend, stripe.GetBackendWithConfig(stripe.APIBackend, &stripe.BackendConfig{
		URL:               stripe.String(f.srv.URL),
		LeveledLogger:     &stripe.LeveledLogger{Level: stripe.LevelNull},
		MaxNetworkRetries: stripe.Int64(0),
	}))
	t.Cleanup(func() {
		stripe.SetBackend(stripe.APIBackend, orig)
		f.srv.Close()
	})
	return f
}

func (f *fakeStripe) next(prefix string) string {
	f.n++
	return fmt.Sprintf("%s_test_%d_%s", prefix, f.n, uniq())
}

func (f *fakeStripe) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_ = r.ParseForm()
	path := strings.TrimPrefix(r.URL.Path, "/v1/")
	parts := strings.Split(path, "/")
	f.requests[r.Method+" "+parts[0]]++

	if key := r.Header.Get("Idempotency-Key"); key != "" && r.Method == http.MethodPost {
		if body, ok := f.idem[key]; ok {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(body)
			return
		}
		rec := httptest.NewRecorder()
		f.route(rec, r, parts)
		if rec.Code == http.StatusOK {
			f.idem[key] = rec.Body.Bytes()
		}
		for k, v := range rec.Header() {
			w.Header()[k] = v
		}
		w.WriteHeader(rec.Code)
		_, _ = w.Write(rec.Body.Bytes())
		return
	}
	f.route(w, r, parts)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func formMap(r *http.Request, prefix string) map[string]string {
	out := map[string]string{}
	for k, v := range r.Form {
		if strings.HasPrefix(k, prefix+"[") && strings.HasSuffix(k, "]") {
			out[strings.TrimSuffix(strings.TrimPrefix(k, prefix+"["), "]")] = v[0]
		}
	}
	return out
}

func formInt(r *http.Request, key string) int64 {
	v, _ := strconv.ParseInt(r.Form.Get(key), 10, 64)
	return v
}

func (f *fakeStripe) route(w http.ResponseWriter, r *http.Request, parts []string) {
	switch {
	case r.Method == http.MethodPost && len(parts) == 2 && parts[0] == "checkout" && parts[1] == "sessions":
		s := &fakeSession{
			ID: f.next("cs"), PI: f.next("pi"),
			Unit:        formInt(r, "line_items[0][price_data][unit_amount]"),
			Qty:         formInt(r, "line_items[0][quantity]"),
			AppFee:      formInt(r, "payment_intent_data[application_fee_amount]"),
			Destination: r.Form.Get("payment_intent_data[transfer_data][destination]"),
			Metadata:    formMap(r, "metadata"),
			PIMetadata:  formMap(r, "payment_intent_data[metadata]"),
			Email:       r.Form.Get("customer_email"),
			Created:     time.Now().Unix(),
		}
		f.sessions[s.ID] = s
		f.order = append(f.order, s.ID)
		writeJSON(w, 200, f.sessionJSON(s))

	case r.Method == http.MethodGet && len(parts) == 3 && parts[0] == "checkout" && parts[1] == "sessions":
		s, ok := f.sessions[parts[2]]
		if !ok {
			writeJSON(w, 404, map[string]any{"error": map[string]any{"type": "invalid_request_error", "message": "No such checkout.session"}})
			return
		}
		writeJSON(w, 200, f.sessionJSON(s))

	case r.Method == http.MethodGet && len(parts) == 2 && parts[0] == "checkout" && parts[1] == "sessions":
		data := []any{}
		for _, id := range f.order {
			if s := f.sessions[id]; s.Paid {
				data = append(data, f.sessionJSON(s))
			}
		}
		writeJSON(w, 200, map[string]any{"object": "list", "data": data, "has_more": false, "url": "/v1/checkout/sessions"})

	case r.Method == http.MethodPost && len(parts) == 1 && parts[0] == "refunds":
		pi, ok := f.pis[r.Form.Get("payment_intent")]
		if !ok {
			writeJSON(w, 400, map[string]any{"error": map[string]any{"type": "invalid_request_error", "message": "No such payment_intent"}})
			return
		}
		amount := formInt(r, "amount")
		if amount == 0 {
			amount = pi.Amount - pi.Refunded
		}
		if amount > pi.Amount-pi.Refunded {
			writeJSON(w, 400, map[string]any{"error": map[string]any{"type": "invalid_request_error", "message": "Refund is greater than unrefunded amount"}})
			return
		}
		pi.Refunded += amount
		ref := map[string]any{
			"id": f.next("re"), "object": "refund", "amount": amount, "currency": "usd", "status": "succeeded",
			"payment_intent": r.Form.Get("payment_intent"), "metadata": formMap(r, "metadata"),
			"reverse_transfer": r.Form.Get("reverse_transfer"), "refund_application_fee": r.Form.Get("refund_application_fee"),
		}
		pi.Refunds = append(pi.Refunds, ref)
		writeJSON(w, 200, ref)

	case r.Method == http.MethodGet && len(parts) == 2 && parts[0] == "payment_intents":
		pi, ok := f.pis[parts[1]]
		if !ok {
			writeJSON(w, 404, map[string]any{"error": map[string]any{"type": "invalid_request_error", "message": "No such payment_intent"}})
			return
		}
		writeJSON(w, 200, map[string]any{
			"id": parts[1], "object": "payment_intent", "amount": pi.Amount, "currency": "usd", "status": "succeeded",
			"latest_charge": map[string]any{
				"id": "ch_" + parts[1], "object": "charge", "amount": pi.Amount, "amount_refunded": pi.Refunded,
				"disputed":            pi.Disputed,
				"balance_transaction": map[string]any{"id": "txn_" + parts[1], "object": "balance_transaction", "fee": pi.Fee},
			},
		})

	default:
		f.t.Errorf("fake Stripe: unexpected %s %s", r.Method, r.URL.Path)
		writeJSON(w, 404, map[string]any{"error": map[string]any{"type": "invalid_request_error", "message": "unknown endpoint"}})
	}
}

func (f *fakeStripe) sessionJSON(s *fakeSession) map[string]any {
	status, payment := "open", "unpaid"
	if s.Paid {
		status, payment = "complete", "paid"
	}
	return map[string]any{
		"id": s.ID, "object": "checkout.session", "url": "https://checkout.stripe.test/" + s.ID,
		"status": status, "payment_status": payment, "currency": "usd", "amount_total": s.amount(),
		"payment_intent": s.PI, "metadata": s.Metadata, "created": s.Created,
		"customer_details": map[string]any{"email": s.Email},
	}
}

// refundOutside records a refund made directly in the Stripe dashboard.
func (f *fakeStripe) refundOutside(pi string, amount int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pis[pi].Refunded += amount
}

func (f *fakeStripe) setDisputed(pi string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pis[pi].Disputed = true
}

func (f *fakeStripe) refundsFor(pi string) []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pis[pi].Refunds
}

// pay marks a session paid (as if the buyer completed checkout) with the
// given processing fee, and returns it.
func (f *fakeStripe) pay(sessionID string, fee int64) *fakeSession {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.sessions[sessionID]
	if !ok {
		f.t.Fatalf("fake Stripe: no session %s", sessionID)
	}
	s.Paid = true
	f.pis[s.PI] = &fakePI{Amount: s.amount(), Fee: fee}
	return s
}

func (f *fakeStripe) lastSession() *fakeSession {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.order) == 0 {
		f.t.Fatal("fake Stripe: no session created")
	}
	return f.sessions[f.order[len(f.order)-1]]
}

func (f *fakeStripe) sessionCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.order)
}

// doReq2 is doReq with extra request headers.
func doReq2(t *testing.T, app *fiber.App, method, path, body string, headers map[string]string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := app.Test(req, -1)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// stripeEvent builds a signed webhook delivery for any event type.
func stripeEvent(t *testing.T, eventID, eventType string, object map[string]any) (string, string) {
	t.Helper()
	payload, _ := json.Marshal(map[string]any{
		"id": eventID, "object": "event", "api_version": stripe.APIVersion, "type": eventType,
		"data": map[string]any{"object": object},
	})
	signed := webhook.GenerateTestSignedPayload(&webhook.UnsignedPayload{Payload: payload, Secret: testWebhookSecret})
	return string(signed.Payload), signed.Header
}

func (f *fakeStripe) sessionEvent(s *fakeSession) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sessionJSON(s)
}

func refundObject(id, pi, orderID string, amount int64, status string) map[string]any {
	return map[string]any{"id": id, "object": "refund", "amount": amount, "currency": "usd", "status": status,
		"payment_intent": pi, "metadata": map[string]string{"order_id": orderID}}
}

// disputeObject: balance transactions are the money movements so far
// (negative = withdrawn, positive = reinstated), each with its fee.
func disputeObject(id, pi string, amount int64, status string, bts ...[2]int64) map[string]any {
	list := []any{}
	for i, bt := range bts {
		list = append(list, map[string]any{"id": fmt.Sprintf("txn_%s_%d", id, i), "object": "balance_transaction",
			"amount": bt[0], "fee": bt[1]})
	}
	return map[string]any{"id": id, "object": "dispute", "amount": amount, "currency": "usd", "status": status,
		"payment_intent": pi, "charge": "ch_" + pi, "reason": "fraudulent", "balance_transactions": list}
}

// createStripeHost is a host whose ticket sales go through Stripe Connect.
func createStripeHost(t *testing.T) string {
	t.Helper()
	db := testDB(t)
	host := createTestHost(t, db)
	if _, err := db.Exec(context.Background(),
		`INSERT INTO connected_accounts (user_id, stripe_account_id, payout_enabled, payout_gateway)
		 VALUES ($1, $2, true, 'stripe')`, host, "acct_test_"+uniq()); err != nil {
		t.Fatal(err)
	}
	return host
}
