package billing_test

import (
	"context"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/pricing"
)

// Plan price fixtures for the billing integration tests (ADR-130).

// priceOn is one price a fixture puts on a plan: a meter, the events it
// takes (nil: every event) and its shape.
type priceOn struct {
	meterID string
	match   map[string]string
	shape   domain.PriceShape
}

// perUnitShape is a per_unit shape at cents per unit.
func perUnitShape(cents string) domain.PriceShape {
	return domain.PriceShape{Mode: domain.PricingPerUnit, UnitAmountCents: decimal.RequireFromString(cents)}
}

// catchAll is a per_unit price of meterID that takes every event.
func catchAll(meterID, cents string) priceOn {
	return priceOn{meterID: meterID, shape: perUnitShape(cents)}
}

// createPlanWithPrices writes the plan and its prices through the store,
// as POST /v1/plans does, and returns the plan and its prices in order.
func createPlanWithPrices(t *testing.T, db *postgres.DB, ctx context.Context, tenantID string, plan domain.Plan, prices ...priceOn) (domain.Plan, []domain.PlanPrice) {
	t.Helper()
	if plan.Status == "" {
		plan.Status = domain.PlanActive
	}
	adds := make([]pricing.PriceChange, 0, len(prices))
	for _, p := range prices {
		adds = append(adds, pricing.PriceChange{Op: pricing.PriceAdd, MeterID: p.meterID, Match: p.match, Shape: p.shape})
	}
	created, out, err := pricing.NewPostgresStore(db).CreatePlanWithPrices(ctx, tenantID, plan, adds)
	if err != nil {
		t.Fatalf("create plan %s with prices: %v", plan.Code, err)
	}
	return created, out
}

// addPrice adds a price to a plan through the store's chokepoint: at once on
// a plan no subscription used, from each subscriber's next period on a used
// one.
func addPrice(t *testing.T, db *postgres.DB, ctx context.Context, tenantID, planID string, p priceOn) domain.PlanPrice {
	t.Helper()
	pw, err := pricing.NewPostgresStore(db).WritePrice(ctx, tenantID, planID, pricing.PriceChange{Op: pricing.PriceAdd, MeterID: p.meterID, Match: p.match, Shape: p.shape}, nil)
	if err != nil {
		t.Fatalf("add price to plan %s: %v", planID, err)
	}
	return pw.Price
}

// editPrice changes a price's content through the store's chokepoint.
func editPrice(t *testing.T, db *postgres.DB, ctx context.Context, tenantID, planID, priceID string, shape domain.PriceShape) pricing.PriceWrite {
	t.Helper()
	pw, err := pricing.NewPostgresStore(db).WritePrice(ctx, tenantID, planID, pricing.PriceChange{Op: pricing.PriceEdit, PriceID: priceID, Shape: shape}, nil)
	if err != nil {
		t.Fatalf("edit price %s: %v", priceID, err)
	}
	return pw
}

// backdatePriceVersions moves every version of the price to valid_from at,
// on a fixture that means the price to be in force from then on, as if it
// had been written then. at is wall time.
func backdatePriceVersions(t *testing.T, db *postgres.DB, ctx context.Context, priceID string, at time.Time) {
	t.Helper()
	tx, err := db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin bypass: %v", err)
	}
	defer postgres.Rollback(tx)
	if _, err := tx.ExecContext(ctx, `UPDATE plan_price_versions SET valid_from = $1 WHERE price_id = $2`, at, priceID); err != nil {
		t.Fatalf("backdate versions of price %s: %v", priceID, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit backdate: %v", err)
	}
}

// backdateLatestVersion moves the price's latest version to valid_from at:
// an edit a fixture means to be in force from then on, as if saved then.
func backdateLatestVersion(t *testing.T, db *postgres.DB, ctx context.Context, priceID string, at time.Time) {
	t.Helper()
	tx, err := db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin bypass: %v", err)
	}
	defer postgres.Rollback(tx)
	if _, err := tx.ExecContext(ctx, `
		UPDATE plan_price_versions SET valid_from = $1
		WHERE id = (SELECT id FROM plan_price_versions WHERE price_id = $2 ORDER BY valid_from DESC LIMIT 1)`, at, priceID); err != nil {
		t.Fatalf("backdate the latest version of price %s: %v", priceID, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit backdate: %v", err)
	}
}

// namedShape is a price shape with the name a golden prints for its lines.
type namedShape struct {
	name  string
	shape domain.PriceShape
}

func flatRule(name, cents string) namedShape {
	return namedShape{name, perUnitShape(cents)}
}

func gradRule(name string, tiers []domain.RatingTier) namedShape {
	return namedShape{name, domain.PriceShape{Mode: domain.PricingGraduated, Tiers: tiers}}
}

func packageRule(name string, size int64, cents string) namedShape {
	return namedShape{name, domain.PriceShape{Mode: domain.PricingPackage, PackageSize: size, PackageAmountCents: decimal.RequireFromString(cents)}}
}

// createRule adds a price for match on the fixture's meter, in force from
// before the period opened (the fixture's plan is used, so a price added now
// would take part only from the next period).
func createRule(t *testing.T, lf *liveFixture, match map[string]string, ns namedShape) domain.PlanPrice {
	t.Helper()
	p := addPrice(t, lf.db, lf.ctx, lf.tenantID, lf.planID, priceOn{meterID: lf.meterID, match: match, shape: ns.shape})
	backdatePriceVersions(t, lf.db, lf.ctx, p.ID, lf.cycleStart.Add(-2*time.Hour))
	return p
}

// publishCallsRule edits the fixture's catch-all price to ns, in force from
// before the period opened. The price keeps its id.
func publishCallsRule(t *testing.T, lf *liveFixture, ns namedShape) domain.PlanPrice {
	t.Helper()
	pw := editPrice(t, lf.db, lf.ctx, lf.tenantID, lf.planID, lf.priceID, ns.shape)
	backdateLatestVersion(t, lf.db, lf.ctx, lf.priceID, lf.cycleStart.Add(-2*time.Hour))
	return pw.Price
}
