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
	// invoices is the rater's invoice reader: set its fire to give every
	// subscription a threshold invoice.
	invoices *thresholdInvoices
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
	invoices := &thresholdInvoices{}
	engine := billing.NewEngine(subStore, usageStore, pricingSvc, invoices, nil, nil, nil, nil, clock.Real())
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
		invoices:    invoices,
	}
}

// thresholdInvoices is the rater's invoice reader: its only reads are the
// threshold watermark's. With no fire, a subscription has no threshold
// invoice; with one, every subscription has a threshold invoice with no
// lines that billed its usage up to the fire. Any other call panics (the
// embedded interface is nil).
type thresholdInvoices struct {
	billing.InvoiceWriter
	fire *time.Time
}

func (t *thresholdInvoices) GetLatestThresholdInvoiceForCycle(context.Context, string, string, time.Time, time.Time) (domain.Invoice, error) {
	if t.fire == nil {
		return domain.Invoice{}, errs.ErrNotFound
	}
	return domain.Invoice{ID: "vlx_inv_fire", BillingPeriodEnd: *t.fire}, nil
}

func (t *thresholdInvoices) ListLineItems(context.Context, string, string) ([]domain.InvoiceLineItem, error) {
	return nil, nil
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
	planID, subID = f.seedSubIn(t, ctx, cust.ID, planCode, prices, currency, cycleStart, cycleEnd)
	return cust.ID, planID, subID
}

