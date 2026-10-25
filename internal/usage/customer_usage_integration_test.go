package usage_test

import (
	"context"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/sagarsuperuser/velox/internal/billing"
	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
	"github.com/sagarsuperuser/velox/internal/platform/clock"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/pricing"
	"github.com/sagarsuperuser/velox/internal/subscription"
	"github.com/sagarsuperuser/velox/internal/testutil"
	"github.com/sagarsuperuser/velox/internal/usage"
)

// customerUsageFixture wires real per-domain stores against a clean test
// DB, with billing's engine as the rater, so the customer-usage path
// exercises the full SQL → rater → response chain the server runs.
type customerUsageFixture struct {
	db          *postgres.DB
	tenantID    string
	customerSvc *customer.Service
	pricingSvc  *pricing.Service
	subStore    *subscription.PostgresStore
	usageSvc    *usage.Service
	custUsage   *usage.CustomerUsageService
}

func newCustomerUsageFixture(t *testing.T, name string) *customerUsageFixture {
	t.Helper()
	db := testutil.SetupTestDB(t)
	tenantID := testutil.CreateTestTenant(t, db, name)

	customerStore := customer.NewPostgresStore(db)
	pricingSvc := pricing.NewService(pricing.NewPostgresStore(db))
	subStore := subscription.NewPostgresStore(db)
	usageStore := usage.NewPostgresStore(db)
	usageSvc := usage.NewService(usageStore)
	custUsage := usage.NewCustomerUsageService(usageSvc, customerStore, subStore, pricingSvc)
	engine := billing.NewEngine(subStore, usageStore, pricingSvc, noThresholdInvoices{}, nil, nil, nil, nil, clock.Real())
	engine.SetIntervalReader(subStore)
	custUsage.SetRater(engine)

	return &customerUsageFixture{
		db:          db,
		tenantID:    tenantID,
		customerSvc: customer.NewService(customerStore),
		pricingSvc:  pricingSvc,
		subStore:    subStore,
		usageSvc:    usageSvc,
		custUsage:   custUsage,
	}
}

// noThresholdInvoices is the rater's invoice reader for subscriptions that
// have no threshold invoice: its only read is the threshold watermark. Any
// other call panics (the embedded interface is nil).
type noThresholdInvoices struct{ billing.InvoiceWriter }

func (noThresholdInvoices) GetLatestThresholdInvoiceForCycle(context.Context, string, string, time.Time, time.Time) (domain.Invoice, error) {
	return domain.Invoice{}, errs.ErrNotFound
}

// meter creates a meter.
func (f *customerUsageFixture) meter(t *testing.T, ctx context.Context, eventName string) domain.Meter {
	t.Helper()
	m, err := f.pricingSvc.CreateMeter(ctx, f.tenantID, pricing.CreateMeterInput{EventName: eventName, Name: eventName, Unit: "tokens", Aggregation: "sum"})
	if err != nil {
		t.Fatalf("create meter: %v", err)
	}
	return m
}

// price is a per_unit price input.
func price(meterID string, cents int64, match map[string]string) pricing.PriceInput {
	return pricing.PriceInput{MeterID: meterID, DimensionMatch: match, PriceShape: domain.PriceShape{Mode: domain.PricingPerUnit, UnitAmountCents: decimal.NewFromInt(cents)}}
}

// seedCustomerWithSub creates a customer, a USD plan with prices, and an
// active subscription on it with a current billing cycle of [from, to).
func (f *customerUsageFixture) seedCustomerWithSub(
	t *testing.T,
	ctx context.Context,
	externalCustomerID string,
	planCode string,
	prices []pricing.PriceInput,
	cycleStart, cycleEnd time.Time,
) (custID, planID, subID string) {
	t.Helper()
	return f.seedCustomerWithSubIn(t, ctx, externalCustomerID, planCode, prices, "USD", cycleStart, cycleEnd)
}

