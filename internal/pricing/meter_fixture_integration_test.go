package pricing_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/pricing"
	"github.com/sagarsuperuser/velox/internal/subscription"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// meterEditFixture is a test-mode tenant with the pricing service on real
// Postgres, for the meter edit tests.
type meterEditFixture struct {
	db       *postgres.DB
	svc      *pricing.Service
	tenantID string
	ctx      context.Context
}

func newMeterEditFixture(t *testing.T) meterEditFixture {
	t.Helper()
	if testing.Short() {
		t.Skip("integration: skipped in -short mode")
	}
	db := testutil.SetupTestDB(t)
	svc := pricing.NewService(pricing.NewPostgresStore(db))
	svc.SetSubscriptionPlanUsageReader(subscription.NewPostgresStore(db))
	return meterEditFixture{
		db:       db,
		svc:      svc,
		tenantID: testutil.CreateTestTenant(t, db, "meter edit"),
		ctx:      postgres.WithLivemode(context.Background(), false),
	}
}

func (f meterEditFixture) meter(t *testing.T, eventName string) domain.Meter {
	t.Helper()
	m, err := f.svc.CreateMeter(f.ctx, f.tenantID, pricing.CreateMeterInput{
		EventName: eventName, Name: eventName, Unit: "tokens", Aggregation: "sum",
	})
	if err != nil {
		t.Fatalf("create meter %s: %v", eventName, err)
	}
	return m
}

// plan is a monthly plan with a 1c catch-all price on each meter.
func (f meterEditFixture) plan(t *testing.T, code string, meterIDs ...string) domain.Plan {
	t.Helper()
	var prices []pricing.PriceInput
	for _, id := range meterIDs {
		prices = append(prices, pricing.PriceInput{MeterID: id, PriceShape: domain.PriceShape{Mode: domain.PricingPerUnit, UnitAmountCents: decimal.NewFromInt(1)}})
	}
	p, _, err := f.svc.CreatePlan(f.ctx, f.tenantID, pricing.CreatePlanInput{
		Code: code, Name: code, Currency: "USD", BillingInterval: domain.BillingMonthly, Prices: prices,
	})
	if err != nil {
		t.Fatalf("create plan %s: %v", code, err)
	}
	return p
}

// insertSub writes a customer, a subscription with the given status whose
// current period started at periodStart, and one item on planID, on tx.
func (f meterEditFixture) insertSub(t *testing.T, tx *sql.Tx, planID, status string, periodStart time.Time) string {
	t.Helper()
	custID := postgres.NewID("vlx_cus")
	if _, err := tx.ExecContext(f.ctx, `
		INSERT INTO customers (id, tenant_id, external_id, display_name, created_at, updated_at)
		VALUES ($1, $2, $3, $3, $4, $4)
	`, custID, f.tenantID, "ext_"+custID, periodStart); err != nil {
		t.Fatal(fmt.Errorf("insert customer: %w", err))
	}
	subID := postgres.NewID("vlx_sub")
	if _, err := tx.ExecContext(f.ctx, `
		INSERT INTO subscriptions (
			id, tenant_id, code, display_name, customer_id, status, billing_time,
			current_billing_period_start, current_billing_period_end, next_billing_at,
			created_at, updated_at
		) VALUES ($1, $2, $3, $3, $4, $5, 'anniversary', $6, $7, $7, $6, $6)
	`, subID, f.tenantID, "code_"+subID, custID, status, periodStart, periodStart.AddDate(0, 1, 0)); err != nil {
		t.Fatal(fmt.Errorf("insert sub: %w", err))
	}
	if _, err := tx.ExecContext(f.ctx, `
		INSERT INTO subscription_items (id, tenant_id, subscription_id, plan_id, quantity, metadata, created_at, updated_at)
		VALUES ($1, $2, $3, $4, 1, '{}'::jsonb, $5, $5)
	`, postgres.NewID("vlx_si"), f.tenantID, subID, planID, periodStart); err != nil {
		t.Fatal(fmt.Errorf("insert item: %w", err))
	}
	return subID
}

// sub commits a subscription on planID with the given status, current
// period starting now.
func (f meterEditFixture) sub(t *testing.T, planID, status string) string {
	t.Helper()
	tx := f.begin(t)
	defer postgres.Rollback(tx)
	id := f.insertSub(t, tx, planID, status, time.Now().UTC())
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit sub: %v", err)
	}
	return id
}

func (f meterEditFixture) begin(t *testing.T) *sql.Tx {
	t.Helper()
	tx, err := f.db.BeginTx(f.ctx, postgres.TxTenant, f.tenantID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	return tx
}

func (f meterEditFixture) getMeter(t *testing.T, id string) domain.Meter {
	t.Helper()
	m, err := f.svc.GetMeter(f.ctx, f.tenantID, id)
	if err != nil {
		t.Fatalf("get meter: %v", err)
	}
	return m
}

func ptr(s string) *string { return &s }
