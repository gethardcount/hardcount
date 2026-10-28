package billing_test

import (
	"context"
	"testing"
	"time"

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

// TestManualInvoice_IsSimulatedFromCustomerPin is the wire-shape regression
// guard for the simulated-timestamp badge on manual (operator-composed)
// invoices. It exercises the REAL production path end-to-end:
//
//   - The FLAG (invoice.is_simulated) is captured at write time from the
//     customer's test-clock pin (invoiceSvc.SetCustomerReader → the same
//     authoritative customer.TestClockID check the engine uses on subs). This
//     is NOT inferred from the ctx clock-binding: bindForCreate binds ctx to
//     the resolver's effective-now even for UNPINNED customers (it returns
//     wall-clock), so a binding-based check would mis-flag EVERY manual
//     invoice as simulated — the bug this test pins shut.
//   - The TIMESTAMPS (issued_at) ride the resolver-bound frozen time
//     (invoiceSvc.SetResolver(engine) → EffectiveNowForCustomer).
//
// Production wires both: router.go SetResolver + invoiceSvc.SetCustomerReader +
// engine.SetCustomerReader + engine.SetTestClockReader.
func TestManualInvoice_IsSimulatedFromCustomerPin(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test needs postgres")
	}
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false) // test clocks are test-mode only

	customerStore := customer.NewPostgresStore(db)
	invoiceStore := invoice.NewPostgresStore(db)
	settingsStore := tenant.NewSettingsStore(db)
	testClockStore := testclock.NewPostgresStore(db)
	tenantID := testutil.CreateTestTenant(t, db, "Pinned Manual Corp")

	frozen := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	clk, err := testClockStore.Create(ctx, tenantID, domain.TestClock{Name: "sim", FrozenTime: frozen})
	if err != nil {
		t.Fatalf("create test clock: %v", err)
	}

	// Engine as the clock.Resolver — wired like production for the customer-pin
	// path. Only customers + testClocks are exercised here; other deps unused.
	engine := billing.NewEngine(nil, nil, nil, nil, nil, settingsStore, testPaymentSetupsNoPM{}, testChargerSentinel{}, clock.Real())
	engine.SetCustomerReader(customerStore)
	engine.SetTestClockReader(testClockStore)

	invoiceSvc := invoice.NewService(invoiceStore, clock.Real(), settingsStore)
	invoiceSvc.SetResolver(engine)              // drives simulated timestamps
	invoiceSvc.SetCustomerReader(customerStore) // drives the is_simulated flag

	line := []invoice.AddLineItemInput{{Description: "Service", Quantity: 1, UnitAmountCents: 1000}}

	// (1) PINNED customer → is_simulated=true AND issued_at lands on frozen time.
	pinned, err := customerStore.Create(ctx, tenantID, domain.Customer{
		ExternalID: "cus_pinned", DisplayName: "Pinned Customer", TestClockID: clk.ID,
	})
	if err != nil {
		t.Fatalf("create pinned customer: %v", err)
	}
	inv, err := invoiceSvc.Create(ctx, tenantID, invoice.CreateInput{
		CustomerID: pinned.ID, Currency: "USD", LineItems: line,
	})
	if err != nil {
		t.Fatalf("create invoice (pinned): %v", err)
	}
	if !inv.IsSimulated {
		t.Error("manual invoice for clock-pinned customer: is_simulated=false, want true (customer-pin reader not consulted / not wired)")
	}
	if inv.IssuedAt == nil || !inv.IssuedAt.Equal(frozen) {
		t.Errorf("issued_at: got %v, want frozen %v (resolver did not bind the customer's frozen time)", inv.IssuedAt, frozen)
	}
	if reloaded, rerr := invoiceStore.Get(ctx, tenantID, inv.ID); rerr != nil {
		t.Fatalf("reload: %v", rerr)
	} else if !reloaded.IsSimulated {
		t.Error("reloaded pinned-customer invoice: is_simulated=false, want true (did not persist)")
	}

	// (2) UNPINNED customer → is_simulated=false. This is the case the
	// binding-based check got WRONG (bindForCreate binds ctx to wall-clock for
	// unpinned customers, so a ctx-binding check returned true).
	plain, err := customerStore.Create(ctx, tenantID, domain.Customer{
		ExternalID: "cus_plain", DisplayName: "Plain Customer",
	})
	if err != nil {
		t.Fatalf("create plain customer: %v", err)
	}
	inv2, err := invoiceSvc.Create(ctx, tenantID, invoice.CreateInput{
		CustomerID: plain.ID, Currency: "USD", LineItems: line,
	})
	if err != nil {
		t.Fatalf("create invoice (plain): %v", err)
	}
	if inv2.IsSimulated {
		t.Error("manual invoice for unpinned customer: is_simulated=true, want false")
	}
}
