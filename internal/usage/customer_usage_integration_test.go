package usage_test

import (
	"context"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/pricing"
	"github.com/sagarsuperuser/velox/internal/subscription"
	"github.com/sagarsuperuser/velox/internal/testutil"
	"github.com/sagarsuperuser/velox/internal/usage"
)

// customerUsageFixture wires real per-domain stores against a clean test
// DB so the customer-usage path exercises the full SQL → service → response
// chain. Tests then ingest events and assert that the response matches what
// the cycle scan would produce — the parity guarantee the whole feature
// rests on.
type customerUsageFixture struct {
	db          *postgres.DB
	tenantID    string
	customerSvc *customer.Service
	pricingSvc  *pricing.Service
	subStore    *subscription.PostgresStore
	subSvc      *subscription.Service
	usageSvc    *usage.Service
	custUsage   *usage.CustomerUsageService
}

func newCustomerUsageFixture(t *testing.T, name string) *customerUsageFixture {
	t.Helper()
	db := testutil.SetupTestDB(t)
	tenantID := testutil.CreateTestTenant(t, db, name)

	customerStore := customer.NewPostgresStore(db)
	customerSvc := customer.NewService(customerStore)
	pricingStore := pricing.NewPostgresStore(db)
	pricingSvc := pricing.NewService(pricingStore)
	subStore := subscription.NewPostgresStore(db)
	subSvc := subscription.NewService(subStore, nil)
	usageStore := usage.NewPostgresStore(db)
	usageSvc := usage.NewService(usageStore)
	custUsage := usage.NewCustomerUsageService(usageSvc, customerStore, subStore, pricingSvc)

	return &customerUsageFixture{
		db:          db,
		tenantID:    tenantID,
		customerSvc: customerSvc,
		pricingSvc:  pricingSvc,
		subStore:    subStore,
		subSvc:      subSvc,
		usageSvc:    usageSvc,
		custUsage:   custUsage,
	}
}

// seedCustomerWithSub creates a customer + plan + active subscription with
// a current billing cycle of [from, to). Returns customer id, plan id, and
// the real meter id (the test wires meterID separately so the caller can
// assemble multi-meter scenarios). Plan is wired with the supplied meter.
func (f *customerUsageFixture) seedCustomerWithSub(
	t *testing.T,
	ctx context.Context,
	externalCustomerID string,
	planCode string,
	meterID string,
	cycleStart, cycleEnd time.Time,
) (custID, planID, subID string) {
	t.Helper()
	return f.seedCustomerWithSubIn(t, ctx, externalCustomerID, planCode, meterID, "USD", cycleStart, cycleEnd)
}

