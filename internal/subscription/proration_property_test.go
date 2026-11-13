package subscription

import (
	"math"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
)

// Property tests for the proration arithmetic.
//
// The existing proration tests are example-based: ~44 cases across 14 files,
// each pinning a specific input to a specific output. They are good tests and
// they encode real regressions. What they cannot do is explore the inputs
// nobody thought of, and that is where this codebase's arithmetic bugs have
// actually come from — the ADR-091 timezone-seam overbill, the Jan-31→Feb-28
// clamp, the $4.43 proration floor were all found by walking flows by hand,
// not by the suite.
//
// So these assert PROPERTIES over generated inputs instead of outputs over
// chosen ones. Each property below is either (a) explicitly promised by a doc
// comment in proration_math.go, or (b) a relation that must hold for the
// result to be money-safe. A failure here is a real defect, not a changed
// expectation.
//
// Deliberately using a seeded generator over math/rand/v2 rather than adding a
// property-testing dependency: the generators need domain constraints
// (remaining <= total, positive periods, realistic magnitudes) that are
// clearer written directly, and a fixed seed makes any failure reproducible.

const propIters = 20000

// genRand returns a deterministically seeded source. A fixed seed means a
// failure is reproducible from the test output alone; bump the seed to widen
// the search rather than making it time-based, which would make failures
// unreproducible.
func genRand() *rand.Rand { return rand.New(rand.NewPCG(0x5EED, 0xB1115)) }

// realistic money magnitudes in cents: sub-cent edge cases up to ~$10M lines.
func genAmount(r *rand.Rand) int64 {
	switch r.IntN(4) {
	case 0:
		return int64(r.IntN(100)) // 0..99c — rounding lives here
	case 1:
		return int64(r.IntN(100_000)) // up to $1,000
	case 2:
		return int64(r.IntN(100_000_000)) // up to $1M
	default:
		return int64(r.IntN(1_000_000_000)) // up to $10M
	}
}

// genPeriod returns (remainingDays, totalDays) with 0 <= remaining <= total,
// total >= 1 — the domain the callers actually pass. Yearly cycles reach 366.
func genPeriod(r *rand.Rand) (int64, int64) {
	total := int64(r.IntN(366) + 1)
	return int64(r.IntN(int(total) + 1)), total
}

// TestProperty_ProrationCents_FullPeriodIsFullDelta: when no time has elapsed
// (remaining == total) the customer is charged the entire difference. Any
// scaling error shows up immediately here because the ratio is exactly 1.
func TestProperty_ProrationCents_FullPeriodIsFullDelta(t *testing.T) {
	r := genRand()
	for i := 0; i < propIters; i++ {
		oldA, newA := genAmount(r), genAmount(r)
		_, total := genPeriod(r)
		if got, want := mustProrationCents(oldA, newA, total, total), newA-oldA; got != want {
			t.Fatalf("remaining==total must yield the whole delta: old=%d new=%d total=%d got=%d want=%d",
				oldA, newA, total, got, want)
		}
	}
}

// TestProperty_ProrationCents_NoTimeNoMoney: a change with zero remaining days
// prorates to nothing. The customer is not charged for time they will not use.
func TestProperty_ProrationCents_NoTimeNoMoney(t *testing.T) {
	r := genRand()
	for i := 0; i < propIters; i++ {
		oldA, newA := genAmount(r), genAmount(r)
		_, total := genPeriod(r)
		if got := mustProrationCents(oldA, newA, 0, total); got != 0 {
			t.Fatalf("zero remaining days must prorate to 0: old=%d new=%d total=%d got=%d",
				oldA, newA, total, got)
		}
	}
}

// TestProperty_ProrationCents_SameAmountIsFree: swapping to a plan that costs
// the same must never move money, at any point in the cycle. A sign or
// rounding bug that survives the examples would surface here.
func TestProperty_ProrationCents_SameAmountIsFree(t *testing.T) {
	r := genRand()
	for i := 0; i < propIters; i++ {
		amt := genAmount(r)
		rem, total := genPeriod(r)
		if got := mustProrationCents(amt, amt, rem, total); got != 0 {
			t.Fatalf("equal amounts must prorate to 0: amt=%d remaining=%d total=%d got=%d",
				amt, rem, total, got)
		}
	}
}

// TestProperty_ProrationCents_Antisymmetric: upgrading A→B and downgrading B→A
// at the same instant must be equal and opposite. If they are not, a customer
// could be moved back and forth to extract or lose money.
func TestProperty_ProrationCents_Antisymmetric(t *testing.T) {
	r := genRand()
	for i := 0; i < propIters; i++ {
		a, b := genAmount(r), genAmount(r)
		rem, total := genPeriod(r)
		up := mustProrationCents(a, b, rem, total)
		down := mustProrationCents(b, a, rem, total)
		if up != -down {
			t.Fatalf("A→B and B→A must be equal and opposite: a=%d b=%d remaining=%d total=%d up=%d down=%d",
				a, b, rem, total, up, down)
		}
	}
}