// seedCustomerWithSubIn is seedCustomerWithSub with the plan in currency.
// The subscription, its item and its billing interval are written directly.
func (f *customerUsageFixture) seedCustomerWithSubIn(
	t *testing.T,
	ctx context.Context,
	externalCustomerID string,
	planCode string,
	prices []pricing.PriceInput,
	currency string,
	cycleStart, cycleEnd time.Time,
) (custID, planID, subID string) {
	t.Helper()

	cust, err := f.customerSvc.Create(ctx, f.tenantID, customer.CreateInput{
		ExternalID:  externalCustomerID,
		DisplayName: externalCustomerID,
		Email:       externalCustomerID + "@example.test",
	})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}

	plan, _, err := f.pricingSvc.CreatePlan(ctx, f.tenantID, pricing.CreatePlanInput{
		Code:            planCode,
		Name:            planCode,
		Currency:        currency,
		BillingInterval: domain.BillingMonthly,
		Prices:          prices,
	})
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}

	subID = postgres.NewID("vlx_sub")
	itemID := postgres.NewID("vlx_si")
	tx, err := f.db.BeginTx(ctx, postgres.TxTenant, f.tenantID)
	if err != nil {
		t.Fatalf("begin sub: %v", err)
	}
	defer postgres.Rollback(tx)

	now := time.Now().UTC()
	_, err = tx.ExecContext(ctx, `
		INSERT INTO subscriptions (
			id, tenant_id, code, display_name, customer_id, status, billing_time,
			current_billing_period_start, current_billing_period_end, next_billing_at,
			created_at, updated_at
		) VALUES ($1, $2, $3, $4, $5, 'active', 'anniversary', $6, $7, $7, $8, $8)
	`, subID, f.tenantID, "code-"+externalCustomerID, planCode+"-sub", cust.ID,
		cycleStart, cycleEnd, now)
	if err != nil {
		t.Fatalf("insert sub: %v", err)
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO subscription_items (id, tenant_id, subscription_id, plan_id, quantity, metadata, created_at, updated_at)
		VALUES ($1, $2, $3, $4, 1, '{}'::jsonb, $5, $5)
	`, itemID, f.tenantID, subID, plan.ID, now)
	if err != nil {
		t.Fatalf("insert sub item: %v", err)
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO billing_intervals (tenant_id, subscription_id, subscription_item_id, plan_id, quantity, starts_at, source)
		VALUES ($1, $2, $3, $4, 1, $5, 'create')
	`, f.tenantID, subID, itemID, plan.ID, cycleStart)
	if err != nil {
		t.Fatalf("insert billing interval: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit sub: %v", err)
	}
	return cust.ID, plan.ID, subID
}

func TestCustomerUsage_SingleMeterFlatParity(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipped in -short mode")
	}
	f := newCustomerUsageFixture(t, "Customer Usage Single Meter")
	ctx, cancel := context.WithTimeout(postgres.WithLivemode(context.Background(), false), 30*time.Second)
	defer cancel()

	cycleStart := time.Now().UTC().Truncate(time.Hour).Add(-120 * time.Hour) // far enough back that 100 hourly events stay in the past (future live ingest is rejected)
	cycleEnd := cycleStart.Add(30 * 24 * time.Hour)

	// 1 cent per token.
	meter := f.meter(t, ctx, "tokens")
	custID, _, _ := f.seedCustomerWithSub(t, ctx, "cus_singlemeter", "pln_single", []pricing.PriceInput{price(meter.ID, 1, nil)}, cycleStart, cycleEnd)

	// Ingest 100 in-cycle events of qty=10 each = 1000 total.
	for i := 0; i < 100; i++ {
		ts := cycleStart.Add(time.Duration(i) * time.Hour)
		if _, err := f.usageSvc.Ingest(ctx, f.tenantID, usage.IngestInput{
			CustomerID: custID, MeterID: meter.ID,
			Quantity: decimal.NewFromInt(10), Timestamp: &ts,
		}); err != nil {
			t.Fatalf("ingest %d: %v", i, err)
		}
	}

	// One event outside the cycle — should NOT count for default-period query.
	outside := cycleStart.Add(-48 * time.Hour)
	if _, err := f.usageSvc.Ingest(ctx, f.tenantID, usage.IngestInput{
		CustomerID: custID, MeterID: meter.ID,
		Quantity: decimal.NewFromInt(999999), Timestamp: &outside,
	}); err != nil {
		t.Fatalf("ingest outside: %v", err)
	}

	res, err := f.custUsage.Get(ctx, f.tenantID, custID, usage.CustomerUsagePeriod{})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	if res.Period.Source != "current_billing_cycle" {
		t.Errorf("period source: got %q, want current_billing_cycle", res.Period.Source)
	}
	if len(res.Meters) != 1 {
		t.Fatalf("meters: got %d, want 1", len(res.Meters))
	}
	if res.Meters[0].TotalAmountCents != 1000 {
		t.Errorf("total cents: got %d, want 1000", res.Meters[0].TotalAmountCents)
	}
	if !res.Meters[0].TotalQuantity.Equal(decimal.NewFromInt(1000)) {
		t.Errorf("total quantity: got %s, want 1000", res.Meters[0].TotalQuantity.String())
	}
	if len(res.Totals) != 1 || res.Totals[0].Currency != "USD" || res.Totals[0].AmountCents != 1000 {
		t.Errorf("totals: got %+v", res.Totals)
	}
}

