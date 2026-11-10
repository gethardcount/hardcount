package billing

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stripe/stripe-go/v82"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/tax"
)

// taggedProvider records the commits and reversals it receives, under the
// tax_provider setting it was resolved for.
type taggedProvider struct {
	stubProvider
	setting string
	calls   *[]string
}

func (p *taggedProvider) Commit(context.Context, string, string) (string, error) {
	*p.calls = append(*p.calls, "commit at "+p.setting)
	return "", nil
}

func (p *taggedProvider) Reverse(context.Context, tax.ReversalRequest) (*tax.ReversalResult, error) {
	*p.calls = append(*p.calls, "reverse at "+p.setting)
	return &tax.ReversalResult{TransactionID: "tax_rev"}, nil
}

// taggingResolver hands out a provider tagged with the setting it resolved.
type taggingResolver struct{ calls []string }

func (r *taggingResolver) Resolve(_ context.Context, ts domain.TenantSettings) (tax.Provider, error) {
	return &taggedProvider{setting: ts.TaxProvider, calls: &r.calls}, nil
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
		}, "reverse at stripe_tax"},
		{"commit of a Stripe calculation", func(e *Engine) error {
			return e.CommitTax(context.Background(), "t1", "inv_1", "taxcalc_123")
		}, "commit at stripe_tax"},
		{"a reversal with no upstream transaction keeps the tenant's provider", func(e *Engine) error {
			_, err := e.ReverseTax(context.Background(), "t1", tax.ReversalRequest{Reference: "inv_void_2"})
			return err
		}, "reverse at manual"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := &taggingResolver{}
			e := &Engine{
				settings:     &taxSettings{provider: "manual"}, // switched away from Stripe Tax
				taxProviders: res,
				taxCalcStore: &fakeCalcStore{createdAt: time.Now().UTC()},
			}
			if err := tc.call(e); err != nil {
				t.Fatalf("call: %v", err)
			}
			if len(res.calls) != 1 || res.calls[0] != tc.want {
				t.Fatalf("provider calls = %v, want [%s]", res.calls, tc.want)
			}
		})
	}
}

// noStripeClients is a Stripe client resolver for a tenant with no Stripe
// credentials connected.
type noStripeClients struct{}

func (noStripeClients) ResolveCtx(context.Context) (*stripe.Client, error) { return nil, nil }

// Through the real resolver and the real Stripe Tax provider: a tenant on the
// manual provider with Stripe disconnected. A reversal of a Stripe transaction
// and a commit of a Stripe calculation must fail, so their sweeps keep them.
// Resolved from the tenant's setting, both answered "nothing to do" with no
// error, and the document was marked done.
func TestTaxDocument_StripeDisconnected_FailsInsteadOfDoingNothing(t *testing.T) {
	e := &Engine{
		settings:     &taxSettings{provider: "manual"},
		taxProviders: tax.NewResolver(noStripeClients{}),
	}
	res, err := e.ReverseTax(context.Background(), "t1", tax.ReversalRequest{OriginalTransactionID: "tax_123", Reference: "inv_void_1"})
	if err == nil || !strings.Contains(err.Error(), "no Stripe credentials") {
		t.Fatalf("ReverseTax = (%+v, %v), want the no-Stripe-credentials error", res, err)
	}
	if err := e.CommitTax(context.Background(), "t1", "inv_1", "taxcalc_123"); err == nil || !strings.Contains(err.Error(), "no Stripe credentials") {
		t.Fatalf("CommitTax = %v, want the no-Stripe-credentials error", err)
	}
}
