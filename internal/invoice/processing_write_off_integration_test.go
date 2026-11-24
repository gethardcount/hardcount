package invoice_test

import (
	"context"
	"testing"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/invoice"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/tenant"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// TestProcessingCharge_RefusesWriteOff: a charge Stripe is still processing
// is resolving, so a write-off would race its settle. Only an unclear
// ('unknown') charge may be written off.
func TestProcessingCharge_RefusesWriteOff(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Processing Write-off")
	store := invoice.NewPostgresStore(db)
	svc := invoice.NewService(store, nil, tenant.NewSettingsStore(db))

	inv := seedClaimableInvoice(t, db, ctx, tenantID, "INV-PROC-UNCOLL")
	stampOutcome(t, db, store, ctx, tenantID, inv.ID, domain.PaymentProcessing, "pi_live_1", "")
	if _, err := svc.MarkUncollectible(ctx, tenantID, inv.ID); err == nil {
		t.Fatal("a charge that is in flight must not be write-off-able: it is about to settle")
	}
}
