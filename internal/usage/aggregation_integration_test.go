package usage_test

import (
	"context"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
	"github.com/sagarsuperuser/velox/internal/usage"
)

// TestAggregateClaims is the runtime contract test for the claim
// aggregation (docs/dev/design/multi-dim-meters.md, ADR-130 §2). Each
// sub-test exercises one piece of it:
//
//   - the meter's one aggregation: sum, count, max, last
//   - the claim: the first matching claim in rank order wins, no double-count
//   - subset-match: extra dimensions on the event do not disqualify it
//   - unclaimed: events matching no claim become the unclaimed bucket
//   - decimal precision: NUMERIC(38,12) round-trips losslessly
//
// The same Postgres schema is shared across sub-tests, but each writes a
// fresh tenant so RLS keeps them isolated.
func TestAggregateClaims(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx, cancel := context.WithTimeout(postgres.WithLivemode(context.Background(), false), 30*time.Second)
	defer cancel()

	store := usage.NewPostgresStore(db)
	aggregate := func(t *testing.T, tenantID, customerID, meterID, agg string, claims []domain.UsageClaim, from, to time.Time) []domain.ClaimedUsage {
		t.Helper()
		got, err := store.AggregateClaims(ctx, tenantID, customerID, meterID, agg, claims, from, to)
		if err != nil {
			t.Fatalf("aggregate: %v", err)
		}
		return got
	}

	t.Run("sum adds the claimed events", func(t *testing.T) {
		tenantID := testutil.CreateTestTenant(t, db, "Agg Sum")
		customerID := insertTestCustomer(t, db, tenantID, "cus_sum")
		meterID := insertTestMeter(t, db, tenantID, "mtr_sum", "tokens_sum")
		rrvID := insertTestRatingRule(t, db, tenantID, "rate_sum")
		c := insertTestPricingRule(t, db, tenantID, meterID, rrvID, map[string]any{"model": "gpt-4"}, 100)

		from := time.Now().Add(-1 * time.Hour)
		to := time.Now().Add(1 * time.Hour)

		ingest(t, ctx, store, tenantID, customerID, meterID, decimal.NewFromInt(10), map[string]any{"model": "gpt-4"})
		ingest(t, ctx, store, tenantID, customerID, meterID, decimal.NewFromInt(20), map[string]any{"model": "gpt-4"})
		ingest(t, ctx, store, tenantID, customerID, meterID, decimal.NewFromInt(99), map[string]any{"model": "claude"})

		assertRuleAggregations(t, aggregate(t, tenantID, customerID, meterID, "sum", []domain.UsageClaim{c}, from, to), map[string]decimal.Decimal{
			rrvID: decimal.NewFromInt(30), // gpt-4 events
			"":    decimal.NewFromInt(99), // unclaimed
		})
	})

	t.Run("count counts the claimed events, and the unclaimed ones too", func(t *testing.T) {
		tenantID := testutil.CreateTestTenant(t, db, "Agg Count")
		customerID := insertTestCustomer(t, db, tenantID, "cus_cnt")
		meterID := insertTestMeter(t, db, tenantID, "mtr_cnt", "tokens_cnt")
		rrvID := insertTestRatingRule(t, db, tenantID, "rate_cnt")
		c := insertTestPricingRule(t, db, tenantID, meterID, rrvID, map[string]any{"model": "gpt-4"}, 100)

		from := time.Now().Add(-1 * time.Hour)
		to := time.Now().Add(1 * time.Hour)

		ingest(t, ctx, store, tenantID, customerID, meterID, decimal.NewFromInt(10), map[string]any{"model": "gpt-4"})
		ingest(t, ctx, store, tenantID, customerID, meterID, decimal.NewFromInt(50), map[string]any{"model": "gpt-4"})
		ingest(t, ctx, store, tenantID, customerID, meterID, decimal.NewFromInt(99), map[string]any{"model": "gpt-4"})
		ingest(t, ctx, store, tenantID, customerID, meterID, decimal.NewFromInt(7), map[string]any{"model": "claude"})

		assertRuleAggregations(t, aggregate(t, tenantID, customerID, meterID, "count", []domain.UsageClaim{c}, from, to), map[string]decimal.Decimal{
			rrvID: decimal.NewFromInt(3), // count, not sum
			"":    decimal.NewFromInt(1),
		})
	})

	t.Run("max takes each bucket's largest quantity", func(t *testing.T) {
		tenantID := testutil.CreateTestTenant(t, db, "Agg Max")
		customerID := insertTestCustomer(t, db, tenantID, "cus_max")
		meterID := insertTestMeter(t, db, tenantID, "mtr_max", "tokens_max")
		rrvID := insertTestRatingRule(t, db, tenantID, "rate_max")
		c := insertTestPricingRule(t, db, tenantID, meterID, rrvID, map[string]any{"model": "gpt-4"}, 100)

		from := time.Now().Add(-1 * time.Hour)
		to := time.Now().Add(1 * time.Hour)

		ingest(t, ctx, store, tenantID, customerID, meterID, decimal.NewFromInt(10), map[string]any{"model": "gpt-4"})
		ingest(t, ctx, store, tenantID, customerID, meterID, decimal.NewFromInt(50), map[string]any{"model": "gpt-4"})
		ingest(t, ctx, store, tenantID, customerID, meterID, decimal.NewFromInt(25), map[string]any{"model": "gpt-4"})
		ingest(t, ctx, store, tenantID, customerID, meterID, decimal.NewFromInt(80), map[string]any{"model": "claude"})
		ingest(t, ctx, store, tenantID, customerID, meterID, decimal.NewFromInt(30), map[string]any{"model": "claude"})

		assertRuleAggregations(t, aggregate(t, tenantID, customerID, meterID, "max", []domain.UsageClaim{c}, from, to), map[string]decimal.Decimal{
			rrvID: decimal.NewFromInt(50),
			"":    decimal.NewFromInt(80),
		})
	})

	t.Run("last takes the latest event in the window", func(t *testing.T) {
		tenantID := testutil.CreateTestTenant(t, db, "Agg Last")
		customerID := insertTestCustomer(t, db, tenantID, "cus_ldp")
		meterID := insertTestMeter(t, db, tenantID, "mtr_ldp", "tokens_ldp")
		rrvID := insertTestRatingRule(t, db, tenantID, "rate_ldp")
		c := insertTestPricingRule(t, db, tenantID, meterID, rrvID, map[string]any{"model": "gpt-4"}, 100)

		now := time.Now()
		from := now.Add(-2 * time.Hour)
		to := now.Add(-2 * time.Minute)

		// Three in-window events at distinct timestamps; latest qty=42.
		// One after the window, the latest of all, is not in it.
		ingestAt(t, ctx, store, tenantID, customerID, meterID, decimal.NewFromInt(10), map[string]any{"model": "gpt-4"}, now.Add(-90*time.Minute))
		ingestAt(t, ctx, store, tenantID, customerID, meterID, decimal.NewFromInt(99), map[string]any{"model": "gpt-4"}, now.Add(-30*time.Minute))
		ingestAt(t, ctx, store, tenantID, customerID, meterID, decimal.NewFromInt(42), map[string]any{"model": "gpt-4"}, now.Add(-5*time.Minute))
		ingestAt(t, ctx, store, tenantID, customerID, meterID, decimal.NewFromInt(7), map[string]any{"model": "gpt-4"}, now.Add(-1*time.Minute))

		assertRuleAggregations(t, aggregate(t, tenantID, customerID, meterID, "last", []domain.UsageClaim{c}, from, to), map[string]decimal.Decimal{
			rrvID: decimal.NewFromInt(42),
		})
	})

	t.Run("the first matching claim wins when claims overlap; no double-count", func(t *testing.T) {
		tenantID := testutil.CreateTestTenant(t, db, "Agg Priority")
		customerID := insertTestCustomer(t, db, tenantID, "cus_prio")
		meterID := insertTestMeter(t, db, tenantID, "mtr_prio", "tokens_prio")
		highRRV := insertTestRatingRule(t, db, tenantID, "rate_high")
		lowRRV := insertTestRatingRule(t, db, tenantID, "rate_low")

		// Specific rule (priority 100): cached gpt-4 events at premium.
		// Coarse rule (priority 50): all gpt-4 events at base rate.
		// An event matching both is claimed by the first claim, the
		// priority-100 rule.
		high := insertTestPricingRule(t, db, tenantID, meterID, highRRV, map[string]any{"model": "gpt-4", "cached": "true"}, 100)
		low := insertTestPricingRule(t, db, tenantID, meterID, lowRRV, map[string]any{"model": "gpt-4"}, 50)

		from := time.Now().Add(-1 * time.Hour)
		to := time.Now().Add(1 * time.Hour)

		ingest(t, ctx, store, tenantID, customerID, meterID, decimal.NewFromInt(10), map[string]any{"model": "gpt-4", "cached": "true"})
		ingest(t, ctx, store, tenantID, customerID, meterID, decimal.NewFromInt(20), map[string]any{"model": "gpt-4", "cached": "false"})
		ingest(t, ctx, store, tenantID, customerID, meterID, decimal.NewFromInt(5), map[string]any{"model": "gpt-4", "cached": "true"})

		assertRuleAggregations(t, aggregate(t, tenantID, customerID, meterID, "sum", []domain.UsageClaim{high, low}, from, to), map[string]decimal.Decimal{
			highRRV: decimal.NewFromInt(15), // 10 + 5 cached gpt-4
			lowRRV:  decimal.NewFromInt(20), // 20 uncached gpt-4
		})
		// The claim order is the caller's: the other order gives the coarse
		// claim every gpt-4 event.
		assertRuleAggregations(t, aggregate(t, tenantID, customerID, meterID, "sum", []domain.UsageClaim{low, high}, from, to), map[string]decimal.Decimal{
			lowRRV: decimal.NewFromInt(35),
		})
	})

	t.Run("subset match: extra event dimensions do not disqualify", func(t *testing.T) {
		tenantID := testutil.CreateTestTenant(t, db, "Agg Subset")
		customerID := insertTestCustomer(t, db, tenantID, "cus_subset")
		meterID := insertTestMeter(t, db, tenantID, "mtr_subset", "tokens_subset")
		rrvID := insertTestRatingRule(t, db, tenantID, "rate_subset")
		// Rule asks for {model: gpt-4} only — events with extra
		// dimensions (operation, cached, region) must still match.
		c := insertTestPricingRule(t, db, tenantID, meterID, rrvID, map[string]any{"model": "gpt-4"}, 100)

		from := time.Now().Add(-1 * time.Hour)
		to := time.Now().Add(1 * time.Hour)

		ingest(t, ctx, store, tenantID, customerID, meterID, decimal.NewFromInt(10),
			map[string]any{"model": "gpt-4", "operation": "input", "cached": false, "region": "us-east"})
		ingest(t, ctx, store, tenantID, customerID, meterID, decimal.NewFromInt(20),
			map[string]any{"model": "gpt-4", "operation": "output", "cached": true})

		assertRuleAggregations(t, aggregate(t, tenantID, customerID, meterID, "sum", []domain.UsageClaim{c}, from, to), map[string]decimal.Decimal{
			rrvID: decimal.NewFromInt(30),
		})
	})

	t.Run("no claims: every event lands in the unclaimed bucket", func(t *testing.T) {
		tenantID := testutil.CreateTestTenant(t, db, "Agg NoRules")
		customerID := insertTestCustomer(t, db, tenantID, "cus_nor")
		meterID := insertTestMeter(t, db, tenantID, "mtr_nor", "tokens_nor")

		from := time.Now().Add(-1 * time.Hour)
		to := time.Now().Add(1 * time.Hour)

		ingest(t, ctx, store, tenantID, customerID, meterID, decimal.NewFromInt(40), map[string]any{"model": "gpt-4"})
		ingest(t, ctx, store, tenantID, customerID, meterID, decimal.NewFromInt(2), map[string]any{"model": "claude"})

		assertRuleAggregations(t, aggregate(t, tenantID, customerID, meterID, "sum", nil, from, to), map[string]decimal.Decimal{
			"": decimal.NewFromInt(42),
		})
	})

	t.Run("an aggregation the meter cannot have is refused", func(t *testing.T) {
		tenantID := testutil.CreateTestTenant(t, db, "Agg Bad")
		customerID := insertTestCustomer(t, db, tenantID, "cus_bad")
		meterID := insertTestMeter(t, db, tenantID, "mtr_bad", "tokens_bad")
		for _, agg := range []string{"last_ever", "last_during_period", ""} {
			if _, err := store.AggregateClaims(ctx, tenantID, customerID, meterID, agg, nil, time.Now().Add(-time.Hour), time.Now()); err == nil {
				t.Errorf("aggregation %q: want an error", agg)
			}
		}
	})

	t.Run("decimal precision round-trips through aggregation", func(t *testing.T) {
		tenantID := testutil.CreateTestTenant(t, db, "Agg Decimal")
		customerID := insertTestCustomer(t, db, tenantID, "cus_dec")
		meterID := insertTestMeter(t, db, tenantID, "mtr_dec", "tokens_dec")
		rrvID := insertTestRatingRule(t, db, tenantID, "rate_dec")
		c := insertTestPricingRule(t, db, tenantID, meterID, rrvID, map[string]any{"model": "gpt-4"}, 100)

		from := time.Now().Add(-1 * time.Hour)
		to := time.Now().Add(1 * time.Hour)

		// Use 12-decimal-place quantities — the column is NUMERIC(38, 12).
		q1, _ := decimal.NewFromString("1234.567890123456")
		q2, _ := decimal.NewFromString("0.000000000001")
		want, _ := decimal.NewFromString("1234.567890123457")
		ingest(t, ctx, store, tenantID, customerID, meterID, q1, map[string]any{"model": "gpt-4"})
		ingest(t, ctx, store, tenantID, customerID, meterID, q2, map[string]any{"model": "gpt-4"})

		var foundRule decimal.Decimal
		for _, r := range aggregate(t, tenantID, customerID, meterID, "sum", []domain.UsageClaim{c}, from, to) {
			if r.PriceID == rrvID {
				foundRule = r.Quantity
			}
		}
		if !foundRule.Equal(want) {
			t.Errorf("decimal precision: got %s, want %s", foundRule.String(), want.String())
		}
	})
}

