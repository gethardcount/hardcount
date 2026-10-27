package usage_test

import (
	"context"
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

// The ingest INSERT stamps provider_cost_micros = unit_cost_cents × quantity ×
// 10^4, a BIGINT. A quantity inside its limits, at a rate inside its limits,
// can give a cost that does not fit. The stamp is the operator's cost, not
// what the customer is billed, so such an event is still stored and billed:
// it gets no cost and counts as unresolved, which leaves the margin out
// (ADR-079). Every door shares the INSERT, so each is checked here: a single
// ingest, a batch, a LiteLLM call and a held-usage release.
//
// Mutation-verify: drop the BIGINT range guard in ingestOneTx's stamp → every
// door below fails with a database error.
func TestIngest_ProviderCostTooLargeIsStoredWithNoCost(t *testing.T) {
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
	// 0.003 cents = 30 micros per token. MaxInt64 / 30 = 307445734561825860.2,
	// and MinInt64 / 30 = -307445734561825860.3.
	seedRate(t, db, ctx, tenantID, "anthropic", "claude-3.5-sonnet", "input", "0.003")
	store := usage.NewPostgresStore(db)
	svc := usage.NewService(store)
	dims := map[string]any{"provider": "anthropic", "model": "claude-3.5-sonnet", "token_type": "input"}
	ts := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)
	in := func(qty, key string) usage.IngestInput {
		return usage.IngestInput{
			CustomerID: cust.ID, MeterID: meter.ID, Quantity: decimal.RequireFromString(qty),
			Dimensions: dims, Timestamp: &ts, IdempotencyKey: key,
		}
	}
	type stamp struct {
		qty    string
		micros *int64
		source string
	}
	stored := func(key string) stamp {
		t.Helper()
		tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer postgres.Rollback(tx)
		var s stamp
		if err := tx.QueryRowContext(ctx,
			`SELECT quantity::text, provider_cost_micros, COALESCE(provider_cost_source,'') FROM usage_events WHERE idempotency_key = $1`,
			key).Scan(&s.qty, &s.micros, &s.source); err != nil {
			t.Fatalf("event %s: %v", key, err)
		}
		return s
	}
	wantNoCost := func(key, qty string) {
		t.Helper()
		s := stored(key)
		if !decimal.RequireFromString(s.qty).Equal(decimal.RequireFromString(qty)) || s.micros != nil || s.source != "" {
			t.Errorf("%s: stored %s with cost %v/%q, want %s with no cost (unresolved)", key, s.qty, s.micros, s.source, qty)
		}
	}

	// The largest costs that fit, either sign, are stamped exactly.
	for _, tc := range []struct {
		qty  string
		want int64
	}{
		{"307445734561825860", 9_223_372_036_854_775_800},
		{"-307445734561825860", -9_223_372_036_854_775_800},
	} {
		ev, err := svc.Ingest(ctx, tenantID, in(tc.qty, "edge"+tc.qty))
		if err != nil {
			t.Fatalf("ingest %s: %v", tc.qty, err)
		}
		if ev.ProviderCostMicros == nil || *ev.ProviderCostMicros != tc.want || ev.ProviderCostSource != "table" {
			t.Errorf("edge %s stamp = %v/%s, want %d/table", tc.qty, ev.ProviderCostMicros, ev.ProviderCostSource, tc.want)
		}
	}

	// Past either edge, and far past it, the event is stored with no cost.
	for _, qty := range []string{"307445734561825861", "-307445734561825861", "1000000000000000000000"} {
		ev, err := svc.Ingest(ctx, tenantID, in(qty, "single"+qty))
		if err != nil {
			t.Fatalf("ingest %s: %v", qty, err)
		}
		if ev.ProviderCostMicros != nil || ev.ProviderCostSource != "" {
			t.Errorf("ingest %s answered cost %v/%q, want none", qty, ev.ProviderCostMicros, ev.ProviderCostSource)
		}
		wantNoCost("single"+qty, qty)
	}

	// A batch writes both events.
	if _, batchErrs := svc.BatchIngest(ctx, tenantID, []usage.IngestInput{in("1", "batch-0"), in("307445734561825861", "batch-1")}); len(batchErrs) != 0 {
		t.Fatalf("batch errs = %v, want none", batchErrs)
	}
	if s := stored("batch-0"); s.micros == nil || *s.micros != 30 || s.source != "table" {
		t.Errorf("batch-0 stamp = %v/%q, want 30/table", s.micros, s.source)
	}
	wantNoCost("batch-1", "307445734561825861")

	// A LiteLLM call is written whole.
	res, err := svc.IngestCall(ctx, tenantID, usage.CallInput{
		CustomerID: cust.ID, MeterID: meter.ID,
		Events: []usage.CallEvent{
			{Quantity: decimal.RequireFromString("1000000000000000000000"), Dimensions: dims, IdempotencyKey: "call-1:input", Timestamp: &ts},
		},
	})
	if err != nil {
		t.Fatalf("ingest call: %v", err)
	}
	if res.Inserted != 1 {
		t.Fatalf("call inserted %d, want 1", res.Inserted)
	}
	wantNoCost("call-1:input", "1000000000000000000000")

	// A held row is released, not refused.
	if _, err := svc.HoldCall(ctx, tenantID, usage.HoldCallInput{
		Source:             domain.HeldUsageSourceLiteLLM,
		ExternalCustomerID: "cus_late",
		EventName:          "tokens",
		Reason:             domain.HeldReasonCustomerNotFound,
		Events: []usage.CallEvent{{
			Quantity:       decimal.RequireFromString("1000000000000000000000"),
			Dimensions:     dims,
			IdempotencyKey: "held-1:input",
			Timestamp:      &ts,
		}},
	}); err != nil {
		t.Fatalf("hold: %v", err)
	}
	late, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{ExternalID: "cus_late", DisplayName: "Late"})
	if err != nil {
		t.Fatalf("late customer: %v", err)
	}
	rel, err := svc.Release(ctx, tenantID, usage.ReleaseInput{
		ExternalCustomerID: "cus_late", CustomerID: late.ID,
		MeterIDs: map[string]string{"tokens": meter.ID},
	})
	if err != nil {
		t.Fatalf("release: %v", err)
	}
	if rel.Released != 1 {
		t.Fatalf("release = %+v, want 1 released", rel)
	}
	wantNoCost("held-1:input", "1000000000000000000000")

	// The margin report counts each uncosted event as unresolved.
	from, to := ts.Add(-time.Hour), ts.Add(time.Hour)
	if _, unresolved, err := store.CustomerProviderCost(ctx, tenantID, cust.ID, from, to); err != nil || unresolved != 5 {
		t.Errorf("cus_big unresolved = %d (err %v), want 5", unresolved, err)
	}
	if _, unresolved, err := store.CustomerProviderCost(ctx, tenantID, late.ID, from, to); err != nil || unresolved != 1 {
		t.Errorf("cus_late unresolved = %d (err %v), want 1", unresolved, err)
	}
}
