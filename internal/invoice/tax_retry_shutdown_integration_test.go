package invoice

import (
	"context"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/pricing"
	"github.com/sagarsuperuser/velox/internal/subscription"
)

// G7: a SIGTERM or a lost lease cancels the tax_retry sweep's tick ctx
// (main.go's signal.NotifyContext). When it lands after RetryTax's tax write
// commits and before its finalize does, the row is an engine draft with tax
// ok. Every tax sweep lists only pending or failed tax, so nothing ever
// finalizes it: the period is never issued or collected. These tests inject
// that exact gap against real Postgres and the real subscription store.

// cancellingTaxRetrier lets the tax write commit, then cancels the caller's
// ctx: the shutdown landing between RetryTax's two writes.
type cancellingTaxRetrier struct {
	inner  *resolvingTaxRetrier
	cancel context.CancelFunc
}

func (f *cancellingTaxRetrier) RetryTaxForInvoice(ctx context.Context, tenantID, invoiceID string) (domain.Invoice, error) {
	inv, err := f.inner.RetryTaxForInvoice(ctx, tenantID, invoiceID)
	f.cancel()
	return inv, err
}

func (f *cancellingTaxRetrier) ComputeTaxForInvoice(ctx context.Context, tenantID, invoiceID string) (domain.Invoice, error) {
	return f.inner.ComputeTaxForInvoice(ctx, tenantID, invoiceID)
}

// subTaxPendingDraft is taxPendingDraft on a real subscription, wired as the
// pause reader the way router.go wires it. paused puts keep_as_draft in force.
func (r *snapRig) subTaxPendingDraft(t *testing.T, num string, paused bool) string {
	t.Helper()
	plan, err := pricing.NewPostgresStore(r.db).CreatePlan(r.ctx, r.tenantID, domain.Plan{
		Code: "g7-plan-" + num, Name: "G7", Currency: "EUR",
		BillingInterval: domain.BillingMonthly, Status: domain.PlanActive, BaseAmountCents: 5000,
	})
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}
	subs := subscription.NewPostgresStore(r.db)
	ps := time.Now().UTC().AddDate(0, -1, 0)
	pe := ps.AddDate(0, 1, 0)
	sub, err := subs.Create(r.ctx, r.tenantID, domain.Subscription{
		Code: "g7-sub-" + num, DisplayName: "G7 Sub", CustomerID: r.custID,
		Status: domain.SubscriptionActive, BillingTime: domain.BillingTimeCalendar,
		StartedAt: &ps, Items: []domain.SubscriptionItem{{PlanID: plan.ID, Quantity: 1}},
		CurrentBillingPeriodStart: &ps, CurrentBillingPeriodEnd: &pe,
	})
	if err != nil {
		t.Fatalf("create subscription: %v", err)
	}
	if paused {
		if _, err := subs.SetPauseCollection(r.ctx, r.tenantID, sub.ID, domain.PauseCollection{Behavior: domain.PauseCollectionKeepAsDraft}); err != nil {
			t.Fatalf("pause collection: %v", err)
		}
	}
	p := r.protoInvoice(num, domain.InvoiceDraft, domain.BillingReasonSubscriptionCycle)
	p.SubscriptionID = sub.ID
	p.TaxStatus = domain.InvoiceTaxPending
	p.TaxErrorCode = "provider_outage"
	d, err := r.store.CreateWithLineItems(r.ctx, r.tenantID, p, nil)
	if err != nil {
		t.Fatalf("create tax-pending draft: %v", err)
	}
	r.svc.SetSubscriptionPauseReader(subs)
	return d.ID
}

func TestRetryTax_CancelAfterTaxCommit(t *testing.T) {
	cases := []struct {
		name string
		// seed returns the draft's id.
		seed func(t *testing.T, r *snapRig) string
		// wantFinalized: false only when keep_as_draft is in force.
		wantFinalized bool
	}{
		// T1: the finalize half. No subscription, so no pause read; the
		// cancelled ctx reaches finalize's first read.
		{"no subscription: finalize still runs", func(t *testing.T, r *snapRig) string {
			return r.taxPendingDraft(t, "G7-T1")
		}, true},
		// T1b: the pause-read half. The real store's read fails on a done ctx.
		{"running subscription: the pause read and finalize still run", func(t *testing.T, r *snapRig) string {
			return r.subTaxPendingDraft(t, "G7-T1B", false)
		}, true},
		// T2, negative control: keep_as_draft is legal. The row stays a draft
		// and RetryTax does not report it as an error.
		{"paused subscription: stays a draft, no error", func(t *testing.T, r *snapRig) string {
			return r.subTaxPendingDraft(t, "G7-T2", true)
		}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newSnapRig(t, "G7 Shutdown Corp")
			id := c.seed(t, r)

			ctx, cancel := context.WithCancel(r.ctx)
			defer cancel()
			r.svc.SetTaxRetrier(&cancellingTaxRetrier{
				inner:  &resolvingTaxRetrier{db: r.db, store: r.store},
				cancel: cancel,
			})

			if _, err := r.svc.RetryTax(ctx, r.tenantID, id); err != nil {
				t.Fatalf("RetryTax: %v", err)
			}

			got, err := r.store.Get(r.ctx, r.tenantID, id)
			if err != nil {
				t.Fatalf("get: %v", err)
			}
			if got.TaxStatus != domain.InvoiceTaxOK {
				t.Fatalf("tax_status = %s, want ok (the injected tax write did not commit)", got.TaxStatus)
			}
			if !c.wantFinalized {
				if got.Status != domain.InvoiceDraft || got.AutoChargePending {
					t.Errorf("status = %s, auto_charge_pending = %v; want a draft, not queued (keep_as_draft)", got.Status, got.AutoChargePending)
				}
				return
			}
			if got.Status != domain.InvoiceFinalized {
				t.Fatalf("status = %s, want finalized: a draft with tax ok is listed by no sweep, so this period is never issued", got.Status)
			}
			if !got.AutoChargePending {
				t.Error("auto_charge_pending = false, want true: no sweep would collect it")
			}
		})
	}
}
