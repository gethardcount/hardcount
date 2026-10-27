package pricing_test

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/pricing"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// PriceMatchesAt, ingest's one read of every open period's pins, answers
// each pin with the meter and dimension_match of every price of PlanPricesAt
// at that pin: in wall time, on a test clock with stamps, and on a clock
// without.
//
// Each pin also says whether it had passed in its time domain: a wall pin
// before now, a clock pin at or before the clock's time.
//
// Mutation: read the versions in wall time for every pin (drop the clock),
// and the clock pins answer the wall-time meters. Answer every pin passed →
// the future wall pin and the clock pins ahead of clock C pass.
func TestPostgresStore_PriceMatchesAtMatchesPlanPricesAt(t *testing.T) {
	db := testutil.SetupTestDB(t)
	store := pricing.NewPostgresStore(db)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Meters priced at")

	var meters []string
	for _, ev := range []string{"m1", "m2", "m3"} {
		m, err := store.CreateMeter(ctx, tenantID, domain.Meter{EventName: ev, Name: ev, Unit: "units", Aggregation: "sum"})
		if err != nil {
			t.Fatalf("meter: %v", err)
		}
		meters = append(meters, m.ID)
	}
	m1, m2, m3 := meters[0], meters[1], meters[2]
	perUnit := domain.PriceShape{Mode: domain.PricingPerUnit, UnitAmountCents: decimal.NewFromInt(1)}
	plan, prices, err := store.CreatePlanWithPrices(ctx, tenantID, domain.Plan{
		Code: "mpa", Name: "mpa", Currency: "USD", BillingInterval: domain.BillingMonthly, Status: domain.PlanActive,
	}, []pricing.PriceChange{
		{Op: pricing.PriceAdd, MeterID: m1, Shape: perUnit},
		{Op: pricing.PriceAdd, MeterID: m1, Match: map[string]string{"model": "x", "tier": "1"}, Shape: perUnit},
		{Op: pricing.PriceAdd, MeterID: m2, Shape: perUnit},
	})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}

	// From t1 in wall time, and from t2 on clock C: m2's price is removed
	// and m3 is priced. Clock D has no stamps.
	t1 := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID) // test mode: the rows' livemode
	if err != nil {
		t.Fatal(err)
	}
	defer postgres.Rollback(tx)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := tx.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	for _, c := range []string{"vlx_tclk_mpa_c", "vlx_tclk_mpa_d"} {
		exec(`INSERT INTO test_clocks (id, tenant_id, name, frozen_time, status) VALUES ($1, $2, $1, $3, 'ready')`, c, tenantID, t1)
	}
	exec(`INSERT INTO plan_prices (id, tenant_id, plan_id, meter_id, dimension_match) VALUES ('vlx_prc_mpa3', $1, $2, $3, '{}')`, tenantID, plan.ID, m3)
	exec(`INSERT INTO plan_price_versions (id, tenant_id, price_id, valid_from, removed) VALUES ('vlx_ppv_mpa2', $1, $2, $3, true)`, tenantID, prices[2].ID, t1)
	exec(`INSERT INTO plan_price_versions (id, tenant_id, price_id, valid_from, mode, unit_amount_cents) VALUES ('vlx_ppv_mpa3', $1, 'vlx_prc_mpa3', $2, 'per_unit', 1)`, tenantID, t1)
	for _, v := range []string{"vlx_ppv_mpa2", "vlx_ppv_mpa3"} {
		exec(`INSERT INTO plan_price_version_clocks (tenant_id, version_id, test_clock_id, valid_from) VALUES ($1, $2, 'vlx_tclk_mpa_c', $3)`, tenantID, v, t2)
	}

	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	pins := []pricing.PlanPin{
		{PlanID: plan.ID, At: t1.Add(-time.Hour)},
		{PlanID: plan.ID, At: t1},
		{PlanID: plan.ID, At: t2.Add(-time.Hour), ClockID: "vlx_tclk_mpa_c"},
		{PlanID: plan.ID, At: t2, ClockID: "vlx_tclk_mpa_c"},
		{PlanID: plan.ID, At: t1.Add(-time.Hour), ClockID: "vlx_tclk_mpa_d"},
		{PlanID: "vlx_pln_none", At: t1},
		{PlanID: plan.ID, At: time.Now().UTC().Add(time.Hour)},
		{PlanID: plan.ID, At: t1, ClockID: "vlx_tclk_mpa_d"},
	}
	m1x := m1 + `{"model":"x","tier":"1"}`
	want := [][]string{{m1 + "{}", m1x, m2 + "{}"}, {m1 + "{}", m1x, m3 + "{}"}, {m1 + "{}", m1x, m2 + "{}"}, {m1 + "{}", m1x, m3 + "{}"}, {m1 + "{}", m1x, m3 + "{}"}, nil, {m1 + "{}", m1x, m3 + "{}"}, {m1 + "{}", m1x, m3 + "{}"}}
	wantPassed := []bool{true, true, false, false, true, true, false, true}
	got, err := store.PriceMatchesAt(ctx, tenantID, pins)
	if err != nil {
		t.Fatalf("PriceMatchesAt: %v", err)
	}
	if len(got) != len(pins) {
		t.Fatalf("answers = %d, want %d", len(got), len(pins))
	}
	key := func(meterID string, match map[string]string) string {
		b, err := json.Marshal(match)
		if err != nil {
			t.Fatal(err)
		}
		return meterID + string(b)
	}
	for i, pin := range pins {
		one, err := store.PlanPricesAt(ctx, tenantID, pin.PlanID, pin.At, pin.ClockID)
		if err != nil {
			t.Fatalf("PlanPricesAt: %v", err)
		}
		var single, g []string
		for _, p := range one {
			single = append(single, key(p.MeterID, p.DimensionMatch))
		}
		if got[i].Passed != wantPassed[i] {
			t.Errorf("pin %d %+v: passed = %v, want %v", i, pin, got[i].Passed, wantPassed[i])
		}
		for _, pm := range got[i].Matches {
			g = append(g, key(pm.MeterID, pm.Match))
		}
		slices.Sort(single)
		slices.Sort(g)
		w := slices.Clone(want[i])
		slices.Sort(w)
		if fmt.Sprint(g) != fmt.Sprint(single) || fmt.Sprint(g) != fmt.Sprint(w) {
			t.Errorf("pin %d %+v: PriceMatchesAt %v, PlanPricesAt %v, want %v", i, pin, g, single, w)
		}
	}
}
