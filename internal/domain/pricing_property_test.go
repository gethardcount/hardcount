package domain

import (
	"math/rand/v2"
	"testing"

	"github.com/shopspring/decimal"
)

// Property tests for usage pricing.
//
// ComputeAmountCents is the function that turns metered quantity into money.
// It has three modes (per_unit, graduated tiers, package), and the
// graduated path walks tier boundaries — historically the single most
// bug-prone shape in any billing engine, because the errors hide exactly at
// the boundary and nowhere else. An example test picks boundaries someone
// thought of; a generator walks every quantity across randomly-shaped ladders.
//
// The properties here are the ones a customer would notice being wrong:
// pricing that goes DOWN when you use more, a boundary that double-charges or
// skips a unit, and a total that is not the sum of its tiers.
//
// Seeded generator over math/rand/v2 rather than a new dependency: the inputs
// need structural constraints (ascending tier ceilings, an open last tier) that
// are clearer built directly, and a fixed seed makes failures reproducible.

const pricingIters = 5000

func pricingRand() *rand.Rand { return rand.New(rand.NewPCG(0xB111, 0x1237)) }

// genLadder builds a structurally VALID graduated ladder: zero to four
// bounded tiers with strictly ascending ceilings, then the open last tier
// (UpTo nil), which every ladder ends with.
func genLadder(r *rand.Rand) []RatingTier {
	n := r.IntN(5)
	tiers := make([]RatingTier, 0, n+1)
	upto := int64(0)
	for i := 0; i < n; i++ {
		upto += int64(r.IntN(500) + 1)
		tiers = append(tiers, RatingTier{
			UpTo:            new(upto),
			UnitAmountCents: decimal.NewFromInt(int64(r.IntN(500))),
		})
	}
	return append(tiers, RatingTier{UpTo: nil, UnitAmountCents: decimal.NewFromInt(int64(r.IntN(500)))})
}

// TestProperty_Pricing_MonotonicInQuantity is the property a customer would
// notice first: using MORE must never cost LESS. Every tier-boundary
// off-by-one produces a local dip or jump-back, so this catches boundary bugs
// without needing to know the correct price at any point.
func TestProperty_Pricing_MonotonicInQuantity(t *testing.T) {
	r := pricingRand()
	for i := 0; i < pricingIters; i++ {
		tiers := genLadder(r)
		rule := RatingRuleVersion{Mode: PricingGraduated, Tiers: tiers}

		q1 := int64(r.IntN(2000) + 1)
		q2 := int64(r.IntN(2000) + 1)
		if q1 > q2 {
			q1, q2 = q2, q1
		}

		a1, err1 := ComputeAmountCents(rule, decimal.NewFromInt(q1))
		a2, err2 := ComputeAmountCents(rule, decimal.NewFromInt(q2))
		if err1 != nil || err2 != nil {
			t.Fatalf("a valid ladder failed to rate: %v, %v\ntiers=%+v", err1, err2, tiers)
		}
		if a1 > a2 {
			t.Fatalf("using more cost less: q=%d -> %dc but q=%d -> %dc\ntiers=%+v", q1, a1, q2, a2, tiers)
		}
	}
}

// TestProperty_Pricing_ZeroQuantityIsFree: no usage, no charge. A tier walk
// that charges a minimum for zero would silently bill every idle customer.
func TestProperty_Pricing_ZeroQuantityIsFree(t *testing.T) {
	r := pricingRand()
	for i := 0; i < pricingIters; i++ {
		var rule RatingRuleVersion
		switch r.IntN(3) {
		case 0:
			rule = RatingRuleVersion{Mode: PricingPerUnit, UnitAmountCents: decimal.NewFromInt(int64(r.IntN(10000)))}
		case 1:
			rule = RatingRuleVersion{Mode: PricingGraduated, Tiers: genLadder(r)}
		default:
			rule = RatingRuleVersion{
				Mode: PricingPackage, PackageSize: int64(r.IntN(100) + 1),
				PackageAmountCents: decimal.NewFromInt(int64(r.IntN(10000))),
			}
		}
		got, err := ComputeAmountCents(rule, decimal.Zero)
		if err != nil {
			t.Fatalf("a valid rule failed to rate 0: %v rule=%+v", err, rule)
		}
		if got != 0 {
			t.Fatalf("zero quantity charged %dc: mode=%s rule=%+v", got, rule.Mode, rule)
		}
	}
}

