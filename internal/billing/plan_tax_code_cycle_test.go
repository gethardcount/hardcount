package billing

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/tax"
)

// codeRecordingProvider is the manual provider, recording the tax code of
// each line it is asked to tax: the code Stripe Tax would price the line by.
type codeRecordingProvider struct {
	tax.Provider
	mu    sync.Mutex
	codes []string
}

func (p *codeRecordingProvider) Calculate(ctx context.Context, req tax.Request) (*tax.Result, error) {
	p.mu.Lock()
	for _, li := range req.LineItems {
		p.codes = append(p.codes, li.TaxCode)
	}
	p.mu.Unlock()
	return p.Provider.Calculate(ctx, req)
}

// TestCycleClose_BaseLineCarriesPlanTaxCode: a plan's tax_code is the
// product tax code of its base fee. The first in-advance invoice sent it, but
// the cycle close sent the base line with no code, so Stripe Tax priced every
// later period's base fee by the account's default_product_tax_code. A plan
// coded nontaxable (txcd_00000000), or taxed differently from the default,
// got the wrong tax from its second invoice on.
//
// Mutation: drop TaxCode from emitBaseSegmentLine and the in_arrears case
// fails; drop it from closeBaseFeeLines' in_advance line and that case fails.
func TestCycleClose_BaseLineCarriesPlanTaxCode(t *testing.T) {
	const planCode = "txcd_00000000"
	for _, timing := range []domain.BillTiming{domain.BillInArrears, domain.BillInAdvance} {
		t.Run(string(timing), func(t *testing.T) {
			periodStart := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
			periodEnd := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
			nextBilling := periodEnd
			subs := &mockSubs{
				subs: map[string]domain.Subscription{
					"sub_1": {
						ID: "sub_1", TenantID: "t1", CustomerID: "cus_1",
						Items:  []domain.SubscriptionItem{{ID: "si_1", PlanID: "pln_1", Quantity: 1}},
						Status: domain.SubscriptionActive, BillingTime: domain.BillingTimeCalendar,
						CurrentBillingPeriodStart: &periodStart, CurrentBillingPeriodEnd: &periodEnd,
						NextBillingAt: &nextBilling,
					},
				},
				cycleUpdated: make(map[string]bool),
			}
			pricing := &mockPricing{plans: map[string]domain.Plan{
				"pln_1": {
					ID: "pln_1", Name: "Pro Plan", Currency: "USD", BillingInterval: domain.BillingMonthly,
					BaseAmountCents: 4900, BaseBillTiming: timing, TaxCode: planCode,
				},
			}}
			invoices := &mockInvoices{}
			engine := NewEngine(subs, &mockUsage{}, pricing, invoices, nil, &mockSettings{}, nil, nil,
				clock.NewFake(periodEnd.Add(time.Nanosecond)))
			wireBaseTax(engine)
			rec := &codeRecordingProvider{Provider: tax.NewManualProvider(10, "VAT")}
			engine.SetTaxProviderResolver(stubResolver(rec))

			if _, errs := engine.RunCycle(postgres.WithLivemode(context.Background(), true), 50, nil); len(errs) > 0 {
				t.Fatalf("RunCycle: %v", errs)
			}
			var bases int
			for _, li := range invoices.lineItems {
				if li.LineType == domain.LineTypeBaseFee {
					bases++
				}
			}
			// The plan has no usage prices, so every line taxed is a base line.
			if bases == 0 || len(rec.codes) != bases {
				t.Fatalf("base lines billed = %d, lines taxed = %d (%v), want the same, at least one", bases, len(rec.codes), rec.codes)
			}
			for _, c := range rec.codes {
				if c != planCode {
					t.Errorf("tax request line code = %q, want the plan's %q (codes sent: %v)", c, planCode, rec.codes)
				}
			}
		})
	}
}