// seedSubIn creates a plan with prices in currency and an active
// subscription of custID on it with a current billing cycle of
// [cycleStart, cycleEnd). The subscription, its item and its billing
// interval are written directly.
func (f *customerUsageFixture) seedSubIn(
	t *testing.T,
	ctx context.Context,
	custID string,
	planCode string,
	prices []pricing.PriceInput,
	currency string,
	cycleStart, cycleEnd time.Time,
) (planID, subID string) {
	t.Helper()
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
	`, subID, f.tenantID, "code-"+planCode, planCode+"-sub", custID,
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
	return plan.ID, subID
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

// TestCustomerUsage_ExplicitWindowLeavesOutACanceledSubscription pins what
// the API reference says of `period.source: explicit` and what the
// dashboard's estimate note says: only active and trialing subscriptions are
// rated, so a canceled subscription's usage in a chosen window is not
// counted, though its invoices billed it.
//
// Mutation: rate canceled subscriptions too (filterActiveSubs), and the
// canceled subscription's 500 cents stay in the window's totals.
func TestCustomerUsage_ExplicitWindowLeavesOutACanceledSubscription(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipped in -short mode")
	}
	f := newCustomerUsageFixture(t, "Customer Usage Canceled Sub")
	ctx, cancel := context.WithTimeout(postgres.WithLivemode(context.Background(), false), 30*time.Second)
	defer cancel()

	cycleStart := time.Now().UTC().Truncate(time.Hour).Add(-48 * time.Hour)
	cycleEnd := cycleStart.Add(30 * 24 * time.Hour)
	meter := f.meter(t, ctx, "calls")
	custID, _, subID := f.seedCustomerWithSub(t, ctx, "cus_canceled", "pln_canceled", []pricing.PriceInput{price(meter.ID, 1, nil)}, cycleStart, cycleEnd)
	ts := cycleStart.Add(time.Hour)
	if _, err := f.usageSvc.Ingest(ctx, f.tenantID, usage.IngestInput{
		CustomerID: custID, MeterID: meter.ID, Quantity: decimal.NewFromInt(500), Timestamp: &ts,
	}); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	window := usage.CustomerUsagePeriod{From: cycleStart.Add(-24 * time.Hour), To: time.Now().UTC()}

	res, err := f.custUsage.Get(ctx, f.tenantID, custID, window)
	if err != nil {
		t.Fatalf("Get while active: %v", err)
	}
	if res.Period.Source != "explicit" || len(res.Totals) != 1 || res.Totals[0].AmountCents != 500 {
		t.Fatalf("while active: source %q, totals %+v; want explicit and 500 cents", res.Period.Source, res.Totals)
	}

	tx, err := f.db.BeginTx(ctx, postgres.TxTenant, f.tenantID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	if _, err := tx.ExecContext(ctx, `UPDATE subscriptions SET status = 'canceled', canceled_at = now() WHERE id = $1`, subID); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit cancel: %v", err)
	}

	res, err = f.custUsage.Get(ctx, f.tenantID, custID, window)
	if err != nil {
		t.Fatalf("Get after the cancel: %v", err)
	}
	if res.Period.Source != "explicit" || len(res.Totals) != 0 || len(res.Meters) != 0 || len(res.Subscriptions) != 0 || len(res.Warnings) != 0 {
		t.Fatalf("after the cancel: source %q, totals %+v, meters %d, subscriptions %d, warnings %v; want explicit and nothing counted",
			res.Period.Source, res.Totals, len(res.Meters), len(res.Subscriptions), res.Warnings)
	}
}

// TestCustomerUsage_TwoSubscriptionsOnTheirOwnPeriods: with no window, each
// subscription is rated over its own current period (ADR-130 §10), so the
// response's period spans every one of them, and each meter's daily buckets
// cover its own subscription's period: the chart and the totals read the
// same usage.
//
// Mutation: report only the latest-starting subscription's period, or read
// every meter's buckets over the response's period, and meter a's usage
// falls outside the period with no bucket, or meter b's previous-period
// usage shows in a bucket but in no total.
func TestCustomerUsage_TwoSubscriptionsOnTheirOwnPeriods(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipped in -short mode")
	}
	f := newCustomerUsageFixture(t, "Customer Usage Two Periods")
	ctx, cancel := context.WithTimeout(postgres.WithLivemode(context.Background(), false), 30*time.Second)
	defer cancel()

	now := time.Now().UTC().Truncate(time.Hour)
	aStart, aEnd := now.Add(-10*24*time.Hour), now.Add(20*24*time.Hour)
	bStart, bEnd := now.Add(-24*time.Hour), now.Add(29*24*time.Hour)
	meterA := f.meter(t, ctx, "calls_a")
	meterB := f.meter(t, ctx, "calls_b")
	custID, _, _ := f.seedCustomerWithSub(t, ctx, "cus_two", "pln_a", []pricing.PriceInput{price(meterA.ID, 1, nil)}, aStart, aEnd)
	f.seedSubIn(t, ctx, custID, "pln_b", []pricing.PriceInput{price(meterB.ID, 1, nil)}, "USD", bStart, bEnd)

	ingest := func(meterID string, qty int64, ts time.Time) {
		t.Helper()
		if _, err := f.usageSvc.Ingest(ctx, f.tenantID, usage.IngestInput{
			CustomerID: custID, MeterID: meterID, Quantity: decimal.NewFromInt(qty), Timestamp: &ts,
		}); err != nil {
			t.Fatalf("ingest: %v", err)
		}
	}
	ingest(meterA.ID, 7, aStart.Add(time.Hour)) // in A's period, before B's
	ingest(meterB.ID, 5, aStart.Add(time.Hour)) // before B's period: billed by B's previous period
	ingest(meterB.ID, 3, bStart.Add(time.Hour)) // in B's period

	res, err := f.custUsage.Get(ctx, f.tenantID, custID, usage.CustomerUsagePeriod{})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !res.Period.From.Equal(aStart) || !res.Period.To.Equal(bEnd) {
		t.Errorf("period = [%v, %v), want [%v, %v): every subscription's current period", res.Period.From, res.Period.To, aStart, bEnd)
	}
	totals := map[string]decimal.Decimal{}
	for _, m := range res.Meters {
		totals[m.MeterID] = m.TotalQuantity
	}
	charted := map[string]decimal.Decimal{}
	for _, b := range res.Buckets {
		if b.BucketStart.Before(res.Period.From.Truncate(24*time.Hour)) || !b.BucketStart.Before(res.Period.To) {
			t.Errorf("bucket %v outside the period", b.BucketStart)
		}
		for id, q := range b.PerMeter {
			charted[id] = charted[id].Add(q)
		}
	}
	for id, want := range map[string]int64{meterA.ID: 7, meterB.ID: 3} {
		if !totals[id].Equal(decimal.NewFromInt(want)) {
			t.Errorf("meter %s total = %s, want %d", id, totals[id], want)
		}
		if !charted[id].Equal(decimal.NewFromInt(want)) {
			t.Errorf("meter %s buckets add up to %s, want %d (its total)", id, charted[id], want)
		}
	}
}

// After a threshold invoice, customer usage still shows the whole current
// period: its totals and its daily buckets count the same usage. The
// threshold invoice billed part of it; customer usage is the period's usage,
// not what is left to bill (that is the live balance's unbilled_usage_cents).
//
// Mutation: rate customer usage after the threshold watermark (the live
// balance's read), and the meter's total drops to the usage since the fire
// while its buckets still add up to the whole period.
func TestCustomerUsage_WholePeriodAfterAThresholdInvoice(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipped in -short mode")
	}
	f := newCustomerUsageFixture(t, "Customer Usage After A Fire")
	ctx, cancel := context.WithTimeout(postgres.WithLivemode(context.Background(), false), 30*time.Second)
	defer cancel()

	now := time.Now().UTC().Truncate(time.Hour)
	start, end := now.Add(-5*24*time.Hour), now.Add(25*24*time.Hour)
	m := f.meter(t, ctx, "calls_fire")
	custID, _, _ := f.seedCustomerWithSub(t, ctx, "cus_fire", "pln_fire", []pricing.PriceInput{price(m.ID, 1, nil)}, start, end)
	for _, ev := range []struct {
		qty int64
		at  time.Time
	}{{100, start.Add(time.Hour)}, {5, now.Add(-time.Hour)}} {
		at := ev.at
		if _, err := f.usageSvc.Ingest(ctx, f.tenantID, usage.IngestInput{
			CustomerID: custID, MeterID: m.ID, Quantity: decimal.NewFromInt(ev.qty), Timestamp: &at,
		}); err != nil {
			t.Fatalf("ingest: %v", err)
		}
	}
	fire := now.Add(-2 * 24 * time.Hour) // billed the 100, not the 5
	f.invoices.fire = &fire

	res, err := f.custUsage.Get(ctx, f.tenantID, custID, usage.CustomerUsagePeriod{})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(res.Meters) != 1 {
		t.Fatalf("meters = %d, want 1", len(res.Meters))
	}
	got := res.Meters[0]
	if !got.TotalQuantity.Equal(decimal.NewFromInt(105)) || got.TotalAmountCents != 105 {
		t.Errorf("meter total = %s units, %d cents; want 105 units, 105 cents: the whole period", got.TotalQuantity, got.TotalAmountCents)
	}
	if len(res.Totals) != 1 || res.Totals[0].AmountCents != 105 {
		t.Errorf("totals = %+v, want USD 105", res.Totals)
	}
	charted := decimal.Zero
	for _, b := range res.Buckets {
		charted = charted.Add(b.PerMeter[m.ID])
	}
	if !charted.Equal(got.TotalQuantity) {
		t.Errorf("buckets add up to %s, the total is %s: they must count the same usage", charted, got.TotalQuantity)
	}
}
