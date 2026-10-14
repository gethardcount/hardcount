package billing_test

import (
	"context"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/clock"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/subscription"
)

// TestEngineIssuedInvoices_CarryTheIssuedDocumentCopy (ADR-109, #964) runs
// the billing engine's own issue paths end to end against Postgres and checks
// that each invoice it issues carries the bill-to and supplier copy, with the
// details in force at that moment. The store-level writer test
// (internal/invoice TestIssuedInvoice_EveryFinalizeWriterStamps) covers each
// SQL site; this one proves the engine reaches them with the right customer
// under the right tenant and mode.
func TestEngineIssuedInvoices_CarryTheIssuedDocumentCopy(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipped in -short mode")
	}
	ctx, cancel := context.WithTimeout(postgres.WithLivemode(context.Background(), false), 60*time.Second)
	defer cancel()

	checkCopy := func(t *testing.T, label string, inv domain.Invoice) {
		t.Helper()
		if inv.Status == domain.InvoiceDraft {
			t.Fatalf("%s: invoice %s is a draft; this test needs an issued one", label, inv.ID)
		}
		if inv.BillToSnapshot == nil || inv.BillToSnapshot.Name() != "Engine Buyer GmbH" || inv.BillToSnapshot.AddressLine1 != "Engine Strasse 1" {
			t.Errorf("%s: bill-to copy = %+v, want the profile in force at issue", label, inv.BillToSnapshot)
		}
		if inv.SupplierSnapshot == nil || inv.SupplierSnapshot.Name != "Engine Seller Ltd" {
			t.Errorf("%s: supplier copy = %+v, want company Engine Seller Ltd", label, inv.SupplierSnapshot)
		}
	}

	t.Run("usage threshold", func(t *testing.T) {
		f := newSnapshotEngineFixture(t, ctx, "Engine Snapshot Threshold")
		f.ingestUsage(t, ctx, 100, 10)
		if _, err := f.subSvc.SetBillingThresholds(ctx, f.tenantID, f.subID, subscription.BillingThresholdsInput{AmountGTE: 500}); err != nil {
			t.Fatalf("set threshold: %v", err)
		}
		if fired, errs := f.engine.ScanThresholds(ctx, 50); len(errs) > 0 || fired != 1 {
			t.Fatalf("threshold scan: fired=%d errs=%v, want 1 and none", fired, errs)
		}
		invs := f.listInvoices(t, ctx)
		if len(invs) != 1 {
			t.Fatalf("invoices after threshold = %d, want 1", len(invs))
		}
		checkCopy(t, "threshold invoice", invs[0])
	})

	t.Run("cycle close", func(t *testing.T) {
		f := newSnapshotEngineFixture(t, ctx, "Engine Snapshot Cycle")
		f.ingestUsage(t, ctx, 10, 10)
		before := len(f.listInvoices(t, ctx))
		at := clock.WithEffectiveNow(ctx, f.cycleEnd.Add(time.Hour))
		if gen, failures := f.engine.RunCycleForTenant(at, f.tenantID, 50); gen != 1 || len(failures) != 0 {
			t.Fatalf("cycle close: generated=%d failures=%v, want 1 and none", gen, failures)
		}
		invs := f.listInvoices(t, ctx)
		if len(invs) != before+1 {
			t.Fatalf("invoices after cycle close = %d, want %d", len(invs), before+1)
		}
		for _, inv := range invs {
			if inv.BillingReason == domain.BillingReasonSubscriptionCycle {
				checkCopy(t, "cycle invoice", inv)
				return
			}
		}
		t.Fatal("no subscription_cycle invoice after the cycle close")
	})
}

// newSnapshotEngineFixture is the threshold fixture with a billing profile
// and a company name in force.
func newSnapshotEngineFixture(t *testing.T, ctx context.Context, name string) *thresholdFixture {
	t.Helper()
	f := newThresholdFixture(t, name)

	if _, err := customer.NewPostgresStore(f.db).UpsertBillingProfile(ctx, f.tenantID, domain.CustomerBillingProfile{
		CustomerID: f.customerID, LegalName: "Engine Buyer GmbH", AddressLine1: "Engine Strasse 1",
		Country: "DE", TaxStatus: domain.TaxStatusStandard, ProfileStatus: domain.BillingProfileReady,
	}); err != nil {
		t.Fatalf("upsert profile: %v", err)
	}
	ts, err := f.settings.Get(ctx, f.tenantID)
	if err != nil {
		t.Fatalf("get settings: %v", err)
	}
	ts.CompanyName = "Engine Seller Ltd"
	if _, err := f.settings.Upsert(ctx, ts); err != nil {
		t.Fatalf("upsert settings: %v", err)
	}

	return f
}
