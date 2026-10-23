package usage_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/pricing"
	"github.com/sagarsuperuser/velox/internal/testutil"
	"github.com/sagarsuperuser/velox/internal/usage"
)

// The ingest INSERT stamps provider_cost_micros = cost_per_token × quantity ×
// 10^6, a BIGINT. A quantity inside its limits, at a rate inside its limits,
// can give a cost that does not fit. Such an event is refused with a 422
// naming quantity (in a batch, the batch is refused and nothing is written);
// before, the INSERT failed with a raw Postgres error.
func TestIngest_ProviderCostPastBigintRefusedNamingQuantity(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Provider Cost Overflow")
	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{ExternalID: "cus_big", DisplayName: "Big"})
	if err != nil {
		t.Fatalf("customer: %v", err)
	}
	meter, err := pricing.NewPostgresStore(db).CreateMeter(ctx, tenantID, domain.Meter{
		EventName: "tokens", Name: "Tokens", Aggregation: "sum", Unit: "tokens",
	})
	if err != nil {
		t.Fatalf("meter: %v", err)
	}
	// 0.00003 dollars = 30 micros per token. MaxInt64 / 30 = 307445734561825860.2.
	seedRate(t, db, ctx, tenantID, "anthropic", "claude-3.5-sonnet", "input", "0.00003")
	svc := usage.NewService(usage.NewPostgresStore(db))
	dims := map[string]any{"provider": "anthropic", "model": "claude-3.5-sonnet", "token_type": "input"}
	ts := time.Now().UTC().Add(-time.Minute)
	in := func(qty, key string) usage.IngestInput {
		return usage.IngestInput{
			CustomerID: cust.ID, MeterID: meter.ID, Quantity: decimal.RequireFromString(qty),
			Dimensions: dims, Timestamp: &ts, IdempotencyKey: key,
		}
	}
	count := func() int {
		t.Helper()
		tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer postgres.Rollback(tx)
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM usage_events`).Scan(&n); err != nil {
			t.Fatalf("count: %v", err)
		}
		return n
	}

	// The largest quantity whose cost fits is stamped exactly.
	ev, err := svc.Ingest(ctx, tenantID, in("307445734561825860", "fits"))
	if err != nil {
		t.Fatalf("ingest at the edge: %v", err)
	}
	if ev.ProviderCostMicros == nil || *ev.ProviderCostMicros != 9_223_372_036_854_775_800 || ev.ProviderCostSource != "table" {
		t.Fatalf("edge stamp = %v/%s, want 9223372036854775800/table", ev.ProviderCostMicros, ev.ProviderCostSource)
	}

	// One more token, and a quantity far inside the quantity limit, are refused.
	for _, qty := range []string{"307445734561825861", "1000000000000000000000"} {
		_, err := svc.Ingest(ctx, tenantID, in(qty, "single-"+qty))
		if !errors.Is(err, errs.ErrValidation) || errs.Field(err) != "quantity" {
			t.Errorf("ingest %s: err = %v, want a validation error naming quantity", qty, err)
		}
	}

	// In a batch the second event is named and nothing is written.
	_, batchErrs := svc.BatchIngest(ctx, tenantID, []usage.IngestInput{in("1", "batch-0"), in("307445734561825861", "batch-1")})
	if len(batchErrs) != 1 || !errors.Is(batchErrs[0], errs.ErrValidation) || !strings.Contains(batchErrs[0].Error(), "event[1]") {
		t.Errorf("batch errs = %v, want one validation error naming event[1]", batchErrs)
	}
	if n := count(); n != 1 {
		t.Errorf("%d events stored, want 1 (only the edge event)", n)
	}
}
