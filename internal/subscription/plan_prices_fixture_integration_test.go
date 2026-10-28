package subscription

import (
	"context"
	"testing"

	"github.com/shopspring/decimal"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/pricing"
)

// createPlanPricing creates the plan with a 1c catch-all price on each
// meter, so the plan prices those meters (ADR-130 §1: a plan's meters are
// its prices' meters). A meter id the tenant has no meter for is created
// with that id: the fixtures name meters by id.
func createPlanPricing(t *testing.T, db *postgres.DB, ctx context.Context, tenantID string, plan domain.Plan, meterIDs ...string) domain.Plan {
	t.Helper()
	tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	for _, id := range meterIDs {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO meters (id, tenant_id, event_name, name, unit, aggregation)
			VALUES ($1, $2, $1, $1, 'units', 'sum')
			ON CONFLICT (id) DO NOTHING`, id, tenantID); err != nil {
			t.Fatalf("create meter %s: %v", id, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit meters: %v", err)
	}
	adds := make([]pricing.PriceChange, 0, len(meterIDs))
	for _, id := range meterIDs {
		adds = append(adds, pricing.PriceChange{Op: pricing.PriceAdd, MeterID: id,
			Shape: domain.PriceShape{Mode: domain.PricingPerUnit, UnitAmountCents: decimal.NewFromInt(1)}})
	}
	created, _, err := pricing.NewPostgresStore(db).CreatePlanWithPrices(ctx, tenantID, plan, adds)
	if err != nil {
		t.Fatalf("create plan %s: %v", plan.Code, err)
	}
	return created
}
