package services

import "testing"

func TestDollarsToCentsIsExact(t *testing.T) {
	ok := map[float64]int64{
		0: 0, 0.01: 1, 1: 100, 19.99: 1999, 0.29: 29, 4.35: 435, 1.15: 115,
		1234.56: 123456, 99999999.99: 9999999999,
	}
	for in, want := range ok {
		got, err := DollarsToCents(in)
		if err != nil || got != want {
			t.Errorf("DollarsToCents(%v) = %d, %v; want %d", in, got, err, want)
		}
	}
	// The bug this replaces: at run time int64(price*100) is 1998 for 19.99,
	// so Stripe was asked to charge a cent less than the listed price.
	price := 19.99
	if int64(price*100) != 1998 {
		t.Fatal("float truncation assumption changed")
	}
	for _, bad := range []float64{-0.01, 19.999, 0.001, 100000000, 1e300} {
		if _, err := DollarsToCents(bad); err == nil {
			t.Errorf("DollarsToCents(%v) accepted", bad)
		}
	}
}

func TestParseAndFormatDecimalCents(t *testing.T) {
	ok := map[string]int64{"0": 0, "5": 500, "12.3": 1230, "12.30": 1230, "0.07": 7, "19.99": 1999}
	for in, want := range ok {
		got, err := ParseDecimalCents(in)
		if err != nil || got != want {
			t.Errorf("ParseDecimalCents(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "-1.00", "1.234", "abc", ".5", "1e3", "+2"} {
		if _, err := ParseDecimalCents(bad); err == nil {
			t.Errorf("ParseDecimalCents(%q) accepted", bad)
		}
	}
	for cents, want := range map[int64]string{0: "0.00", 7: "0.07", 1230: "12.30", 1999: "19.99", -250: "-2.50"} {
		if got := FormatCents(cents); got != want {
			t.Errorf("FormatCents(%d) = %q, want %q", cents, got, want)
		}
	}
}

func TestPlatformFeeRoundsHalfUpAndAddsUp(t *testing.T) {
	rule := FeeRule{BasisPoints: 1000}
	cases := map[int64]int64{0: 0, 1: 0, 5: 1, 4: 0, 1999: 200, 2500: 250, 1995: 200, 1994: 199}
	for amount, want := range cases {
		if got := rule.PlatformFee(amount); got != want {
			t.Errorf("fee(%d) = %d, want %d", amount, got, want)
		}
	}
	if rule.Name() != "commission-1000bps" {
		t.Errorf("rule name %q", rule.Name())
	}
	// Splitting refunds proportionally never exceeds the original split.
	for _, amount := range []int64{1, 333, 1999, 2500, 10001} {
		fee := rule.PlatformFee(amount)
		var refundedFee int64
		for _, part := range []int64{amount / 3, amount / 3, amount - 2*(amount/3)} {
			refundedFee += ProportionalShare(part, fee, amount)
		}
		if refundedFee > fee+1 {
			t.Errorf("amount %d: proportional refunds %d exceed fee %d", amount, refundedFee, fee)
		}
	}
}