// TestProperty_ProrationCents_SignAndBound: an upgrade never credits, a
// downgrade never charges, and neither ever exceeds the full-period delta.
// The bound is what stops a partial period costing more than a whole one.
func TestProperty_ProrationCents_SignAndBound(t *testing.T) {
	r := genRand()
	for i := 0; i < propIters; i++ {
		oldA, newA := genAmount(r), genAmount(r)
		rem, total := genPeriod(r)
		got := mustProrationCents(oldA, newA, rem, total)
		delta := newA - oldA
		switch {
		case delta > 0 && got < 0:
			t.Fatalf("upgrade produced a credit: old=%d new=%d rem=%d total=%d got=%d", oldA, newA, rem, total, got)
		case delta < 0 && got > 0:
			t.Fatalf("downgrade produced a charge: old=%d new=%d rem=%d total=%d got=%d", oldA, newA, rem, total, got)
		}
		if abs64(got) > abs64(delta) {
			t.Fatalf("partial period exceeded the full delta: old=%d new=%d rem=%d total=%d got=%d delta=%d",
				oldA, newA, rem, total, got, delta)
		}
	}
}

// TestProperty_ProrationCents_MonotoneInRemainingDays: more remaining time
// never costs less. A tier/rounding bug that inverts this would let a customer
// pay less by changing plans EARLIER in the cycle.
func TestProperty_ProrationCents_MonotoneInRemainingDays(t *testing.T) {
	r := genRand()
	for i := 0; i < propIters; i++ {
		oldA, newA := genAmount(r), genAmount(r)
		if newA <= oldA {
			continue // fix the sign so "more" is unambiguous
		}
		_, total := genPeriod(r)
		r1 := int64(r.IntN(int(total) + 1))
		r2 := int64(r.IntN(int(total) + 1))
		if r1 > r2 {
			r1, r2 = r2, r1
		}
		a := mustProrationCents(oldA, newA, r1, total)
		b := mustProrationCents(oldA, newA, r2, total)
		if a > b {
			t.Fatalf("fewer remaining days cost MORE: old=%d new=%d total=%d r1=%d->%d r2=%d->%d",
				oldA, newA, total, r1, a, r2, b)
		}
	}
}

// TestProperty_SplitUpgradeProration_ConservesNetAndTax verifies the guarantee
// the implementation's own doc comment makes:
//
//	"guarantees creditCents+chargeCents == netCents and
//	 creditTax+chargeTax == taxCents EXACTLY (int64) ... the split can never
//	 drift ±1 cent from it"
//
// This is the property that keeps an invoice's subtotal and tax unchanged when
// a single net proration is displayed as two lines. A ±1c drift here is a
// visible, customer-facing wrong total, and it is exactly the kind of defect
// that independent rounding of both halves would introduce.
func TestProperty_SplitUpgradeProration_ConservesNetAndTax(t *testing.T) {
	r := genRand()
	for i := 0; i < propIters; i++ {
		oldAmount := genAmount(r)
		rem, denom := genPeriod(r)
		net := int64(r.IntN(10_000_000)) + 1 // callers pass net > 0 (upgrade branch)
		tax := int64(r.IntN(3_000_000))      // 0 for none/manual/deferred

		credit, charge, creditTax, chargeTax := mustSplitUpgradeProration(oldAmount, rem, denom, net, tax)

		if credit+charge != net {
			t.Fatalf("net not conserved: old=%d rem=%d denom=%d net=%d tax=%d credit=%d charge=%d sum=%d",
				oldAmount, rem, denom, net, tax, credit, charge, credit+charge)
		}
		if creditTax+chargeTax != tax {
			t.Fatalf("tax not conserved: old=%d rem=%d denom=%d net=%d tax=%d creditTax=%d chargeTax=%d sum=%d",
				oldAmount, rem, denom, net, tax, creditTax, chargeTax, creditTax+chargeTax)
		}
	}
}

// TestProperty_SplitUpgradeProration_CreditIsNeverPositive: the credit line
// represents unused time on the OLD plan and is displayed as a negative
// amount. A positive "credit" would read as an extra charge on the invoice.
func TestProperty_SplitUpgradeProration_CreditIsNeverPositive(t *testing.T) {
	r := genRand()
	for i := 0; i < propIters; i++ {
		oldAmount := genAmount(r)
		rem, denom := genPeriod(r)
		net := int64(r.IntN(10_000_000)) + 1
		credit, _, _, _ := mustSplitUpgradeProration(oldAmount, rem, denom, net, 0)
		if credit > 0 {
			t.Fatalf("credit line must not be positive: old=%d rem=%d denom=%d net=%d credit=%d",
				oldAmount, rem, denom, net, credit)
		}
	}
}

