package billing_test

import (
	"context"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/billing"
	"github.com/gethardcount/hardcount/internal/credit"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/pricing"
	"github.com/gethardcount/hardcount/internal/subscription"
	"github.com/gethardcount/hardcount/internal/tax"
	"github.com/gethardcount/hardcount/internal/usage"
)

// A spend-threshold invoice tests credit grant expiry at its issued_at, the
// fire's instant (ADR-071 §2), not at the end of the window it bills. The
// window ends one invoice grace before the fire (ADR-120 amendment), so a
// grant that expires inside that grace was live for all the usage the invoice
// bills and still does not pay it: the card is charged. That is the chosen
// behaviour, written in ADR-071 and the CHANGELOG. Testing at the window's
// end instead would need the expiry sweep to hold such a grant until the fire
// (domain.CreditDrawOwedSQL has no term for an invoice not yet made).
func TestThresholdFire_DrawsCreditLiveAtTheFire(t *testing.T) {
	for _, tc := range []struct {
		name      string
		expiresIn time.Duration // from the scan
		wantDrawn int64
	}{
		{"a grant live at the fire pays the invoice", time.Hour, 6000},
		{"a grant that expired inside the grace before the fire does not", -30 * time.Minute, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newThresholdFixture(t, "Threshold Credit "+tc.name)
			ctx := postgres.WithLivemode(context.Background(), false)
			credits := credit.NewService(credit.NewPostgresStore(f.db))
			charger := &perInvoiceCharger{}
			eng := billing.NewEngine(
				f.subAdapter, &usageStoreAdapter{usage.NewPostgresStore(f.db)},
				&pricingStoreAdapter{pricing.NewPostgresStore(f.db)}, f.invAdapter,
				credits, f.settings, testPaymentSetupsReady{}, charger, nil,
			)
			eng.SetIntervalReader(f.subStore)
			eng.SetTaxProviderResolver(tax.NewResolver(nil))
			eng.SetNoPaymentMethodNotifier(&testNoPMNotifier{})
			eng.SetDunningStarter(testDunningStarter{})
			eng.SetStripeReadiness(testStripeConnected{})
			eng.SetInvoiceEmailer(&testInvoiceEmailer{})
			eng.SetDunningResolver(&testDunningResolver{})
			eng.SetInvoiceGrace(time.Hour)

			if _, err := f.subSvc.SetBillingThresholds(ctx, f.tenantID, f.subID, subscription.BillingThresholdsInput{AmountGTE: 5000}); err != nil {
				t.Fatalf("set thresholds: %v", err)
			}
			f.ingestUsage(t, ctx, 6, 1000) // $60 of usage, days ago

			future := time.Now().UTC().Add(24 * time.Hour)
			g, err := credits.Grant(ctx, f.tenantID, credit.GrantInput{
				Currency: "USD", CustomerID: f.customerID, AmountCents: 10000, Description: "prepaid", ExpiresAt: &future,
			})
			if err != nil {
				t.Fatalf("grant: %v", err)
			}
			expires := time.Now().UTC().Add(tc.expiresIn)
			tx, err := f.db.BeginTx(ctx, postgres.TxBypass, "")
			if err != nil {
				t.Fatalf("begin: %v", err)
			}
			if _, err := tx.ExecContext(ctx, `UPDATE customer_credit_ledger SET expires_at = $1 WHERE id = $2`, expires, g.ID); err != nil {
				t.Fatalf("set expires_at: %v", err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatalf("commit: %v", err)
			}

			if fired, errs := eng.ScanThresholds(ctx, 50); len(errs) > 0 || fired != 1 {
				t.Fatalf("threshold scan: fired=%d errs=%v, want 1 fire", fired, errs)
			}
			invs := f.listInvoices(t, ctx)
			if len(invs) != 1 {
				t.Fatalf("invoices = %d, want the one threshold invoice", len(invs))
			}
			fire := invs[0]
			if !fire.BillingPeriodEnd.Before(expires) && tc.expiresIn < 0 {
				t.Fatalf("setup: the grant must expire after the fire's window ends (%s), and it expires at %s", fire.BillingPeriodEnd, expires)
			}

			var consumed int64
			rtx, err := f.db.BeginTx(ctx, postgres.TxBypass, "")
			if err != nil {
				t.Fatalf("begin read: %v", err)
			}
			defer postgres.Rollback(rtx)
			if err := rtx.QueryRowContext(ctx, `SELECT consumed_cents FROM customer_credit_ledger WHERE id = $1`, g.ID).Scan(&consumed); err != nil {
				t.Fatalf("read consumed: %v", err)
			}
			if consumed != tc.wantDrawn {
				t.Fatalf("the grant paid %d of the threshold invoice, want %d", consumed, tc.wantDrawn)
			}
			charged := charger.charges(fire.ID)
			wantCharge := 6000 - tc.wantDrawn
			if wantCharge == 0 && len(charged) != 0 {
				t.Fatalf("card charges %v, want none: the grant paid the invoice", charged)
			}
			if wantCharge > 0 && (len(charged) != 1 || charged[0] != wantCharge) {
				t.Fatalf("card charges %v, want one of %d", charged, wantCharge)
			}
		})
	}
}
