package tax

import (
	"context"
	"fmt"

	"github.com/gethardcount/hardcount/internal/domain"
)

// Resolver maps a tenant's configured tax_provider to a concrete Provider
// instance. Wired into the billing engine so per-tenant selection happens at
// invoice-build time without hard-coding provider construction into the
// engine itself.
type Resolver struct {
	stripe StripeClientResolver
}

// NewResolver returns a resolver that serves NoneProvider and ManualProvider
// unconditionally, plus StripeTaxProvider when stripeClients != nil. A nil
// stripeClients resolver does NOT silently fall back for tax_provider=
// 'stripe_tax' — Resolve returns an error (ADR-041 no-silent-fallback);
// operators wanting manual must select tax_provider=manual explicitly.
func NewResolver(stripeClients StripeClientResolver) *Resolver {
	return &Resolver{stripe: stripeClients}
}

// Resolve picks the right Provider for the tenant.
//
//   - 'none' / ” → NoneProvider (a legitimate "we don't collect tax" choice).
//   - 'manual'    → ManualProvider at the tenant's flat rate.
//   - 'stripe_tax' with a wired client → StripeTaxProvider; without one,
//     returns an error (defers the invoice per ADR-041, never substitutes
//     manual or zero).
//   - any OTHER string → error. tax_provider is constrained to the known set
//     by validateSettings at write time, so an unrecognized value means a
//     corrupted/hand-edited settings row. Failing loud is correct: a stuck
//     billing cycle is far more discoverable than silently emitting $0-tax
//     invoices (under-collection nobody notices until an audit).
func (r *Resolver) Resolve(_ context.Context, ts domain.TenantSettings) (Provider, error) {
	switch ts.TaxProvider {
	case "stripe_tax":
		if r.stripe == nil {
			return nil, fmt.Errorf("tax_provider=stripe_tax but no Stripe client wired — set tax_provider=manual or configure Stripe")
		}
		return NewStripeTaxProvider(r.stripe), nil
	case "manual":
		return NewManualProvider(ts.TaxRate, ts.TaxName), nil
	case "none", "":
		return NewNoneProvider(), nil
	default:
		return nil, fmt.Errorf("unrecognized tax_provider %q — expected one of none/manual/stripe_tax (settings row is corrupted)", ts.TaxProvider)
	}
}
