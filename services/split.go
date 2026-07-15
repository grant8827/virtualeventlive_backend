package services

import "math"

type SplitPayout struct {
	GrossAmount float64 `json:"gross_amount"`
	StripeFee   float64 `json:"stripe_fee"`
	PlatformFee float64 `json:"platform_fee"`
	HostPayout  float64 `json:"host_payout"`
}

// CalculateSplit computes the 10% platform cut plus Stripe processing fee.
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
