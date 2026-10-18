package usage_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
	"github.com/sagarsuperuser/velox/internal/usage"
)

// TestAggregateByPricingRules is the runtime contract test for
// docs/design/multi-dim-meters.md priority+claim resolution. Each
// sub-test exercises one piece of the algorithm:
//
//   - per-mode aggregation: sum, count, max, last_during_period, last_ever
//   - priority+claim: the top-priority matching rule wins, no double-count
//   - subset-match: extra dimensions on the event do not disqualify it
//   - default fallback: events matching no rule become the unclaimed bucket
//   - decimal precision: NUMERIC(38,12) round-trips losslessly
//
// The same Postgres schema is shared across sub-tests, but each writes a
// fresh tenant so RLS keeps them isolated and the priority counters can
// be assigned freely.
func TestAggregateByPricingRules(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx, cancel := context.WithTimeout(postgres.WithLivemode(context.Background(), false), 30*time.Second)
	defer cancel()

	store := usage.NewPostgresStore(db)

	t.Run("sum mode aggregates claimed events", func(t *testing.T) {
		tenantID := testutil.CreateTestTenant(t, db, "Agg Sum")
		customerID := insertTestCustomer(t, db, tenantID, "cus_sum")
		meterID := insertTestMeter(t, db, tenantID, "mtr_sum", "tokens_sum")
		rrvID := insertTestRatingRule(t, db, tenantID, "rate_sum")
		insertTestPricingRule(t, db, tenantID, meterID, rrvID,
			map[string]any{"model": "gpt-4"}, domain.AggSum, 100)

		from := time.Now().Add(-1 * time.Hour)
		to := time.Now().Add(1 * time.Hour)

		ingest(t, ctx, store, tenantID, customerID, meterID, decimal.NewFromInt(10), map[string]any{"model": "gpt-4"})
		ingest(t, ctx, store, tenantID, customerID, meterID, decimal.NewFromInt(20), map[string]any{"model": "gpt-4"})
		ingest(t, ctx, store, tenantID, customerID, meterID, decimal.NewFromInt(99), map[string]any{"model": "claude"})

		got, err := store.AggregateByPricingRules(ctx, tenantID, customerID, meterID, domain.AggSum, from, to)
		if err != nil {
			t.Fatalf("aggregate: %v", err)
		}
		want := map[string]decimal.Decimal{
			rrvID: decimal.NewFromInt(30), // gpt-4 events
			"":    decimal.NewFromInt(99), // unclaimed default
		}
		assertRuleAggregations(t, got, want)
	})

	t.Run("count mode counts claimed events", func(t *testing.T) {
		tenantID := testutil.CreateTestTenant(t, db, "Agg Count")
		customerID := insertTestCustomer(t, db, tenantID, "cus_cnt")
		meterID := insertTestMeter(t, db, tenantID, "mtr_cnt", "tokens_cnt")
		rrvID := insertTestRatingRule(t, db, tenantID, "rate_cnt")
		insertTestPricingRule(t, db, tenantID, meterID, rrvID,
			map[string]any{"model": "gpt-4"}, domain.AggCount, 100)

		from := time.Now().Add(-1 * time.Hour)
		to := time.Now().Add(1 * time.Hour)

		ingest(t, ctx, store, tenantID, customerID, meterID, decimal.NewFromInt(10), map[string]any{"model": "gpt-4"})
		ingest(t, ctx, store, tenantID, customerID, meterID, decimal.NewFromInt(50), map[string]any{"model": "gpt-4"})
		ingest(t, ctx, store, tenantID, customerID, meterID, decimal.NewFromInt(99), map[string]any{"model": "gpt-4"})

		got, err := store.AggregateByPricingRules(ctx, tenantID, customerID, meterID, domain.AggSum, from, to)
		if err != nil {
			t.Fatalf("aggregate: %v", err)
		}
		assertRuleAggregations(t, got, map[string]decimal.Decimal{
			rrvID: decimal.NewFromInt(3), // count, not sum
		})
	})

	t.Run("max mode picks largest claimed quantity", func(t *testing.T) {
		tenantID := testutil.CreateTestTenant(t, db, "Agg Max")
		customerID := insertTestCustomer(t, db, tenantID, "cus_max")
		meterID := insertTestMeter(t, db, tenantID, "mtr_max", "tokens_max")
		rrvID := insertTestRatingRule(t, db, tenantID, "rate_max")
		insertTestPricingRule(t, db, tenantID, meterID, rrvID,
			map[string]any{"model": "gpt-4"}, domain.AggMax, 100)

		from := time.Now().Add(-1 * time.Hour)
		to := time.Now().Add(1 * time.Hour)

		ingest(t, ctx, store, tenantID, customerID, meterID, decimal.NewFromInt(10), map[string]any{"model": "gpt-4"})
		ingest(t, ctx, store, tenantID, customerID, meterID, decimal.NewFromInt(50), map[string]any{"model": "gpt-4"})
		ingest(t, ctx, store, tenantID, customerID, meterID, decimal.NewFromInt(25), map[string]any{"model": "gpt-4"})

		got, err := store.AggregateByPricingRules(ctx, tenantID, customerID, meterID, domain.AggSum, from, to)
		if err != nil {
			t.Fatalf("aggregate: %v", err)
		}
		assertRuleAggregations(t, got, map[string]decimal.Decimal{
			rrvID: decimal.NewFromInt(50),
		})
	})

	t.Run("last_during_period picks latest event in window", func(t *testing.T) {
		tenantID := testutil.CreateTestTenant(t, db, "Agg LDP")
		customerID := insertTestCustomer(t, db, tenantID, "cus_ldp")
		meterID := insertTestMeter(t, db, tenantID, "mtr_ldp", "tokens_ldp")
		rrvID := insertTestRatingRule(t, db, tenantID, "rate_ldp")
		insertTestPricingRule(t, db, tenantID, meterID, rrvID,
			map[string]any{"model": "gpt-4"}, domain.AggLastDuringPeriod, 100)

		now := time.Now()
		from := now.Add(-2 * time.Hour)
		to := now.Add(1 * time.Hour)

		// Three in-period events at distinct timestamps; latest qty=42.
		ts1 := now.Add(-90 * time.Minute)
		ts2 := now.Add(-30 * time.Minute)
		ts3 := now.Add(-5 * time.Minute)
		ingestAt(t, ctx, store, tenantID, customerID, meterID, decimal.NewFromInt(10), map[string]any{"model": "gpt-4"}, ts1)
		ingestAt(t, ctx, store, tenantID, customerID, meterID, decimal.NewFromInt(99), map[string]any{"model": "gpt-4"}, ts2)
		ingestAt(t, ctx, store, tenantID, customerID, meterID, decimal.NewFromInt(42), map[string]any{"model": "gpt-4"}, ts3)

		got, err := store.AggregateByPricingRules(ctx, tenantID, customerID, meterID, domain.AggSum, from, to)
		if err != nil {
			t.Fatalf("aggregate: %v", err)
		}
		assertRuleAggregations(t, got, map[string]decimal.Decimal{
			rrvID: decimal.NewFromInt(42),
		})
	})

	t.Run("last_ever ignores period bounds", func(t *testing.T) {
		tenantID := testutil.CreateTestTenant(t, db, "Agg LE")
		customerID := insertTestCustomer(t, db, tenantID, "cus_le")
		meterID := insertTestMeter(t, db, tenantID, "mtr_le", "tokens_le")
		rrvID := insertTestRatingRule(t, db, tenantID, "rate_le")
		insertTestPricingRule(t, db, tenantID, meterID, rrvID,
			map[string]any{"metric": "seats"}, domain.AggLastEver, 100)

		now := time.Now()
		// "Period" is the last hour, but the relevant event is 30 days
		// ago. last_ever should still pick it up.
		from := now.Add(-1 * time.Hour)
		to := now.Add(1 * time.Hour)

		oldTs := now.Add(-30 * 24 * time.Hour)
		ingestAt(t, ctx, store, tenantID, customerID, meterID, decimal.NewFromInt(7), map[string]any{"metric": "seats"}, oldTs)
		// One newer event NOT matching the rule — must not bleed in.
		ingest(t, ctx, store, tenantID, customerID, meterID, decimal.NewFromInt(99), map[string]any{"metric": "other"})

		got, err := store.AggregateByPricingRules(ctx, tenantID, customerID, meterID, domain.AggSum, from, to)
		if err != nil {
			t.Fatalf("aggregate: %v", err)
		}
		// last_ever rule sees the 30-day-old seat-count event; the
		// other event ('metric=other') is unclaimed and rolls into the
		// default sum bucket — but only events INSIDE the period are
		// in that bucket, so unclaimed=99.
		assertRuleAggregations(t, got, map[string]decimal.Decimal{
			rrvID: decimal.NewFromInt(7),  // last_ever: 30d-old seats=7
			"":    decimal.NewFromInt(99), // unclaimed in period
		})
	})

	t.Run("last_ever winner excluded from period buckets (no double-count)", func(t *testing.T) {
		// Regression: an event whose top-priority matching rule is
		// last_ever must NOT also be aggregated by a lower-priority
		// period rule (or the unclaimed default). Before the fix, the
		// period-bounded query filtered last_ever out of the LATERAL
		// candidate set, so such events fell through to the next rule
		// AND were claimed again by the last_ever pass — double-counting.
		tenantID := testutil.CreateTestTenant(t, db, "Agg LE NoDouble")
		customerID := insertTestCustomer(t, db, tenantID, "cus_le_dbl")
		meterID := insertTestMeter(t, db, tenantID, "mtr_le_dbl", "tokens_le_dbl")
		leverRRV := insertTestRatingRule(t, db, tenantID, "rate_le_dbl")
		sumRRV := insertTestRatingRule(t, db, tenantID, "rate_sum_dbl")

		// High-priority last_ever rule + lower-priority sum rule, both
		// matching {model: gpt-4}. The events match BOTH; last_ever wins.
		insertTestPricingRule(t, db, tenantID, meterID, leverRRV,
			map[string]any{"model": "gpt-4"}, domain.AggLastEver, 100)
		insertTestPricingRule(t, db, tenantID, meterID, sumRRV,
			map[string]any{"model": "gpt-4"}, domain.AggSum, 50)

		now := time.Now()
		from := now.Add(-2 * time.Hour)
		to := now.Add(1 * time.Hour)

		ingestAt(t, ctx, store, tenantID, customerID, meterID, decimal.NewFromInt(10), map[string]any{"model": "gpt-4"}, now.Add(-90*time.Minute))
		ingestAt(t, ctx, store, tenantID, customerID, meterID, decimal.NewFromInt(42), map[string]any{"model": "gpt-4"}, now.Add(-5*time.Minute))

		got, err := store.AggregateByPricingRules(ctx, tenantID, customerID, meterID, domain.AggSum, from, to)
		if err != nil {
			t.Fatalf("aggregate: %v", err)
		}
		// Only the last_ever bucket should exist (latest event qty=42).
		// The sum rule and the unclaimed default must be absent — the
		// events belong solely to last_ever.
		assertRuleAggregations(t, got, map[string]decimal.Decimal{
			leverRRV: decimal.NewFromInt(42),
		})
	})

	t.Run("priority claim prevents double-count when rules overlap", func(t *testing.T) {
		tenantID := testutil.CreateTestTenant(t, db, "Agg Priority")
		customerID := insertTestCustomer(t, db, tenantID, "cus_prio")
		meterID := insertTestMeter(t, db, tenantID, "mtr_prio", "tokens_prio")
		highRRV := insertTestRatingRule(t, db, tenantID, "rate_high")
		lowRRV := insertTestRatingRule(t, db, tenantID, "rate_low")

		// Specific rule (priority 100): cached gpt-4 events at premium.
		// Coarse rule (priority 50): all gpt-4 events at base rate.
		// An event matching both must be claimed by the priority-100 rule.
		insertTestPricingRule(t, db, tenantID, meterID, highRRV,
			map[string]any{"model": "gpt-4", "cached": true}, domain.AggSum, 100)
		insertTestPricingRule(t, db, tenantID, meterID, lowRRV,
			map[string]any{"model": "gpt-4"}, domain.AggSum, 50)

		from := time.Now().Add(-1 * time.Hour)
		to := time.Now().Add(1 * time.Hour)

		// Cached gpt-4 → high-priority rule.
		ingest(t, ctx, store, tenantID, customerID, meterID, decimal.NewFromInt(10), map[string]any{"model": "gpt-4", "cached": true})
		// Uncached gpt-4 → matches only low-priority rule.
		ingest(t, ctx, store, tenantID, customerID, meterID, decimal.NewFromInt(20), map[string]any{"model": "gpt-4", "cached": false})
		// Another cached gpt-4 → high.
		ingest(t, ctx, store, tenantID, customerID, meterID, decimal.NewFromInt(5), map[string]any{"model": "gpt-4", "cached": true})

		got, err := store.AggregateByPricingRules(ctx, tenantID, customerID, meterID, domain.AggSum, from, to)
		if err != nil {
			t.Fatalf("aggregate: %v", err)
		}
		assertRuleAggregations(t, got, map[string]decimal.Decimal{
			highRRV: decimal.NewFromInt(15), // 10 + 5 cached gpt-4
			lowRRV:  decimal.NewFromInt(20), // 20 uncached gpt-4
		})
	})

	t.Run("subset match: extra event dimensions do not disqualify", func(t *testing.T) {
		tenantID := testutil.CreateTestTenant(t, db, "Agg Subset")
		customerID := insertTestCustomer(t, db, tenantID, "cus_subset")
		meterID := insertTestMeter(t, db, tenantID, "mtr_subset", "tokens_subset")
		rrvID := insertTestRatingRule(t, db, tenantID, "rate_subset")
		// Rule asks for {model: gpt-4} only — events with extra
		// dimensions (operation, cached, region) must still match.
		insertTestPricingRule(t, db, tenantID, meterID, rrvID,
			map[string]any{"model": "gpt-4"}, domain.AggSum, 100)

		from := time.Now().Add(-1 * time.Hour)
		to := time.Now().Add(1 * time.Hour)

		ingest(t, ctx, store, tenantID, customerID, meterID, decimal.NewFromInt(10),
			map[string]any{"model": "gpt-4", "operation": "input", "cached": false, "region": "us-east"})
		ingest(t, ctx, store, tenantID, customerID, meterID, decimal.NewFromInt(20),
			map[string]any{"model": "gpt-4", "operation": "output", "cached": true})

		got, err := store.AggregateByPricingRules(ctx, tenantID, customerID, meterID, domain.AggSum, from, to)
		if err != nil {
			t.Fatalf("aggregate: %v", err)
		}
		assertRuleAggregations(t, got, map[string]decimal.Decimal{
			rrvID: decimal.NewFromInt(30),
		})
	})

	t.Run("no rules: every event lands in unclaimed bucket", func(t *testing.T) {
		tenantID := testutil.CreateTestTenant(t, db, "Agg NoRules")
		customerID := insertTestCustomer(t, db, tenantID, "cus_nor")
		meterID := insertTestMeter(t, db, tenantID, "mtr_nor", "tokens_nor")

		from := time.Now().Add(-1 * time.Hour)
		to := time.Now().Add(1 * time.Hour)

		ingest(t, ctx, store, tenantID, customerID, meterID, decimal.NewFromInt(40), map[string]any{"model": "gpt-4"})
		ingest(t, ctx, store, tenantID, customerID, meterID, decimal.NewFromInt(2), map[string]any{"model": "claude"})

		got, err := store.AggregateByPricingRules(ctx, tenantID, customerID, meterID, domain.AggSum, from, to)
		if err != nil {
			t.Fatalf("aggregate: %v", err)
		}
		assertRuleAggregations(t, got, map[string]decimal.Decimal{
			"": decimal.NewFromInt(42),
		})
	})

	t.Run("decimal precision round-trips through aggregation", func(t *testing.T) {
		tenantID := testutil.CreateTestTenant(t, db, "Agg Decimal")
		customerID := insertTestCustomer(t, db, tenantID, "cus_dec")
		meterID := insertTestMeter(t, db, tenantID, "mtr_dec", "tokens_dec")
		rrvID := insertTestRatingRule(t, db, tenantID, "rate_dec")
		insertTestPricingRule(t, db, tenantID, meterID, rrvID,
			map[string]any{"model": "gpt-4"}, domain.AggSum, 100)

		from := time.Now().Add(-1 * time.Hour)
		to := time.Now().Add(1 * time.Hour)

		// Use 12-decimal-place quantities — the column is NUMERIC(38, 12).
		q1, _ := decimal.NewFromString("1234.567890123456")
		q2, _ := decimal.NewFromString("0.000000000001")
		want, _ := decimal.NewFromString("1234.567890123457")
		ingest(t, ctx, store, tenantID, customerID, meterID, q1, map[string]any{"model": "gpt-4"})
		ingest(t, ctx, store, tenantID, customerID, meterID, q2, map[string]any{"model": "gpt-4"})

		got, err := store.AggregateByPricingRules(ctx, tenantID, customerID, meterID, domain.AggSum, from, to)
		if err != nil {
			t.Fatalf("aggregate: %v", err)
		}
		var foundRule decimal.Decimal
		for _, r := range got {
			if r.RatingRuleVersionID == rrvID {
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

func assertRuleAggregations(t *testing.T, got []domain.RuleAggregation, want map[string]decimal.Decimal) {
	t.Helper()
	gotByRRV := make(map[string]decimal.Decimal, len(got))
	for _, r := range got {
		// Key by rating_rule_version_id; "" is the unclaimed bucket.
		gotByRRV[r.RatingRuleVersionID] = r.Quantity
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

// insertTestRatingRule creates a minimal flat-rate rating rule version so
// pricing-rule rows have a valid FK target. The actual prices are not
// exercised here — aggregation only cares about the rrv id.
func insertTestRatingRule(t *testing.T, db *postgres.DB, tenantID, key string) string {
	t.Helper()

	id := postgres.NewID("vlx_rrv")
	tx, err := db.BeginTx(postgres.WithLivemode(context.Background(), false), postgres.TxTenant, tenantID)
	if err != nil {
		t.Fatalf("begin rrv: %v", err)
	}
	defer postgres.Rollback(tx)

	_, err = tx.ExecContext(context.Background(), `
		INSERT INTO rating_rule_versions (id, tenant_id, rule_key, name, version, lifecycle_state, mode, currency, flat_amount_cents)
		VALUES ($1, $2, $3, $4, 1, 'active', 'flat', 'USD', 100)
	`, id, tenantID, key, key)
	if err != nil {
		t.Fatalf("insert rrv: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit rrv: %v", err)
	}
	return id
}

// insertTestPricingRule writes a row to meter_pricing_rules. We hit the
// table directly rather than going through the pricing service because
// these tests are scoped to the aggregation query — the service-layer
// coverage already lives in pricing/service_test.go.
func insertTestPricingRule(t *testing.T, db *postgres.DB,
	tenantID, meterID, rrvID string,
	dims map[string]any, mode domain.AggregationMode, priority int,
) {
	t.Helper()

	tx, err := db.BeginTx(postgres.WithLivemode(context.Background(), false), postgres.TxTenant, tenantID)
	if err != nil {
		t.Fatalf("begin mpr: %v", err)
	}
	defer postgres.Rollback(tx)

	dimsJSON, err := jsonbValue(dims)
	if err != nil {
		t.Fatalf("marshal dims: %v", err)
	}

	_, err = tx.ExecContext(context.Background(), `
		INSERT INTO meter_pricing_rules
		    (id, tenant_id, meter_id, rating_rule_version_id, dimension_match, aggregation_mode, priority)
		VALUES ($1, $2, $3, $4, $5::jsonb, $6, $7)
	`, postgres.NewID("vlx_mpr"), tenantID, meterID, rrvID, dimsJSON, string(mode), priority)
	if err != nil {
		t.Fatalf("insert mpr: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit mpr: %v", err)
	}
}

func jsonbValue(m map[string]any) (string, error) {
	if len(m) == 0 {
		return "{}", nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
