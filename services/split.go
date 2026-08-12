package services

import "math"

type SplitPayout struct {
	GrossAmount float64 `json:"gross_amount"`
	StripeFee   float64 `json:"stripe_fee"`
	PlatformFee float64 `json:"platform_fee"`
	HostPayout  float64 `json:"host_payout"`
}

// CalculateSplit computes the 10% platform cut plus Stripe processing fee.
//
// HostPayout is what a WiPay/PayPal host receives: the platform holds the
// full charge and disburses manually, so it nets out an estimated processing
// fee before sending. It does NOT apply to Stripe-connected hosts — use
// StripeDestinationPayout for those instead.
func CalculateSplit(price float64) SplitPayout {
	stripeFee := math.Round(((price*0.029)+0.30)*100) / 100
	platformFee := math.Round(price*0.10*100) / 100
	hostPayout := math.Round((price-platformFee-stripeFee)*100) / 100

	return SplitPayout{
		GrossAmount: price,
		StripeFee:   stripeFee,
		PlatformFee: platformFee,
		HostPayout:  hostPayout,
	}
}

// StripeDestinationPayout returns what a Stripe-connected host actually
// receives via the checkout destination charge: gross minus the platform's
// application fee, full stop. Stripe deducts its own processing fee from the
// platform's kept application fee, not from the host's transfer, so it must
// not be subtracted again here the way HostPayout does for manual gateways.
func (s SplitPayout) StripeDestinationPayout() float64 {
	return math.Round((s.GrossAmount-s.PlatformFee)*100) / 100
}
