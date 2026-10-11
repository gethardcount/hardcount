package tax

import "context"

type calcScopeKey struct{}

// WithCalculationScope names the billing step a tax calculation belongs to —
// e.g. one subscription's cycle close for one period. Stripe Tax uses it, with
// the request itself, as the calculation's idempotency key, so retrying a
// failed step with the same lines does not pay for a second calculation.
// Unscoped calculations (a tax retry, which must mint a fresh calculation once
// the old one expires) get no key.
func WithCalculationScope(ctx context.Context, scope string) context.Context {
	return context.WithValue(ctx, calcScopeKey{}, scope)
}

func calculationScope(ctx context.Context) string {
	s, _ := ctx.Value(calcScopeKey{}).(string)
	return s
}