// TestProperty_Pricing_NeverNegative: no quantity may produce a credit. A
// negative line item on an invoice reads as a refund the customer did not earn.
func TestProperty_Pricing_NeverNegative(t *testing.T) {
	r := pricingRand()
	for i := 0; i < pricingIters; i++ {
		tiers := genLadder(r)
		rule := RatingRuleVersion{Mode: PricingGraduated, Tiers: tiers}
		q := int64(r.IntN(2000) + 1)
		got, err := ComputeAmountCents(rule, decimal.NewFromInt(q))
		if err != nil {
			continue
		}
		if got < 0 {
			t.Fatalf("negative charge: q=%d -> %dc tiers=%+v", q, got, tiers)
		}
	}
}

// TestProperty_Pricing_GraduatedIsSumOfItsTiers independently recomputes the
// ladder walk and requires the implementation to agree. This is a differential
// oracle: the reference is written from the DEFINITION of graduated pricing
// (each tier prices only the units that fall inside it), so agreement means the
// implementation matches the definition rather than matching itself.
//
// Restricted to whole-unit quantities and integer rates so the reference needs
// no rounding of its own — otherwise the two would differ on rounding rather
// than on the tier walk, which is the thing under test.
func TestProperty_Pricing_GraduatedIsSumOfItsTiers(t *testing.T) {
	r := pricingRand()
	for i := 0; i < pricingIters; i++ {
		tiers := genLadder(r)
		rule := RatingRuleVersion{Mode: PricingGraduated, Tiers: tiers}
		q := int64(r.IntN(2000) + 1)

		got, err := ComputeAmountCents(rule, decimal.NewFromInt(q))
		if err != nil {
			continue
		}

		// Reference walk, straight from the definition.
		var want, lastUpper, remaining int64 = 0, 0, q
		for _, tier := range tiers {
			if remaining <= 0 {
				break
			}
			rate := tier.UnitAmountCents.IntPart()
			if tier.UpTo == nil { // the open last tier
				want += remaining * rate
				break
			}
			capacity := *tier.UpTo - lastUpper
			take := remaining
			if take > capacity {
				take = capacity
			}
			want += take * rate
			remaining -= take
			lastUpper = *tier.UpTo
		}
		if got != want {
			t.Fatalf("graduated total disagrees with an independent tier walk: q=%d got=%dc want=%dc\ntiers=%+v",
				q, got, want, tiers)
		}
	}
}

// TestProperty_Pricing_PackageChargesWholePackages checks the package model
// against its definition (ADR-130 §3): ceil(q/size) packages at the package
// price, so a part package bills as a whole one. Ceil makes the price never
// go down as usage grows, which the threshold ladder relies on.
func TestProperty_Pricing_PackageChargesWholePackages(t *testing.T) {
	r := pricingRand()
	for i := 0; i < pricingIters; i++ {
		size := int64(r.IntN(100) + 1)
		pkgCents := int64(r.IntN(10000))
		rule := RatingRuleVersion{
			Mode: PricingPackage, PackageSize: size,
			PackageAmountCents: decimal.NewFromInt(pkgCents),
		}
		q := int64(r.IntN(5000))
		got, err := ComputeAmountCents(rule, decimal.NewFromInt(q))
		if err != nil {
			t.Fatalf("q=%d size=%d: %v", q, size, err)
		}
		want := (q + size - 1) / size * pkgCents
		if got != want {
			t.Fatalf("package pricing disagrees with definition: q=%d size=%d pkg=%dc got=%dc want=%dc",
				q, size, pkgCents, got, want)
		}
		more, err := ComputeAmountCents(rule, decimal.NewFromInt(q+int64(r.IntN(50))))
		if err != nil || more < got {
			t.Fatalf("using more cost less: q=%d -> %dc, more -> %dc (%v)", q, got, more, err)
		}
	}
}

