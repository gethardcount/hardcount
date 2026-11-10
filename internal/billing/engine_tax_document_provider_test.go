package billing

import (
	"context"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/tax"
)

// recordingResolver records the tax_provider it was asked to resolve.
type recordingResolver struct{ asked []string }

func (r *recordingResolver) Resolve(_ context.Context, ts domain.TenantSettings) (tax.Provider, error) {
	r.asked = append(r.asked, ts.TaxProvider)
	return &stubProvider{}, nil
}

// A document that carries a Stripe Tax reference is committed and reversed at
// Stripe Tax, whatever provider the tenant has selected since. The tenant here
// switched to manual after the invoice was taxed by Stripe: resolved from the
// current setting, the reversal went to the manual provider, which answers
// "nothing to reverse" with no error, and the void stamped the invoice
// reversed with the sale still on Stripe's report.
func TestTaxDocument_GoesToTheProviderThatMadeIt(t *testing.T) {
	for _, tc := range []struct {
		name string
		call func(e *Engine) error
		want string
	}{
		{"reversal of a Stripe transaction", func(e *Engine) error {
			_, err := e.ReverseTax(context.Background(), "t1", tax.ReversalRequest{OriginalTransactionID: "tax_123", Reference: "inv_void_1"})
			return err
		}, "stripe_tax"},
		{"commit of a Stripe calculation", func(e *Engine) error {
			return e.CommitTax(context.Background(), "t1", "inv_1", "taxcalc_123")
		}, "stripe_tax"},
		{"the commit's expiry guard", func(e *Engine) error {
			return e.CheckTaxCalculationUsable(context.Background(), "t1", "inv_1", "taxcalc_123")
		}, "stripe_tax"},
		{"a reversal with no upstream transaction keeps the tenant's provider", func(e *Engine) error {
			_, err := e.ReverseTax(context.Background(), "t1", tax.ReversalRequest{Reference: "inv_void_2"})
			return err
		}, "manual"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := &recordingResolver{}
			e := &Engine{
				settings:     &taxSettings{provider: "manual"}, // switched away from Stripe Tax
				taxProviders: res,
				taxCalcStore: &fakeCalcStore{createdAt: time.Now().UTC()},
			}
			if err := tc.call(e); err != nil {
				t.Fatalf("call: %v", err)
			}
			if len(res.asked) == 0 {
				t.Fatal("the provider was never resolved")
			}
			for _, got := range res.asked {
				if got != tc.want {
					t.Fatalf("resolved provider %q, want %q", got, tc.want)
				}
			}
		})
	}
}