func TestCustomerUsage_MultiDimDimensionMatchEcho(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipped in -short mode")
	}
	f := newCustomerUsageFixture(t, "Customer Usage Multi Dim")
	ctx, cancel := context.WithTimeout(postgres.WithLivemode(context.Background(), false), 30*time.Second)
	defer cancel()

	cycleStart := time.Now().UTC().Truncate(time.Hour).Add(-24 * time.Hour)
	cycleEnd := cycleStart.Add(30 * 24 * time.Hour)

	meter := f.meter(t, ctx, "tokens_dim")
	custID, planID, _ := f.seedCustomerWithSub(t, ctx, "cus_multidim", "pln_multidim", []pricing.PriceInput{
		price(meter.ID, 3, map[string]string{"operation": "input"}),
		price(meter.ID, 5, map[string]string{"operation": "output"}),
	}, cycleStart, cycleEnd)

	// Seed 10 input events qty=100 = 1000 input units, 5 output events qty=20 = 100 output units.
	for i := 0; i < 10; i++ {
		ts := cycleStart.Add(time.Duration(i) * time.Hour)
		if _, err := f.usageSvc.Ingest(ctx, f.tenantID, usage.IngestInput{
			CustomerID: custID, MeterID: meter.ID, Quantity: decimal.NewFromInt(100),
			Dimensions: map[string]any{"operation": "input"}, Timestamp: &ts,
		}); err != nil {
			t.Fatalf("ingest input %d: %v", i, err)
		}
	}
	for i := 0; i < 5; i++ {
		ts := cycleStart.Add(time.Duration(i+11) * time.Hour)
		if _, err := f.usageSvc.Ingest(ctx, f.tenantID, usage.IngestInput{
			CustomerID: custID, MeterID: meter.ID, Quantity: decimal.NewFromInt(20),
			Dimensions: map[string]any{"operation": "output"}, Timestamp: &ts,
		}); err != nil {
			t.Fatalf("ingest output %d: %v", i, err)
		}
	}

	res, err := f.custUsage.Get(ctx, f.tenantID, custID, usage.CustomerUsagePeriod{})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(res.Meters) != 1 {
		t.Fatalf("meters: got %d, want 1", len(res.Meters))
	}
	rows := res.Meters[0].Prices
	if len(rows) != 2 {
		t.Fatalf("price rows: got %d, want 2", len(rows))
	}

	// Each row echoes its price's dimension_match and plan, and its amount
	// is qty × rate.
	var totalCents int64
	for _, row := range rows {
		if row.DimensionMatch["operation"] == "" || row.PlanID != planID || row.PriceID == "" {
			t.Errorf("row %+v must echo the price's dimension_match, plan and id", row)
		}
		totalCents += row.AmountCents
	}
	// 1000 input * 3¢ + 100 output * 5¢ = 3500.
	if res.Meters[0].TotalAmountCents != totalCents {
		t.Errorf("meter total != sum(rules): meter=%d, sum=%d", res.Meters[0].TotalAmountCents, totalCents)
	}
	if totalCents != 3500 {
		t.Errorf("expected 3500 cents (1000×3 + 100×5), got %d", totalCents)
	}
}

func TestCustomerUsage_CrossTenantIsolation(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipped in -short mode")
	}
	f := newCustomerUsageFixture(t, "Customer Usage Cross Tenant A")
	ctx, cancel := context.WithTimeout(postgres.WithLivemode(context.Background(), false), 15*time.Second)
	defer cancel()

	meter := f.meter(t, ctx, "x")
	cycleStart := time.Now().UTC().Truncate(time.Hour).Add(-24 * time.Hour)
	cycleEnd := cycleStart.Add(30 * 24 * time.Hour)
	custA, _, _ := f.seedCustomerWithSub(t, ctx, "cus_tenantA", "pln_a", []pricing.PriceInput{price(meter.ID, 1, nil)}, cycleStart, cycleEnd)

	// Tenant B asks for tenant A's customer ID — RLS should hide it,
	// surfacing as a not-found error.
	tenantB := testutil.CreateTestTenant(t, f.db, "Customer Usage Cross Tenant B")
	_, err := f.custUsage.Get(ctx, tenantB, custA, usage.CustomerUsagePeriod{})
	if err == nil {
		t.Fatal("expected not-found error for cross-tenant lookup")
	}
}

func TestCustomerUsage_NoSubscriptionRequiresExplicitWindow(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipped in -short mode")
	}
	f := newCustomerUsageFixture(t, "Customer Usage No Sub")
	ctx, cancel := context.WithTimeout(postgres.WithLivemode(context.Background(), false), 15*time.Second)
	defer cancel()

	cust, err := f.customerSvc.Create(ctx, f.tenantID, customer.CreateInput{
		ExternalID: "cus_nosub", DisplayName: "No Sub", Email: "nosub@example.test",
	})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}

	_, err = f.custUsage.Get(ctx, f.tenantID, cust.ID, usage.CustomerUsagePeriod{})
	if err == nil {
		t.Fatal("expected error when customer has no subscription and no period given")
	}
	if errs.Code(err) != "customer_has_no_subscription" {
		t.Errorf("error code: got %q, want customer_has_no_subscription", errs.Code(err))
	}

	// With explicit window the response is valid (empty meters).
	from := time.Now().UTC().Add(-7 * 24 * time.Hour)
	to := time.Now().UTC()
	res, err := f.custUsage.Get(ctx, f.tenantID, cust.ID, usage.CustomerUsagePeriod{From: from, To: to})
	if err != nil {
		t.Fatalf("Get with explicit window: %v", err)
	}
	if len(res.Meters) != 0 {
		t.Errorf("meters: got %d, want 0 (no subscription means no plan-side meters)", len(res.Meters))
	}
	if len(res.Subscriptions) != 0 {
		t.Errorf("subscriptions: got %d, want 0", len(res.Subscriptions))
	}
	if res.Period.Source != "explicit" {
		t.Errorf("period source: got %q, want explicit", res.Period.Source)
	}
}
