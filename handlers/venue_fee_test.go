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
		if got := venueFeeStripeSessionMatches(tc.sess, event, 45); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestVenueFeePaymentMatches(t *testing.T) {
	const event = "6b34a2a3-0b11-467d-a9b2-0569fc99a147"
	ok := &services.CapturedOrder{Reference: "venue-fee-" + event, Currency: "USD", Amount: 30}

	cases := []struct {
		name     string
		captured *services.CapturedOrder
		fee      float64
		want     bool
	}{
		{"exact payment", ok, 30, true},
		{"float rounding", &services.CapturedOrder{Reference: ok.Reference, Currency: "USD", Amount: 29.999999}, 30, true},
		{"order for another event", &services.CapturedOrder{Reference: "venue-fee-other", Currency: "USD", Amount: 30}, 30, false},
		{"cheaper order", &services.CapturedOrder{Reference: ok.Reference, Currency: "USD", Amount: 15}, 30, false},
		{"wrong currency", &services.CapturedOrder{Reference: ok.Reference, Currency: "JMD", Amount: 30}, 30, false},
		{"ticket order reused", &services.CapturedOrder{Reference: "ticket-" + event, Currency: "USD", Amount: 30}, 30, false},
		{"nothing captured", nil, 30, false},
	}
	for _, tc := range cases {
		if got := venueFeePaymentMatches(tc.captured, event, tc.fee); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}
