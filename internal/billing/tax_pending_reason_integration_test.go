package billing_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/invoice"
	"github.com/gethardcount/hardcount/internal/tax"
)

// failingTax fails every calculation with err.
type failingTax struct {
	tax.Provider
	err error
}

func (p failingTax) Calculate(context.Context, tax.Request) (*tax.Result, error) {
	return nil, p.err
}

// TestTaxDeferredClose_ReasonCutInsideACharacter_SavesTheDraft: Stripe Tax
// fails at the cycle close, and its error is long and not plain ASCII. The
// close saves the period's invoice as a draft with tax pending, and the
// error, cut before the character the cut would split, in tax_pending_reason.
// Postgres refuses text that is not valid UTF-8 (SQLSTATE 22021), so when the
// cut split the character the close failed on every retry, until the
// subscription was parked.
//
// Two cuts feed the column: tax.CleanMessage keeps 200 bytes of a plain
// error and adds "…", and the engine keeps 500 bytes of the message in a
// Stripe JSON envelope.
func TestTaxDeferredClose_ReasonCutInsideACharacter_SavesTheDraft(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{
			name: "plain error, 200-byte cut",
			err:  errors.New(strings.Repeat("a", 199) + "é and more" + strings.Repeat("b", 100)),
			want: strings.Repeat("a", 199) + "…",
		},
		{
			name: "JSON envelope, 500-byte cut",
			err:  errors.New(`stripe tax: {"message":"` + strings.Repeat("a", 499) + `é and more"}`),
			want: strings.Repeat("a", 499),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newBillFailureEnv(t, "Tax Reason "+tc.name)
			eng := env.newEngine(&subStoreAdapter{env.subs}, env.pricing)
			eng.SetTaxProviderResolver(fixedTaxResolver{failingTax{Provider: tax.NewNoneProvider(), err: tc.err}})
			sub := env.dueSub(t, "sub-tax-reason", env.planOK, env.periodEnd)

			if n, errs := eng.RunCycle(env.ctx, 50, nil); n != 1 || len(errs) > 0 {
				t.Fatalf("close: generated=%d errs=%v, want 1 invoice", n, errs)
			}
			invs, _, err := env.invoices.List(env.ctx, invoice.ListFilter{TenantID: env.tenantID, SubscriptionID: sub.ID})
			if err != nil || len(invs) != 1 {
				t.Fatalf("list invoices: %d invoices, err %v; want 1", len(invs), err)
			}
			inv, err := env.invoices.Get(env.ctx, env.tenantID, invs[0].ID)
			if err != nil {
				t.Fatalf("get invoice: %v", err)
			}
			if inv.Status != domain.InvoiceDraft || inv.TaxStatus != domain.InvoiceTaxPending {
				t.Errorf("invoice status/tax = %s/%s, want draft/pending", inv.Status, inv.TaxStatus)
			}
			if inv.TaxPendingReason != tc.want {
				t.Errorf("tax_pending_reason = %q, want %q", inv.TaxPendingReason, tc.want)
			}
		})
	}
}
