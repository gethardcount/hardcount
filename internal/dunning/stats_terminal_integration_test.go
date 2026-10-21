package dunning_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/dunning"
	"github.com/sagarsuperuser/velox/internal/invoice"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// TestGetStats_RecoveredAndAtRiskLeaveOutClosedInvoices covers sweep A6/E7:
// the Dunning page's "Recovered" card counted every resolved run, so a run
// closed by a void or a write-off read as money recovered, and "At risk"
// summed amount_due on runs whose invoice was already voided or written off
// (a void keeps its amount_due; a write-off final action leaves the run
// escalated).
//
// Seeded world, all USD:
//
//	active    on an open invoice           $100  at risk
//	escalated on an open invoice           $200  at risk (final action: none)
//	escalated on a written-off invoice     $400  not at risk
//	active    on a voided invoice          $800  not at risk (a cancel's void, sweep A1)
//	resolved  payment_recovered, paid      $0    recovered
//	resolved  invoice_voided               $16   resolved, not recovered
//	resolved  invoice_not_collectible      $32   resolved, not recovered
//
// Mutation checks (each makes this test fail):
//   - recovered_count counting state = 'resolved' alone → 3
//   - the at-risk filter without i.status = 'finalized' → 150000 cents in both sums
func TestGetStats_RecoveredAndAtRiskLeaveOutClosedInvoices(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)

	tenantID := testutil.CreateTestTenant(t, db, "Dunning Stats Terminal")
	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{
		ExternalID: "cus_dun_term", DisplayName: "Dun Term",
	})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}

	store := dunning.NewPostgresStore(db)
	policy, err := store.UpsertPolicy(ctx, tenantID, domain.DunningPolicy{
		Name: "default", Enabled: true,
		RetrySchedule: []string{"72h"}, MaxRetryAttempts: 1,
		FinalSubscriptionAction: domain.SubActionNone, FinalInvoiceAction: domain.InvActionMarkUncollectible, GracePeriodDays: 3,
	})
	if err != nil {
		t.Fatalf("upsert policy: %v", err)
	}

	invStore := invoice.NewPostgresStore(db)
	n := 0
	mk := func(status domain.InvoiceStatus, payment domain.InvoicePaymentStatus, due int64, state domain.DunningRunState, resolution domain.DunningResolution) {
		t.Helper()
		n++
		now := time.Now().UTC()
		issued := now
		inv, err := invStore.Create(ctx, tenantID, domain.Invoice{
			CustomerID: cust.ID, InvoiceNumber: "INV-TERM-" + string(rune('A'+n)),
			Status: status, PaymentStatus: payment,
			Currency: "USD", SubtotalCents: due, TotalAmountCents: due, AmountDueCents: due,
			BillingPeriodStart: now.Add(-time.Hour), BillingPeriodEnd: now, IssuedAt: &issued,
		})
		if err != nil {
			t.Fatalf("create invoice %d: %v", n, err)
		}
		run, err := store.CreateRun(ctx, tenantID, domain.InvoiceDunningRun{
			InvoiceID: inv.ID, CustomerID: cust.ID, PolicyID: policy.ID,
			State: domain.DunningActive, Reason: "payment_failed",
		})
		if err != nil {
			t.Fatalf("create run %d: %v", n, err)
		}
		if state == domain.DunningActive {
			return
		}
		run.State = state
		run.Resolution = resolution
		run.ResolvedAt = &now
		if ok, err := store.ResolveRun(ctx, tenantID, run, false); err != nil || !ok {
			t.Fatalf("move run %d to %s: ok=%v err=%v", n, state, ok, err)
		}
	}
	mk(domain.InvoiceFinalized, domain.PaymentFailed, 100_00, domain.DunningActive, "")
	mk(domain.InvoiceFinalized, domain.PaymentFailed, 200_00, domain.DunningEscalated, domain.ResolutionRetriesExhausted)
	mk(domain.InvoiceUncollectible, domain.PaymentFailed, 400_00, domain.DunningEscalated, domain.ResolutionRetriesExhausted)
	mk(domain.InvoiceVoided, domain.PaymentFailed, 800_00, domain.DunningActive, "")
	mk(domain.InvoicePaid, domain.PaymentSucceeded, 0, domain.DunningResolved, domain.ResolutionPaymentRecovered)
	mk(domain.InvoiceVoided, domain.PaymentFailed, 16_00, domain.DunningResolved, domain.ResolutionInvoiceVoided)
	mk(domain.InvoiceUncollectible, domain.PaymentFailed, 32_00, domain.DunningResolved, domain.ResolutionInvoiceNotCollectible)

	stats, err := store.GetStats(ctx, tenantID)
	if err != nil {
		t.Fatalf("get stats: %v", err)
	}
	// The dashboard reads the JSON, so the contract is the key it reads.
	raw, err := json.Marshal(stats)
	if err != nil {
		t.Fatalf("marshal stats: %v", err)
	}
	var wire map[string]any
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatalf("unmarshal stats: %v", err)
	}
	if got, ok := wire["recovered_count"].(float64); !ok || got != 1 {
		t.Errorf("recovered_count: got %v, want 1 (only the run a payment closed; a void or write-off is not a recovery)", wire["recovered_count"])
	}
	if stats.ResolvedCount != 3 {
		t.Errorf("resolved_count: got %d, want 3 (every resolved run, whatever closed it)", stats.ResolvedCount)
	}
	if stats.ActiveCount != 2 || stats.EscalatedCount != 2 {
		t.Errorf("active/escalated: got %d/%d, want 2/2 (run counts are by run state)", stats.ActiveCount, stats.EscalatedCount)
	}
	if stats.AtRiskCents != 300_00 {
		t.Errorf("at_risk_cents: got %d, want 30000 (open invoices only; the void's and the write-off's amount_due is not owed)", stats.AtRiskCents)
	}
	if len(stats.AtRiskByCurrency) != 1 || stats.AtRiskByCurrency[0] != (dunning.CurrencyAmount{Currency: "USD", AmountCents: 300_00}) {
		t.Errorf("at_risk_by_currency: got %+v, want [USD 30000]", stats.AtRiskByCurrency)
	}
}
