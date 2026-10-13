package billing_test

import (
	"database/sql"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/pricing"
	"github.com/sagarsuperuser/velox/internal/usage"
)

// TestLiveBalance_ReadLatency times the live credit read for one customer
// with a large period of usage. It is a measurement, not a gate: it asserts
// only that the figure is right, logs the timings, and runs only when
// VELOX_LIVE_BALANCE_TIMING_EVENTS names the event counts to seed, e.g.
//
//	VELOX_LIVE_BALANCE_TIMING_EVENTS=100000,1000000 go test -p 1 ./internal/billing/ \
//	  -run TestLiveBalance_ReadLatency -count=1 -short=false -v -timeout 30m
//
// The number decides how the periodic pass of issue #1019 step two is built
// (how many customers one pass can afford), so rerun it on the hardware that
// matters before relying on it.
//
// Shape: one active subscription 29 days into a 30-day period, one sum meter,
// events spread evenly over the 29 days, each carrying a model and a
// token_type. Measured twice: with no pricing rules (every event falls to the
// meter's default rule) and with four dimension-matched pricing rules (the
// AI-token shape, where each event is matched against the rules).
func TestLiveBalance_ReadLatency(t *testing.T) {
	spec := os.Getenv("VELOX_LIVE_BALANCE_TIMING_EVENTS")
	if spec == "" {
		t.Skip("measurement only: set VELOX_LIVE_BALANCE_TIMING_EVENTS (e.g. 100000,1000000) to run")
	}
	for _, field := range strings.Split(spec, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(field))
		if err != nil || n <= 0 {
			t.Fatalf("VELOX_LIVE_BALANCE_TIMING_EVENTS: bad count %q", field)
		}
		t.Run(fmt.Sprintf("%d_events", n), func(t *testing.T) { timeLiveRead(t, n) })
	}
}

func timeLiveRead(t *testing.T, events int) {
	lf := newLiveFixture(t, fmt.Sprintf("Live Balance Timing %d", events), 0)
	t.Cleanup(func() {
		// Leave the shared test database small for whoever runs next.
		lf.exec(t, `DELETE FROM usage_events WHERE tenant_id = $1`, lf.tenantID)
	})

	periodStart := time.Now().UTC().Truncate(time.Hour).Add(-29 * 24 * time.Hour)
	periodEnd := periodStart.Add(30 * 24 * time.Hour)
	lf.exec(t, `UPDATE subscriptions SET current_billing_period_start = $1, current_billing_period_end = $2, next_billing_at = $2 WHERE id = $3`,
		periodStart, periodEnd, lf.subID)
	lf.exec(t, `UPDATE billing_intervals SET starts_at = $1 WHERE subscription_id = $2`, periodStart, lf.subID)

	seedStart := time.Now()
	lf.exec(t, `
		INSERT INTO usage_events (id, tenant_id, customer_id, meter_id, quantity, properties, timestamp, livemode)
		SELECT 'vlx_ue_timing_' || g, $1, $2, $3, 1,
		       jsonb_build_object(
		           'model',      (ARRAY['gpt-4o','claude-sonnet'])[1 + g % 2],
		           'token_type', (ARRAY['input','output'])[1 + (g / 2) % 2]),
		       $4::timestamptz + (g * ($5::double precision / $6) * interval '1 second'),
		       false
		FROM generate_series(0, $6 - 1) AS g
	`, lf.tenantID, lf.customerID, lf.meterID, periodStart, (29 * 24 * time.Hour).Seconds(), events)
	t.Logf("seeded %d events in %s", events, time.Since(seedStart).Round(time.Millisecond))

	// Fresh planner statistics, as autovacuum would have them in a running
	// system. ANALYZE needs the table owner, so it goes through the admin URL.
	admin, err := sql.Open("pgx", os.Getenv("TEST_ADMIN_DATABASE_URL"))
	if err != nil {
		t.Fatalf("open admin: %v", err)
	}
	defer func() { _ = admin.Close() }()
	if _, err := admin.Exec(`ANALYZE usage_events`); err != nil {
		t.Fatalf("analyze: %v", err)
	}
	var tableRows int64
	if err := admin.QueryRow(`SELECT count(*) FROM usage_events`).Scan(&tableRows); err != nil {
		t.Fatalf("count: %v", err)
	}
	t.Logf("usage_events holds %d rows in total", tableRows)

	want := int64(events) // quantity 1 per event at 1c
	measure := func(label string) {
		t.Helper()
		const runs = 7
		liveTimes := make([]time.Duration, 0, runs)
		for i := 0; i < runs; i++ {
			start := time.Now()
			live, err := lf.engine.LiveCreditBalance(lf.ctx, lf.tenantID, lf.customerID)
			liveTimes = append(liveTimes, time.Since(start))
			if err != nil {
				t.Fatalf("%s: live read: %v", label, err)
			}
			if live.UnbilledUsageCents != want {
				t.Fatalf("%s: unbilled usage = %d, want %d", label, live.UnbilledUsageCents, want)
			}
		}
		dashTimes := make([]time.Duration, 0, runs)
		var queries, hits int
		for i := 0; i < runs; i++ {
			ctx, memo := usage.WithAggregationMemo(lf.ctx)
			start := time.Now()
			if _, err := lf.dashboard.GetByToken(ctx, lf.token); err != nil {
				t.Fatalf("%s: dashboard: %v", label, err)
			}
			dashTimes = append(dashTimes, time.Since(start))
			queries, hits = memo.Queries(), memo.Hits()
		}
		t.Logf("%-28s live read       first %8s | of the next %d: min %8s median %8s max %8s",
			label, liveTimes[0].Round(time.Millisecond), runs-1, minOf(liveTimes[1:]), medianOf(liveTimes[1:]), maxOf(liveTimes[1:]))
		t.Logf("%-28s public dashboard first %8s | of the next %d: min %8s median %8s max %8s (%d aggregation queries, %d reused)",
			label, dashTimes[0].Round(time.Millisecond), runs-1, minOf(dashTimes[1:]), medianOf(dashTimes[1:]), maxOf(dashTimes[1:]), queries, hits)
	}

	measure(fmt.Sprintf("%d events, no pricing rules", events))

	for _, m := range []map[string]any{
		{"model": "gpt-4o", "token_type": "input"},
		{"model": "gpt-4o", "token_type": "output"},
		{"model": "claude-sonnet", "token_type": "input"},
		{"model": "claude-sonnet", "token_type": "output"},
	} {
		rule := lf.seededRule(t, lf.ctx)
		if _, err := lf.pricingSvc.UpsertMeterPricingRule(lf.ctx, lf.tenantID, pricing.UpsertMeterPricingRuleInput{
			MeterID: lf.meterID, RatingRuleVersionID: rule.ID, DimensionMatch: m, AggregationMode: domain.AggSum,
		}); err != nil {
			t.Fatalf("pricing rule %v: %v", m, err)
		}
	}
	measure(fmt.Sprintf("%d events, 4 pricing rules", events))
}

func sortedCopy(d []time.Duration) []time.Duration {
	out := slices.Clone(d)
	slices.Sort(out)
	return out
}

func minOf(d []time.Duration) time.Duration { return sortedCopy(d)[0].Round(time.Millisecond) }
func maxOf(d []time.Duration) time.Duration {
	return sortedCopy(d)[len(d)-1].Round(time.Millisecond)
}
func medianOf(d []time.Duration) time.Duration {
	return sortedCopy(d)[len(d)/2].Round(time.Millisecond)
}