// seedCustomerWithSubIn is seedCustomerWithSub with the plan in currency.
func (f *customerUsageFixture) seedCustomerWithSubIn(
	t *testing.T,
	ctx context.Context,
	externalCustomerID string,
	planCode string,
	meterID string,
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

	plan, err := f.pricingSvc.CreatePlan(ctx, f.tenantID, pricing.CreatePlanInput{
		Code:            planCode,
		Name:            planCode,
		Currency:        currency,
		BillingInterval: domain.BillingMonthly,
		BaseAmountCents: 0,
		MeterIDs:        []string{meterID},
	})
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}

	subID = postgres.NewID("vlx_sub")
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
	`, postgres.NewID("vlx_si"), f.tenantID, subID, plan.ID, now)
	if err != nil {
		t.Fatalf("insert sub item: %v", err)
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

	// Rating rule: 1 cent per token.
	rrv, err := f.pricingSvc.CreateRatingRule(ctx, f.tenantID, pricing.CreateRatingRuleInput{
		RuleKey: "tokens_flat", Name: "Tokens Flat",
		Mode: domain.PricingFlat, Currency: "USD", FlatAmountCents: decimal.NewFromInt(1),
	})
	if err != nil {
		t.Fatalf("create rrv: %v", err)
	}

	meter, err := f.pricingSvc.CreateMeter(ctx, f.tenantID, pricing.CreateMeterInput{
		Key: "tokens", Name: "Tokens", Unit: "tokens",
		Aggregation: "sum", RatingRuleVersionID: rrv.ID,
	})
	if err != nil {
		t.Fatalf("create meter: %v", err)
	}

	custID, _, _ := f.seedCustomerWithSub(t, ctx, "cus_singlemeter", "pln_single", meter.ID, cycleStart, cycleEnd)

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

	rrvIn, err := f.pricingSvc.CreateRatingRule(ctx, f.tenantID, pricing.CreateRatingRuleInput{
		RuleKey: "tokens_input", Name: "Input", Mode: domain.PricingFlat, Currency: "USD", FlatAmountCents: decimal.NewFromInt(3),
	})
	if err != nil {
		t.Fatalf("create rrv input: %v", err)
	}
	rrvOut, err := f.pricingSvc.CreateRatingRule(ctx, f.tenantID, pricing.CreateRatingRuleInput{
		RuleKey: "tokens_output", Name: "Output", Mode: domain.PricingFlat, Currency: "USD", FlatAmountCents: decimal.NewFromInt(5),
	})
	if err != nil {
		t.Fatalf("create rrv output: %v", err)
	}

	// Default rating rule on the meter (used for unclaimed events) — set
	// to the input rule so unclaimed events would still rate sensibly.
	meter, err := f.pricingSvc.CreateMeter(ctx, f.tenantID, pricing.CreateMeterInput{
		Key: "tokens_dim", Name: "Tokens Dim", Unit: "tokens",
		Aggregation: "sum", RatingRuleVersionID: rrvIn.ID,
	})
	if err != nil {
		t.Fatalf("create meter: %v", err)
	}

	if _, err := f.pricingSvc.UpsertMeterPricingRule(ctx, f.tenantID, pricing.UpsertMeterPricingRuleInput{
		MeterID: meter.ID, RatingRuleVersionID: rrvIn.ID,
		DimensionMatch:  map[string]any{"operation": "input"},
		AggregationMode: domain.AggSum, Priority: 10,
	}); err != nil {
		t.Fatalf("upsert mpr input: %v", err)
	}
	if _, err := f.pricingSvc.UpsertMeterPricingRule(ctx, f.tenantID, pricing.UpsertMeterPricingRuleInput{
		MeterID: meter.ID, RatingRuleVersionID: rrvOut.ID,
		DimensionMatch:  map[string]any{"operation": "output"},
		AggregationMode: domain.AggSum, Priority: 10,
	}); err != nil {
		t.Fatalf("upsert mpr output: %v", err)
	}

	custID, _, _ := f.seedCustomerWithSub(t, ctx, "cus_multidim", "pln_multidim", meter.ID, cycleStart, cycleEnd)

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
	rules := res.Meters[0].Rules
	if len(rules) < 2 {
		t.Fatalf("rules: got %d, want at least 2", len(rules))
	}

	// Verify each rule entry has dimension_match echoed and amount = qty * cents.
	var totalCents int64
	for _, rule := range rules {
		if rule.DimensionMatch == nil {
			t.Errorf("rule %q dimension_match must echo the meter pricing rule's filter", rule.RuleKey)
		}
		totalCents += rule.AmountCents
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

	rrv, _ := f.pricingSvc.CreateRatingRule(ctx, f.tenantID, pricing.CreateRatingRuleInput{
		RuleKey: "x", Name: "x", Mode: domain.PricingFlat, Currency: "USD", FlatAmountCents: decimal.NewFromInt(1),
	})
	meter, _ := f.pricingSvc.CreateMeter(ctx, f.tenantID, pricing.CreateMeterInput{
		Key: "x", Name: "x", Unit: "u", Aggregation: "sum", RatingRuleVersionID: rrv.ID,
	})
	cycleStart := time.Now().UTC().Truncate(time.Hour).Add(-24 * time.Hour)
	cycleEnd := cycleStart.Add(30 * 24 * time.Hour)
	custA, _, _ := f.seedCustomerWithSub(t, ctx, "cus_tenantA", "pln_a", meter.ID, cycleStart, cycleEnd)

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