// TestProperty_Pricing_PackageCountsAnyPart: a quantity of k whole packages
// plus any part, down to 10^-12 of a unit (the finest a quantity holds),
// bills k+1 packages, at any package size. The count is exact: it does not
// depend on how finely a quotient is rounded.
func TestProperty_Pricing_PackageCountsAnyPart(t *testing.T) {
	r := pricingRand()
	tiny := decimal.New(1, -12)
	for i := 0; i < pricingIters; i++ {
		size := r.Int64N(1_000_000_000_000) + 1
		pkgCents := int64(r.IntN(10000))
		rule := RatingRuleVersion{
			Mode: PricingPackage, PackageSize: size,
			PackageAmountCents: decimal.NewFromInt(pkgCents),
		}
		k := int64(r.IntN(1000))
		// A part from 10^-12 up to just under one package.
		part := tiny.Mul(decimal.NewFromInt(r.Int64N(1_000_000) + 1))
		if lim := decimal.NewFromInt(size); part.GreaterThanOrEqual(lim) {
			part = lim.Sub(tiny)
		}
		q := decimal.NewFromInt(k * size).Add(part)
		got, err := ComputeAmountCents(rule, q)
		if err != nil {
			t.Fatalf("q=%s size=%d: %v", q, size, err)
		}
		if want := (k + 1) * pkgCents; got != want {
			t.Fatalf("a part package was not billed: q=%s size=%d pkg=%dc got=%dc want=%dc",
				q, size, pkgCents, got, want)
		}
	}
}

// TestProperty_Pricing_RejectsNegativeQuantity: metered usage cannot be
// negative, and a silently-accepted negative quantity would emit a credit line.
func TestProperty_Pricing_RejectsNegativeQuantity(t *testing.T) {
	r := pricingRand()
	for i := 0; i < 500; i++ {
		rule := RatingRuleVersion{Mode: PricingGraduated, Tiers: genLadder(r)}
		q := decimal.NewFromInt(-int64(r.IntN(1000) + 1))
		if _, err := ComputeAmountCents(rule, q); err == nil {
			t.Fatalf("negative quantity %s was accepted", q)
		}
	}
}

// TestProperty_Pricing_RejectsOpenTierBeforeEnd: an open tier
// (`up_to: null`) prices all remaining quantity, so a tier after it is dead
// config that would never price anything. Only the last tier is open.
func TestProperty_Pricing_RejectsOpenTierBeforeEnd(t *testing.T) {
	r := pricingRand()
	for i := 0; i < 500; i++ {
		tiers := genLadder(r)
		bad := append([]RatingTier{{UpTo: nil, UnitAmountCents: decimal.NewFromInt(5)}}, tiers...)
		rule := RatingRuleVersion{Mode: PricingGraduated, Tiers: bad}
		if _, err := ComputeAmountCents(rule, decimal.NewFromInt(int64(r.IntN(100)+1))); err == nil {
			t.Fatalf("an open tier before the end was accepted: %+v", bad)
		}
	}
}

// TestProperty_Pricing_RejectsBoundedLastTier: the last tier must be open. A
// bounded one leaves the usage above it with no price, so even a quantity
// inside the ladder is refused: the table itself is invalid.
func TestProperty_Pricing_RejectsBoundedLastTier(t *testing.T) {
	r := pricingRand()
	for i := 0; i < 500; i++ {
		tiers := genLadder(r)
		last := int64(1)
		if len(tiers) > 1 {
			last = *tiers[len(tiers)-2].UpTo + 1
		}
		tiers[len(tiers)-1].UpTo = new(last + int64(r.IntN(100)))
		rule := RatingRuleVersion{Mode: PricingGraduated, Tiers: tiers}
		if _, err := ComputeAmountCents(rule, decimal.NewFromInt(1)); err == nil {
			t.Fatalf("a bounded last tier was accepted: %+v", tiers)
		}
	}
}