// TestProperty_ProrationCents_RoundsToNearestNotToward0 closes a gap mutation
// testing exposed: every earlier property also passes if banker's rounding is
// replaced by integer truncation, because truncation is antisymmetric too
// (trunc(-x) == -trunc(x)) and stays within the same bounds. But truncation is
// BIASED TOWARD ZERO — it systematically under-charges every upgrade and
// under-credits every downgrade by up to a cent, on every invoice, forever.
//
// The distinguishing property is accuracy against the exact rational: the
// result must be the NEAREST integer, so it can never be more than half a cent
// from the true value. Truncation violates this whenever the remainder exceeds
// half the denominator.
func TestProperty_ProrationCents_RoundsToNearestNotToward0(t *testing.T) {
	r := genRand()
	for i := 0; i < propIters; i++ {
		oldA, newA := genAmount(r), genAmount(r)
		rem, total := genPeriod(r)
		got := mustProrationCents(oldA, newA, rem, total)

		// Compare 2*got*total against 2*numerator: staying in integers avoids
		// introducing the float error we are trying to detect.
		num := (newA - oldA) * rem
		diff := got*total - num // exact error, scaled by total
		if abs64(diff)*2 > total {
			t.Fatalf("result is not the nearest integer (truncation-style bias): "+
				"old=%d new=%d rem=%d total=%d got=%d exact=%d/%d err*total=%d",
				oldA, newA, rem, total, got, num, total, diff)
		}
	}
}

// TestProperty_ProrationCents_DegenerateAndOverRangePeriods covers inputs the
// earlier generator never produced — mutation testing showed the totalDays<=0
// guard could be removed without a single test noticing, because genPeriod
// always returned total >= 1.
func TestProperty_ProrationCents_DegenerateAndOverRangePeriods(t *testing.T) {
	r := genRand()
	for i := 0; i < 2000; i++ {
		oldA, newA := genAmount(r), genAmount(r)
		// A zero or negative denominator means "no period to prorate over".
		// It must return 0, not panic and not divide.
		for _, total := range []int64{0, -1, -365} {
			if got := mustProrationCents(oldA, newA, int64(r.IntN(400)), total); got != 0 {
				t.Fatalf("non-positive totalDays must prorate to 0: old=%d new=%d total=%d got=%d",
					oldA, newA, total, got)
			}
		}
	}
}

// TestProperty_CycleDays_SaneAcrossZonesAndAnchors targets the code where
// this codebase's proration bugs have lived: the date math that produces the
// denominator (ADR-091's seam overbill, the Jan 31 → Feb 28 clamp), not
// prorationCents' integer arithmetic above.
//
// A period's stored anchor need not agree with its start: a swap rewrites the
// anchor, and a zone change reads the start on another day. So the anchor is
// any day 1 to 31, and the end anything up to the boundary the start and that
// anchor give. CycleDays is never shorter than the period, and is either its
// own length or one interval: 28 to 31 days, or 365 to 366 for yearly.
func TestProperty_CycleDays_SaneAcrossZonesAndAnchors(t *testing.T) {
	zones := []string{"UTC", "Asia/Kolkata", "America/New_York", "Pacific/Chatham", "Australia/Lord_Howe"}
	locs := make([]*time.Location, 0, len(zones))
	for _, z := range zones {
		loc, err := time.LoadLocation(z)
		if err != nil {
			t.Skipf("tzdata unavailable for %s: %v", z, err)
		}
		locs = append(locs, loc)
	}
	r := genRand()
	for i := 0; i < propIters; i++ {
		loc := locs[r.IntN(len(locs))]
		start := time.Date(2024, time.Month(r.IntN(12)+1), r.IntN(31)+1,
			r.IntN(24), r.IntN(60), 0, 0, loc).AddDate(r.IntN(10), 0, 0)
		anchor := r.IntN(31) + 1

		for _, iv := range []domain.BillingInterval{domain.BillingMonthly, domain.BillingYearly} {
			for _, bt := range []domain.SubscriptionBillingTime{domain.BillingTimeCalendar, domain.BillingTimeAnniversary} {
				boundary := domain.NextBillingPeriodEnd(start, bt, iv, loc, anchor)
				span := boundary.Sub(start)
				if span <= 0 {
					continue
				}
				end := start.Add(time.Duration(r.Int64N(int64(span))) + time.Minute)
				if end.After(boundary) {
					end = boundary
				}
				p := domain.CyclePeriod{Start: start, End: end, BillingTime: bt, Interval: iv, Loc: loc, StoredAnchor: anchor}
				days := int64(p.CycleDays())
				own := int64(math.Round(end.Sub(start).Hours() / 24))

				lo, hi := int64(28), int64(31)
				if iv == domain.BillingYearly {
					lo, hi = 365, 366
				}
				if days < own || (days != own && (days < lo || days > hi)) {
					t.Fatalf("CycleDays = %d: want the period's own %d days or %d..%d, and never less than its own; start=%s end=%s zone=%s billing=%s anchor=%d interval=%s",
						days, own, lo, hi, start.Format(time.RFC3339), end.Format(time.RFC3339), loc, bt, anchor, iv)
				}
			}
		}
	}
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}
