package billing

import (
	"fmt"
	"math"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/money"
)

// The engine's money sums and products are checked against int64: a wrapped
// value would bill, credit or show a wrong amount. Each returns
// domain.ErrAmountOverflow instead. On an invoice path that refuses the
// invoice (nothing is written; a close is retried, ADR-119); on a read path
// it is the read's error. Inside int64 they return what the plain operators
// did, so no amount that fitted changes.

// addCents adds two cent amounts of any sign.
func addCents(a, b int64) (int64, error) {
	return domain.SumCents(a, b)
}

// subCents is a − b for cent amounts of any sign.
func subCents(a, b int64) (int64, error) {
	if b == math.MinInt64 {
		if a >= 0 {
			return 0, domain.ErrAmountOverflow
		}
		return a - b, nil
	}
	return addCents(a, -b)
}

// baseFeeCents is a plan's base fee for qty units: base × qty.
func baseFeeCents(plan domain.Plan, qty int64) (int64, error) {
	fee, ok := money.Mul(plan.BaseAmountCents, qty)
	if !ok {
		return 0, fmt.Errorf("base fee of plan %s × quantity %d: %w", plan.ID, qty, domain.ErrAmountOverflow)
	}
	return fee, nil
}

// prorateCents is fee × days / cycleDays rounded half to even
// (money.RoundHalfToEven's rounding), the product worked in 128 bits.
// cycleDays > 0.
func prorateCents(fee int64, days, cycleDays int) (int64, error) {
	v, ok := money.MulDivRoundHalfToEvenSigned(fee, int64(days), int64(cycleDays))
	if !ok {
		return 0, fmt.Errorf("prorate %d cents by %d/%d days: %w", fee, days, cycleDays, domain.ErrAmountOverflow)
	}
	return v, nil
}
