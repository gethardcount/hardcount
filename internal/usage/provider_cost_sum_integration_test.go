package usage_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/pricing"
	"github.com/sagarsuperuser/velox/internal/testutil"
	"github.com/sagarsuperuser/velox/internal/usage"
)

// The margin report adds each model's provider_cost_micros. Postgres adds
// them as numeric; the total is range-checked against int64 in Go, so a sum
// that does not fit is ErrAmountOverflow, and a sum up to MaxInt64 is
// returned exactly. Costs this large (about $9.2 trillion) are not realistic.
func TestCustomerProviderCost_SumRangeChecked(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Provider Cost Sum")
	meter, err := pricing.NewPostgresStore(db).CreateMeter(ctx, tenantID, domain.Meter{
		EventName: "tokens", Name: "Tokens", Aggregation: "sum", Unit: "tokens",
	})
	if err != nil {
		t.Fatalf("meter: %v", err)
	}
	// 30 micros per token.
	seedRate(t, db, ctx, tenantID, "anthropic", "claude-3.5-sonnet", "input", "0.00003")
	store := usage.NewPostgresStore(db)
	svc := usage.NewService(store)
	dims := map[string]any{"provider": "anthropic", "model": "claude-3.5-sonnet", "token_type": "input"}
	ts := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)
	from, to := ts.Add(-time.Hour), ts.Add(time.Hour)

	costOf := func(ext string, qtys ...string) ([]usage.CustomerCostByModel, error) {
		t.Helper()
		cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{ExternalID: ext, DisplayName: ext})
		if err != nil {
			t.Fatalf("customer: %v", err)
		}
		for i, q := range qtys {
			if _, err := svc.Ingest(ctx, tenantID, usage.IngestInput{
				CustomerID: cust.ID, MeterID: meter.ID, Quantity: decimal.RequireFromString(q),
				Dimensions: dims, Timestamp: &ts, IdempotencyKey: ext + string(rune('a'+i)),
			}); err != nil {
				t.Fatalf("ingest %s: %v", q, err)
			}
		}
		byModel, _, err := store.CustomerProviderCost(ctx, tenantID, cust.ID, from, to)
		return byModel, err
	}

	// 2 × 153722867280912930 tokens × 30 = 9223372036854775800 micros: fits.
	byModel, err := costOf("cus_fits", "153722867280912930", "153722867280912930")
	if err != nil {
		t.Fatalf("cost that fits: %v", err)
	}
	if len(byModel) != 1 || byModel[0].CostMicros != 9_223_372_036_854_775_800 || byModel[0].Events != 2 {
		t.Errorf("cost by model = %+v, want one model at 9223372036854775800 over 2 events", byModel)
	}

	// 2 × 166666666666666666 × 30 = 9999999999999999960 micros: each event's
	// cost fits, the sum does not.
	if _, err := costOf("cus_past", "166666666666666666", "166666666666666666"); !errors.Is(err, domain.ErrAmountOverflow) {
		t.Errorf("cost past int64: err = %v, want ErrAmountOverflow", err)
	}
}
