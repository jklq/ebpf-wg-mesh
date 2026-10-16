package billing

import (
	"fmt"
	"math/big"
	"sort"
	"time"
)

// Meter names a billable quantity. Units match the stored usage quantities
// in docs/resource-usage.md: CPU microseconds, byte-milliseconds for the
// sampled gauges, and bytes for egress.
type Meter string

const (
	MeterCPU    Meter = "cpu"
	MeterMemory Meter = "memory"
	MeterVolume Meter = "volume"
	MeterEgress Meter = "egress"
)

// Meters in stable statement order.
var Meters = []Meter{MeterCPU, MeterMemory, MeterVolume, MeterEgress}

// Unit describes one stored quantity unit of a meter.
func (m Meter) Unit() string {
	switch m {
	case MeterCPU:
		return "cpu-microseconds"
	case MeterMemory, MeterVolume:
		return "byte-milliseconds"
	case MeterEgress:
		return "bytes"
	default:
		return ""
	}
}

// monthSeconds defines the month used to convert monthly reference prices
// to exact per-unit rates: exactly 30 days.
const monthSeconds = 2_592_000

const bytesPerGiB = 1 << 30

// Rate is an exact price in cents per quantity unit, numerator/denominator.
type Rate struct {
	Num *big.Int
	Den *big.Int
}

func rate(num, den int64) Rate {
	r := new(big.Rat).SetFrac64(num, den)
	return Rate{Num: r.Num(), Den: r.Denom()}
}

// Price returns the exact cost in cents of quantity units at this rate.
func (r Rate) Price(quantity *big.Int) *big.Rat {
	out := new(big.Rat).SetFrac(new(big.Int).Set(r.Num), new(big.Int).Set(r.Den))
	return out.Mul(out, new(big.Rat).SetInt(quantity))
}

// RateVersion is one immutable set of meter rates.
type RateVersion struct {
	Version  int
	Currency string
	Rates    map[Meter]Rate
}

// RateVersion1 derives exact per-unit rates from the Railway reference
// prices in docs/usage-billing-plan.md, denominated in EUR and GiB:
//
//	CPU     €20.00 per vCPU-month
//	Memory  €10.00 per GiB-month
//	Volume  €0.15  per GiB-month
//	Egress  €0.05  per GiB
//
// These prices are provisional shadow rates, not a launch price decision.
func RateVersion1() RateVersion {
	monthMillis := int64(monthSeconds) * 1000
	return RateVersion{
		Version:  1,
		Currency: Currency,
		Rates: map[Meter]Rate{
			MeterCPU:    rate(2000, monthSeconds*1_000_000),
			MeterMemory: rate(1000, bytesPerGiB*monthMillis),
			MeterVolume: rate(15, bytesPerGiB*monthMillis),
			MeterEgress: rate(5, bytesPerGiB),
		},
	}
}

// RoundHalfUpCents rounds an exact cent amount to integer cents, half away
// from zero (amounts here are never negative).
func RoundHalfUpCents(value *big.Rat) int64 {
	half := big.NewRat(1, 2)
	shifted := new(big.Rat).Add(value, half)
	// Rat.Num/Denom with truncation toward zero is floor for positives.
	q := new(big.Int).Quo(shifted.Num(), shifted.Denom())
	return q.Int64()
}

// Totals is one period's integer-cent arithmetic. Gross is rounded once from
// the exact rated sum; the minimum, allowance, and credits are already
// integer cents, so this equals rounding the exact period total once.
type Totals struct {
	GrossCents          int64
	AfterAllowanceCents int64
	CreditAppliedCents  int64
	PeriodTotalCents    int64
}

// ComputeTotals applies the plan equation:
//
//	usage_after_allowance = max(0, gross - included_usage)
//	period_total = monthly_minimum + usage_after_allowance - credit
//
// Credit applies only after the allowance, never to the paid minimum.
func ComputeTotals(grossExact *big.Rat, minimumCents, includedCents, creditAvailableCents int64) Totals {
	gross := RoundHalfUpCents(grossExact)
	after := gross - includedCents
	if after < 0 {
		after = 0
	}
	credit := creditAvailableCents
	if credit > after {
		credit = after
	}
	if credit < 0 {
		credit = 0
	}
	return Totals{
		GrossCents:          gross,
		AfterAllowanceCents: after,
		CreditAppliedCents:  credit,
		PeriodTotalCents:    minimumCents + after - credit,
	}
}

// AllocateCents splits total cents across exact line amounts with the
// largest-remainder method. Ties break toward the earlier line, so the
// allocation is deterministic for a deterministic line order.
func AllocateCents(exact []*big.Rat, total int64) []int64 {
	out := make([]int64, len(exact))
	if len(exact) == 0 {
		return out
	}
	type remainder struct {
		index int
		frac  *big.Rat
	}
	var allocated int64
	parts := make([]remainder, len(exact))
	for i, value := range exact {
		floor := new(big.Int).Quo(value.Num(), value.Denom())
		out[i] = floor.Int64()
		allocated += out[i]
		parts[i] = remainder{index: i, frac: new(big.Rat).Sub(value, new(big.Rat).SetInt(floor))}
	}
	sort.SliceStable(parts, func(a, b int) bool {
		return parts[a].frac.Cmp(parts[b].frac) > 0
	})
	// Each remainder is < 1 cent, so the deficit is smaller than len(parts)
	// whenever total is the rounded sum of the exact amounts.
	for i := int64(0); i < total-allocated; i++ {
		out[parts[i%int64(len(parts))].index]++
	}
	return out
}

// AddAnchorMonths returns the instant months calendar months after anchor,
// keeping the anchor's time of day and clamping the day from the original
// anchor day: Jan 31 anchors Feb 28 (or 29) and then Mar 31, never Mar 28.
func AddAnchorMonths(anchor time.Time, months int) time.Time {
	anchor = anchor.UTC()
	y, m, d := anchor.Date()
	total := int(m) - 1 + months
	y += total / 12
	m = time.Month(total%12 + 1)
	if last := daysInMonth(y, m); d > last {
		d = last
	}
	return time.Date(y, m, d, anchor.Hour(), anchor.Minute(), anchor.Second(), anchor.Nanosecond(), time.UTC)
}

func daysInMonth(year int, month time.Month) int {
	// Day 0 of the next month is this month's last day.
	return time.Date(year, month+1, 0, 0, 0, 0, 0, time.UTC).Day()
}

// FormatCents renders integer cents for display and CSV export.
func FormatCents(cents int64) string {
	sign := ""
	if cents < 0 {
		sign = "-"
		cents = -cents
	}
	return fmt.Sprintf("%s%d.%02d", sign, cents/100, cents%100)
}
