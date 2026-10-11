package subscription

import (
	"math"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/money"
)

// fullBillingCycleDays is the whole-day length of ONE full billing interval
// anchored at periodStart — the correct proration denominator.
//
// Proration divides the (new − old) period delta by the FULL cycle, never by
// the current period length. On a stub/partial period (mid-cycle signup) the
// current period is shorter than a cycle, so dividing by it over-charges
// upgrades / over-credits downgrades. Derived from the shared
// domain.AddBillingInterval so this matches the engine's day-1 stub base-fee
// proration (segDays/fullCycleDays) and the cross-interval swap refund exactly.
//
// loc is the tenant's billing timezone: the cycle advance is computed in loc
// (ADR-058) so the denominator is host-TZ-independent and agrees with the
// period boundaries the engine writes (which also advance in loc). Without it
// the same instant yields 30 or 31 days depending on the host time.Local —
// mischarging every mid-cycle plan change for an offset-TZ tenant.
func fullBillingCycleDays(periodStart time.Time, interval domain.BillingInterval, loc *time.Location, anchorDay int) int64 {
	end := domain.AddBillingInterval(periodStart, interval, loc, anchorDay)
	return int64(math.Round(end.Sub(periodStart).Hours() / 24))
}

// prorationCents computes the immediate plan-change proration amount, in cents,
// for an in_advance subscription item changed mid-period.
//
// It is the difference between the new and old whole-period charge, scaled by
// the integer day ratio remainingDays/totalDays and banker's-rounded:
//
//	round_half_to_even( (newAmount - oldAmount) * remainingDays / totalDays )
//
// oldAmount / newAmount are the already-quantity-multiplied period charges in
// cents (basePerUnit * quantity). A positive result is an additional charge
// (upgrade); a negative result is a credit (downgrade / quantity reduction).
//
// The math is deliberately PURE INTEGER (ADR-042). The pre-ADR-042 path used
// float64 `diff * (remaining/total)` then math.RoundToEven, which introduced
// ULP drift on large amounts — a $36M delta could land a cent off because the
// day-ratio is not exactly representable in float64. Staying in int64 with
// money.RoundHalfToEven makes the result exact for every input that doesn't
// overflow int64 (the numerator (newAmount-oldAmount)*remainingDays overflows
// only past ~$3 quadrillion of delta, ~8 orders of magnitude beyond any real
// invoice). The operator-facing ProrationFactor is derived separately as a
// display-only float64 and never feeds the cents.
//
// Multiply-before-divide is intentional: it keeps the single rounding step at
// the end, so the result matches an exact rational computed at full precision.
//
// Returns 0 when totalDays <= 0 (no period span to prorate over) — callers
// treat a 0 result as "no proration artifact".
func prorationCents(oldAmount, newAmount, remainingDays, totalDays int64) int64 {
	if totalDays <= 0 {
		return 0
	}
	return money.RoundHalfToEven((newAmount-oldAmount)*remainingDays, totalDays)
}

// splitUpgradeProration partitions a positive net upgrade proration into the
// two display lines the industry-standard invoice shows (ADR-048 Phase C,
// Stripe/Recurly/Chargebee/Orb convergence): a NEGATIVE credit for the unused
// time on the OLD plan and a POSITIVE charge for the remaining time on the NEW
// plan. It is a pure PARTITION of the already-computed net and its tax:
//
//	creditCents = -round(oldAmount × remaining ÷ denom)   // unused old, negative
//	chargeCents = netCents − creditCents                  // remaining new (residual)
//	creditTax   = round(taxCents × creditCents ÷ netCents) // tax reversed on the old slice, negative
//	chargeTax   = taxCents − creditTax                     // residual
//
// Deriving the charge half (and its tax) as the residual — not by independently
// rounding both halves — guarantees creditCents+chargeCents == netCents and
// creditTax+chargeTax == taxCents EXACTLY (int64), so the invoice
// subtotal/tax/total are unchanged by construction; the single rounded net is
// the source of truth and the split can never drift ±1 cent from it. Caller
// passes netCents > 0 (the proratedCents>0 upgrade branch); taxCents is the
// already-computed invoice tax on that net (0 for none/manual/deferred).
//
// Overflow: the tax-apportionment numerator taxCents×creditCents stays in int64
// up to ~$30M on a single proration line (a base-fee mid-cycle delta is orders
// below that — a much lower ceiling than prorationCents's ~$250T numerator, but
// still far beyond any real single-line base fee). Realistic invoices never
// approach it; flagged here for parity with prorationCents's overflow note.
func splitUpgradeProration(oldAmount, remainingDays, denomDays, netCents, taxCents int64) (creditCents, chargeCents, creditTax, chargeTax int64) {
	creditCents = -money.RoundHalfToEven(oldAmount*remainingDays, denomDays)
	chargeCents = netCents - creditCents
	if netCents != 0 {
		creditTax = money.RoundHalfToEven(taxCents*creditCents, netCents)
	}
	chargeTax = taxCents - creditTax
	return
}

// grossUpByInvoiceRatio scales a net (tax-exclusive) amount up to the gross
// (tax-inclusive) amount the customer actually paid, using the source
// invoice's own Total/Subtotal ratio. Identity when the invoice carried no tax
// (subtotalCents <= 0 — no provider or zero-rated). Mirrors the engine's
// creditUnusedPrebill gross-up (ADR-048) so the downgrade clawback credits the
// same gross the cancel/swap clawbacks do, and so the credit-note's own
// proportional-tax breakout (which uses the same invoice ratio) reverses
// exactly the tax slice on that gross.
func grossUpByInvoiceRatio(net, subtotalCents, totalCents int64) int64 {
	if subtotalCents <= 0 {
		return net
	}
	return money.RoundHalfToEven(net*totalCents, subtotalCents)
}
