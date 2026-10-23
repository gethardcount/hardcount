package billing_test

import (
	"context"
	"testing"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/pricing"
)

// seedPricingRule writes a meter pricing rule through the store, without the
// service's meter-in-use guard (D05). It is for fixtures that add the rule
// after the subscription exists but mean it to be in place for the whole
// period, as if it had been set up first. The guard itself is tested in
// internal/pricing through pricing.Service.
func seedPricingRule(t *testing.T, db *postgres.DB, ctx context.Context, tenantID string, in pricing.UpsertMeterPricingRuleInput) domain.MeterPricingRule {
	t.Helper()
	rule, err := pricing.NewPostgresStore(db).UpsertMeterPricingRule(ctx, tenantID, domain.MeterPricingRule{
		MeterID:             in.MeterID,
		RatingRuleVersionID: in.RatingRuleVersionID,
		DimensionMatch:      in.DimensionMatch,
		Priority:            in.Priority,
	}, nil)
	if err != nil {
		t.Fatalf("seed pricing rule: %v", err)
	}
	return rule
}