// ---- helpers --------------------------------------------------------------

func ingest(t *testing.T, ctx context.Context, store *usage.PostgresStore,
	tenantID, customerID, meterID string, qty decimal.Decimal, props map[string]any,
) {
	t.Helper()
	svc := usage.NewService(store)
	if _, err := svc.Ingest(ctx, tenantID, usage.IngestInput{
		CustomerID: customerID, MeterID: meterID,
		Quantity: qty, Dimensions: props,
	}); err != nil {
		t.Fatalf("ingest: %v", err)
	}
}

func ingestAt(t *testing.T, ctx context.Context, store *usage.PostgresStore,
	tenantID, customerID, meterID string, qty decimal.Decimal, props map[string]any, ts time.Time,
) {
	t.Helper()
	svc := usage.NewService(store)
	if _, err := svc.Ingest(ctx, tenantID, usage.IngestInput{
		CustomerID: customerID, MeterID: meterID,
		Quantity: qty, Dimensions: props, Timestamp: &ts,
	}); err != nil {
		t.Fatalf("ingestAt: %v", err)
	}
}

func assertRuleAggregations(t *testing.T, got []domain.ClaimedUsage, want map[string]decimal.Decimal) {
	t.Helper()
	gotByRRV := make(map[string]decimal.Decimal, len(got))
	for _, r := range got {
		// Key by price id; "" is the unclaimed bucket.
		gotByRRV[r.PriceID] = r.Quantity
	}
	if len(gotByRRV) != len(want) {
		t.Errorf("aggregation count: got %d entries (%v), want %d (%v)",
			len(gotByRRV), gotByRRV, len(want), want)
	}
	for rrv, exp := range want {
		actual, ok := gotByRRV[rrv]
		if !ok {
			t.Errorf("missing rrv=%q in result; got %v", rrv, gotByRRV)
			continue
		}
		if !actual.Equal(exp) {
			t.Errorf("rrv=%q: got %s, want %s", rrv, actual.String(), exp.String())
		}
	}
}

// insertTestRatingRule names a price for a claim. A claim is in memory
// (AggregateClaims takes the list), so nothing is stored: the id is what the
// buckets come back keyed by.
func insertTestRatingRule(t *testing.T, _ *postgres.DB, _, key string) string {
	t.Helper()
	return "hc_prc_" + key
}

// insertTestPricingRule is the claim of price priceID on dims. Claims are
// listed in the order the caller passes them; priority is not read.
func insertTestPricingRule(t *testing.T, _ *postgres.DB,
	_, _, priceID string,
	dims map[string]any, _ int,
) domain.UsageClaim {
	t.Helper()
	match := map[string]string{}
	for k, v := range dims {
		s, ok := v.(string)
		if !ok {
			t.Fatalf("claim label %q is %T, want a string", k, v)
		}
		match[k] = s
	}
	return domain.UsageClaim{PriceID: priceID, Match: match}
}
