package services

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Money is handled as integer cents (PAY-06). Floats only appear at the edges
// — JSON request bodies and display totals — and are converted here with
// explicit rounding rules, never by truncation.

// MaxAmountCents is the largest single amount accepted: the events table
// stores prices as NUMERIC(10,2).
const MaxAmountCents = 99_999_999_99

// Ticket price bounds: Stripe's minimum and maximum card charge in USD.
const (
	MinTicketPriceCents = 50
	MaxTicketPriceCents = 999_999_99
)

var errInvalidAmount = errors.New("amount must be a non-negative dollar value with at most two decimal places")

// DollarsToCents converts a dollar amount from a request body to cents. It
// rejects negative, non-finite, oversized amounts and fractions of a cent
// (19.999), and is immune to float noise (19.99*100 is 1998.9999...).
func DollarsToCents(v float64) (int64, error) {
	if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
		return 0, errInvalidAmount
	}
	scaled := v * 100
	cents := math.Round(scaled)
	if math.Abs(scaled-cents) > 1e-6 || cents > MaxAmountCents {
		return 0, errInvalidAmount
	}
	return int64(cents), nil
}

// ParseDecimalCents parses a provider amount string such as "12.30" or "5"
// exactly, without going through a float.
func ParseDecimalCents(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" || strings.HasPrefix(s, "-") || strings.HasPrefix(s, "+") {
		return 0, fmt.Errorf("invalid amount %q", s)
	}
	whole, frac, _ := strings.Cut(s, ".")
	if len(frac) > 2 || whole == "" {
		return 0, fmt.Errorf("invalid amount %q", s)
	}
	for len(frac) < 2 {
		frac += "0"
	}
	w, err := strconv.ParseInt(whole, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid amount %q", s)
	}
	f, err := strconv.ParseInt(frac, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid amount %q", s)
	}
	if w > MaxAmountCents/100 {
		return 0, fmt.Errorf("invalid amount %q", s)
	}
	return w*100 + f, nil
}

// FormatCents renders cents as a provider decimal string ("12.30").
func FormatCents(cents int64) string {
	sign := ""
	if cents < 0 {
		sign, cents = "-", -cents
	}
	return fmt.Sprintf("%s%d.%02d", sign, cents/100, cents%100)
}

// CentsToDollars is for display totals in JSON responses only.
func CentsToDollars(cents int64) float64 {
	return float64(cents) / 100
}

// FeeRule is the platform commission applied to an order. It is stored on
// every order (PAY-07), so changing PLATFORM_FEE_BPS later never rewrites
// what past buyers and hosts were charged.
type FeeRule struct {
	// BasisPoints of the ticket amount kept by the platform (1000 = 10%).
	BasisPoints int
}

// DefaultPlatformFeeBPS is the commission the site has always advertised:
// hosts keep 90% of ticket revenue.
const DefaultPlatformFeeBPS = 1000

// Name identifies the rule in the orders table.
func (r FeeRule) Name() string {
	return "commission-" + strconv.Itoa(r.BasisPoints) + "bps"
}

// PlatformFee is the platform's share of amount, rounded half up to the cent.
func (r FeeRule) PlatformFee(amountCents int64) int64 {
	return ProportionalShare(amountCents, int64(r.BasisPoints), 10_000)
}

// ProportionalShare returns amount*num/den rounded half up, for non-negative
// inputs. Used to split refunds between platform and host in the same
// proportion as the original sale.
func ProportionalShare(amount, num, den int64) int64 {
	if den <= 0 || amount <= 0 || num <= 0 {
		return 0
	}
	return (amount*num*2 + den) / (den * 2)
}
