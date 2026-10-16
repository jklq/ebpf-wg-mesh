package billing

import (
	"math/big"
	"testing"
	"time"
)

func TestRateVersion1ReferencePrices(t *testing.T) {
	v1 := RateVersion1()
	monthMicros := big.NewInt(monthSeconds * 1_000_000)
	monthGiBMillis := big.NewInt(bytesPerGiB * monthSeconds * 1000)
	tests := []struct {
		meter    Meter
		quantity *big.Int
		cents    int64
	}{
		{MeterCPU, monthMicros, 2000},                              // one vCPU-month
		{MeterMemory, monthGiBMillis, 1000},                        // one GiB-month
		{MeterVolume, monthGiBMillis, 15},                          // one GiB-month stored
		{MeterEgress, big.NewInt(bytesPerGiB), 5},                  // one GiB out
		{MeterCPU, big.NewInt(monthSeconds * 1_000_000 / 2), 1000}, // half a vCPU-month
	}
	for _, test := range tests {
		if got := RoundHalfUpCents(v1.Rates[test.meter].Price(test.quantity)); got != test.cents {
			t.Errorf("%s x %s = %d cents, want %d", test.meter, test.quantity, got, test.cents)
		}
	}
}

func TestComputeTotalsAllowanceAndCredit(t *testing.T) {
	rat := func(cents int64) *big.Rat { return big.NewRat(cents, 1) }
	tests := []struct {
		name                                  string
		gross, minimum, included, creditAvail int64
		want                                  Totals
	}{
		{"below allowance pays the minimum", 300, 500, 500, 0, Totals{300, 0, 0, 500}},
		{"above allowance pays gross when minimum equals allowance", 1200, 500, 500, 0, Totals{1200, 700, 0, 1200}},
		{"credit applies after allowance only", 1200, 500, 500, 300, Totals{1200, 700, 300, 900}},
		{"credit never offsets the minimum", 300, 500, 500, 900, Totals{300, 0, 0, 500}},
		{"credit is capped at after-allowance usage", 800, 500, 500, 900, Totals{800, 300, 300, 500}},
		{"trial has no minimum or allowance", 1510, 0, 0, 500, Totals{1510, 1510, 500, 1010}},
	}
	for _, test := range tests {
		if got := ComputeTotals(rat(test.gross), test.minimum, test.included, test.creditAvail); got != test.want {
			t.Errorf("%s: got %+v, want %+v", test.name, got, test.want)
		}
	}
}

func TestRoundHalfUpCents(t *testing.T) {
	cases := []struct{ num, den, want int64 }{
		{1, 2, 1}, {3, 2, 2}, {5, 2, 3}, {1, 3, 0}, {2, 3, 1}, {100, 1, 100},
	}
	for _, tc := range cases {
		if got := RoundHalfUpCents(big.NewRat(tc.num, tc.den)); got != tc.want {
			t.Errorf("round(%d/%d) = %d, want %d", tc.num, tc.den, got, tc.want)
		}
	}
}

func TestAllocateCentsLargestRemainder(t *testing.T) {
	// Exact 0.5 + 0.5 + 0.5 rounds to 2; the two earliest lines win ties.
	exact := []*big.Rat{big.NewRat(1, 2), big.NewRat(1, 2), big.NewRat(1, 2)}
	got := AllocateCents(exact, RoundHalfUpCents(big.NewRat(3, 2)))
	want := []int64{1, 1, 0}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("AllocateCents = %v, want %v", got, want)
		}
	}
	exact = []*big.Rat{big.NewRat(1000, 3), big.NewRat(1000, 3), big.NewRat(1000, 3)}
	got = AllocateCents(exact, 1000)
	if got[0]+got[1]+got[2] != 1000 {
		t.Fatalf("AllocateCents sum = %d, want 1000 (%v)", got[0]+got[1]+got[2], got)
	}
}

func TestAddAnchorMonthsClampsFromAnchorDay(t *testing.T) {
	anchor := time.Date(2026, 1, 31, 10, 30, 0, 0, time.UTC)
	tests := map[int]time.Time{
		1:  time.Date(2026, 2, 28, 10, 30, 0, 0, time.UTC),
		2:  time.Date(2026, 3, 31, 10, 30, 0, 0, time.UTC),
		12: time.Date(2027, 1, 31, 10, 30, 0, 0, time.UTC),
		13: time.Date(2027, 2, 28, 10, 30, 0, 0, time.UTC),
	}
	for months, want := range tests {
		if got := AddAnchorMonths(anchor, months); !got.Equal(want) {
			t.Errorf("anchor + %d months = %s, want %s", months, got, want)
		}
	}
	leap := time.Date(2028, 1, 31, 0, 0, 0, 0, time.UTC)
	if got, want := AddAnchorMonths(leap, 1), time.Date(2028, 2, 29, 0, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Errorf("leap anchor + 1 = %s, want %s", got, want)
	}
}
