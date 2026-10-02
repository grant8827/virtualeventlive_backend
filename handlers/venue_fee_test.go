package handlers

import (
	"testing"

	stripe "github.com/stripe/stripe-go/v82"

	"vertualeventlive/backend/services"
)

func TestVenueFeeStripeSessionMatches(t *testing.T) {
	const event = "6b34a2a3-0b11-467d-a9b2-0569fc99a147"
	paid := func(mod func(s *stripe.CheckoutSession)) *stripe.CheckoutSession {
		s := &stripe.CheckoutSession{
			Metadata:      map[string]string{"type": "venue_fee", "event_id": event},
			PaymentStatus: stripe.CheckoutSessionPaymentStatusPaid,
			Currency:      "usd",
			AmountTotal:   4500,
		}
		if mod != nil {
			mod(s)
		}
		return s
	}

	cases := []struct {
		name string
		sess *stripe.CheckoutSession
		want bool
	}{
		{"paid in full", paid(nil), true},
		{"not paid yet", paid(func(s *stripe.CheckoutSession) { s.PaymentStatus = stripe.CheckoutSessionPaymentStatusUnpaid }), false},
		{"another event", paid(func(s *stripe.CheckoutSession) { s.Metadata["event_id"] = "other" }), false},
		{"ticket session", paid(func(s *stripe.CheckoutSession) { s.Metadata["type"] = "ticket" }), false},
		{"cheaper amount", paid(func(s *stripe.CheckoutSession) { s.AmountTotal = 1500 }), false},
		{"wrong currency", paid(func(s *stripe.CheckoutSession) { s.Currency = "jmd" }), false},
		{"no session", nil, false},
	}
	for _, tc := range cases {
		if got := venueFeeStripeSessionMatches(tc.sess, event, 4500); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

// PAY-04: a PayPal capture is only accepted when PayPal took exactly the
// order's amount, in USD, for this event, into the host's account.
func TestPayPalCaptureMatches(t *testing.T) {
	const event = "6b34a2a3-0b11-467d-a9b2-0569fc99a147"
	o := &order{ID: "o1", EventID: event, AmountCents: 2500}
	ok := services.CapturedOrder{Reference: "ticket-" + event, Currency: "USD", AmountCents: 2500,
		PayeeMerchantID: "HOSTMERCHANT", CaptureID: "CAP1"}
	with := func(f func(*services.CapturedOrder)) *services.CapturedOrder { c := ok; f(&c); return &c }

	cases := map[string]struct {
		captured *services.CapturedOrder
		want     bool
	}{
		"exact payment":        {&ok, true},
		"a cent short":         {with(func(c *services.CapturedOrder) { c.AmountCents = 2499 }), false},
		"more than the order":  {with(func(c *services.CapturedOrder) { c.AmountCents = 2600 }), false},
		"wrong currency":       {with(func(c *services.CapturedOrder) { c.Currency = "JMD" }), false},
		"another event":        {with(func(c *services.CapturedOrder) { c.Reference = "ticket-other" }), false},
		"booking fee reused":   {with(func(c *services.CapturedOrder) { c.Reference = "venue-fee-" + event }), false},
		"paid to someone else": {with(func(c *services.CapturedOrder) { c.PayeeMerchantID = "ATTACKER" }), false},
		"no capture ID":        {with(func(c *services.CapturedOrder) { c.CaptureID = "" }), false},
		"nothing captured":     {nil, false},
	}
	for name, tc := range cases {
		if err := paypalCaptureMatches(tc.captured, o, "HOSTMERCHANT"); (err == nil) != tc.want {
			t.Errorf("%s: got %v, want match=%v", name, err, tc.want)
		}
	}
}
