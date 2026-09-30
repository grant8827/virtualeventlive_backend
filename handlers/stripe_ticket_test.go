package handlers

import (
	"testing"

	stripe "github.com/stripe/stripe-go/v82"
)

func TestStripeTicketSessionPaid(t *testing.T) {
	valid := func() *stripe.CheckoutSession {
		return &stripe.CheckoutSession{
			AmountTotal:   2500,
			Currency:      stripe.CurrencyUSD,
			PaymentStatus: stripe.CheckoutSessionPaymentStatusPaid,
			Metadata: map[string]string{
				"type":     "ticket",
				"event_id": "event-1",
			},
		}
	}

	tests := []struct {
		name string
		edit func(*stripe.CheckoutSession)
		want bool
	}{
		{"valid", func(*stripe.CheckoutSession) {}, true},
		{"unpaid", func(s *stripe.CheckoutSession) { s.PaymentStatus = stripe.CheckoutSessionPaymentStatusUnpaid }, false},
		{"wrong currency", func(s *stripe.CheckoutSession) { s.Currency = stripe.CurrencyJMD }, false},
		{"zero amount", func(s *stripe.CheckoutSession) { s.AmountTotal = 0 }, false},
		{"wrong type", func(s *stripe.CheckoutSession) { s.Metadata["type"] = "venue_fee" }, false},
		{"missing event", func(s *stripe.CheckoutSession) { delete(s.Metadata, "event_id") }, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sess := valid()
			tc.edit(sess)
			if got := stripeTicketSessionPaid(sess); got != tc.want {
				t.Fatalf("stripeTicketSessionPaid() = %v, want %v", got, tc.want)
			}
		})
	}
}
