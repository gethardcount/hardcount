package billing_test

import (
	"context"
	"testing"

	"github.com/gethardcount/hardcount/internal/billing"
	"github.com/gethardcount/hardcount/internal/customer"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/invoice"
	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/tenant"
	"github.com/gethardcount/hardcount/internal/testclock"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// TestManualInvoice_UnpinnedIsNotSimulated: a manual invoice's is_simulated
// flag is captured at write time from the customer's test-clock pin
// (invoiceSvc.SetCustomerReader), not inferred from the ctx's time. A create
// binds an instant for an unpinned customer too, so a check on the ctx's
// binding would flag every manual invoice as simulated.
//
// Wired as production wires it: router.go SetResolver +
// invoiceSvc.SetCustomerReader + engine.SetCustomerReader +
// engine.SetTestClockReader.
func TestManualInvoice_UnpinnedIsNotSimulated(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)

	customerStore := customer.NewPostgresStore(db)
	invoiceStore := invoice.NewPostgresStore(db)
	settingsStore := tenant.NewSettingsStore(db)
	tenantID := testutil.CreateTestTenant(t, db, "Unpinned Manual Corp")

	engine := billing.NewEngine(nil, nil, nil, nil, nil, settingsStore, testPaymentSetupsNoPM{}, testChargerSentinel{}, clock.Real())
	engine.SetCustomerReader(customerStore)
	engine.SetTestClockReader(testclock.NewReader(db))

	invoiceSvc := invoice.NewService(invoiceStore, clock.Real(), settingsStore)
	invoiceSvc.SetResolver(engine)
	invoiceSvc.SetCustomerReader(customerStore)

	plain, err := customerStore.Create(ctx, tenantID, domain.Customer{
		ExternalID: "cus_plain", DisplayName: "Plain Customer",
	})
	if err != nil {
		t.Fatalf("create plain customer: %v", err)
	}
	inv, err := invoiceSvc.Create(ctx, tenantID, invoice.CreateInput{
		CustomerID: plain.ID, Currency: "USD",
		LineItems: []invoice.AddLineItemInput{{Description: "Service", Quantity: 1, UnitAmountCents: 1000}},
	})
	if err != nil {
		t.Fatalf("create invoice: %v", err)
	}
	if inv.IsSimulated {
		t.Error("manual invoice for unpinned customer: is_simulated=true, want false")
	}
}
